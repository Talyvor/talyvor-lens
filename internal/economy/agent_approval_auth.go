package economy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/approvalchat"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/webpush"
)

// agent_approval_auth.go — B19.16: APPROVALS SIGNED WITH A PASSKEY, AND A WEB PUSH WHEN ONE IS FILED.
//
// A workspace's owner registers passkeys; once the workspace has one, approving or denying an agent's
// request takes a fresh assertion from one of them over a single-use challenge naming that approval
// (AuthorizeApprovalDecision). A workspace with none decides as before. Filing an approval — a request
// refused for its amount (fileApproval) or an agent asking (RequestPaymentApproval) — sends every push
// subscription of the workspace one encrypted push naming the agent, the amount and the reason.

// ErrPasskeyRequired: the workspace approves with a passkey and the decision carried no assertion.
var ErrPasskeyRequired = errors.New("This workspace approves with a passkey or the phone's key, so this needs one of them.") //nolint:staticcheck // ST1005: shown to a person as this sentence (B55.50)

// ErrPushNotConfigured: Lens has no VAPID key (LENS_VAPID_PRIVATE_KEY), so it sends no pushes.
var ErrPushNotConfigured = errors.New("economy: web push is not configured on this server")

// challengeTTL is how long a challenge may be answered.
const challengeTTL = 5 * time.Minute

// ApprovalPusher sends one web push (internal/webpush.Sender).
type ApprovalPusher interface {
	Send(ctx context.Context, endpoint, p256dh, auth string, payload []byte) error
	Allows(endpoint string) bool
	PublicKey() string
}

// ApprovalChat posts an approval to the workspace's Slack or Teams (internal/approvalchat.Store, B56.16).
type ApprovalChat interface {
	Post(ctx context.Context, workspaceID string, a approvalchat.Approval) error
}

// SetApprovalChat turns on the Slack or Teams post sent for each approval filed, to a workspace that saved a webhook.
func (s *DualTokenStore) SetApprovalChat(c ApprovalChat) { s.approvalChat = c }

// SetApprovalAuth sets the relying party passkeys are checked for and, when pusher is not nil, turns on
// the push sent for each approval filed.
func (s *DualTokenStore) SetApprovalAuth(rp passkey.RelyingParty, pusher ApprovalPusher) {
	s.approvalRP, s.approvalPusher = &rp, pusher
}

// Passkey is one of a workspace's registered passkeys.
type Passkey struct {
	CredentialID string     `json:"credential_id"`
	Name         string     `json:"name"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"` // B41.116: revoked by a lockdown, it signs nothing
}

func (s *DualTokenStore) newChallenge(ctx context.Context, workspaceID, purpose string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	c := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := s.pool.Exec(ctx, `INSERT INTO webauthn_challenges (challenge, workspace_id, purpose, expires_at) VALUES ($1, $2, $3, $4)`,
		c, workspaceID, purpose, time.Now().Add(challengeTTL)); err != nil {
		return "", fmt.Errorf("economy: challenge: %w", err)
	}
	return c, nil
}

// useChallenge spends an open challenge of workspaceID for purpose, inside tx.
func useChallenge(ctx context.Context, tx pgx.Tx, workspaceID, purpose, challenge string) error {
	tag, err := tx.Exec(ctx, `UPDATE webauthn_challenges SET used_at = now()
		WHERE challenge = $1 AND workspace_id = $2 AND purpose = $3 AND used_at IS NULL AND expires_at > now()`, challenge, workspaceID, purpose)
	if err != nil {
		return fmt.Errorf("economy: challenge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no open challenge of this server answers it (each is used once, within %s)", passkey.ErrInvalid, challengeTTL)
	}
	return nil
}

// PasskeyRegistrationChallenge issues the challenge a new passkey is created over, and the RP ID.
func (s *DualTokenStore) PasskeyRegistrationChallenge(ctx context.Context, workspaceID string) (challenge, rpID string, err error) {
	if s.approvalRP == nil {
		return "", "", errors.New("economy: passkeys are not configured on this server")
	}
	c, err := s.newChallenge(ctx, workspaceID, "register")
	return c, s.approvalRP.ID, err
}

