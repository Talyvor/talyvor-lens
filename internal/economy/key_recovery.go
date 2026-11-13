package economy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/passkey"
)

// key_recovery.go — B55.48: A WORKSPACE LEFT WITH NO LIVE KEY GETS ONE BACK, BY ITS RECOVERY CODE OR AFTER 72 HOURS.
//
// A workspace whose every passkey and phone key was revoked or removed approves nothing and, since authorizeEnrol counts
// those keys, lets no new key in. Its owner has two ways back, and only while no key is live and no lockdown holds:
//   - the recovery code, made with the workspace's first key (or by NewRecoveryCode, signed like any key change) and kept
//     only as a hash: it adds a key at once, is spent, and a new code is answered in its place. Five wrong codes an hour,
//     per workspace and per IP, are all anyone may try;
//   - a recovery request: everyone with a push subscription or a phone key of the workspace is told, any owner or admin
//     can cancel it, and from 72 hours after it began the person who began it may add a key, for 24 hours.
//
// Both add the key under the key_enrol lock authorizeEnrol takes, in one transaction with the audit entry. No other way
// lets a key in without a live key's signature.

const (
	recoveryWait          = 72 * time.Hour
	recoveryWindow        = 24 * time.Hour
	recoveryCodeTries     = 5
	recoveryCodeTriesSpan = time.Hour
)

// ErrHasLiveKey: the workspace still has a live key, which lets a new one in instead.
var ErrHasLiveKey = errors.New("economy: this workspace still has a live key")

// HasLiveKeySentence is what the owner is told when a recovery is refused with ErrHasLiveKey.
const HasLiveKeySentence = "This workspace still has a key. Add the new one from Approvals and devices on a device that has a key."

// ErrNoKeyToRecover: the workspace never had a key, so its first one is added as usual, for free.
var ErrNoKeyToRecover = errors.New("This workspace has no key yet. Add one from Approvals and devices.") //nolint:staticcheck // ST1005: shown to a person as this sentence

// ErrRecoveryCodeWrong: the code is not the workspace's live recovery code.
var ErrRecoveryCodeWrong = errors.New("That recovery code is not right. Check it and try again.") //nolint:staticcheck // ST1005: shown to a person as this sentence

// ErrRecoveryLockedDown: a lockdown holds the workspace, and only lifting it lets a key in (B41.116).
var ErrRecoveryLockedDown = errors.New("This workspace is locked down. Lift the lockdown first, then add a key.") //nolint:staticcheck // ST1005: shown to a person as this sentence

// ErrRecoveryWaiting: a recovery of the workspace already waits.
var ErrRecoveryWaiting = errors.New("A recovery of this workspace has already started.") //nolint:staticcheck // ST1005: shown to a person as this sentence

// ErrRecoveryNotFound: no recovery of the workspace waits.
var ErrRecoveryNotFound = errors.New("No recovery of this workspace is waiting.") //nolint:staticcheck // ST1005: shown to a person as this sentence

// ErrRecoveryNotYours: the recovery was started by someone else, and only they finish it.
var ErrRecoveryNotYours = errors.New("Only the person who started this recovery can add the new key.") //nolint:staticcheck // ST1005: shown to a person as this sentence

// ErrRecoveryExpired: no key was added within recoveryWindow of ready_at.
var ErrRecoveryExpired = errors.New("This recovery ended because no key was added within a day. Start a new one.") //nolint:staticcheck // ST1005: shown to a person as this sentence

// RecoveryCodeLimited: too many wrong codes within the hour, for the workspace or the IP.
type RecoveryCodeLimited struct{ RetryAfter time.Duration }

func (e *RecoveryCodeLimited) Error() string {
	return "Too many wrong recovery codes. Try again in " + recoveryWaitWords(e.RetryAfter) + "."
}

// RecoveryNotReady: the recovery waits until ReadyAt.
type RecoveryNotReady struct{ ReadyAt time.Time }

func (e *RecoveryNotReady) Error() string {
	return "A new key can be added from " + recoveryTimeWords(e.ReadyAt) + "."
}

