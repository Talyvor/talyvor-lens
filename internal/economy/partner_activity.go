package economy

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// partner_activity.go — B62.5: THE ACCOUNT PARTNER'S RISK TEAM SEES WHAT EVERY AGENT DID, AND UNDER WHICH RULES.
//
// One row per agent payment, newest first: the account (its id and verification level), the agent, the amount and
// currency, the rule and rules version that judged it, whether the owner approved it and with what kind of key, its
// proof record (B62.1) and its state. The rows are every payment the rules let through (payment_proofs) and every
// approval that has not become one (agent_approvals): waiting for the owner, refused by the owner, expired, or
// approved and not sent. A payment the rules refused outright files no row anywhere, so it is not here.
//
// No name, email, phone number, address, payee or memo is in a row: the partner holds the identity file itself, so an
// account is its id and its level. PartnerPaymentFields is the whole list, and the tests hold every row to it.
//
// Each row keeps the mode it was written in (B62.7), and the view shows one mode at a time, never the two mixed: Live
// once anything in this Lens moves live money, with the Live payments only; Sandbox until then, with the Sandbox ones.

// Payment states in the partner's view.
const (
	PartnerStateSent            = "sent"
	PartnerStateWaitingForOwner = "waiting_for_owner"
	PartnerStateRefusedByOwner  = "refused_by_owner"
	PartnerStateExpired         = "expired"
	PartnerStateApprovedNotSent = "approved_not_sent"
)

// PartnerStates is every state a row may be in.
var PartnerStates = []string{PartnerStateSent, PartnerStateWaitingForOwner, PartnerStateRefusedByOwner, PartnerStateExpired,
	PartnerStateApprovedNotSent}

// PartnerPayment is one agent payment as the partner sees it.
type PartnerPayment struct {
	ID                string            `json:"id"` // the proof record's id, or the approval's while there is none
	At                time.Time         `json:"at"`
	AccountID         string            `json:"account_id"`
	VerificationLevel VerificationLevel `json:"verification_level"`
	AgentID           string            `json:"agent_id"`
	AmountMinor       int64             `json:"amount_minor"`
	Currency          string            `json:"currency"`
	Rule              string            `json:"rule"`
	RulesVersion      *int64            `json:"rules_version"`
	OwnerApproved     bool              `json:"owner_approved"`
	ApprovalKey       string            `json:"approval_key"` // passkey | device | approver_agent | ""
	ProofID           string            `json:"proof_id"`     // "" until the payment is sent
	State             string            `json:"state"`
	Mode              string            `json:"mode"` // sandbox | live, kept when the record was written (B62.7)
}

// PartnerPaymentFields is every field of a PartnerPayment on the wire, in the CSV's column order.
var PartnerPaymentFields = []string{"id", "at", "account_id", "verification_level", "agent_id", "amount_minor", "currency",
	"rule", "rules_version", "owner_approved", "approval_key", "proof_id", "state", "mode"}

// PartnerFilter narrows the view. Zero values filter nothing.
type PartnerFilter struct {
	From, To  time.Time // From inclusive, To exclusive
	AccountID string
	State     string
	Mode      string // the view's mode: only the rows written in it
	// Before is the cursor: rows strictly older than (BeforeAt, BeforeID).
	BeforeAt time.Time
	BeforeID string
	Limit    int
}

// PartnerDay is one day's count of agent payments, by state.
type PartnerDay struct {
	Day     string           `json:"day"` // YYYY-MM-DD, UTC
	Count   int64            `json:"count"`
	ByState map[string]int64 `json:"by_state"`
}

// PartnerViewMode is the mode the partner's view shows: "live" once anything in this Lens moves live money (a label
// is Live), "sandbox" while nothing does.
func (s *DualTokenStore) PartnerViewMode(ctx context.Context) (string, error) {
	labels, err := s.Labels(ctx)
	if err != nil {
		return "", err
	}
	for _, l := range labels {
		if l == LabelLive {
			return LabelLive, nil
		}
	}
	return LabelSandbox, nil
}