// RegisterPasskey verifies a registration against the challenge it answers and keeps the passkey; once the workspace
// has a passkey or device key, only with a's assertion by one of them over that same challenge (authorizeEnrol), and
// with none it keeps the passkey waiting (B55.1) and returns *EnrolmentWaiting.
func (s *DualTokenStore) RegisterPasskey(ctx context.Context, workspaceID, name string, r passkey.Registration, a *passkey.Assertion) (Passkey, error) {
	if s.approvalRP == nil {
		return Passkey{}, errors.New("economy: passkeys are not configured on this server")
	}
	challenge := passkey.ChallengeOf(r.ClientDataJSON)
	count, err := s.approvalRP.VerifyRegistration(r, challenge)
	if err != nil {
		return Passkey{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Passkey{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// B54.28: a known passkey is refused now; one still waiting may be registered signed, and its enrolment then 409s.
	// B55.15: a removed passkey keeps its row, so it is never registered again.
	var known, removed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_passkeys WHERE credential_id = $1),
		EXISTS (SELECT 1 FROM workspace_passkeys WHERE credential_id = $1 AND removed_at IS NOT NULL)`, r.CredentialID).Scan(&known, &removed); err != nil {
		return Passkey{}, fmt.Errorf("economy: register passkey: %w", err)
	}
	if removed {
		return Passkey{}, ErrPasskeyRemoved
	}
	if known {
		return Passkey{}, ErrPasskeyRegistered
	}
	if err := s.authorizeEnrol(ctx, tx, workspaceID, "register", challenge, a); errors.Is(err, ErrPasskeyRequired) {
		// B55.1: unsigned, it waits for a key the workspace already has to let it in.
		return Passkey{}, s.waitEnrolment(ctx, tx, workspaceID, "register", challenge, KeyEnrolment{Kind: "passkey", Name: name},
			"", r.CredentialID, r.PublicKey, int64(count), nil)
	} else if err != nil {
		return Passkey{}, err
	}
	p := Passkey{CredentialID: r.CredentialID, Name: name}
	if err := tx.QueryRow(ctx, `INSERT INTO workspace_passkeys (credential_id, workspace_id, name, public_key, sign_count)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`, r.CredentialID, workspaceID, name, r.PublicKey, int64(count)).Scan(&p.CreatedAt); err != nil {
		return p, fmt.Errorf("economy: register passkey: %w", err)
	}
	return p, tx.Commit(ctx)
}

// ListPasskeys reads a workspace's passkeys, removed ones left out.
func (s *DualTokenStore) ListPasskeys(ctx context.Context, workspaceID string) ([]Passkey, error) {
	rows, err := s.pool.Query(ctx, `SELECT credential_id, name, created_at, last_used_at, revoked_at FROM workspace_passkeys
		WHERE workspace_id = $1 AND removed_at IS NULL ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: passkeys: %w", err)
	}
	defer rows.Close()
	out := []Passkey{}
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.CredentialID, &p.Name, &p.CreatedAt, &p.LastUsedAt, &p.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ErrLastKey (B55.15): removing this key would leave the workspace with none that can approve.
var ErrLastKey = errors.New("economy: this is the workspace's last key")

// LastKeySentence is what the owner is told when a removal is refused with ErrLastKey.
const LastKeySentence = "This is your last key. With none, nobody could approve, so add another first."

// ErrKeyOwnsAccount (B55.15): the key owns an agent's smart account, and only it signs for that account.
var ErrKeyOwnsAccount = errors.New("economy: this key owns an agent's smart account")

// KeyOwnsAccountSentence is what the owner is told when a removal is refused with ErrKeyOwnsAccount.
const KeyOwnsAccountSentence = "This key owns an agent's account, so it stays. Without it, nobody could sign for that account."

// refuseAccountOwner is ErrKeyOwnsAccount when the passkey credentialID or the device key deviceID (the other one "")
// owns one of workspaceID's agent smart accounts.
func refuseAccountOwner(ctx context.Context, tx pgx.Tx, workspaceID, credentialID, deviceID string) error {
	var owns bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_smart_accounts WHERE workspace_id = $1
		AND ((owner_credential_id = $2 AND $2 <> '') OR (owner_device_id = $3 AND $3 <> '')))`,
		workspaceID, credentialID, deviceID).Scan(&owns); err != nil {
		return fmt.Errorf("economy: keys: %w", err)
	}
	if owns {
		return ErrKeyOwnsAccount
	}
	return nil
}

// refuseLastKey (B55.15) is ErrLastKey when workspaceID has no active passkey or phone key but the one being removed.
// It holds the workspace's enrolment lock (authorizeEnrol's) until tx ends, so two removals at once cannot both pass.
func refuseLastKey(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('key_enrol:' || $1, 0))`, workspaceID); err != nil {
		return fmt.Errorf("economy: keys: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1 AND removed_at IS NULL AND revoked_at IS NULL)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1 AND revoked_at IS NULL)`, workspaceID).Scan(&n); err != nil {
		return fmt.Errorf("economy: keys: %w", err)
	}
	if n <= 1 {
		return ErrLastKey
	}
	return nil
}

// RemovePasskey (B55.15) removes a passkey at once, with no signature, unless it is the workspace's last key or owns an
// agent's smart account; removing it
// again changes nothing. A removed passkey signs nothing, and still counts as a key, as a revoked phone key does.
func (s *DualTokenStore) RemovePasskey(ctx context.Context, workspaceID, credentialID, by string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	last := refuseLastKey(ctx, tx, workspaceID)
	if last != nil && !errors.Is(last, ErrLastKey) {
		return last
	}
	var removedAt, revokedAt *time.Time
	// FOR UPDATE waits for a signature by this passkey already being checked; every later one sees it removed.
	err = tx.QueryRow(ctx, `SELECT removed_at, revoked_at FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 FOR UPDATE`,
		credentialID, workspaceID).Scan(&removedAt, &revokedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrPasskeyNotFound
	case err != nil:
		return fmt.Errorf("economy: passkey: %w", err)
	case removedAt != nil:
		return nil
	case last != nil && revokedAt == nil: // a passkey a lockdown revoked (B41.116) is not live, so removing it leaves as many
		return last
	}
	if err := refuseAccountOwner(ctx, tx, workspaceID, credentialID, ""); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_passkeys SET removed_at = now(), removed_by = $3 WHERE credential_id = $1 AND workspace_id = $2`,
		credentialID, workspaceID, by); err != nil {
		return fmt.Errorf("economy: remove passkey: %w", err)
	}
	if err := appendAudit(ctx, tx, workspaceID, credentialID, "key_removed", map[string]any{"by": by, "kind": "passkey"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Key (B55.15) is one of a workspace's active keys: a passkey or a phone key.
type Key struct {
	Kind       string     `json:"kind"` // "passkey" or "phone"
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ListKeys reads a workspace's active passkeys and phone keys, oldest first.
func (s *DualTokenStore) ListKeys(ctx context.Context, workspaceID string) ([]Key, error) {
	rows, err := s.pool.Query(ctx, `SELECT 'passkey', credential_id, name, '', created_at, last_used_at FROM workspace_passkeys
		WHERE workspace_id = $1 AND removed_at IS NULL AND revoked_at IS NULL
		UNION ALL SELECT 'phone', id, name, platform, created_at, last_used_at FROM workspace_device_keys
		WHERE workspace_id = $1 AND revoked_at IS NULL ORDER BY 5, 2`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: keys: %w", err)
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.Kind, &k.ID, &k.Name, &k.Platform, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// approvalPurpose names a decision on approvalID; approving it with always allow, the allowance too (B54.29), so a
// signature over the approval alone, or over another amount or period, saves no rule.
func approvalPurpose(approvalID string, allow *PayeeAllowance) string {
	if allow == nil {
		return "approval:" + approvalID
	}
	return fmt.Sprintf("approval:%s:always_allow:%d:%s", approvalID, allow.ULXC, allow.Per)
}

// ApprovalChallenge issues the challenge approving or denying approvalID — with allow, approving it and always
// allowing its payee that much — is signed over, and the workspace's passkeys that may sign it.
func (s *DualTokenStore) ApprovalChallenge(ctx context.Context, workspaceID, approvalID string, allow *PayeeAllowance) (challenge string, credentialIDs []string, err error) {
	var pending bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_approvals WHERE id = $1 AND workspace_id = $2 AND status = 'pending' AND expires_at > now())`,
		approvalID, workspaceID).Scan(&pending); err != nil {
		return "", nil, fmt.Errorf("economy: approval: %w", err)
	}
	if !pending {
		return "", nil, ErrApprovalNotFound
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, approvalPurpose(approvalID, allow))
	return c, credentialIDs, err
}

