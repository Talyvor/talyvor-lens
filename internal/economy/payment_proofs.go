package economy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/passkey"
)

// payment_proofs.go — B62.1: A PROOF RECORD FOR EVERY PAYMENT AN OWNER APPROVED.
//
// The assistant that picks a product should not be the only one able to show what the owner approved. So the owner's
// passkey or phone key signs exactly what the approval sheet shows: the challenge of a decision is the SHA-256 of a
// statement naming each approval, its agent, payee, amount, currency, the rule that asked, the rules version and the
// agent's task when there is one (newStatementChallenge), and an approval keeps the statement, the signature and the key (keepOwnerSignature).
//
// Before any money moves, the payment about to be sent is compared with what was approved (approvalMatches): another
// payee, another amount, another currency or another agent stops it in its own transaction, so whatever it held is
// released, and the owner is told in words. No approval says "up to", so an approval is for its amount exactly.
//
// Every payment the rules let through then gets one payment_proofs row in the same transaction as its money: what the
// owner was shown and signed, the rule and rules version, the key and its signature, and what was sent. A payment
// inside the rules has the same record with no signature, naming the rule that passed it. The table is append-only.

// PaymentProofVersion names the statement form an owner signs.
const PaymentProofVersion = "talyvor.payment_proof.v1"

// Rules that send a payment to the owner, or pass it.
const (
	ProofRuleApprovalAbove = "approval_above" // above the amount the rules ask the owner for
	ProofRulePrepared      = "prepared"       // an assistant's payment, always prepared for the owner (B52.41)
	ProofRuleWithinRules   = "within_rules"   // inside every rule: no approval needed
	ProofRuleAlwaysAllow   = "always_allow"   // inside an "always allow" of its payee (B41.104)
	ProofRuleMandate       = "mandate"        // within a mandate the owner signed in advance (B55.24)
	ProofRuleNoRules       = "no_rules"       // the agent has no rules
)

// ProofShown is one approval as the owner was shown it and signed it.
type ProofShown struct {
	ApprovalID   string `json:"approval_id"`
	AgentID      string `json:"agent_id"`
	AgentName    string `json:"agent_name"`
	Payee        Payee  `json:"payee"`
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
	Rule         string `json:"rule"`
	RulesVersion *int64 `json:"rules_version"`
	// Task is the agent's task on the approval, as kept (B62.2): signed with the rest (B62.8), "" for none, and no part
	// of approvalMatches. Omitted when "", so a statement signed before it carried the task reads it as "".
	Task string `json:"task,omitempty"`
}

// DecisionStatement is what an owner's decision signs: its challenge is the SHA-256 of the statement's JSON.
type DecisionStatement struct {
	Version     string          `json:"version"`
	WorkspaceID string          `json:"workspace_id"`
	Decision    string          `json:"decision,omitempty"` // approve | deny for several at once; "" for one, either
	Approvals   []ProofShown    `json:"approvals"`
	AlwaysAllow *PayeeAllowance `json:"always_allow,omitempty"`
	Nonce       string          `json:"nonce"`
}

