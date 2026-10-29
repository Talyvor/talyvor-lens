package economy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

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
var ErrPasskeyRequired = errors.New("economy: this workspace approves with a passkey — sign this approval's challenge")

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

// RegisterPasskey verifies a registration against the challenge it answers and keeps the passkey.
func (s *DualTokenStore) RegisterPasskey(ctx context.Context, workspaceID, name string, r passkey.Registration) (Passkey, error) {
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
	if err := useChallenge(ctx, tx, workspaceID, "register", challenge); err != nil {
		return Passkey{}, err
	}
	p := Passkey{CredentialID: r.CredentialID, Name: name}
	if err := tx.QueryRow(ctx, `INSERT INTO workspace_passkeys (credential_id, workspace_id, name, public_key, sign_count)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`, r.CredentialID, workspaceID, name, r.PublicKey, int64(count)).Scan(&p.CreatedAt); err != nil {
		return p, fmt.Errorf("economy: register passkey: %w", err)
	}
	return p, tx.Commit(ctx)
}

// ListPasskeys reads a workspace's passkeys.
func (s *DualTokenStore) ListPasskeys(ctx context.Context, workspaceID string) ([]Passkey, error) {
	rows, err := s.pool.Query(ctx, `SELECT credential_id, name, created_at, last_used_at FROM workspace_passkeys
		WHERE workspace_id = $1 ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: passkeys: %w", err)
	}
	defer rows.Close()
	out := []Passkey{}
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.CredentialID, &p.Name, &p.CreatedAt, &p.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ApprovalChallenge issues the challenge approving or denying approvalID is signed over, and the
// workspace's passkeys that may sign it.
func (s *DualTokenStore) ApprovalChallenge(ctx context.Context, workspaceID, approvalID string) (challenge string, credentialIDs []string, err error) {
	var pending bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_approvals WHERE id = $1 AND workspace_id = $2 AND status = 'pending')`,
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
	c, err := s.newChallenge(ctx, workspaceID, "approval:"+approvalID)
	return c, credentialIDs, err
}

// AuthorizeApprovalDecision lets a decision on approvalID through: at once for a workspace with no
// passkey or device key, and otherwise only with an assertion by one of its passkeys or device keys over
// an open challenge naming that approval, which it spends. A revoked device key still counts (B40.4):
// revoking a lost phone must never leave approvals needing no signature at all.
func (s *DualTokenStore) AuthorizeApprovalDecision(ctx context.Context, workspaceID, approvalID string, a *passkey.Assertion) error {
	return s.authorizeSigned(ctx, workspaceID, "approval:"+approvalID, a)
}

// authorizeSigned lets an act for purpose through: at once for a workspace with no passkey or device key, and
// otherwise only with a's assertion over an open challenge for purpose, which it spends.
func (s *DualTokenStore) authorizeSigned(ctx context.Context, workspaceID, purpose string, a *passkey.Assertion) error {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1)`, workspaceID).Scan(&n); err != nil {
		return fmt.Errorf("economy: passkeys: %w", err)
	}
	if n == 0 {
		return nil
	}
	if a == nil {
		return ErrPasskeyRequired
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.spendAssertion(ctx, tx, workspaceID, purpose, *a); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	err := tx.QueryRow(ctx, `SELECT public_key, sign_count FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 FOR UPDATE`,
		a.CredentialID, workspaceID).Scan(&spki, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: not a passkey of this workspace", passkey.ErrInvalid)
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
// approval: which agent, how much, to whom and why.
func (s *DualTokenStore) notifyApproval(workspaceID, approvalID string) {
	pusher := s.approvalPusher
	if pusher == nil && s.nativePusher == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var agentID, agentName, model, reason, payeeName, memo string
		var amount int64
		if err := s.pool.QueryRow(ctx, `SELECT a.agent_id, g.name, a.amount_ulxc, a.model, a.reason, a.payee_name, a.memo
			FROM agent_approvals a JOIN agent_accounts g ON g.id = a.agent_id WHERE a.id = $1`, approvalID).
			Scan(&agentID, &agentName, &amount, &model, &reason, &payeeName, &memo); err != nil {
			slog.Warn("agents: approval push: read approval", slog.String("err", err.Error()))
			return
		}
		if reason == "" && model != "" {
			reason = "a request to " + model
		}
		payload, _ := json.Marshal(map[string]any{"type": "agent_approval", "approval_id": approvalID, "workspace_id": workspaceID,
			"agent_id": agentID, "agent_name": agentName, "amount_ulxc": amount, "amount_lxc": lxcString(amount), "reason": reason,
			"payee_name": payeeName, "memo": memo})
		// B40.15: the phones are told the payee and the amount — not the reason or memo, which the agent wrote.
		to := payeeName
		if to == "" {
			to = model
		}
		body := agentName + " wants to spend " + lxcString(amount) + " LXC"
		if to != "" {
			body = agentName + " wants to pay " + to + " " + lxcString(amount) + " LXC"
		}
		s.pushDevices(ctx, workspaceID, nativepush.Message{Title: "Approval needed", Body: body, Data: map[string]string{
			"type": "agent_approval", "approval_id": approvalID, "workspace_id": workspaceID, "agent_id": agentID}})
		if pusher == nil {
			return
		}
		rows, err := s.pool.Query(ctx, `SELECT endpoint, p256dh, auth FROM workspace_push_subscriptions WHERE workspace_id = $1`, workspaceID)
		if err != nil {
			slog.Warn("agents: approval push: subscriptions", slog.String("err", err.Error()))
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
				slog.Warn("agents: approval push", slog.String("err", err.Error()))
			}
		}
	}()
}
