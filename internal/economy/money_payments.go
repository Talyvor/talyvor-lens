package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// money_payments.go — B30.17: SEND MONEY TO PEOPLE AND COMPANIES OUTSIDE TALYVOR.
//
// A payment out pays a saved payee (B30.16) from the company's account in one currency or from one of its agents'
// sub-accounts, through the account partner — the Test partner until payments_out (RED) is cleared. From an agent's
// account it is judged by the agent's rules as spending, at what it is worth in LXC at the conversion partner's rate:
// its limits per payment, hour, day, week and month, its payee lists and caps, and its approval amount — a payment
// above that files an approval and moves nothing until the owner approves it and the agent asks again. Then it is
// screened (B30.6), its money held, and it is sent.
//
// Its states are pending (held, not yet with the partner), sent (the partner has it), completed (the money went:
// hold → partner), failed (it never went: hold → back to the account) and returned (it went, then came back:
// partner → back to the account). Every movement is a money entry under a key derived from the payment, so a retry
// moves nothing twice. A payment's later states are read from the partner when it is read (RefreshOutsidePayment).

var (
	// ErrOutsidePayment: a payment out without a payee, a positive amount in GBP, EUR or USD, or an idempotency key.
	ErrOutsidePayment = errors.New("economy: invalid payment")
	// ErrOutsidePaymentNotFound: no such payment out in this workspace, or not the agent's.
	ErrOutsidePaymentNotFound = errors.New("economy: no such payment")
)

// The states of a payment out.
const (
	PaymentPending   = "pending"
	PaymentSent      = "sent"
	PaymentCompleted = "completed"
	PaymentFailed    = "failed"
	PaymentReturned  = "returned"
)