func recoveryWaitWords(d time.Duration) string {
	if m := int(math.Ceil(d.Minutes())); m > 1 {
		return fmt.Sprintf("%d minutes", m)
	}
	return "a minute"
}

func recoveryTimeWords(t time.Time) string { return t.UTC().Format("2 Jan 2006 at 15:04 UTC") }

// RecoveryKey is the key a recovery adds: a passkey or a phone key, registered as POST …/agents/passkeys or
// POST /v1/money/devices takes it, over a challenge of the workspace's for that route.
type RecoveryKey struct {
	Name    string
	Person  string // a phone key's person
	Passkey *passkey.Registration
	Device  *devicekey.Registration
}

// RecoveredKey is the key a recovery added and the workspace's new recovery code, shown this once.
type RecoveredKey struct {
	Key          Key    `json:"key"`
	RecoveryCode string `json:"recovery_code"`
}

// KeyRecovery is a recovery started without the code.
type KeyRecovery struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"` // waiting, cancelled, completed or expired
	CreatedAt time.Time `json:"created_at"`
	ReadyAt   time.Time `json:"ready_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Mine      bool      `json:"mine"` // started by the person asking, who alone adds its key
	userID    string
}

const recoveryCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford's base32

// newRecoveryCode is 100 random bits as 20 Crockford base32 characters, in five groups of four.
func newRecoveryCode() (string, error) {
	raw := make([]byte, 13)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i := range 20 {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		bit, v := i*5, 0
		for j := range 5 {
			v = v<<1 | int(raw[(bit+j)/8]>>(7-(bit+j)%8)&1)
		}
		b.WriteByte(recoveryCodeAlphabet[v])
	}
	return b.String(), nil
}

// normaliseRecoveryCode is code as typed, without spaces or hyphens, in upper case, with O read as 0 and I or L as 1;
// "" when it is not 20 Crockford base32 characters.
func normaliseRecoveryCode(code string) string {
	n := strings.NewReplacer(" ", "", "-", "", "O", "0", "I", "1", "L", "1").Replace(strings.ToUpper(code))
	if len(n) != 20 || strings.Trim(n, recoveryCodeAlphabet) != "" {
		return ""
	}
	return n
}

func recoveryCodeHash(workspaceID, normalised string) string {
	h := sha256.Sum256([]byte(workspaceID + ":" + normalised))
	return hex.EncodeToString(h[:])
}

// issueRecoveryCode makes workspaceID a new recovery code inside tx and deletes the one not yet used.
func issueRecoveryCode(ctx context.Context, tx pgx.Tx, workspaceID string) (string, error) {
	code, err := newRecoveryCode()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_recovery_codes WHERE workspace_id = $1 AND used_at IS NULL`, workspaceID); err != nil {
		return "", fmt.Errorf("economy: recovery code: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_recovery_codes (workspace_id, hash) VALUES ($1, $2)`,
		workspaceID, recoveryCodeHash(workspaceID, normaliseRecoveryCode(code))); err != nil {
		return "", fmt.Errorf("economy: recovery code: %w", err)
	}
	return code, nil
}