// ProofSent is the payment as it was sent.
type ProofSent struct {
	AgentID     string `json:"agent_id"`
	Payee       Payee  `json:"payee"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Rail        string `json:"rail"` // what paid it: account | card | lxc | marketplace_bill
	Reference   string `json:"reference"`
	Memo        string `json:"memo,omitempty"`
	Task        string `json:"task,omitempty"` // what the agent says it was trying to do (B62.2); no part of approvalMatches
}

// PaymentProof is the record of one payment the rules let through.
type PaymentProof struct {
	ID           string          `json:"id"`
	WorkspaceID  string          `json:"workspace_id"`
	AgentID      string          `json:"agent_id"`
	PaymentRef   string          `json:"payment_ref"`
	ApprovalID   string          `json:"approval_id,omitempty"`
	Rule         string          `json:"rule"`
	RulesVersion *int64          `json:"rules_version"`
	Shown        ProofShown      `json:"shown"`
	Statement    string          `json:"statement,omitempty"`
	Signature    json.RawMessage `json:"signature,omitempty"`
	KeyKind      string          `json:"key_kind,omitempty"`
	KeyID        string          `json:"key_id,omitempty"`
	SignedAt     *time.Time      `json:"signed_at,omitempty"`
	Sent         ProofSent       `json:"sent"`
	CreatedAt    time.Time       `json:"created_at"`
	// Verified: the signature is by KeyID over Statement, and Statement shows exactly Shown. False with no signature.
	Verified bool `json:"verified"`
}

// ErrPaymentProofNotFound: no payment of this workspace has a proof record by that id or reference.
var ErrPaymentProofNotFound = errors.New("economy: no payment record by that id")

// shownAmount is the amount and currency a payment of amountULXC judged with req is shown in: what pays it, as the
// approval sheet names it (paidFrom).
func shownAmount(req AgentRequest, amountULXC int64) (int64, string) {
	switch {
	case req.PaidFrom.Kind != "":
		return req.PaidFrom.AmountMinor, req.PaidFrom.Currency
	case req.Listing != "":
		// The bill is kept in micro-dollars and collected in cents (B32): the cents the use adds to it.
		return amountULXC / ULXCPerUSDMicro / 10_000, "USD"
	}
	return amountULXC, "LXC"
}

// railOf is what pays a payment judged with req.
func railOf(req AgentRequest) string {
	switch {
	case req.ByCard:
		return "card"
	case req.PaidFrom.Kind != "":
		return req.PaidFrom.Kind
	case req.Listing != "":
		return "marketplace_bill"
	}
	return "lxc"
}

// latestRulesVersion is the agent's newest rules version, nil when its rules were never versioned.
func latestRulesVersion(ctx context.Context, q pgxDB, agentID string) (*int64, error) {
	var v *int64
	if err := q.QueryRow(ctx, `SELECT max(version)::bigint FROM agent_rules_versions WHERE agent_id = $1`, agentID).Scan(&v); err != nil {
		return nil, fmt.Errorf("economy: rules version: %w", err)
	}
	return v, nil
}

// shownApprovals reads ids of workspaceID as the owner is shown them, in the order of ids.
func shownApprovals(ctx context.Context, q pgxDB, workspaceID string, ids []string) ([]ProofShown, error) {
	rows, err := q.Query(ctx, `SELECT a.id, a.agent_id, COALESCE(g.name, ''), a.payee_kind, a.payee_id, a.payee_name,
		a.paid_from_currency, a.paid_from_amount_minor, a.amount_ulxc, a.rule, a.rules_version::bigint, a.task
		FROM agent_approvals a LEFT JOIN agent_accounts g ON g.id = a.agent_id WHERE a.id = ANY($1) AND a.workspace_id = $2`, ids, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: approval shown: %w", err)
	}
	defer rows.Close()
	byID := map[string]ProofShown{}
	for rows.Next() {
		var p ProofShown
		var minor *int64
		var ulxc int64
		if err := rows.Scan(&p.ApprovalID, &p.AgentID, &p.AgentName, &p.Payee.Kind, &p.Payee.ID, &p.Payee.Name,
			&p.Currency, &minor, &ulxc, &p.Rule, &p.RulesVersion, &p.Task); err != nil {
			return nil, fmt.Errorf("economy: approval shown: %w", err)
		}
		if p.Currency == "" || minor == nil { // filed before what pays it was kept (B57.11): its LXC
			p.Currency, p.AmountMinor = "LXC", ulxc
		} else {
			p.AmountMinor = *minor
		}
		byID[p.ApprovalID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ProofShown, 0, len(ids))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// statementHash is the challenge a statement is signed as: base64url of its SHA-256.
func statementHash(statement string) string {
	h := sha256.Sum256([]byte(statement))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// newStatementChallenge issues the challenge for purpose that signs st, with a fresh nonce, and keeps st beside it.
func (s *DualTokenStore) newStatementChallenge(ctx context.Context, workspaceID, purpose string, st DecisionStatement) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	st.Version, st.WorkspaceID, st.Nonce = PaymentProofVersion, workspaceID, base64.RawURLEncoding.EncodeToString(nonce)
	b, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	c := statementHash(string(b))
	if _, err := s.pool.Exec(ctx, `INSERT INTO webauthn_challenges (challenge, workspace_id, purpose, expires_at, statement) VALUES ($1, $2, $3, $4, $5)`,
		c, workspaceID, purpose, time.Now().Add(challengeTTL), string(b)); err != nil {
		return "", fmt.Errorf("economy: challenge: %w", err)
	}
	return c, nil
}

// signedChallenge is the challenge a answers: a device key's own, or the one in a passkey's clientDataJSON.
func signedChallenge(a passkey.Assertion) string {
	if a.DeviceID != "" {
		return a.Challenge
	}
	cdj, _ := passkey.Decode64(a.ClientDataJSON)
	return passkey.ChallengeOf(cdj)
}

// keepOwnerSignature keeps, on each of ids, the statement a signed — just checked and spent by spendAssertion — the
// signature and the key, inside tx.
func keepOwnerSignature(ctx context.Context, tx pgx.Tx, workspaceID string, ids []string, a passkey.Assertion) error {
	var statement string
	if err := tx.QueryRow(ctx, `SELECT statement FROM webauthn_challenges WHERE challenge = $1 AND workspace_id = $2`,
		signedChallenge(a), workspaceID).Scan(&statement); err != nil {
		return fmt.Errorf("economy: signed statement: %w", err)
	}
	kind, id := "passkey", a.CredentialID
	if a.DeviceID != "" {
		kind, id = "device", a.DeviceID
	}
	sig, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_approvals SET owner_statement = $3, owner_signature = $4, owner_key_kind = $5, owner_key_id = $6
		WHERE id = ANY($1) AND workspace_id = $2`, ids, workspaceID, statement, string(sig), kind, id); err != nil {
		return fmt.Errorf("economy: keep signature: %w", err)
	}
	return nil
}