// ErrApprovalsChanged: one of the approvals a batch decision names is no longer pending, so none is decided.
var ErrApprovalsChanged = errors.New("economy: one of these approvals is no longer pending, so none was decided")

// batchApprovalPurpose (B41.204) is the purpose one challenge over several approvals is issued for: the decision and
// the set of ids, sorted, so the signature covers exactly that decision on exactly those approvals.
func batchApprovalPurpose(approve bool, ids []string) string {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	return "approvals:" + map[bool]string{true: "approve", false: "deny"}[approve] + ":" + strings.Join(ids, ",")
}

// BatchApprovalChallenge (B41.204) issues one challenge approving or denying every one of ids is signed over, and the
// workspace's passkeys that may sign it. ErrApprovalNotFound unless every id is pending in workspaceID.
func (s *DualTokenStore) BatchApprovalChallenge(ctx context.Context, workspaceID string, ids []string, approve bool) (challenge string, credentialIDs []string, err error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	var pending int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE id = ANY($1) AND workspace_id = $2 AND status = 'pending' AND expires_at > now()`,
		ids, workspaceID).Scan(&pending); err != nil {
		return "", nil, fmt.Errorf("economy: approval: %w", err)
	}
	if len(ids) == 0 || pending != len(ids) {
		return "", nil, ErrApprovalNotFound
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, batchApprovalPurpose(approve, ids))
	return c, credentialIDs, err
}

// DecideAgentApprovals (B41.204) decides every one of ids with one assertion a over BatchApprovalChallenge's challenge
// for exactly that decision and set, which it spends; a workspace with no passkey or device key needs none. All in one
// transaction: if any is no longer pending, ErrApprovalsChanged and none is decided.
func (s *DualTokenStore) DecideAgentApprovals(ctx context.Context, workspaceID string, ids []string, approve bool, a *passkey.Assertion) ([]AgentApproval, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return nil, ErrApprovalNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var keys int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1)`, workspaceID).Scan(&keys); err != nil {
		return nil, fmt.Errorf("economy: passkeys: %w", err)
	}
	if keys == 0 {
		a = nil // unsigned, as AuthorizeApprovalDecision lets a workspace with no key decide
	} else if a == nil {
		return nil, ErrPasskeyRequired
	} else if err := s.spendAssertion(ctx, tx, workspaceID, batchApprovalPurpose(approve, ids), *a); err != nil {
		return nil, err
	}
	out := make([]AgentApproval, 0, len(ids))
	for _, id := range ids {
		ap, err := s.decideInTx(ctx, tx, workspaceID, id, approve, a)
		if errors.Is(err, ErrApprovalNotFound) {
			return nil, ErrApprovalsChanged
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, tx.Commit(ctx)
}

// AuthorizeApprovalDecision lets a decision on approvalID through: at once for a workspace with no
// passkey or device key, and otherwise only with an assertion by one of its passkeys or device keys over
// an open challenge naming that approval, and allow when it is approved with one, which it spends. A revoked device
// key still counts (B40.4): revoking a lost phone must never leave approvals needing no signature at all.
func (s *DualTokenStore) AuthorizeApprovalDecision(ctx context.Context, workspaceID, approvalID string, allow *PayeeAllowance, a *passkey.Assertion) error {
	return s.authorizeSigned(ctx, workspaceID, approvalPurpose(approvalID, allow), a)
}

// authorizeSigned lets an act for purpose through: at once for a workspace with no passkey or device key, and
// otherwise only with a's assertion over an open challenge for purpose, which it spends.
func (s *DualTokenStore) authorizeSigned(ctx context.Context, workspaceID, purpose string, a *passkey.Assertion) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.authorizeSignedTx(ctx, tx, workspaceID, purpose, a, ErrPasskeyRequired); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// authorizeSignedTx is authorizeSigned inside tx; unsigned is the refusal of an act that carried no assertion.
func (s *DualTokenStore) authorizeSignedTx(ctx context.Context, tx pgx.Tx, workspaceID, purpose string, a *passkey.Assertion, unsigned error) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1)`, workspaceID).Scan(&n); err != nil {
		return fmt.Errorf("economy: passkeys: %w", err)
	}
	if n == 0 {
		return nil
	}
	if a == nil {
		return unsigned
	}
	return s.spendAssertion(ctx, tx, workspaceID, purpose, *a)
}

// authorizeEnrol (B50.20) spends the challenge a new passkey or device key answers: at once for a workspace with no
// passkey or device key, and otherwise only with a's assertion, by one of them, over that same challenge — so a session
// alone cannot add a key and then pass every passkey gate. It holds the workspace's enrolment lock until tx ends, so two
// keys registering at once cannot both be the free first one. A revoked device key counts, as in authorizeSigned —
// except for the one key registered over the challenge a lift returned, after a lockdown left no key live
// (reenrolAfterLift).
func (s *DualTokenStore) authorizeEnrol(ctx context.Context, tx pgx.Tx, workspaceID, purpose, challenge string, a *passkey.Assertion) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('key_enrol:' || $1, 0))`, workspaceID); err != nil {
		return fmt.Errorf("economy: passkeys: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1)`, workspaceID).Scan(&n); err != nil {
		return fmt.Errorf("economy: passkeys: %w", err)
	}
	if n == 0 {
		return useChallenge(ctx, tx, workspaceID, purpose, challenge)
	}
	if free, err := reenrolAfterLift(ctx, tx, workspaceID, purpose, challenge); free || err != nil {
		return err
	}
	if a == nil {
		return ErrPasskeyRequired
	}
	signed := a.Challenge
	if a.DeviceID == "" {
		cdj, _ := passkey.Decode64(a.ClientDataJSON)
		signed = passkey.ChallengeOf(cdj)
	}
	if signed != challenge {
		return fmt.Errorf("%w: sign the new key's own registration challenge with a key this workspace already has", passkey.ErrInvalid)
	}
	return s.spendAssertion(ctx, tx, workspaceID, purpose, *a)
}