// firstKeyRecoveryCode is a new recovery code when the key just added inside tx is workspaceID's first, and "" otherwise.
func firstKeyRecoveryCode(ctx context.Context, tx pgx.Tx, workspaceID string) (string, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1)`, workspaceID).Scan(&n); err != nil {
		return "", fmt.Errorf("economy: recovery code: %w", err)
	}
	if n != 1 {
		return "", nil
	}
	return issueRecoveryCode(ctx, tx, workspaceID)
}

// recoveryCodePurpose is what a new recovery code is signed for.
const recoveryCodePurpose = "recovery-code:new"

// RecoveryCodeChallenge issues the challenge a new recovery code is signed over, and the passkeys that may sign it.
func (s *DualTokenStore) RecoveryCodeChallenge(ctx context.Context, workspaceID string) (challenge string, credentialIDs []string, err error) {
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, recoveryCodePurpose)
	return c, credentialIDs, err
}

// NewRecoveryCode makes workspaceID a new recovery code and voids the old one, signed as any key change is
// (authorizeSigned), and records it in the audit chain; the code is answered this once.
func (s *DualTokenStore) NewRecoveryCode(ctx context.Context, workspaceID, by string, a *passkey.Assertion) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.authorizeSignedTx(ctx, tx, workspaceID, recoveryCodePurpose, a, ErrPasskeyRequired); err != nil {
		return "", err
	}
	code, err := issueRecoveryCode(ctx, tx, workspaceID)
	if err != nil {
		return "", err
	}
	if err := appendAudit(ctx, tx, workspaceID, "recovery:"+workspaceID, "recovery_code_made", map[string]any{"by": by}); err != nil {
		return "", err
	}
	return code, tx.Commit(ctx)
}

// lockForRecovery takes workspaceID's key_enrol lock inside tx and lets a recovery on only while the workspace has a key,
// none of them live, and no lockdown holds it.
func lockForRecovery(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('key_enrol:' || $1, 0))`, workspaceID); err != nil {
		return fmt.Errorf("economy: recovery: %w", err)
	}
	var all, live int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1),
		(SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1 AND removed_at IS NULL AND revoked_at IS NULL)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1 AND revoked_at IS NULL)`, workspaceID).Scan(&all, &live); err != nil {
		return fmt.Errorf("economy: recovery: %w", err)
	}
	switch {
	case live > 0:
		return ErrHasLiveKey
	case all == 0:
		return ErrNoKeyToRecover
	}
	if err := refuseIfLockedDown(ctx, tx, workspaceID); errors.Is(err, ErrLockedDown) {
		return ErrRecoveryLockedDown
	} else if err != nil {
		return err
	}
	return nil
}

// addRecoveredKey verifies k against the registration challenge it answers, spends it, and keeps the key live, inside tx.
func (s *DualTokenStore) addRecoveredKey(ctx context.Context, tx pgx.Tx, workspaceID string, k RecoveryKey) (Key, error) {
	switch {
	case k.Passkey != nil && k.Device == nil:
		if s.approvalRP == nil {
			return Key{}, errors.New("economy: passkeys are not configured on this server")
		}
		r := *k.Passkey
		challenge := passkey.ChallengeOf(r.ClientDataJSON)
		count, err := s.approvalRP.VerifyRegistration(r, challenge)
		if err != nil {
			return Key{}, err
		}
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_passkeys WHERE credential_id = $1)`, r.CredentialID).Scan(&known); err != nil {
			return Key{}, fmt.Errorf("economy: recovery: %w", err)
		}
		if known {
			return Key{}, ErrPasskeyRegistered
		}
		if err := useChallenge(ctx, tx, workspaceID, "register", challenge); err != nil {
			return Key{}, err
		}
		key := Key{Kind: "passkey", ID: r.CredentialID, Name: k.Name}
		if err := tx.QueryRow(ctx, `INSERT INTO workspace_passkeys (credential_id, workspace_id, name, public_key, sign_count)
			VALUES ($1, $2, $3, $4, $5) RETURNING created_at`, r.CredentialID, workspaceID, k.Name, r.PublicKey, int64(count)).Scan(&key.CreatedAt); err != nil {
			return Key{}, fmt.Errorf("economy: recovery: %w", err)
		}
		return key, nil
	case k.Device != nil && k.Passkey == nil:
		r := *k.Device
		if err := s.deviceVerifier.VerifyRegistration(r); err != nil {
			return Key{}, err
		}
		if err := useChallenge(ctx, tx, workspaceID, "device-register", r.Challenge); err != nil {
			return Key{}, fmt.Errorf("%w: %v", devicekey.ErrInvalid, err)
		}
		key := Key{Kind: "phone", ID: "dev_" + uuid.NewString(), Name: k.Name, Platform: r.Platform}
		if err := tx.QueryRow(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, person, platform, name, public_key, attestation)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`, key.ID, workspaceID, k.Person, r.Platform, k.Name, r.PublicKey,
			append([]byte{}, r.Attestation...)).Scan(&key.CreatedAt); err != nil {
			return Key{}, fmt.Errorf("economy: recovery: %w", err)
		}
		return key, nil
	}
	return Key{}, fmt.Errorf("%w: send one passkey or one phone key", passkey.ErrInvalid)
}

// RecoverWithCode adds k to workspaceID, which has keys but none live, when code is its live recovery code; the code is
// then spent and a new one answered in its place. A wrong code is counted, and once recoveryCodeTries were wrong within
// the hour for the workspace or for ip, every try is refused with *RecoveryCodeLimited before anything else is checked.
func (s *DualTokenStore) RecoverWithCode(ctx context.Context, workspaceID, ip, by, code string, k RecoveryKey) (RecoveredKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RecoveredKey{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('key_enrol:' || $1, 0))`, workspaceID); err != nil {
		return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
	}
	var wsWrong, ipWrong int
	var retry float64
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE workspace_id = $1), count(*) FILTER (WHERE ip = $2 AND $2 <> ''),
		COALESCE(EXTRACT(EPOCH FROM min(failed_at) + make_interval(secs => $3) - now()), 0)
		FROM key_recovery_failures WHERE failed_at > now() - make_interval(secs => $3) AND (workspace_id = $1 OR (ip = $2 AND $2 <> ''))`,
		workspaceID, ip, recoveryCodeTriesSpan.Seconds()).Scan(&wsWrong, &ipWrong, &retry); err != nil {
		return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
	}
	if wsWrong >= recoveryCodeTries || ipWrong >= recoveryCodeTries {
		return RecoveredKey{}, &RecoveryCodeLimited{RetryAfter: max(time.Duration(retry*float64(time.Second)), time.Second)}
	}
	if err := lockForRecovery(ctx, tx, workspaceID); err != nil {
		return RecoveredKey{}, err
	}
	var stored string
	err = tx.QueryRow(ctx, `SELECT hash FROM workspace_recovery_codes WHERE workspace_id = $1 AND used_at IS NULL FOR UPDATE`, workspaceID).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
	}
	given := recoveryCodeHash(workspaceID, normaliseRecoveryCode(code))
	if subtle.ConstantTimeCompare([]byte(given), []byte(stored)) != 1 || stored == "" {
		if _, err := tx.Exec(ctx, `INSERT INTO key_recovery_failures (workspace_id, ip) VALUES ($1, $2)`, workspaceID, ip); err != nil {
			return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return RecoveredKey{}, err
		}
		return RecoveredKey{}, ErrRecoveryCodeWrong
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_recovery_codes SET used_at = now() WHERE workspace_id = $1 AND hash = $2`, workspaceID, stored); err != nil {
		return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
	}
	out, err := s.finishRecovery(ctx, tx, workspaceID, by, "code", k)
	if err != nil {
		return RecoveredKey{}, err
	}
	s.notifyRecovery(workspaceID, "key_recovered", func(ws string) string { return "A new key was added to " + ws + " with its recovery code." })
	return out, nil
}

