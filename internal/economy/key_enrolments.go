package economy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/passkey"
)

// key_enrolments.go — B55.1: A NEW KEY JOINS FROM ANOTHER DEVICE, SIGNED BY A KEY THE WORKSPACE ALREADY HAS.
//
// A key on one device cannot sign a registration running on another, so a passkey or phone key registered with no
// assertion in a workspace that already has a key is not refused: its registration is verified and its challenge spent
// as before, and it waits in key_enrolments, signing nothing, for 10 minutes. The workspace's phones are pushed; an
// existing passkey or unrevoked phone key then approves it over a challenge naming it (enrol:{id}), under the same
// key_enrol lock authorizeEnrol holds, and only then is it copied into workspace_passkeys or workspace_device_keys.

// enrolmentTTL is how long a waiting key may be let in; maxWaitingEnrolments how many may wait at once per workspace.
const (
	enrolmentTTL         = 10 * time.Minute
	maxWaitingEnrolments = 3
)

// ErrEnrolmentNotFound: no such enrolment waiting in the workspace (declined, expired, approved or another's).
var ErrEnrolmentNotFound = errors.New("economy: no such key waiting to join this workspace")

// ErrTooManyEnrolments: maxWaitingEnrolments keys already wait in the workspace.
var ErrTooManyEnrolments = errors.New("Three keys already wait to join this workspace. Answer one of them first.") //nolint:staticcheck // ST1005: shown to a person as this sentence (B55.50)

// ErrEnrolmentCodeTaken: a key with the same code already waits, so the two could not be told apart on screen.
var ErrEnrolmentCodeTaken = errors.New("A key with the same code already waits to join this workspace. Answer that one first.") //nolint:staticcheck // ST1005: shown to a person as this sentence (B55.50)

// ErrPasskeyRegistered (B54.28): the passkey is already a workspace's key.
var ErrPasskeyRegistered = errors.New("This passkey is already added. Use it as it is, or make a new passkey on this device.") //nolint:staticcheck // ST1005: shown to a person as this sentence (B55.50)

// ErrNoLiveKey (B54.28): every key the workspace has is revoked, so none could let a waiting key in.
var ErrNoLiveKey = errors.New("Every key of this workspace was removed, so no key can let a new one in.") //nolint:staticcheck // ST1005: shown to a person as this sentence (B55.50)