// spendAssertion checks a, by one of workspaceID's passkeys or device keys, over an open challenge for purpose, and
// spends the challenge, inside tx.
func (s *DualTokenStore) spendAssertion(ctx context.Context, tx pgx.Tx, workspaceID, purpose string, a passkey.Assertion) error {
	if a.DeviceID != "" {
		return spendDeviceSignature(ctx, tx, workspaceID, purpose, a)
	}
	if s.approvalRP == nil {
		return fmt.Errorf("%w: passkeys are not configured on this server", passkey.ErrInvalid)
	}
	var spki []byte
	var count int64
	err := tx.QueryRow(ctx, `SELECT public_key, sign_count FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 AND removed_at IS NULL AND revoked_at IS NULL FOR UPDATE`,
		a.CredentialID, workspaceID).Scan(&spki, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: not a passkey of this workspace, or it was removed", passkey.ErrInvalid)
	}
	if err != nil {
		return fmt.Errorf("economy: passkey: %w", err)
	}
	cdj, err := passkey.Decode64(a.ClientDataJSON)
	if err != nil {
		return fmt.Errorf("%w: clientDataJSON is not base64url", passkey.ErrInvalid)
	}
	challenge := passkey.ChallengeOf(cdj)
	next, err := s.approvalRP.VerifyAssertion(a, challenge, spki, uint32(count))
	if err != nil {
		return err
	}
	if err := useChallenge(ctx, tx, workspaceID, purpose, challenge); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_passkeys SET sign_count = $2, last_used_at = now() WHERE credential_id = $1`,
		a.CredentialID, int64(next)); err != nil {
		return fmt.Errorf("economy: passkey: %w", err)
	}
	return nil
}

// PushPublicKey is the VAPID public key a browser subscribes with; ok is false when push is off.
func (s *DualTokenStore) PushPublicKey() (key string, ok bool) {
	if s.approvalPusher == nil {
		return "", false
	}
	return s.approvalPusher.PublicKey(), true
}

// SavePushSubscription keeps a device's push subscription for workspaceID.
func (s *DualTokenStore) SavePushSubscription(ctx context.Context, workspaceID, endpoint, p256dh, auth string) error {
	if s.approvalPusher == nil {
		return ErrPushNotConfigured
	}
	if !s.approvalPusher.Allows(endpoint) {
		return fmt.Errorf("%w: %q is not a browser push service", ErrAgentRule, endpoint)
	}
	if k, err := webpush.Decode64(p256dh); err != nil || len(k) != 65 {
		return fmt.Errorf("%w: keys.p256dh must be a P-256 public key, base64url", ErrAgentRule)
	}
	if a, err := webpush.Decode64(auth); err != nil || len(a) != 16 {
		return fmt.Errorf("%w: keys.auth must be 16 bytes, base64url", ErrAgentRule)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_push_subscriptions (endpoint, workspace_id, p256dh, auth) VALUES ($1, $2, $3, $4)
		ON CONFLICT (endpoint) DO UPDATE SET workspace_id = EXCLUDED.workspace_id, p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth`,
		endpoint, workspaceID, p256dh, auth); err != nil {
		return fmt.Errorf("economy: push subscription: %w", err)
	}
	return nil
}

