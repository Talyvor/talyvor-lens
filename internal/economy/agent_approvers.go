package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/ap2"
	"github.com/talyvor/lens/internal/passkey"
)

// agent_approvers.go — B45.28: AN AGENT APPROVES ANOTHER AGENT'S REQUESTS WITHIN ITS OWN LIMITS; THE OWNER'S APPROVALS
// ARE AP2 v0.2 MANDATES (internal/ap2, docs/ap2-mandates.md).
//
// The owner lets an approver agent approve one agent's requests, signing on the Talyvor app with a passkey or device
// key: SetAgentApprover keeps that as an AP2 open Payment Mandate whose limit is the approver's own — the lower of its
// per-request limit and its approval amount; an agent with neither approves nothing. ApproveAsAgent lets the approver
// approve a pending request only up to the lower of that limit and its limit now, and within its daily limit counted
// over what it approved today; anything above waits for the owner. Its approval is kept as Talyvor-signed evidence
// naming both agents and the open mandate it ran under. The owner's own signed approval is kept as an AP2 closed
// Payment Mandate (DecideAgentApprovalSigned). Nothing here moves money: an approved request goes through once, as before.

var (
	// ErrApprover: an approver the owner cannot set, as asked.
	ErrApprover = errors.New("economy: invalid approver")
	// ErrNotApprover: the agent is not the approver in force for that request's agent.
	ErrNotApprover = errors.New("economy: this agent is not the approver of that agent's requests")
	// ErrAboveApproverLimits: the request is above what the approver may approve, so it waits for the owner.
	ErrAboveApproverLimits = errors.New("economy: above the approver's own limits — the request waits for the workspace's owner")
	// ErrMandatesNotConfigured: Lens has no mandate signer.
	ErrMandatesNotConfigured = errors.New("economy: mandates are not configured on this server")
)

// AgentApprover is the approver in force for an agent's requests, and the owner's open mandate that says so.
type AgentApprover struct {
	ID              string    `json:"id"` // the open mandate's jti
	AgentID         string    `json:"agent_id"`
	ApproverAgentID string    `json:"approver_agent_id"`
	LimitULXC       int64     `json:"limit_ulxc"`
	Mandate         string    `json:"mandate"`   // the AP2 v0.2 open Payment Mandate, an SD-JWT
	SignedBy        string    `json:"signed_by"` // the passkey's credential id or the device key's id
	CreatedAt       time.Time `json:"created_at"`
}

// ApprovalEvidence is what proves an approval: the owner's closed mandate, or an agent's evidence and the open mandate
// it ran under.
type ApprovalEvidence struct {
	ApprovalID        string `json:"approval_id"`
	OwnerMandate      string `json:"owner_mandate,omitempty"`
	ApprovedByAgentID string `json:"approved_by_agent_id,omitempty"`
	AgentEvidence     string `json:"agent_evidence,omitempty"`
	ApproverMandate   string `json:"approver_mandate,omitempty"`
}

// SetMandateSigner sets the key mandates and approval evidence are signed with.
func (s *DualTokenStore) SetMandateSigner(signer *ap2.Signer) { s.mandateSigner = signer }

// approverLimit is the most an agent with rules r spends on one request without asking; 0 when nothing bounds it.
func approverLimit(r AgentRules) int64 {
	limit := int64(0)
	for _, v := range []int64{r.MaxPerRequestULXC, r.ApprovalAboveULXC} {
		if v > 0 && (limit == 0 || v < limit) {
			limit = v
		}
	}
	return limit
}

func approverPurpose(agentID, approverID string) string {
	return "approver:" + agentID + ":" + approverID
}

// lxcAmount is amount µLXC as an AP2 payment amount: LXC, counted in millionths.
func lxcAmount(ulxc int64) map[string]any { return map[string]any{"currency": "LXC", "amount": ulxc} }