// finishRecovery adds k, records it as recovered way, makes a new recovery code and commits tx.
func (s *DualTokenStore) finishRecovery(ctx context.Context, tx pgx.Tx, workspaceID, by, way string, k RecoveryKey) (RecoveredKey, error) {
	key, err := s.addRecoveredKey(ctx, tx, workspaceID, k)
	if err != nil {
		return RecoveredKey{}, err
	}
	if err := appendAudit(ctx, tx, workspaceID, "recovery:"+workspaceID, "key_recovered",
		map[string]any{"way": way, "by": by, "kind": key.Kind, "key": key.ID}); err != nil {
		return RecoveredKey{}, err
	}
	// A recovery still waiting is over once a key is back, so it never adds another after this one is lost.
	if _, err := tx.Exec(ctx, `UPDATE recovery_requests SET status = 'cancelled', cancelled_by = 'superseded'
		WHERE workspace_id = $1 AND status = 'waiting'`, workspaceID); err != nil {
		return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
	}
	code, err := issueRecoveryCode(ctx, tx, workspaceID)
	if err != nil {
		return RecoveredKey{}, err
	}
	return RecoveredKey{Key: key, RecoveryCode: code}, tx.Commit(ctx)
}

const keyRecoveryCols = `id, status, created_at, ready_at, user_id`