// DeletePushSubscription forgets a device's subscription.
func (s *DualTokenStore) DeletePushSubscription(ctx context.Context, workspaceID, endpoint string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM workspace_push_subscriptions WHERE endpoint = $1 AND workspace_id = $2`, endpoint, workspaceID); err != nil {
		return fmt.Errorf("economy: push subscription: %w", err)
	}
	return nil
}

// notifyApproval sends, in the background, one push per subscription of workspaceID for a newly filed
// approval: which agent, how much, to whom and why — and, B56.16, one post to its Slack or Teams.
func (s *DualTokenStore) notifyApproval(workspaceID, approvalID string) {
	pusher := s.approvalPusher
	if pusher == nil && s.nativePusher == nil && s.approvalChat == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var agentID, agentName, model, reason, payeeName, memo string
		var amount int64
		var expires time.Time
		if err := s.pool.QueryRow(ctx, `SELECT a.agent_id, g.name, a.amount_ulxc, a.model, a.reason, a.payee_name, a.memo, a.expires_at
			FROM agent_approvals a JOIN agent_accounts g ON g.id = a.agent_id WHERE a.id = $1`, approvalID).
			Scan(&agentID, &agentName, &amount, &model, &reason, &payeeName, &memo, &expires); err != nil {
			slog.Warn("agents: approval push: read approval", slog.String("err", err.Error()))
			return
		}
		if reason == "" && model != "" {
			reason = "a request to " + model
		}
		// B40.15: the phones are told the payee and the amount — not the reason or memo, which the agent wrote.
		to := payeeName
		if to == "" {
			to = model
		}
		body := agentName + " wants to spend " + LXCMoney(amount)
		if to != "" {
			body = agentName + " wants to pay " + to + " " + LXCMoney(amount)
		}
		payload, _ := json.Marshal(map[string]any{"type": "agent_approval", "approval_id": approvalID, "workspace_id": workspaceID,
			"agent_id": agentID, "agent_name": agentName, "amount_ulxc": amount, "amount_lxc": lxcString(amount), "reason": reason,
			"payee_name": payeeName, "memo": memo, "body": body}) // B55.30: body is the sentence the phones show
		// B55.12: the agent_approval category gives the lock screen its Approve button, and the data is what the phone
		// checks the approval against before it signs; an agent's approval counts in µLXC, LXC's minor unit.
		s.pushDevices(ctx, workspaceID, nativepush.Message{Title: "Approval needed", Body: body, Category: "agent_approval", Data: map[string]string{
			"type": "agent_approval", "approval_id": approvalID, "workspace_id": workspaceID, "agent_id": agentID,
			"agent_name": agentName, "payee_name": payeeName, "currency": "LXC", "amount_minor": strconv.FormatInt(amount, 10)}})
		s.pushWeb(ctx, workspaceID, payload)
		if s.approvalChat != nil {
			if err := s.approvalChat.Post(ctx, workspaceID, approvalchat.Approval{ID: approvalID, Agent: agentName, To: to,
				Amount: LXCMoney(amount), Reason: reason, URL: ApprovalURLBase + approvalID, ExpiresAt: expires}); err != nil {
				slog.Warn("approval chat", slog.String("err", err.Error()))
			}
		}
	}()
}

// pushWeb sends payload to each browser subscribed for workspaceID, forgetting the subscriptions the push services say
// are gone; with no VAPID key it sends nothing.
func (s *DualTokenStore) pushWeb(ctx context.Context, workspaceID string, payload []byte) {
	pusher := s.approvalPusher
	if pusher == nil {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT endpoint, p256dh, auth FROM workspace_push_subscriptions WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		slog.Warn("web push: subscriptions", slog.String("err", err.Error()))
		return
	}
	var subs [][3]string
	for rows.Next() {
		var sub [3]string
		if rows.Scan(&sub[0], &sub[1], &sub[2]) == nil {
			subs = append(subs, sub)
		}
	}
	rows.Close()
	for _, sub := range subs {
		err := pusher.Send(ctx, sub[0], sub[1], sub[2], payload)
		switch {
		case errors.Is(err, webpush.ErrGone):
			_ = s.DeletePushSubscription(ctx, workspaceID, sub[0])
		case err != nil:
			slog.Warn("web push", slog.String("err", err.Error()))
		}
	}
}