// usedApproval is an approval as it is used up by the payment it approved.
type usedApproval struct {
	shown     ProofShown
	statement string
	signature []byte
	keyKind   string
	keyID     string
	signedAt  *time.Time
}

// useApproval uses up agentID's approved approval of fingerprint for the payment ref, inside tx, and answers it as
// the owner was shown it: from the statement they signed when they signed one. ok is false when there is none.
func useApproval(ctx context.Context, tx pgx.Tx, workspaceID, agentID, fingerprint, ref string) (u usedApproval, ok bool, err error) {
	var id, approver, evidence string
	err = tx.QueryRow(ctx, `UPDATE agent_approvals SET status = 'used', used_ref = $3
		WHERE id = (SELECT id FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2 AND status = 'approved' AND expires_at > now()
			ORDER BY created_at DESC LIMIT 1 FOR UPDATE)
		RETURNING id, owner_statement, owner_signature, owner_key_kind, owner_key_id, decided_at, approved_by_agent_id, agent_evidence`,
		agentID, fingerprint, ref).Scan(&id, &u.statement, &u.signature, &u.keyKind, &u.keyID, &u.signedAt, &approver, &evidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, false, nil
	}
	if err != nil {
		return u, false, fmt.Errorf("economy: use approval: %w", err)
	}
	shown, err := shownApprovals(ctx, tx, workspaceID, []string{id})
	if err != nil {
		return u, false, err
	}
	if len(shown) == 1 {
		u.shown = shown[0]
	}
	if st, found := statementShows(u.statement, id); found {
		u.shown = st
	}
	if approver != "" { // approved by an approver agent within its limits (B45.28): Talyvor's signed evidence
		u.keyKind, u.keyID = "approver_agent", approver
		u.signature, _ = json.Marshal(map[string]string{"evidence": evidence})
	}
	return u, true, nil
}

// statementShows is approvalID as statement shows it; found is false when statement is empty or does not name it.
func statementShows(statement, approvalID string) (ProofShown, bool) {
	if statement == "" {
		return ProofShown{}, false
	}
	var st DecisionStatement
	if json.Unmarshal([]byte(statement), &st) != nil {
		return ProofShown{}, false
	}
	i := slices.IndexFunc(st.Approvals, func(p ProofShown) bool { return p.ApprovalID == approvalID })
	if i < 0 {
		return ProofShown{}, false
	}
	return st.Approvals[i], true
}

// ApprovalChangedError: the payment about to be sent is not the one the owner approved, so nothing is sent.
type ApprovalChangedError struct {
	Shown       ProofShown
	Sent        ProofSent
	workspaceID string
}

func amountWords(minor int64, currency string) string {
	if currency == "LXC" {
		return LXCMoney(minor)
	}
	return Money(minor, currency)
}

func payeeWords(p Payee) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

func (e *ApprovalChangedError) Error() string {
	return fmt.Sprintf("%v: this payment is not the one you approved, so nothing was sent. You approved %s to %s, and it would have sent %s to %s",
		ErrAgentRule, amountWords(e.Shown.AmountMinor, e.Shown.Currency), payeeWords(e.Shown.Payee),
		amountWords(e.Sent.AmountMinor, e.Sent.Currency), payeeWords(e.Sent.Payee))
}