func scanKeyRecovery(row pgx.Row, userID string) (KeyRecovery, error) {
	var r KeyRecovery
	err := row.Scan(&r.ID, &r.Status, &r.CreatedAt, &r.ReadyAt, &r.userID)
	r.ExpiresAt, r.Mine = r.ReadyAt.Add(recoveryWindow), r.userID == userID
	return r, err
}

// expireRecoveries marks workspaceID's waiting recovery expired once recoveryWindow has passed since its ready_at.
func expireRecoveries(ctx context.Context, q pgxDB, workspaceID string) error {
	if _, err := q.Exec(ctx, `UPDATE recovery_requests SET status = 'expired'
		WHERE workspace_id = $1 AND status = 'waiting' AND ready_at + make_interval(secs => $2) <= now()`, workspaceID, recoveryWindow.Seconds()); err != nil {
		return fmt.Errorf("economy: recovery: %w", err)
	}
	return nil
}

// StartKeyRecovery starts a recovery of workspaceID, which has keys but none live, by userID: a key can be added from
// 72 hours on, unless someone cancels it, and everyone with a push subscription or phone key of the workspace is told.
func (s *DualTokenStore) StartKeyRecovery(ctx context.Context, workspaceID, userID string) (KeyRecovery, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return KeyRecovery{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockForRecovery(ctx, tx, workspaceID); err != nil {
		return KeyRecovery{}, err
	}
	if err := expireRecoveries(ctx, tx, workspaceID); err != nil {
		return KeyRecovery{}, err
	}
	r, err := scanKeyRecovery(tx.QueryRow(ctx, `INSERT INTO recovery_requests (id, workspace_id, user_id, ready_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4)) RETURNING `+keyRecoveryCols,
		"rcv_"+uuid.NewString(), workspaceID, userID, recoveryWait.Seconds()), userID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return KeyRecovery{}, ErrRecoveryWaiting
	}
	if err != nil {
		return KeyRecovery{}, fmt.Errorf("economy: recovery: %w", err)
	}
	if err := appendAudit(ctx, tx, workspaceID, "recovery:"+workspaceID, "key_recovery_started",
		map[string]any{"by": userID, "recovery": r.ID, "ready_at": r.ReadyAt.UTC().Format(time.RFC3339Nano)}); err != nil {
		return KeyRecovery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return KeyRecovery{}, err
	}
	s.notifyRecovery(workspaceID, "key_recovery_started", func(ws string) string {
		return "The owner started a recovery of " + ws + ". A new key can be added from " + recoveryTimeWords(r.ReadyAt) + " unless someone cancels it."
	})
	return r, nil
}

// WaitingKeyRecovery is workspaceID's waiting recovery, as userID sees it; nil when none waits.
func (s *DualTokenStore) WaitingKeyRecovery(ctx context.Context, workspaceID, userID string) (*KeyRecovery, error) {
	if err := expireRecoveries(ctx, s.pool, workspaceID); err != nil {
		return nil, err
	}
	r, err := scanKeyRecovery(s.pool.QueryRow(ctx, `SELECT `+keyRecoveryCols+` FROM recovery_requests
		WHERE workspace_id = $1 AND status = 'waiting'`, workspaceID), userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("economy: recovery: %w", err)
	}
	return &r, nil
}

// CancelKeyRecovery cancels workspaceID's waiting recovery, by by, and records it in the audit chain.
func (s *DualTokenStore) CancelKeyRecovery(ctx context.Context, workspaceID, by string) (KeyRecovery, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return KeyRecovery{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := expireRecoveries(ctx, tx, workspaceID); err != nil {
		return KeyRecovery{}, err
	}
	r, err := scanKeyRecovery(tx.QueryRow(ctx, `UPDATE recovery_requests SET status = 'cancelled', cancelled_by = $2
		WHERE workspace_id = $1 AND status = 'waiting' RETURNING `+keyRecoveryCols, workspaceID, by), "")
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyRecovery{}, ErrRecoveryNotFound
	}
	if err != nil {
		return KeyRecovery{}, fmt.Errorf("economy: recovery: %w", err)
	}
	if err := appendAudit(ctx, tx, workspaceID, "recovery:"+workspaceID, "key_recovery_cancelled",
		map[string]any{"by": by, "recovery": r.ID}); err != nil {
		return KeyRecovery{}, err
	}
	return r, tx.Commit(ctx)
}

// CompleteKeyRecovery adds k to workspaceID through its waiting recovery: only for userID, who started it, from its
// ready_at until recoveryWindow later, and only while the workspace still has no live key.
func (s *DualTokenStore) CompleteKeyRecovery(ctx context.Context, workspaceID, userID string, k RecoveryKey) (RecoveredKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RecoveredKey{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockForRecovery(ctx, tx, workspaceID); err != nil {
		return RecoveredKey{}, err
	}
	var r KeyRecovery
	var ready, expired, superseded bool
	// superseded: a key was added since it began (after a lockdown was lifted, say), so it no longer stands.
	err = tx.QueryRow(ctx, `SELECT `+keyRecoveryCols+`, ready_at <= now(), ready_at + make_interval(secs => $2) <= now(),
		EXISTS (SELECT 1 FROM workspace_passkeys k WHERE k.workspace_id = $1 AND k.created_at > r.created_at)
		OR EXISTS (SELECT 1 FROM workspace_device_keys d WHERE d.workspace_id = $1 AND d.created_at > r.created_at)
		FROM recovery_requests r WHERE workspace_id = $1 AND status = 'waiting' FOR UPDATE OF r`, workspaceID, recoveryWindow.Seconds()).
		Scan(&r.ID, &r.Status, &r.CreatedAt, &r.ReadyAt, &r.userID, &ready, &expired, &superseded)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return RecoveredKey{}, ErrRecoveryNotFound
	case err != nil:
		return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
	case superseded:
		if _, err := tx.Exec(ctx, `UPDATE recovery_requests SET status = 'cancelled', cancelled_by = 'superseded' WHERE id = $1`, r.ID); err != nil {
			return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return RecoveredKey{}, err
		}
		return RecoveredKey{}, ErrRecoveryNotFound
	case r.userID != userID:
		return RecoveredKey{}, ErrRecoveryNotYours
	case expired:
		if err := expireRecoveries(ctx, tx, workspaceID); err != nil {
			return RecoveredKey{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RecoveredKey{}, err
		}
		return RecoveredKey{}, ErrRecoveryExpired
	case !ready:
		return RecoveredKey{}, &RecoveryNotReady{ReadyAt: r.ReadyAt}
	}
	if _, err := tx.Exec(ctx, `UPDATE recovery_requests SET status = 'completed', completed_at = now() WHERE id = $1`, r.ID); err != nil {
		return RecoveredKey{}, fmt.Errorf("economy: recovery: %w", err)
	}
	out, err := s.finishRecovery(ctx, tx, workspaceID, userID, "wait", k)
	if err != nil {
		return RecoveredKey{}, err
	}
	s.notifyRecovery(workspaceID, "key_recovered", func(ws string) string { return "A new key was added to " + ws + " after its recovery wait." })
	return out, nil
}

// notifyRecovery tells every phone key and browser subscribed for workspaceID, in the background, the sentence body
// makes from the workspace's name.
func (s *DualTokenStore) notifyRecovery(workspaceID, kind string, body func(workspace string) string) {
	if s.approvalPusher == nil && s.nativePusher == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var name string
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), '')`, workspaceID).Scan(&name)
		if name == "" || name == workspaceID {
			name = "your workspace"
		}
		text := body(name)
		s.pushDevices(ctx, workspaceID, nativepush.Message{Title: "Your keys", Body: text,
			Data: map[string]string{"type": kind, "workspace_id": workspaceID}})
		payload, _ := json.Marshal(map[string]any{"type": kind, "workspace_id": workspaceID, "body": text})
		s.pushWeb(ctx, workspaceID, payload)
	}()
}
