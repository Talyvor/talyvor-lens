package economy

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/passkey"
)

// negotiation_deals.go — B52.13: COUNTER CLOSES INSIDE THE MANDATE; EVERYTHING ELSE GOES TO THE OWNER'S PHONE.
//
// CloseNegotiation accepts a counterparty's latest offer, exactly as recorded on its term sheet. Counter accepts on its
// own only when the signed mandate's authority is accept_inside_mandate, the price is inside the walk-away and the offer
// meets every must-have, and — when it pays — the payment is inside the agent's limits. Anything else is a binding
// commitment the owner makes: the deal waits, pays nothing, and the owner's phones are told. PrepareDealApproval
// writes the statement they sign — the counterparty's exact terms against the mandate, why it is beyond it, and the
// payment — and ApproveNegotiationDeal keeps it only with an assertion over that statement by one of the workspace's
// passkeys or device keys, while it is still exactly the deal as it stands. An accepted deal closes the negotiation.
//
// Payment goes only through the payment request path (B40.3) as the client "counter", which a permission mandate
// (B40.2) names to let Counter pay: the agent's rules and the mandate's limits, payee scope and counterparty rules
// (B45.30) judge it there, and a payment they do not allow waits for the owner's approval (B40.5). The request is
// prepared only once the deal is accepted, so a deal waiting for the owner has nothing anyone could pay.

// ErrNegotiationDealDecided: the deal was accepted or declined already; deciding it again does nothing.
var ErrNegotiationDealDecided = errors.New("economy: this deal was already decided")

// ClientCounter is the client Counter prepares a deal's payment as; only a permission mandate naming it lets Counter
// pay on its own, and no caller can name it.
const ClientCounter = "counter"

// DealApprovalVersion names the statement form the owner signs.
const DealApprovalVersion = "talyvor.negotiation_deal_approval.v1"

// NegotiationDeal is a deal Counter closed or put to the owner.
type NegotiationDeal struct {
	ID            string           `json:"id"`
	NegotiationID string           `json:"negotiation_id"`
	Counterparty  string           `json:"counterparty"`
	Currency      string           `json:"currency"`
	Terms         NegotiationTerms `json:"terms"`   // theirs, exactly as recorded
	Outside       []string         `json:"outside"` // why it is beyond the mandate; empty when Counter accepted it
	AgentID       string           `json:"agent_id,omitempty"`
	PayeeID       string           `json:"payee_id,omitempty"`
	Status        string           `json:"status"`               // accepted, awaiting_approval or declined
	RequestID     string           `json:"request_id,omitempty"` // the payment request that pays it
	CreatedBy     string           `json:"created_by"`
	DecidedBy     string           `json:"decided_by,omitempty"` // "mandate" when Counter accepted it inside the mandate
	CreatedAt     time.Time        `json:"created_at"`
	DecidedAt     *time.Time       `json:"decided_at,omitempty"`
}

// NegotiationCloseIn is the offer to accept, by counterparty, and who pays whom: a saved payee, paid by AgentID's
// account or the workspace's own when "". No payee: the deal is recorded and nothing is paid here.
type NegotiationCloseIn struct {
	Counterparty string `json:"counterparty"`
	AgentID      string `json:"agent_id"`
	PayeeID      string `json:"payee_id"`
}