// Unwrap makes it a refusal by the agent's rules, answered as one.
func (e *ApprovalChangedError) Unwrap() error { return ErrAgentRule }

// approvalMatches refuses sent unless it is exactly the payment shown: the same agent, payee, currency and amount. The
// task is not compared: it never decides anything (B62.2).
func approvalMatches(workspaceID string, shown ProofShown, sent ProofSent) error {
	if shown.AgentID == sent.AgentID && shown.Payee.Kind == sent.Payee.Kind && shown.Payee.ID == sent.Payee.ID &&
		shown.Currency == sent.Currency && shown.AmountMinor == sent.AmountMinor {
		return nil
	}
	return &ApprovalChangedError{Shown: shown, Sent: sent, workspaceID: workspaceID}
}

// sentOf is workspaceID's payment of amountULXC judged with req, as it is sent, with the task ctx carries for it.
func sentOf(ctx context.Context, q pgxDB, workspaceID, agentID, ref string, req AgentRequest, amountULXC int64) (ProofSent, error) {
	payee, err := namePayee(ctx, q, req.Payee)
	if err != nil {
		return ProofSent{}, err
	}
	minor, currency := shownAmount(req, amountULXC)
	return ProofSent{AgentID: agentID, Payee: payee, AmountMinor: minor, Currency: currency, Rail: railOf(req), Reference: ref, Memo: req.Memo,
		Task: taskIn(ctx, workspaceID)}, nil
}