// trustedSurface is the owner's signature on the Talyvor app, as a mandate carries it.
func trustedSurface(a passkey.Assertion) map[string]any {
	if a.DeviceID != "" {
		return map[string]any{"app": "talyvor", "method": "device_key", "device_id": a.DeviceID, "challenge": a.Challenge, "signature": a.Signature}
	}
	return map[string]any{"app": "talyvor", "method": "passkey", "credential_id": a.CredentialID, "client_data_json": a.ClientDataJSON,
		"authenticator_data": a.AuthenticatorData, "signature": a.Signature}
}

func signerOf(a passkey.Assertion) string {
	if a.DeviceID != "" {
		return a.DeviceID
	}
	return a.CredentialID
}

func payeeClaim(a AgentApproval) map[string]any {
	if a.Payee == nil {
		return map[string]any{"kind": "model", "id": a.Model}
	}
	return map[string]any{"kind": a.Payee.Kind, "id": a.Payee.ID, "name": a.Payee.Name}
}

func agentName(ctx context.Context, q pgxDB, agentID string) (string, error) {
	p, err := namePayee(ctx, q, Payee{Kind: "agent", ID: agentID})
	return p.Name, err
}

// approverTerms checks approverID may approve agentID's requests and is the limit it would do so up to.
func (s *DualTokenStore) approverTerms(ctx context.Context, workspaceID, agentID, approverID string) (int64, error) {
	if agentID == approverID {
		return 0, fmt.Errorf("%w: an agent never approves its own requests", ErrApprover)
	}
	if _, err := s.GetAgentRules(ctx, workspaceID, agentID); err != nil {
		return 0, err
	}
	rules, err := s.GetAgentRules(ctx, workspaceID, approverID)
	if err != nil {
		return 0, err
	}
	var archived bool
	if err := s.pool.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM agent_accounts WHERE id = $1`, approverID).Scan(&archived); err != nil {
		return 0, fmt.Errorf("economy: approver: %w", err)
	}
	if archived {
		return 0, ErrAgentArchived
	}
	limit := approverLimit(rules)
	if limit == 0 {
		return 0, fmt.Errorf("%w: the approver has no limit of its own (max_per_request_ulxc or approval_above_ulxc), so it may approve nothing", ErrApprover)
	}
	return limit, nil
}

// ApproverChallenge issues the challenge the owner signs to let approverID approve agentID's requests, and the limit
// the approver would approve up to.
func (s *DualTokenStore) ApproverChallenge(ctx context.Context, workspaceID, agentID, approverID string) (challenge string, limitULXC int64, err error) {
	if limitULXC, err = s.approverTerms(ctx, workspaceID, agentID, approverID); err != nil {
		return "", 0, err
	}
	challenge, err = s.newChallenge(ctx, workspaceID, approverPurpose(agentID, approverID))
	return challenge, limitULXC, err
}

// SetAgentApprover lets approverID approve agentID's requests, signed by the owner with a over ApproverChallenge's
// challenge, and keeps it as an AP2 v0.2 open Payment Mandate bound to the approver's Agent Card key. It replaces the
// approver in force. Without a signature nothing is set: a workspace with no passkey or device key has no approvers.
func (s *DualTokenStore) SetAgentApprover(ctx context.Context, workspaceID, agentID, approverID string, a *passkey.Assertion) (AgentApprover, error) {
	if s.mandateSigner == nil {
		return AgentApprover{}, ErrMandatesNotConfigured
	}
	if a == nil {
		return AgentApprover{}, ErrPasskeyRequired
	}
	limit, err := s.approverTerms(ctx, workspaceID, agentID, approverID)
	if err != nil {
		return AgentApprover{}, err
	}
	key, ok, _, err := s.AgentCardKeyInForce(ctx, approverID)
	if err != nil {
		return AgentApprover{}, err
	}
	if !ok {
		return AgentApprover{}, fmt.Errorf("%w: the approver has no Agent Card key in force", ErrApprover)
	}
	agent, err := agentName(ctx, s.pool, agentID)
	if err != nil {
		return AgentApprover{}, err
	}
	approver, err := agentName(ctx, s.pool, approverID)
	if err != nil {
		return AgentApprover{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentApprover{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.spendAssertion(ctx, tx, workspaceID, approverPurpose(agentID, approverID), *a); err != nil {
		return AgentApprover{}, err
	}
	out := AgentApprover{ID: "mnd_" + uuid.NewString(), AgentID: agentID, ApproverAgentID: approverID, LimitULXC: limit, SignedBy: signerOf(*a)}
	out.Mandate, err = s.mandateSigner.Issue(map[string]any{
		"iss": s.mandateSigner.Issuer, "iat": time.Now().Unix(), "jti": out.ID, "vct": ap2.VCTOpen,
		"cnf": map[string]any{"jwk": map[string]any{"kty": "OKP", "crv": "Ed25519", "x": key.PublicKey, "kid": key.Kid}},
	}, map[string]any{
		"constraints": []any{map[string]any{"type": "payment.amount_range", "currency": "LXC", "min": 0, "max": limit}},
		"talyvor_delegation": map[string]any{"workspace_id": workspaceID,
			"approver":  map[string]any{"agent_id": approverID, "name": approver},
			"for_agent": map[string]any{"agent_id": agentID, "name": agent}},
		"trusted_surface": trustedSurface(*a),
	})
	if err != nil {
		return AgentApprover{}, fmt.Errorf("economy: open mandate: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_approvers SET revoked_at = now() WHERE agent_id = $1 AND workspace_id = $2 AND revoked_at IS NULL`,
		agentID, workspaceID); err != nil {
		return AgentApprover{}, fmt.Errorf("economy: approver: %w", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO agent_approvers (id, workspace_id, agent_id, approver_agent_id, limit_ulxc, mandate, signed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`, out.ID, workspaceID, agentID, approverID, limit, out.Mandate, out.SignedBy,
	).Scan(&out.CreatedAt); err != nil {
		return AgentApprover{}, fmt.Errorf("economy: approver: %w", err)
	}
	return out, tx.Commit(ctx)
}

// GetAgentApprover is the approver in force for agentID's requests; ErrNotApprover when it has none.
func (s *DualTokenStore) GetAgentApprover(ctx context.Context, workspaceID, agentID string) (AgentApprover, error) {
	var a AgentApprover
	err := s.pool.QueryRow(ctx, `SELECT id, agent_id, approver_agent_id, limit_ulxc, mandate, signed_by, created_at FROM agent_approvers
		WHERE agent_id = $1 AND workspace_id = $2 AND revoked_at IS NULL`, agentID, workspaceID).Scan(
		&a.ID, &a.AgentID, &a.ApproverAgentID, &a.LimitULXC, &a.Mandate, &a.SignedBy, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotApprover
	}
	if err != nil {
		return a, fmt.Errorf("economy: approver: %w", err)
	}
	return a, nil
}

// RemoveAgentApprover takes agentID's approver away at once; it is not signed, so removing one is never harder than
// setting it. Its open mandate stays, revoked, for the evidence that names it.
func (s *DualTokenStore) RemoveAgentApprover(ctx context.Context, workspaceID, agentID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE agent_approvers SET revoked_at = now() WHERE agent_id = $1 AND workspace_id = $2 AND revoked_at IS NULL`,
		agentID, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: approver: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotApprover
	}
	return nil
}

// ApproveAsAgent approves a pending request by approverID, the approver in force for the request's agent, within its
// own limits; above them it returns ErrAboveApproverLimits and the request stays pending for the owner.
func (s *DualTokenStore) ApproveAsAgent(ctx context.Context, workspaceID, approverID, approvalID string) (AgentApproval, error) {
	if s.mandateSigner == nil {
		return AgentApproval{}, ErrMandatesNotConfigured
	}
	rules, err := s.GetAgentRules(ctx, workspaceID, approverID)
	if err != nil {
		return AgentApproval{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentApproval{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The approver's row first, so what one approver approved today is counted one approval at a time.
	var approver string
	if err := tx.QueryRow(ctx, `SELECT name FROM agent_accounts WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, approverID, workspaceID).Scan(&approver); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentApproval{}, ErrAgentNotFound
		}
		return AgentApproval{}, fmt.Errorf("economy: approver: %w", err)
	}
	if err := refuseIfPaused(ctx, tx, approverID); err != nil {
		return AgentApproval{}, err
	}
	a, err := scanAgentApproval(tx.QueryRow(ctx, `SELECT `+agentApprovalColumns+` FROM agent_approvals
		WHERE id = $1 AND workspace_id = $2 AND status = 'pending' AND expires_at > now() FOR UPDATE`, approvalID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrApprovalNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: approval: %w", err)
	}
	var mandateID, mandate string
	var limit int64
	err = tx.QueryRow(ctx, `SELECT id, mandate, limit_ulxc FROM agent_approvers WHERE agent_id = $1 AND approver_agent_id = $2
		AND workspace_id = $3 AND revoked_at IS NULL`, a.AgentID, approverID, workspaceID).Scan(&mandateID, &mandate, &limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotApprover
	}
	if err != nil {
		return a, fmt.Errorf("economy: approver: %w", err)
	}
	if live := approverLimit(rules); live < limit {
		limit = live // its limits lowered since the owner signed: the lower holds
	}
	if a.AmountULXC > limit {
		return a, fmt.Errorf("%w: %s LXC is above the %s LXC it may approve", ErrAboveApproverLimits, lxcString(a.AmountULXC), lxcString(limit))
	}
	if rules.DailyLimitULXC > 0 {
		tz := rules.Timezone
		if tz == "" {
			tz = "UTC"
		}
		var today int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_approvals WHERE approved_by_agent_id = $1
			AND decided_at >= (date_trunc('day', now() AT TIME ZONE $2) AT TIME ZONE $2)`, approverID, tz).Scan(&today); err != nil {
			return a, fmt.Errorf("economy: approver: %w", err)
		}
		if today+a.AmountULXC > rules.DailyLimitULXC {
			return a, fmt.Errorf("%w: with this, it would have approved %s LXC today, above its daily limit of %s LXC", ErrAboveApproverLimits,
				lxcString(today+a.AmountULXC), lxcString(rules.DailyLimitULXC))
		}
	}
	requester, err := agentName(ctx, tx, a.AgentID)
	if err != nil {
		return a, err
	}
	evidence, err := s.mandateSigner.SignJWT(ap2.TypEvidence, map[string]any{
		"iss": s.mandateSigner.Issuer, "iat": time.Now().Unix(), "jti": a.ID,
		"approval":  map[string]any{"id": a.ID, "payment_amount": lxcAmount(a.AmountULXC), "payee": payeeClaim(a)},
		"requester": map[string]any{"agent_id": a.AgentID, "name": requester},
		"approver":  map[string]any{"agent_id": approverID, "name": approver, "limit_ulxc": limit},
		"mandate":   map[string]any{"id": mandateID, "vct": ap2.VCTOpen, "hash": ap2.Hash(mandate)},
	})
	if err != nil {
		return a, fmt.Errorf("economy: approval evidence: %w", err)
	}
	a, err = scanAgentApproval(tx.QueryRow(ctx, `UPDATE agent_approvals SET status = 'approved', decided_at = now(),
		expires_at = now() + $6 * interval '1 microsecond',
		approved_by_agent_id = $3, approver_mandate_id = $4, agent_evidence = $5 WHERE id = $1 AND workspace_id = $2
		RETURNING `+agentApprovalColumns, approvalID, workspaceID, approverID, mandateID, evidence, s.approvalLife().Microseconds()))
	if err != nil {
		return a, fmt.Errorf("economy: approve: %w", err)
	}
	return a, tx.Commit(ctx)
}

// DecideAgentApprovalSigned decides approvalID with the owner's signature a: checked, and its challenge spent, as
// AuthorizeApprovalDecision does, in the same transaction that keeps an approval with the owner's AP2 v0.2 closed Payment
// Mandate. A workspace with no passkey or device key decides as before, unsigned, and keeps no mandate.
func (s *DualTokenStore) DecideAgentApprovalSigned(ctx context.Context, workspaceID, approvalID string, approve bool, a passkey.Assertion) (AgentApproval, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentApproval{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var keys int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1)`, workspaceID).Scan(&keys); err != nil {
		return AgentApproval{}, fmt.Errorf("economy: passkeys: %w", err)
	}
	if keys == 0 {
		_ = tx.Rollback(ctx)
		return s.DecideAgentApproval(ctx, workspaceID, approvalID, approve)
	}
	if err := s.spendAssertion(ctx, tx, workspaceID, "approval:"+approvalID, a); err != nil {
		return AgentApproval{}, err
	}
	if !approve || s.mandateSigner == nil { // no signer: only a store built without one, as older tests build it
		ap, err := scanAgentApproval(tx.QueryRow(ctx, `UPDATE agent_approvals SET status = $3, decided_at = now(), expires_at = now() + $4 * interval '1 microsecond'
			WHERE id = $1 AND workspace_id = $2 AND status = 'pending' AND expires_at > now() RETURNING `+agentApprovalColumns, approvalID, workspaceID,
			map[bool]string{true: "approved", false: "denied"}[approve], s.approvalLife().Microseconds()))
		if errors.Is(err, pgx.ErrNoRows) {
			return ap, ErrApprovalNotFound
		}
		if err != nil {
			return ap, fmt.Errorf("economy: decide approval: %w", err)
		}
		return ap, tx.Commit(ctx)
	}
	ap, err := scanAgentApproval(tx.QueryRow(ctx, `UPDATE agent_approvals SET status = 'approved', decided_at = now(), expires_at = now() + $3 * interval '1 microsecond'
		WHERE id = $1 AND workspace_id = $2 AND status = 'pending' AND expires_at > now() RETURNING `+agentApprovalColumns, approvalID, workspaceID,
		s.approvalLife().Microseconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return ap, ErrApprovalNotFound
	}
	if err != nil {
		return ap, fmt.Errorf("economy: decide approval: %w", err)
	}
	mandate, err := s.mandateSigner.Issue(map[string]any{
		"iss": s.mandateSigner.Issuer, "iat": time.Now().Unix(), "jti": "mnd_" + uuid.NewString(), "vct": ap2.VCTClosed,
	}, map[string]any{
		"transaction_id":     ap.ID,
		"payee":              payeeClaim(ap),
		"payment_amount":     lxcAmount(ap.AmountULXC),
		"payment_instrument": map[string]any{"type": "talyvor_agent_wallet", "id": ap.AgentID},
		"trusted_surface":    trustedSurface(a),
	})
	if err != nil {
		return ap, fmt.Errorf("economy: closed mandate: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_approvals SET owner_mandate = $2 WHERE id = $1`, ap.ID, mandate); err != nil {
		return ap, fmt.Errorf("economy: closed mandate: %w", err)
	}
	return ap, tx.Commit(ctx)
}

// ApprovalEvidence is what proves approvalID was approved, by the owner or an approver agent.
func (s *DualTokenStore) ApprovalEvidence(ctx context.Context, workspaceID, approvalID string) (ApprovalEvidence, error) {
	e := ApprovalEvidence{ApprovalID: approvalID}
	err := s.pool.QueryRow(ctx, `SELECT a.owner_mandate, a.approved_by_agent_id, a.agent_evidence, COALESCE(m.mandate, '')
		FROM agent_approvals a LEFT JOIN agent_approvers m ON m.id = a.approver_mandate_id AND m.workspace_id = a.workspace_id
		WHERE a.id = $1 AND a.workspace_id = $2`, approvalID, workspaceID).Scan(&e.OwnerMandate, &e.ApprovedByAgentID, &e.AgentEvidence, &e.ApproverMandate)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrApprovalNotFound
	}
	if err != nil {
		return e, fmt.Errorf("economy: approval evidence: %w", err)
	}
	return e, nil
}