// DealApprovalStatement is what the owner signs: the counterparty's exact terms against the mandate.
type DealApprovalStatement struct {
	Version       string           `json:"version"`
	DealID        string           `json:"deal_id"`
	NegotiationID string           `json:"negotiation_id"`
	WorkspaceID   string           `json:"workspace_id"`
	Task          NegotiationTask  `json:"task"`
	Counterparty  string           `json:"counterparty"`
	Currency      string           `json:"currency"`
	Terms         NegotiationTerms `json:"terms"`
	Mandate       struct {
		Side          string   `json:"side"`
		TargetMinor   int64    `json:"target_minor"`
		WalkAwayMinor int64    `json:"walk_away_minor"`
		MustHaves     []string `json:"must_haves"`
		Authority     string   `json:"authority"`
	} `json:"mandate"`
	Outside   []string  `json:"outside"`
	AgentID   string    `json:"agent_id"`
	PayeeID   string    `json:"payee_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// dealOutside is why accepting terms is beyond t: an authority short of accepting, a price at or past the walk-away,
// or a must-have the offer does not meet.
func dealOutside(t NegotiationMandateTerms, terms NegotiationTerms) []string {
	out := []string{}
	if t.Authority != AuthorityAccept {
		out = append(out, "the mandate lets Counter "+strings.ReplaceAll(t.Authority, "_", " ")+", not accept: the owner accepts")
	}
	if t.Side == NegotiationBuy && terms.PriceMinor >= t.WalkAwayMinor || t.Side == NegotiationSell && terms.PriceMinor <= t.WalkAwayMinor {
		out = append(out, "their price is at or past the walk-away")
	}
	for _, mh := range t.MustHaves {
		if !slices.ContainsFunc(terms.MustHavesMet, func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), mh) }) {
			out = append(out, "the offer does not meet the must-have: "+mh)
		}
	}
	return out
}

// dealPayment is the payment request a deal is paid by.
func dealPayment(t NegotiationMandateTerms, d NegotiationDeal) PaymentRequestIn {
	return PaymentRequestIn{Client: ClientCounter, AgentID: d.AgentID, Items: []PaymentRequestItemIn{{PayeeID: d.PayeeID,
		Currency: t.Currency, AmountMinor: d.Terms.PriceMinor, Reference: "Counter " + d.ID}}}
}

// CloseNegotiation accepts in.Counterparty's latest offer under mandate id: at once when it is inside the mandate and
// its payment inside the agent's limits, otherwise as a deal awaiting the owner's signature.
func (s *DualTokenStore) CloseNegotiation(ctx context.Context, workspaceID, id, by string, in NegotiationCloseIn) (NegotiationDeal, error) {
	t, err := s.signedNegotiation(ctx, workspaceID, id)
	if err != nil {
		return NegotiationDeal{}, err
	}
	m, err := s.NegotiationMandate(ctx, workspaceID, id)
	if err != nil {
		return NegotiationDeal{}, err
	}
	bad := func(format string, args ...any) (NegotiationDeal, error) {
		return NegotiationDeal{}, fmt.Errorf("%w: %s", ErrNegotiationMandate, fmt.Sprintf(format, args...))
	}
	d := NegotiationDeal{ID: "deal_" + uuid.NewString(), NegotiationID: id, Counterparty: strings.ToLower(strings.TrimSpace(in.Counterparty)),
		Currency: t.Currency, AgentID: strings.TrimSpace(in.AgentID), PayeeID: strings.TrimSpace(in.PayeeID), CreatedBy: by}
	sheet := m.TermSheets[d.Counterparty]
	if len(sheet) == 0 || sheet[len(sheet)-1].Party != "theirs" {
		return bad("there is no offer of theirs to accept: they have made none, or Counter's is the latest")
	}
	offer := sheet[len(sheet)-1]
	d.Terms, d.Outside = offer.Terms, dealOutside(t, offer.Terms)
	switch {
	case d.AgentID != "" && d.PayeeID == "":
		return bad("name the payee_id the agent pays")
	case d.PayeeID != "" && t.Side != NegotiationBuy:
		return bad("Counter pays only when it buys")
	case d.AgentID != "":
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`, d.AgentID, workspaceID).Scan(&ok); err != nil {
			return NegotiationDeal{}, fmt.Errorf("economy: negotiation deal: %w", err)
		}
		if !ok {
			return bad("%q is not an agent of this workspace", d.AgentID)
		}
	}
	if d.PayeeID != "" {
		// Judged as the payment request will be, and nothing kept: blocked refuses the close, needs approval asks the owner.
		pr := dealPayment(t, d)
		if _, err := pr.normalise(); err != nil {
			return NegotiationDeal{}, err
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return NegotiationDeal{}, err
		}
		items, err := s.judgePaymentItems(ctx, tx, workspaceID, pr)
		_ = tx.Rollback(ctx)
		if err != nil {
			return NegotiationDeal{}, err
		}
		switch it := items[0]; it.Decision {
		case ItemBlocked:
			return bad("Counter cannot pay this: %s", it.Reason)
		case ItemNeedsApproval:
			d.Outside = append(d.Outside, fmt.Sprintf("the payment is beyond the agent's limits (%s): %s", cmp.Or(it.Rule, "mandate"), it.Reason))
		}
	}
	d.Status = "awaiting_approval"
	if len(d.Outside) == 0 {
		d.Status, d.DecidedBy = "accepted", "mandate"
	}
	terms, _ := json.Marshal(d.Terms)
	outside, _ := json.Marshal(d.Outside)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO negotiation_deals (id, mandate_id, workspace_id, counterparty, offer_id, terms, outside, agent_id, payee_id,
			status, created_by, decided_by, decided_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, CASE WHEN $10 = 'accepted' THEN now() END)`,
			d.ID, id, workspaceID, d.Counterparty, offer.ID, string(terms), string(outside), d.AgentID, d.PayeeID, d.Status, by, d.DecidedBy)
		if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
			return fmt.Errorf("%w: this negotiation already has a deal: decline it first", ErrNegotiationDealDecided)
		}
		if err != nil {
			return fmt.Errorf("economy: negotiation deal: %w", err)
		}
		if d.Status == "accepted" {
			return closeNegotiation(ctx, tx, workspaceID, id)
		}
		return nil
	})
	if err != nil {
		return NegotiationDeal{}, err
	}
	if d.Status == "accepted" {
		return s.payDeal(ctx, workspaceID, by, t, d)
	}
	s.notifyDealAwaiting(workspaceID, t, d)
	return s.negotiationDeal(ctx, workspaceID, id)
}

// closeNegotiation ends mandate id once its deal is accepted: Counter makes no further offer under it.
func closeNegotiation(ctx context.Context, tx pgx.Tx, workspaceID, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE negotiation_mandates SET ended_at = now(), ended_by = 'deal' WHERE id = $1 AND workspace_id = $2 AND ended_at IS NULL`,
		id, workspaceID); err != nil {
		return fmt.Errorf("economy: close negotiation: %w", err)
	}
	return nil
}