// recordPaymentProof writes the record of the payment ref the rules let through by rule, inside its transaction tx:
// with u, the approval it used; without, the payment as the rules judged it, signed by nobody.
func recordPaymentProof(ctx context.Context, tx pgx.Tx, workspaceID, agentID, ref, rule string, rulesVersion *int64, sent ProofSent, u *usedApproval) error {
	shown := ProofShown{AgentID: agentID, Payee: sent.Payee, AmountMinor: sent.AmountMinor, Currency: sent.Currency, Rule: rule, RulesVersion: rulesVersion}
	var approvalID, statement, keyKind, keyID string
	var signature []byte
	var signedAt *time.Time
	if u != nil {
		shown, approvalID, statement, signature, keyKind, keyID, signedAt = u.shown, u.shown.ApprovalID, u.statement, u.signature, u.keyKind, u.keyID, u.signedAt
		rule, rulesVersion = shown.Rule, shown.RulesVersion
	}
	if shown.AgentName == "" {
		if err := tx.QueryRow(ctx, `SELECT name FROM agent_accounts WHERE id = $1`, agentID).Scan(&shown.AgentName); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("economy: payment record: %w", err)
		}
	}
	sh, err := json.Marshal(shown)
	if err != nil {
		return err
	}
	se, err := json.Marshal(sent)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_proofs (id, workspace_id, agent_id, payment_ref, approval_id, rule, rules_version, shown,
			statement, signature, key_kind, key_id, signed_at, sent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) ON CONFLICT (agent_id, payment_ref) DO NOTHING`,
		"prf_"+uuid.NewString(), workspaceID, agentID, ref, approvalID, rule, rulesVersion, string(sh), statement, jsonText(signature), keyKind, keyID,
		signedAt, string(se)); err != nil {
		return fmt.Errorf("economy: payment record: %w", err)
	}
	return nil
}

// jsonText is b as JSON text for a jsonb parameter — nil, SQL null, when empty — so the simple protocol, which would
// send []byte as bytea, sends it as text too.
func jsonText(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	t := string(b)
	return &t
}

const paymentProofColumns = `id, workspace_id, agent_id, payment_ref, approval_id, rule, rules_version::bigint, shown, statement, signature,
	key_kind, key_id, signed_at, sent, created_at`

// PaymentProof reads workspaceID's record of one payment, by the record's id, the payment's reference or the id of the
// money entry that moved it, and says whether its signature verifies. Another workspace's is ErrPaymentProofNotFound.
func (s *DualTokenStore) PaymentProof(ctx context.Context, workspaceID, id string) (PaymentProof, error) {
	var p PaymentProof
	var shown, sent []byte
	var sig []byte
	err := s.pool.QueryRow(ctx, `SELECT `+paymentProofColumns+` FROM payment_proofs WHERE workspace_id = $1 AND (id = $2 OR payment_ref = $2
		OR payment_ref = (SELECT idempotency_key FROM money_entries WHERE id = $2 AND workspace_id = $1)) ORDER BY created_at LIMIT 1`,
		workspaceID, id).Scan(&p.ID, &p.WorkspaceID, &p.AgentID, &p.PaymentRef, &p.ApprovalID, &p.Rule, &p.RulesVersion, &shown, &p.Statement,
		&sig, &p.KeyKind, &p.KeyID, &p.SignedAt, &sent, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrPaymentProofNotFound
	}
	if err != nil {
		return p, fmt.Errorf("economy: payment record: %w", err)
	}
	if err := json.Unmarshal(shown, &p.Shown); err != nil {
		return p, fmt.Errorf("economy: payment record: %w", err)
	}
	if err := json.Unmarshal(sent, &p.Sent); err != nil {
		return p, fmt.Errorf("economy: payment record: %w", err)
	}
	if len(sig) > 0 {
		p.Signature = sig
	}
	p.Verified = s.VerifyPaymentProof(ctx, p)
	return p, nil
}

// VerifyPaymentProof says whether p's signature is by its key, over its statement, and the statement shows exactly
// p.Shown: a statement or shown fields changed afterwards, or a signature over another statement, do not verify.
func (s *DualTokenStore) VerifyPaymentProof(ctx context.Context, p PaymentProof) bool {
	if p.Statement == "" || len(p.Signature) == 0 || (p.KeyKind != "passkey" && p.KeyKind != "device") {
		return false
	}
	if st, ok := statementShows(p.Statement, p.ApprovalID); !ok || !sameShown(st, p.Shown) {
		return false
	}
	var a passkey.Assertion
	if json.Unmarshal(p.Signature, &a) != nil || signedChallenge(a) != statementHash(p.Statement) {
		return false
	}
	if p.KeyKind == "device" {
		var spki []byte
		if s.pool.QueryRow(ctx, `SELECT public_key FROM workspace_device_keys WHERE id = $1 AND workspace_id = $2`, p.KeyID, p.WorkspaceID).Scan(&spki) != nil {
			return false
		}
		return a.DeviceID == p.KeyID && devicekey.Verify(spki, a.Challenge, a.Signature) == nil
	}
	if s.approvalRP == nil {
		return false
	}
	var spki []byte
	if s.pool.QueryRow(ctx, `SELECT public_key FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2`, p.KeyID, p.WorkspaceID).Scan(&spki) != nil {
		return false
	}
	_, err := s.approvalRP.VerifyAssertion(a, signedChallenge(a), spki, 0)
	return a.CredentialID == p.KeyID && err == nil
}

func sameShown(a, b ProofShown) bool {
	va, vb := a.RulesVersion, b.RulesVersion
	return a.ApprovalID == b.ApprovalID && a.AgentID == b.AgentID && a.AgentName == b.AgentName && a.Payee == b.Payee &&
		a.AmountMinor == b.AmountMinor && a.Currency == b.Currency && a.Rule == b.Rule && a.Task == b.Task &&
		(va == nil) == (vb == nil) && (va == nil || *va == *vb)
}

// tellApprovalChanged tells the owner, on their phones and browsers, that a payment they approved was stopped because
// what was about to be sent was not what they approved.
func (s *DualTokenStore) tellApprovalChanged(e *ApprovalChangedError) {
	if s.nativePusher == nil && s.approvalPusher == nil {
		return
	}
	body := fmt.Sprintf("%s’s payment was stopped: you approved %s to %s, and it would have sent %s to %s. Nothing was sent.",
		e.Shown.AgentName, amountWords(e.Shown.AmountMinor, e.Shown.Currency), payeeWords(e.Shown.Payee),
		amountWords(e.Sent.AmountMinor, e.Sent.Currency), payeeWords(e.Sent.Payee))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.pushDevices(ctx, e.workspaceID, nativepush.Message{Title: "A payment was stopped", Body: body, Category: "approval_changed",
			Data: map[string]string{"type": "approval_changed", "approval_id": e.Shown.ApprovalID, "workspace_id": e.workspaceID, "agent_id": e.Shown.AgentID}})
		payload, err := json.Marshal(map[string]any{"type": "approval_changed", "approval_id": e.Shown.ApprovalID, "workspace_id": e.workspaceID,
			"agent_id": e.Shown.AgentID, "body": body})
		if err != nil {
			slog.Warn("approval changed: push", slog.String("err", err.Error()))
			return
		}
		s.pushWeb(ctx, e.workspaceID, payload)
	}()
}