// KeyEnrolment is a key waiting to join a workspace. Status is waiting, approved, declined or expired.
type KeyEnrolment struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Platform  string    `json:"platform,omitempty"`
	Code      string    `json:"code"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// EnrolmentWaiting is the error RegisterPasskey and RegisterDeviceKey return when the key was kept waiting instead.
type EnrolmentWaiting struct{ Enrolment KeyEnrolment }

func (e *EnrolmentWaiting) Error() string {
	return "economy: the new key waits for a key this workspace already has to let it in"
}

// enrolmentCode is the 6 digits both devices show for a key: the first 4 bytes of the SHA-256 of its public key (SPKI,
// DER), big-endian, mod 1,000,000, zero-padded.
func enrolmentCode(spki []byte) string {
	sum := sha256.Sum256(spki)
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1_000_000)
}

// waitEnrolment keeps e (with its key material) waiting, inside tx, which holds the key_enrol lock authorizeEnrol took,
// spends the challenge the registration answered, commits and pushes the workspace's phones.
func (s *DualTokenStore) waitEnrolment(ctx context.Context, tx pgx.Tx, workspaceID, purpose, challenge string, e KeyEnrolment,
	person, credentialID string, publicKey []byte, signCount int64, attestation []byte) error {
	var live int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1 AND revoked_at IS NULL)`, workspaceID).Scan(&live); err != nil {
		return fmt.Errorf("economy: key enrolments: %w", err)
	}
	if live == 0 {
		return ErrNoLiveKey
	}
	if err := useChallenge(ctx, tx, workspaceID, purpose, challenge); err != nil {
		return err
	}
	e.ID, e.Code, e.Status = "enr_"+uuid.NewString(), enrolmentCode(publicKey), "waiting"
	var waiting, sameCode int
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE code = $2) FROM key_enrolments
		WHERE workspace_id = $1 AND status = 'waiting' AND expires_at > now()`, workspaceID, e.Code).Scan(&waiting, &sameCode); err != nil {
		return fmt.Errorf("economy: key enrolments: %w", err)
	}
	if waiting >= maxWaitingEnrolments {
		return ErrTooManyEnrolments
	}
	// A key ground to show a waiting key's code would pass for it on the approving screen.
	if sameCode > 0 {
		return ErrEnrolmentCodeTaken
	}
	if err := tx.QueryRow(ctx, `INSERT INTO key_enrolments (id, workspace_id, kind, name, platform, person, credential_id, public_key,
		sign_count, attestation, code, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING created_at, expires_at`, e.ID, workspaceID, e.Kind, e.Name, e.Platform, person, credentialID, publicKey, signCount,
		append([]byte{}, attestation...), e.Code, time.Now().Add(enrolmentTTL)).Scan(&e.CreatedAt, &e.ExpiresAt); err != nil {
		return fmt.Errorf("economy: key enrolment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.pushDevices(ctx, workspaceID, nativepush.Message{Body: "A new device asks to approve payments. Open Talyvor to check it.",
			Data: map[string]string{"type": "key_enrolment", "enrolment_id": e.ID, "workspace_id": workspaceID}})
	}()
	return &EnrolmentWaiting{Enrolment: e}
}

const keyEnrolmentCols = `id, kind, name, platform, code,
	CASE WHEN status = 'waiting' AND expires_at <= now() THEN 'expired' ELSE status END, created_at, expires_at`

func scanKeyEnrolment(row pgx.Row) (KeyEnrolment, error) {
	var e KeyEnrolment
	err := row.Scan(&e.ID, &e.Kind, &e.Name, &e.Platform, &e.Code, &e.Status, &e.CreatedAt, &e.ExpiresAt)
	return e, err
}

// ListKeyEnrolments reads the keys waiting to join workspaceID, oldest first.
func (s *DualTokenStore) ListKeyEnrolments(ctx context.Context, workspaceID string) ([]KeyEnrolment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+keyEnrolmentCols+` FROM key_enrolments
		WHERE workspace_id = $1 AND status = 'waiting' AND expires_at > now() ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: key enrolments: %w", err)
	}
	defer rows.Close()
	out := []KeyEnrolment{}
	for rows.Next() {
		e, err := scanKeyEnrolment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetKeyEnrolment reads one enrolment of workspaceID, whatever its status.
func (s *DualTokenStore) GetKeyEnrolment(ctx context.Context, workspaceID, id string) (KeyEnrolment, error) {
	e, err := scanKeyEnrolment(s.pool.QueryRow(ctx, `SELECT `+keyEnrolmentCols+` FROM key_enrolments WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrEnrolmentNotFound
	}
	return e, err
}

// KeyEnrolmentChallenge issues the challenge approving enrolment id is signed over, and the workspace's passkeys that
// may sign it (a phone key signs it too).
func (s *DualTokenStore) KeyEnrolmentChallenge(ctx context.Context, workspaceID, id string) (challenge string, credentialIDs []string, err error) {
	e, err := s.GetKeyEnrolment(ctx, workspaceID, id)
	if err != nil {
		return "", nil, err
	}
	if e.Status != "waiting" {
		return "", nil, ErrEnrolmentNotFound
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, "enrol:"+id)
	return c, credentialIDs, err
}

// ApproveKeyEnrolment lets waiting enrolment id in: only with a's assertion, by one of workspaceID's passkeys or
// unrevoked phone keys, over KeyEnrolmentChallenge's challenge, which it spends. The key is then the workspace's.
func (s *DualTokenStore) ApproveKeyEnrolment(ctx context.Context, workspaceID, id string, a *passkey.Assertion) (KeyEnrolment, error) {
	if a == nil {
		return KeyEnrolment{}, ErrPasskeyRequired
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return KeyEnrolment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('key_enrol:' || $1, 0))`, workspaceID); err != nil {
		return KeyEnrolment{}, fmt.Errorf("economy: key enrolment: %w", err)
	}
	var kind, name, platform, person, credentialID string
	var publicKey, attestation []byte
	var signCount int64
	err = tx.QueryRow(ctx, `SELECT kind, name, platform, person, credential_id, public_key, sign_count, attestation FROM key_enrolments
		WHERE id = $1 AND workspace_id = $2 AND status = 'waiting' AND expires_at > now() FOR UPDATE`, id, workspaceID).
		Scan(&kind, &name, &platform, &person, &credentialID, &publicKey, &signCount, &attestation)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyEnrolment{}, ErrEnrolmentNotFound
	}
	if err != nil {
		return KeyEnrolment{}, fmt.Errorf("economy: key enrolment: %w", err)
	}
	if err := s.spendAssertion(ctx, tx, workspaceID, "enrol:"+id, *a); err != nil {
		return KeyEnrolment{}, err
	}
	if kind == "passkey" {
		_, err = tx.Exec(ctx, `INSERT INTO workspace_passkeys (credential_id, workspace_id, name, public_key, sign_count) VALUES ($1, $2, $3, $4, $5)`,
			credentialID, workspaceID, name, publicKey, signCount)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, person, platform, name, public_key, attestation)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, "dev_"+uuid.NewString(), workspaceID, person, platform, name, publicKey, attestation)
	}
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
		return KeyEnrolment{}, ErrPasskeyRegistered // registered elsewhere while it waited
	}
	if err != nil {
		return KeyEnrolment{}, fmt.Errorf("economy: key enrolment: add the key: %w", err)
	}
	e, err := scanKeyEnrolment(tx.QueryRow(ctx, `UPDATE key_enrolments SET status = 'approved', decided_at = now() WHERE id = $1
		RETURNING `+keyEnrolmentCols, id))
	if err != nil {
		return e, fmt.Errorf("economy: key enrolment: %w", err)
	}
	return e, tx.Commit(ctx)
}

// DeclineKeyEnrolment turns waiting enrolment id away; it can never be let in after.
func (s *DualTokenStore) DeclineKeyEnrolment(ctx context.Context, workspaceID, id string) (KeyEnrolment, error) {
	e, err := scanKeyEnrolment(s.pool.QueryRow(ctx, `UPDATE key_enrolments SET status = 'declined', decided_at = now()
		WHERE id = $1 AND workspace_id = $2 AND status = 'waiting' AND expires_at > now() RETURNING `+keyEnrolmentCols, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrEnrolmentNotFound
	}
	return e, err
}