// partnerActivity is every agent payment, as the partner sees it, with the columns the filters read.
const partnerActivity = `WITH activity AS (
	SELECT p.id, p.created_at AS at, p.workspace_id, p.agent_id,
	       COALESCE((p.sent->>'amount_minor')::bigint, 0) AS amount_minor, COALESCE(p.sent->>'currency', '') AS currency,
	       p.rule, p.rules_version::bigint AS rules_version,
	       (p.approval_id <> '' AND p.key_kind <> 'approver_agent') AS owner_approved, p.key_kind AS approval_key,
	       p.id AS proof_id, 'sent' AS state, p.mode
	  FROM payment_proofs p
	UNION ALL
	SELECT a.id, a.created_at, a.workspace_id, a.agent_id,
	       CASE WHEN a.paid_from_currency <> '' AND a.paid_from_amount_minor IS NOT NULL THEN a.paid_from_amount_minor ELSE a.amount_ulxc END,
	       CASE WHEN a.paid_from_currency <> '' AND a.paid_from_amount_minor IS NOT NULL THEN a.paid_from_currency ELSE 'LXC' END,
	       a.rule, a.rules_version::bigint,
	       (a.status IN ('approved', 'used') AND a.approved_by_agent_id = ''),
	       CASE WHEN a.status NOT IN ('approved', 'used') THEN ''
	            WHEN a.approved_by_agent_id <> '' THEN 'approver_agent' ELSE a.owner_key_kind END,
	       '',
	       CASE a.status WHEN 'pending' THEN 'waiting_for_owner' WHEN 'denied' THEN 'refused_by_owner'
	                     WHEN 'expired' THEN 'expired' WHEN 'approved' THEN 'approved_not_sent' ELSE 'sent' END,
	       a.mode
	  FROM agent_approvals a
	 WHERE NOT EXISTS (SELECT 1 FROM payment_proofs p WHERE p.approval_id = a.id)
)`

// partnerWhere is the filters, over $1 from, $2 to, $3 account, $4 state and $5 mode.
const partnerWhere = ` WHERE ($1::timestamptz IS NULL OR at >= $1) AND ($2::timestamptz IS NULL OR at < $2)
	AND ($3 = '' OR workspace_id = $3) AND ($4 = '' OR state = $4) AND ($5 = '' OR mode = $5)`

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// PartnerActivity is one page of the partner's view under f, newest first.
func (s *DualTokenStore) PartnerActivity(ctx context.Context, f PartnerFilter) ([]PartnerPayment, error) {
	rows, err := s.pool.Query(ctx, partnerActivity+`
		SELECT id, at, workspace_id, agent_id, amount_minor, currency, rule, rules_version, owner_approved, approval_key,
		       proof_id, state, mode FROM activity`+partnerWhere+`
		   AND ($6::timestamptz IS NULL OR (at, id) < ($6, $7))
		 ORDER BY at DESC, id DESC LIMIT $8`,
		nullTime(f.From), nullTime(f.To), f.AccountID, f.State, f.Mode, nullTime(f.BeforeAt), f.BeforeID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("economy: partner activity: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PartnerPayment, error) {
		var p PartnerPayment
		err := row.Scan(&p.ID, &p.At, &p.AccountID, &p.AgentID, &p.AmountMinor, &p.Currency, &p.Rule, &p.RulesVersion,
			&p.OwnerApproved, &p.ApprovalKey, &p.ProofID, &p.State, &p.Mode)
		p.At = p.At.UTC()
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: partner activity: %w", err)
	}
	// An account's level, read once per account on the page: the level its checks reached. On a Sandbox payment the
	// Sandbox's own checks count; on a Live one, a real provider's only.
	levels := map[string]VerificationLevel{}
	for i := range out {
		id := out[i].AccountID + "\x00" + out[i].Mode
		level, ok := levels[id]
		if !ok {
			checks, err := readVerificationChecks(ctx, s.pool, out[i].AccountID)
			if err != nil {
				return nil, err
			}
			shown, live := reachedLevels(checks)
			if level = shown; out[i].Mode == LabelLive {
				level = live
			}
			levels[id] = level
		}
		out[i].VerificationLevel = level
	}
	return out, nil
}

// PartnerActivitySummary is the count of agent payments under f by UTC day, newest day first, and by state.
func (s *DualTokenStore) PartnerActivitySummary(ctx context.Context, f PartnerFilter) ([]PartnerDay, error) {
	rows, err := s.pool.Query(ctx, partnerActivity+`
		SELECT to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, state, count(*) FROM activity`+partnerWhere+`
		 GROUP BY 1, 2 ORDER BY 1 DESC, 2`,
		nullTime(f.From), nullTime(f.To), f.AccountID, f.State, f.Mode)
	if err != nil {
		return nil, fmt.Errorf("economy: partner summary: %w", err)
	}
	defer rows.Close()
	var out []PartnerDay
	for rows.Next() {
		var day, state string
		var n int64
		if err := rows.Scan(&day, &state, &n); err != nil {
			return nil, fmt.Errorf("economy: partner summary: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Day != day {
			out = append(out, PartnerDay{Day: day, ByState: map[string]int64{}})
		}
		out[len(out)-1].Count += n
		out[len(out)-1].ByState[state] = n
	}
	return out, rows.Err()
}