// OutsidePayment is a payment out to a saved payee, and where it stands.
type OutsidePayment struct {
	ID             string    `json:"id"`
	WorkspaceID    string    `json:"workspace_id"`
	AccountID      string    `json:"account_id"`         // the account it is paid from
	AgentID        string    `json:"agent_id,omitempty"` // the agent whose account pays it
	PayeeID        string    `json:"payee_id"`
	PayeeName      string    `json:"payee_name"`
	AmountMinor    int64     `json:"amount_minor"`
	Currency       string    `json:"currency"`
	Funding        string    `json:"funding"`
	Reference      string    `json:"reference"`
	IdempotencyKey string    `json:"idempotency_key"`
	ValueULXC      int64     `json:"value_ulxc"` // what the agent's rules counted it as; 0 from the company's account
	Status         string    `json:"status"`
	Partner        string    `json:"partner,omitempty"`
	PartnerRef     string    `json:"partner_ref,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	holdAccountID string
	at            time.Time // when the agent's rules judge it (B30.18: a standing order's run); zero means now
}

// key is the idempotency key of the payment's money entry for step: the hold, the failure or the return. The entry
// that moves the money out takes the screening key, so its screening is the payment's own. Each step's prefix is its
// own, so no caller's key makes one payment's entry another's.
func (p OutsidePayment) key(step string) string {
	return "payment_out_" + step + ":" + p.IdempotencyKey
}

func (p OutsidePayment) screenKey() string { return "payment_out:" + p.IdempotencyKey }

func (p OutsidePayment) same(q OutsidePayment) bool {
	return p.PayeeID == q.PayeeID && p.AmountMinor == q.AmountMinor && p.Currency == q.Currency && p.Funding == q.Funding &&
		p.Reference == q.Reference && (q.AccountID == "" || p.AccountID == q.AccountID) && (q.AgentID == "" || p.AgentID == q.AgentID)
}

// minorString is minor units as a decimal amount with the currency: "GBP 12.34".
func minorString(minor int64, currency string) string {
	d := MoneyCurrencies[currency]
	s := fmt.Sprintf("%0*d", d+1, minor)
	return currency + " " + s[:len(s)-d] + "." + s[len(s)-d:]
}

const outsidePaymentCols = `id, workspace_id, account_id, hold_account_id, agent_id, payee_id, payee_name, amount_minor, currency, funding,
	reference, idempotency_key, value_ulxc, status, partner, partner_ref, detail, created_at, updated_at`

func scanOutsidePayment(row pgx.Row) (OutsidePayment, error) {
	var p OutsidePayment
	err := row.Scan(&p.ID, &p.WorkspaceID, &p.AccountID, &p.holdAccountID, &p.AgentID, &p.PayeeID, &p.PayeeName, &p.AmountMinor,
		&p.Currency, &p.Funding, &p.Reference, &p.IdempotencyKey, &p.ValueULXC, &p.Status, &p.Partner, &p.PartnerRef, &p.Detail,
		&p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrOutsidePaymentNotFound
	}
	return p, err
}

// payingAccount is the account a payment is paid from, its company's account in the same currency — which holds the
// money at the partner — and that account's partner account.
type payingAccount struct {
	id, agentID, companyID, partnerRef, partnerID string
}

// PayOutside pays in.AmountMinor of in.Currency to workspaceID's saved payee in.PayeeID, under in.IdempotencyKey: from
// the account in.AccountID, or with in.AgentID set from that agent's account in the currency, or else from the
// company's. The same key again answers the payment it first made; with a different payment, ErrIdempotencyKeyReused.
func (s *DualTokenStore) PayOutside(ctx context.Context, workspaceID string, in OutsidePayment) (OutsidePayment, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Reference = strings.Join(strings.Fields(in.Reference), " ")
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	bad := func(msg string) (OutsidePayment, error) {
		return OutsidePayment{}, fmt.Errorf("%w: %s", ErrOutsidePayment, msg)
	}
	switch {
	case in.PayeeID == "":
		return bad("a payment names its payee (payee_id): save one at /v1/money/payees first")
	case in.Currency != CurrencyGBP && in.Currency != CurrencyEUR && in.Currency != CurrencyUSD:
		return bad(fmt.Sprintf("a payment is in GBP, EUR or USD, not %q", in.Currency))
	case in.AmountMinor <= 0:
		return bad("a payment's amount_minor is a positive number of pence or cents")
	case in.Funding != FundingTest && in.Funding != FundingLive:
		return bad(fmt.Sprintf("funding is test or live, not %q", in.Funding))
	case in.IdempotencyKey == "" || len(in.IdempotencyKey) > 200:
		return bad("a payment carries an idempotency key (the Idempotency-Key header, or idempotency_key), up to 200 characters")
	case len(in.Reference) > 140:
		return bad("a payment's reference is up to 140 characters")
	}
	prior, err := scanOutsidePayment(s.pool.QueryRow(ctx, `SELECT `+outsidePaymentCols+` FROM money_payments
		WHERE workspace_id = $1 AND idempotency_key = $2`, workspaceID, in.IdempotencyKey))
	switch {
	case err == nil && prior.same(in):
		return prior, nil
	case err == nil:
		return OutsidePayment{}, ErrIdempotencyKeyReused
	case !errors.Is(err, ErrOutsidePaymentNotFound):
		return OutsidePayment{}, fmt.Errorf("economy: payment: %w", err)
	}

	from, err := s.payingAccount(ctx, workspaceID, in)
	if err != nil {
		return OutsidePayment{}, err
	}
	payee, err := s.PayableOutsidePayee(ctx, workspaceID, in.PayeeID)
	if err != nil {
		return OutsidePayment{}, err
	}
	switch {
	case s.accountPartners == nil:
		return OutsidePayment{}, fmt.Errorf("%w: no account partner is set", ErrAccountNotOpened)
	case s.screener == nil:
		return OutsidePayment{}, fmt.Errorf("%w: no screener is set, so no money moves out", partners.ErrScreeningUnavailable)
	}
	partner, err := s.accountPartners.Account(ctx, CapabilityPaymentsOut)
	if err != nil {
		return OutsidePayment{}, fmt.Errorf("economy: payment: %w", err)
	}
	p := in
	p.ID, p.WorkspaceID, p.AccountID, p.AgentID, p.PayeeName, p.Status, p.Partner =
		"mpay_"+uuid.NewString(), workspaceID, from.id, from.agentID, payee.Name, PaymentPending, partner.Name()
	if p.AgentID != "" {
		if p.ValueULXC, err = s.paymentValue(ctx, p); err != nil {
			return OutsidePayment{}, err
		}
		fp := sha256.Sum256([]byte("payment_out\x00" + p.AgentID + "\x00" + p.AccountID + "\x00" + p.PayeeID + "\x00" +
			strconv.FormatInt(p.AmountMinor, 10) + "\x00" + p.Currency + "\x00" + p.Funding + "\x00" + p.Reference))
		ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, At: in.at, Fingerprint: hex.EncodeToString(fp[:]),
			Payee: Payee{Kind: "outside", ID: payee.ID, Name: payee.Name},
			Memo:  strings.TrimSuffix(minorString(p.AmountMinor, p.Currency)+" to "+payee.Name+" ("+p.Funding+" money) — "+p.Reference, " — ")})
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OutsidePayment{}, fmt.Errorf("economy: payment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if p.AgentID != "" {
		if err := lockAgent(ctx, tx, workspaceID, p.AgentID); err != nil {
			return OutsidePayment{}, err
		}
	}
	// The company's account and the paying one, in id order: every payment out in this currency waits its turn for the
	// hold, and a payment in waits for the balance read here.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []string{from.companyID, from.id}); err != nil {
		return OutsidePayment{}, fmt.Errorf("economy: payment: %w", err)
	}
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		from.id, p.Funding).Scan(&held); err != nil {
		return OutsidePayment{}, fmt.Errorf("economy: payment: %w", err)
	}
	if held < p.AmountMinor {
		return OutsidePayment{}, fmt.Errorf("%w: the account holds %s of %s money, and this payment is %s", ErrAgentFunds,
			minorString(held, p.Currency), p.Funding, minorString(p.AmountMinor, p.Currency))
	}
	if p.AgentID != "" {
		if err := enforceAgentRules(ctx, tx, workspaceID, p.AgentID, p.ValueULXC, p.ID); err != nil {
			return OutsidePayment{}, s.refusedMovement(ctx, tx, err)
		}
	}
	if err := s.screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: workspaceID, ID: p.screenKey(), Capability: CapabilityPaymentsOut,
		Direction: "out", Counterparty: payee.Name, AmountMinor: p.AmountMinor, Currency: p.Currency, Funding: p.Funding}); err != nil {
		return OutsidePayment{}, err
	}
	if p.holdAccountID, err = holdAccount(ctx, tx, workspaceID, p.Currency); err != nil {
		return OutsidePayment{}, err
	}
	if p, err = scanOutsidePayment(tx.QueryRow(ctx, `INSERT INTO money_payments (id, workspace_id, idempotency_key, account_id, hold_account_id,
		agent_id, payee_id, payee_name, amount_minor, currency, funding, reference, value_ulxc, status, partner)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING `+outsidePaymentCols,
		p.ID, workspaceID, p.IdempotencyKey, p.AccountID, p.holdAccountID, p.AgentID, p.PayeeID, p.PayeeName, p.AmountMinor, p.Currency,
		p.Funding, p.Reference, p.ValueULXC, p.Status, p.Partner)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // the same key, paid at the same moment: answer as that one did
			_ = tx.Rollback(ctx)
			return s.PayOutside(ctx, workspaceID, in)
		}
		return OutsidePayment{}, fmt.Errorf("economy: payment: %w", err)
	}
	if _, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityPaymentsOut, Kind: "payment_out_hold",
		IdempotencyKey: p.key("hold"), Funding: p.Funding, Memo: p.Reference, Postings: []MoneyPosting{
			{AccountID: p.AccountID, AmountMinor: -p.AmountMinor}, {AccountID: p.holdAccountID, AmountMinor: p.AmountMinor}}}, s.screener); err != nil {
		return OutsidePayment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OutsidePayment{}, fmt.Errorf("economy: payment: %w", err)
	}

	res, err := partner.SendPayment(partners.WithWorkspace(ctx, workspaceID), partners.PaymentRequest{ID: p.ID, AccountRef: from.partnerRef,
		Amount: partners.Money{Minor: p.AmountMinor, Currency: p.Currency}, Reference: p.Reference, Payee: partners.Payee{Name: payee.Name,
			SortCode: payee.SortCode, AccountNumber: payee.AccountNumber, IBAN: payee.IBAN, BIC: payee.BIC, RoutingNumber: payee.RoutingNumber}})
	switch {
	case errors.Is(err, partners.ErrInvalid), errors.Is(err, partners.ErrNotFound), errors.Is(err, partners.ErrIDReused):
		res = partners.Result{Status: partners.StatusFailed, Detail: err.Error()}
	case err != nil: // the partner did not answer: the money stays held, and the payment is sent again when it is read
		return p, nil
	}
	return s.applyPartnerResult(ctx, p, from.partnerID, res)
}

// payingAccount finds the account in pays from, which must be open in in.Currency: in.AccountID, which is in.AgentID's
// when that is set; else in.AgentID's account; else the company's.
func (s *DualTokenStore) payingAccount(ctx context.Context, workspaceID string, in OutsidePayment) (payingAccount, error) {
	var a payingAccount
	var purpose, currency, status string
	err := s.pool.QueryRow(ctx, `SELECT a.id, COALESCE(a.agent_id, ''), a.purpose, a.currency, a.status, c.id, c.partner_account_ref,
			COALESCE((SELECT p.id FROM money_accounts p WHERE p.workspace_id = c.workspace_id AND p.purpose = 'partner'
				AND p.partner_account_ref = c.partner_account_ref AND c.partner_account_ref <> '' LIMIT 1), '')
		FROM money_accounts a JOIN money_accounts c ON c.id = COALESCE(a.parent_account_id, a.id)
		WHERE a.workspace_id = $1 AND a.purpose IN ('company', 'agent') AND a.currency = $2
		  AND (($3 <> '' AND a.id = $3) OR ($3 = '' AND $4 <> '' AND a.agent_id = $4 AND a.status <> 'closed')
		    OR ($3 = '' AND $4 = '' AND a.purpose = 'company' AND a.status <> 'closed'))
		ORDER BY a.created_at DESC LIMIT 1`, workspaceID, in.Currency, in.AccountID, in.AgentID).
		Scan(&a.id, &a.agentID, &purpose, &currency, &status, &a.companyID, &a.partnerRef, &a.partnerID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return a, fmt.Errorf("%w: no %s account to pay from — open one at /v1/money/accounts", ErrMoneyAccountNotFound, in.Currency)
	case err != nil:
		return a, fmt.Errorf("economy: payment: %w", err)
	case in.AgentID != "" && a.agentID != in.AgentID:
		return a, fmt.Errorf("%w: an agent pays only from its own account", ErrMoneyAccountNotFound)
	case status != MoneyOpen:
		return a, fmt.Errorf("%w: %s is %s", ErrMoneyAccountNotOpen, a.id, status)
	case a.partnerRef == "" || a.partnerID == "":
		return a, fmt.Errorf("%w: the company's %s account is not held at a partner", ErrMoneyAccountNotOpen, currency)
	}
	return a, nil
}

// paymentValue is what p is worth in µLXC, as the agent's rules count it: dollars at the peg, pounds and euros at the
// conversion partner's rate for dollars.
func (s *DualTokenStore) paymentValue(ctx context.Context, p OutsidePayment) (int64, error) {
	cents := p.AmountMinor
	if p.Currency != CurrencyUSD {
		conv, err := s.accountPartners.FX(ctx, CapabilityPaymentsOut)
		if err != nil {
			return 0, fmt.Errorf("economy: value the payment: %w", err)
		}
		q, err := conv.Quote(partners.WithWorkspace(ctx, p.WorkspaceID), partners.FXQuoteRequest{
			ID: "value:" + p.screenKey() + ":" + strconv.FormatInt(p.AmountMinor, 10) + p.Currency, Sell: partners.Money{Minor: p.AmountMinor, Currency: p.Currency},
			Buy: CurrencyUSD})
		if err != nil {
			return 0, fmt.Errorf("economy: value the payment: %w", err)
		}
		cents = q.Buy.Minor
	}
	if cents > math.MaxInt64/(10_000*ULXCPerUSDMicro) {
		return 0, fmt.Errorf("%w: %s is more than one payment can be", ErrOutsidePayment, minorString(p.AmountMinor, p.Currency))
	}
	return cents * 10_000 * ULXCPerUSDMicro, nil
}

// holdAccount is workspaceID's account holding payments out in currency until the partner has them, opened the first
// time. The caller's transaction holds the company's account in currency, so two payments open one.
func holdAccount(ctx context.Context, tx pgx.Tx, workspaceID, currency string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND purpose = 'hold'
		AND agent_id IS NULL AND status = 'open' ORDER BY created_at LIMIT 1`, workspaceID, currency).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		id = "macc_" + uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO money_accounts (id, workspace_id, currency, purpose, name) VALUES ($1, $2, $3, 'hold', 'Payments out')`,
			id, workspaceID, currency)
	}
	if err != nil {
		return "", fmt.Errorf("economy: payments-out hold: %w", err)
	}
	return id, nil
}

// applyPartnerResult moves p to where the partner says it stands, with the money entries that take it there: sent
// moves nothing; completed moves the held money out; failed gives it back; returned moves it out and back. A state
// already reached moves nothing again.
func (s *DualTokenStore) applyPartnerResult(ctx context.Context, p OutsidePayment, partnerAccountID string, res partners.Result) (OutsidePayment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, fmt.Errorf("economy: payment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if p, err = scanOutsidePayment(tx.QueryRow(ctx, `SELECT `+outsidePaymentCols+` FROM money_payments WHERE id = $1 FOR UPDATE`, p.ID)); err != nil {
		return p, err
	}
	if res.Ref != "" {
		p.PartnerRef = res.Ref
	}
	var moved []MoneyEntry // what moved money through the partner, monitored once committed
	post := func(kind, key string, outside bool, from, to string) error {
		e := MoneyEntry{WorkspaceID: p.WorkspaceID, Capability: CapabilityPaymentsOut, Kind: kind, IdempotencyKey: key, Funding: p.Funding,
			Memo: p.Reference, Postings: []MoneyPosting{{AccountID: from, AmountMinor: -p.AmountMinor}, {AccountID: to, AmountMinor: p.AmountMinor}}}
		if outside {
			e.Counterparty, e.PartnerRef = p.PayeeName, p.PartnerRef
		}
		e, err := postMoneyTx(ctx, tx, e, s.screener)
		if outside && err == nil {
			moved = append(moved, e)
		}
		return err
	}
	status := p.Status
	switch res.Status {
	case partners.StatusPending:
		if status == PaymentPending {
			status = PaymentSent
		}
	case partners.StatusFailed:
		if status == PaymentPending || status == PaymentSent {
			if err := post("payment_out_failed", p.key("failed"), false, p.holdAccountID, p.AccountID); err != nil {
				return p, err
			}
			status = PaymentFailed
		}
	case partners.StatusCompleted, partners.StatusReturned:
		if status == PaymentPending || status == PaymentSent {
			if err := post("payment_out", p.screenKey(), true, p.holdAccountID, partnerAccountID); err != nil {
				return p, err
			}
			status = PaymentCompleted
		}
		if res.Status == partners.StatusReturned && status == PaymentCompleted {
			if err := post("payment_out_returned", p.key("returned"), true, partnerAccountID, p.AccountID); err != nil {
				return p, err
			}
			status = PaymentReturned
		}
	default:
		return p, fmt.Errorf("economy: the partner answered %q for payment %s", res.Status, p.ID)
	}
	if res.Detail != "" {
		p.Detail = res.Detail
	}
	if p, err = scanOutsidePayment(tx.QueryRow(ctx, `UPDATE money_payments SET status = $2, partner_ref = $3, detail = $4, updated_at = now()
		WHERE id = $1 RETURNING `+outsidePaymentCols, p.ID, status, p.PartnerRef, p.Detail)); err != nil {
		return p, fmt.Errorf("economy: payment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return p, fmt.Errorf("economy: payment: %w", err)
	}
	for _, e := range moved {
		s.monitorMoved(ctx, e)
	}
	return p, nil
}

// RefreshOutsidePayment reads workspaceID's payment id — with agentID set, only that agent's — and first asks the
// partner where it stands unless it has failed or come back: a payment the partner never answered is sent again.
func (s *DualTokenStore) RefreshOutsidePayment(ctx context.Context, workspaceID, agentID, id string) (OutsidePayment, error) {
	p, err := scanOutsidePayment(s.pool.QueryRow(ctx, `SELECT `+outsidePaymentCols+` FROM money_payments
		WHERE id = $1 AND workspace_id = $2 AND ($3 = '' OR agent_id = $3)`, id, workspaceID, agentID))
	if err != nil || p.Status == PaymentFailed || p.Status == PaymentReturned || s.accountPartners == nil {
		return p, err
	}
	partner, err := s.accountPartners.Account(ctx, CapabilityPaymentsOut)
	if err != nil {
		return p, fmt.Errorf("economy: payment: %w", err)
	}
	from, err := s.payingAccount(ctx, workspaceID, OutsidePayment{AccountID: p.AccountID, AgentID: p.AgentID, Currency: p.Currency})
	if err != nil {
		return p, err
	}
	var res partners.Result
	pctx := partners.WithWorkspace(ctx, workspaceID)
	if p.PartnerRef == "" {
		payee, err := s.outsidePayee(ctx, s.pool, workspaceID, p.PayeeID, "")
		if err != nil {
			return p, err
		}
		res, err = partner.SendPayment(pctx, partners.PaymentRequest{ID: p.ID, AccountRef: from.partnerRef,
			Amount: partners.Money{Minor: p.AmountMinor, Currency: p.Currency}, Reference: p.Reference, Payee: partners.Payee{Name: payee.Name,
				SortCode: payee.SortCode, AccountNumber: payee.AccountNumber, IBAN: payee.IBAN, BIC: payee.BIC, RoutingNumber: payee.RoutingNumber}})
		if errors.Is(err, partners.ErrInvalid) || errors.Is(err, partners.ErrNotFound) || errors.Is(err, partners.ErrIDReused) {
			res, err = partners.Result{Status: partners.StatusFailed, Detail: err.Error()}, nil
		}
	} else {
		res, err = partner.PaymentStatus(pctx, p.PartnerRef)
	}
	if err != nil { // shortcut: a partner that forgot the payment (the Test partner after a restart) leaves it as it was; upgrade with a real partner's webhooks
		return p, nil
	}
	return s.applyPartnerResult(ctx, p, from.partnerID, res)
}

// OutsidePayments is workspaceID's payments out — with agentID set, only that agent's — newest first, at most 200.
func (s *DualTokenStore) OutsidePayments(ctx context.Context, workspaceID, agentID string) ([]OutsidePayment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+outsidePaymentCols+` FROM money_payments WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2)
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: payments: %w", err)
	}
	defer rows.Close()
	out := []OutsidePayment{}
	for rows.Next() {
		p, err := scanOutsidePayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