// payDeal prepares an accepted deal's payment request, under the deal's own key so it is prepared once, and pays it
// when the agent's limits allow; otherwise it waits for the owner's approval (B40.5).
func (s *DualTokenStore) payDeal(ctx context.Context, workspaceID, by string, t NegotiationMandateTerms, d NegotiationDeal) (NegotiationDeal, error) {
	if d.PayeeID == "" {
		return s.negotiationDeal(ctx, workspaceID, d.NegotiationID)
	}
	r, _, err := s.PreparePaymentRequest(ctx, workspaceID, by, "counter:"+d.ID, dealPayment(t, d))
	if err != nil {
		return d, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE negotiation_deals SET request_id = $2 WHERE id = $1`, d.ID, r.ID); err != nil {
		return d, fmt.Errorf("economy: negotiation deal: %w", err)
	}
	if r.Status == ItemAuto {
		if _, err := s.PayPaymentRequest(ctx, workspaceID, "", r.ID); err != nil {
			return d, err
		}
	}
	return s.negotiationDeal(ctx, workspaceID, d.NegotiationID)
}

// negotiationDeal reads mandate id's deal that is not declined.
func (s *DualTokenStore) negotiationDeal(ctx context.Context, workspaceID, id string) (NegotiationDeal, error) {
	return scanNegotiationDeal(s.pool.QueryRow(ctx, dealOfNegotiation, id, workspaceID))
}

// dealOfNegotiation reads a negotiation's deal that is not declined.
const dealOfNegotiation = `SELECT ` + dealCols + ` FROM negotiation_deals d JOIN negotiation_mandates m ON m.id = d.mandate_id
	WHERE d.mandate_id = $1 AND d.workspace_id = $2 AND d.status <> 'declined'`

const dealCols = `d.id, d.mandate_id, d.counterparty, m.canonical::jsonb->>'currency', d.terms, d.outside, d.agent_id, d.payee_id, d.status,
	d.request_id, d.created_by, d.decided_by, d.created_at, d.decided_at`

func scanNegotiationDeal(row pgx.Row) (NegotiationDeal, error) {
	var d NegotiationDeal
	err := row.Scan(&d.ID, &d.NegotiationID, &d.Counterparty, &d.Currency, &d.Terms, &d.Outside, &d.AgentID, &d.PayeeID, &d.Status,
		&d.RequestID, &d.CreatedBy, &d.DecidedBy, &d.CreatedAt, &d.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, fmt.Errorf("%w: it has no deal waiting or closed", ErrNegotiationNotFound)
	}
	if err != nil {
		return d, fmt.Errorf("economy: negotiation deal: %w", err)
	}
	return d, nil
}

// dealStatement is the statement approving d under t, expiring at expires.
func dealStatement(workspaceID string, t NegotiationMandateTerms, d NegotiationDeal, expires time.Time) string {
	st := DealApprovalStatement{Version: DealApprovalVersion, DealID: d.ID, NegotiationID: d.NegotiationID, WorkspaceID: workspaceID,
		Task: t.Task, Counterparty: d.Counterparty, Currency: t.Currency, Terms: d.Terms, Outside: d.Outside, AgentID: d.AgentID,
		PayeeID: d.PayeeID, ExpiresAt: expires}
	st.Mandate.Side, st.Mandate.TargetMinor, st.Mandate.WalkAwayMinor = t.Side, t.TargetMinor, t.WalkAwayMinor
	st.Mandate.MustHaves, st.Mandate.Authority = t.MustHaves, t.Authority
	b, _ := json.Marshal(st)
	return string(b)
}

// awaitingDeal is mandate id's deal while the owner may still sign it, with the signed mandate it is under.
func (s *DualTokenStore) awaitingDeal(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workspaceID, id string, forUpdate bool) (NegotiationMandateTerms, NegotiationDeal, error) {
	var row pgx.Row
	if forUpdate {
		row = q.QueryRow(ctx, dealOfNegotiation+` FOR UPDATE OF d`, id, workspaceID)
	} else {
		row = q.QueryRow(ctx, dealOfNegotiation, id, workspaceID)
	}
	d, err := scanNegotiationDeal(row)
	switch {
	case err != nil:
		return NegotiationMandateTerms{}, d, err
	case d.Status != "awaiting_approval":
		return NegotiationMandateTerms{}, d, fmt.Errorf("%w: it is %s", ErrNegotiationDealDecided, d.Status)
	case time.Since(d.CreatedAt) >= s.approvalLife():
		return NegotiationMandateTerms{}, d, fmt.Errorf("%w: the deal waited %s unanswered and expired: decline it and close again", ErrNegotiationMandate, s.approvalLife())
	}
	t, err := s.signedNegotiation(ctx, workspaceID, id)
	return t, d, err
}

// PrepareDealApproval writes the statement approving mandate id's waiting deal, the challenge it is signed over, and
// the passkeys that may sign it.
func (s *DualTokenStore) PrepareDealApproval(ctx context.Context, workspaceID, id string) (statement, challenge string, credentialIDs []string, err error) {
	t, d, err := s.awaitingDeal(ctx, s.pool, workspaceID, id, false)
	if err != nil {
		return "", "", nil, err
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	statement = dealStatement(workspaceID, t, d, time.Now().Add(approvalStatementTTL).UTC().Truncate(time.Second))
	return statement, MandateChallenge(statement), credentialIDs, nil
}

// ApproveNegotiationDeal keeps by's approval of mandate id's waiting deal — statement as PrepareDealApproval wrote it,
// signed by a — accepts the deal, closes the negotiation and pays it. A statement altered, expired, another deal's or
// no longer the deal as it stands is refused; a deal approved once is ErrNegotiationDealDecided and pays nothing.
func (s *DualTokenStore) ApproveNegotiationDeal(ctx context.Context, workspaceID, id, by, statement string, a *passkey.Assertion) (NegotiationDeal, error) {
	var st DealApprovalStatement
	dec := json.NewDecoder(strings.NewReader(statement))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return NegotiationDeal{}, fmt.Errorf("%w: the statement is not a deal approval: %v", ErrNegotiationMandate, err)
	}
	if st.Version != DealApprovalVersion || st.WorkspaceID != workspaceID || st.NegotiationID != id {
		return NegotiationDeal{}, fmt.Errorf("%w: the statement is not this negotiation's deal, as prepared for signing", ErrNegotiationMandate)
	}
	if !time.Now().Before(st.ExpiresAt) {
		return NegotiationDeal{}, fmt.Errorf("%w: the statement expired at %s: prepare the approval again", ErrNegotiationMandate, st.ExpiresAt.Format(time.RFC3339))
	}
	if a == nil {
		return NegotiationDeal{}, fmt.Errorf("%w: the approval is not signed: sign its challenge with a passkey or the phone app", ErrNegotiationMandate)
	}
	var t NegotiationMandateTerms
	var d NegotiationDeal
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		// The deal's row is held until the approval is kept: two approvals at once are one, then a replay.
		if t, d, err = s.awaitingDeal(ctx, tx, workspaceID, id, true); err != nil {
			return err
		}
		if dealStatement(workspaceID, t, d, st.ExpiresAt) != statement {
			return fmt.Errorf("%w: the deal is no longer what the statement shows: the approval is invalid", ErrNegotiationMandate)
		}
		if err := s.signOver(ctx, tx, workspaceID, MandateChallenge(statement), *a); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE negotiation_deals SET status = 'accepted', statement = $2, credential_id = $3, device_id = $4,
			client_data_json = $5, authenticator_data = $6, signature = $7, decided_by = $8, decided_at = now() WHERE id = $1`,
			d.ID, statement, a.CredentialID, a.DeviceID, a.ClientDataJSON, a.AuthenticatorData, a.Signature, by); err != nil {
			return fmt.Errorf("economy: negotiation deal: %w", err)
		}
		return closeNegotiation(ctx, tx, workspaceID, id)
	})
	if err != nil {
		return NegotiationDeal{}, err
	}
	return s.payDeal(ctx, workspaceID, by, t, d)
}

// DeclineNegotiationDeal declines mandate id's waiting deal: nothing is accepted or paid, and Counter may close again.
func (s *DualTokenStore) DeclineNegotiationDeal(ctx context.Context, workspaceID, id, by string) (NegotiationDeal, error) {
	d, err := scanNegotiationDeal(s.pool.QueryRow(ctx, `UPDATE negotiation_deals d SET status = 'declined', decided_by = $3, decided_at = now()
		FROM negotiation_mandates m WHERE m.id = d.mandate_id AND d.mandate_id = $1 AND d.workspace_id = $2 AND d.status = 'awaiting_approval'
		RETURNING `+dealCols, id, workspaceID, by))
	if errors.Is(err, ErrNegotiationNotFound) {
		return d, fmt.Errorf("%w: it has no deal waiting for you", ErrNegotiationNotFound)
	}
	return d, err
}

// notifyDealAwaiting tells the workspace's phones, in the background, that a deal waits for the owner: the
// counterparty, their price and what it is for — never the walk-away.
func (s *DualTokenStore) notifyDealAwaiting(workspaceID string, t NegotiationMandateTerms, d NegotiationDeal) {
	if s.nativePusher == nil {
		return
	}
	m := nativepush.Message{Title: "Counter needs your approval", Body: d.Counterparty + " · " + formatMinor(t.Currency, d.Terms.PriceMinor) + " · " + t.Task.What,
		Data: map[string]string{"type": "negotiation_deal", "negotiation_id": d.NegotiationID, "workspace_id": workspaceID}}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.pushDevices(ctx, workspaceID, m)
	}()
}
