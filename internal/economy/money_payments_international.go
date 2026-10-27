package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/partners"
)

// money_payments_international.go — B30.22: PAYMENTS TO OTHER COUNTRIES, WITH THE FEE AND THE RATE SHOWN FIRST.
//
// An international payment pays a saved payee whose account is in another currency than the account it is paid from,
// through the account partner's international route: the partner converts and pays what arrives. Before anything is
// sent, a quote (QuoteInternational) shows the fee — the partner's cost plus Talyvor's fee for the sending currency,
// LENS_INTL_PAYMENT_FEE_MINOR (internal/fees); a currency with no fee set is refused, never sent free — the rate, and
// what arrives. A payment is made at a quote (PayOutside with QuoteID), within the quote's life. Live money goes only
// to a country the capability's clearance lists; the payer is screened as well as the payee.
//
// It holds, sends, completes, fails and returns as a payment out does (money_payments.go), its hold the amount plus
// the fee. On completion one entry posts both currencies: in the sending currency the amount and the partner's cost go
// hold → partner and Talyvor's fee hold → revenue; in the payee's, what arrives is posted through two partner accounts
// of the route — in from the conversion, out to the payee — so each currency sums to zero.

// ErrQuote: an international payment names a quote that is not this workspace's, is for another payment, or has
// expired.
var ErrQuote = errors.New("economy: no such quote, or it has expired or is for another payment")

// InternationalQuote is what an international payment would cost and deliver, good until ExpiresAt.
type InternationalQuote struct {
	ID               string    `json:"id"`
	WorkspaceID      string    `json:"workspace_id"`
	AccountID        string    `json:"account_id"`
	AgentID          string    `json:"agent_id,omitempty"`
	PayeeID          string    `json:"payee_id"`
	PayeeName        string    `json:"payee_name"`
	Country          string    `json:"destination_country"`
	AmountMinor      int64     `json:"amount_minor"` // what is sent, before the fee
	Currency         string    `json:"currency"`
	PartnerCostMinor int64     `json:"partner_cost_minor"` // in Currency
	FeeMinor         int64     `json:"fee_minor"`          // Talyvor's, in Currency
	TotalMinor       int64     `json:"total_minor"`        // what leaves the account: the amount, the partner's cost and the fee
	ArrivesMinor     int64     `json:"arrives_minor"`
	ArrivesCurrency  string    `json:"arrives_currency"`
	Rate             string    `json:"rate"` // ArrivesCurrency per Currency, in whole units
	Funding          string    `json:"funding"`
	Partner          string    `json:"partner"`
	PartnerRef       string    `json:"partner_ref"`
	ExpiresAt        time.Time `json:"expires_at"`
	CreatedAt        time.Time `json:"created_at"`
}

const quoteCols = `id, workspace_id, account_id, agent_id, payee_id, payee_name, destination_country, amount_minor, currency, partner_cost_minor,
	fee_minor, arrives_minor, arrives_currency, rate, funding, partner, partner_ref, expires_at, created_at`

func scanQuote(row pgx.Row) (InternationalQuote, error) {
	var q InternationalQuote
	err := row.Scan(&q.ID, &q.WorkspaceID, &q.AccountID, &q.AgentID, &q.PayeeID, &q.PayeeName, &q.Country, &q.AmountMinor, &q.Currency,
		&q.PartnerCostMinor, &q.FeeMinor, &q.ArrivesMinor, &q.ArrivesCurrency, &q.Rate, &q.Funding, &q.Partner, &q.PartnerRef, &q.ExpiresAt, &q.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return q, ErrQuote
	}
	q.TotalMinor = q.AmountMinor + q.PartnerCostMinor + q.FeeMinor
	return q, err
}

// payeeCurrency is the currency a payee's account details are in.
// shortcut: an IBAN is taken as a euro account, as the partner's own account details are (AccountDetails); upgrade
// when a payee carries its currency.
func payeeCurrency(p OutsidePayee) string {
	switch {
	case p.Address != "":
		return CurrencyUSDC
	case p.IBAN != "":
		return CurrencyEUR
	case p.RoutingNumber != "":
		return CurrencyUSD
	}
	return CurrencyGBP
}

// QuoteInternational prices paying in.AmountMinor of in.Currency to workspaceID's saved payee in.PayeeID, whose account
// is in another currency, from the account in pays from (payingAccount): the partner's cost and Talyvor's fee, the
// rate and what arrives. The quote is kept, and a payment is made at it with PayOutside.
func (s *DualTokenStore) QuoteInternational(ctx context.Context, workspaceID string, in OutsidePayment) (InternationalQuote, error) {
	if err := in.normalise(false); err != nil {
		return InternationalQuote{}, err
	}
	from, err := s.payingAccount(ctx, workspaceID, in)
	if err != nil {
		return InternationalQuote{}, err
	}
	payee, err := s.PayableOutsidePayee(ctx, workspaceID, in.PayeeID)
	if err != nil {
		return InternationalQuote{}, err
	}
	deliver := payeeCurrency(payee)
	if deliver == in.Currency {
		return InternationalQuote{}, fmt.Errorf("%w: %s's account is in %s too, so this is not an international payment: POST /v1/money/payments pays it",
			ErrOutsidePayment, payee.Name, deliver)
	}
	fee, ok := fees.Current().IntlPaymentFeeMinor[in.Currency]
	if !ok {
		return InternationalQuote{}, fmt.Errorf("%w: no international payment fee is set for %s (LENS_INTL_PAYMENT_FEE_MINOR), and one is never sent free",
			ErrOutsidePayment, in.Currency)
	}
	if err := s.destinationAllowed(ctx, in.Funding, payee); err != nil {
		return InternationalQuote{}, err
	}
	if s.accountPartners == nil {
		return InternationalQuote{}, fmt.Errorf("%w: no account partner is set", ErrAccountNotOpened)
	}
	partner, err := s.accountPartners.Account(ctx, CapabilityPaymentsOut)
	if err != nil {
		return InternationalQuote{}, fmt.Errorf("economy: quote: %w", err)
	}
	q := InternationalQuote{ID: "mpq_" + uuid.NewString(), WorkspaceID: workspaceID, AccountID: from.id, AgentID: from.agentID, PayeeID: payee.ID,
		PayeeName: payee.Name, Country: payee.Country, AmountMinor: in.AmountMinor, Currency: in.Currency, FeeMinor: fee, Funding: in.Funding,
		Partner: partner.Name()}
	pq, err := partner.QuoteInternational(partners.WithWorkspace(ctx, workspaceID), partners.InternationalQuoteRequest{ID: q.ID, AccountRef: from.partnerRef,
		Amount: partners.Money{Minor: q.AmountMinor, Currency: q.Currency}, Deliver: deliver, Country: payee.Country, Payee: partners.Payee{Name: payee.Name,
			SortCode: payee.SortCode, AccountNumber: payee.AccountNumber, IBAN: payee.IBAN, BIC: payee.BIC, RoutingNumber: payee.RoutingNumber}})
	switch {
	case errors.Is(err, partners.ErrInvalid):
		return InternationalQuote{}, fmt.Errorf("%w: %v", ErrOutsidePayment, err)
	case err != nil:
		return InternationalQuote{}, fmt.Errorf("economy: quote: %w", err)
	case pq.Cost.Currency != q.Currency || pq.Delivers.Currency != deliver || pq.Cost.Minor < 0 || pq.Delivers.Minor <= 0:
		return InternationalQuote{}, fmt.Errorf("economy: quote: the partner priced %s in %s delivering %s", q.Currency, pq.Cost.Currency, pq.Delivers)
	}
	q.PartnerCostMinor, q.ArrivesMinor, q.ArrivesCurrency, q.Rate, q.PartnerRef, q.ExpiresAt = pq.Cost.Minor, pq.Delivers.Minor, deliver, pq.Rate, pq.Ref, pq.ExpiresAt
	q, err = scanQuote(s.pool.QueryRow(ctx, `INSERT INTO money_payment_quotes (id, workspace_id, account_id, agent_id, payee_id, payee_name,
		destination_country, amount_minor, currency, partner_cost_minor, fee_minor, arrives_minor, arrives_currency, rate, funding, partner, partner_ref, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18) RETURNING `+quoteCols,
		q.ID, q.WorkspaceID, q.AccountID, q.AgentID, q.PayeeID, q.PayeeName, q.Country, q.AmountMinor, q.Currency, q.PartnerCostMinor, q.FeeMinor,
		q.ArrivesMinor, q.ArrivesCurrency, q.Rate, q.Funding, q.Partner, q.PartnerRef, q.ExpiresAt))
	if err != nil {
		return InternationalQuote{}, fmt.Errorf("economy: quote: %w", err)
	}
	return q, nil
}

// destinationAllowed refuses live money to a payee in a country the payments_out clearance does not list. Test money
// reaches any country.
func (s *DualTokenStore) destinationAllowed(ctx context.Context, funding string, payee OutsidePayee) error {
	if funding != FundingLive {
		return nil
	}
	countries, err := clearanceCountries(ctx, s.pool, CapabilityPaymentsOut)
	if err != nil {
		return err
	}
	if !slices.Contains(countries, payee.Country) {
		return fmt.Errorf("%w: live payments out reach %s, not a payee in %s", ErrCapabilityNotCleared, strings.Join(countries, ", "), payee.Country)
	}
	return nil
}

// usableQuote is workspaceID's quote in.QuoteID when in is the payment it priced — the same account, payee, amount,
// currency and funding — and it has not expired.
func (s *DualTokenStore) usableQuote(ctx context.Context, workspaceID string, in OutsidePayment, from payingAccount, payee OutsidePayee) (InternationalQuote, error) {
	if err := s.destinationAllowed(ctx, in.Funding, payee); err != nil {
		return InternationalQuote{}, err
	}
	q, err := scanQuote(s.pool.QueryRow(ctx, `SELECT `+quoteCols+` FROM money_payment_quotes WHERE id = $1 AND workspace_id = $2`, in.QuoteID, workspaceID))
	switch {
	case err != nil:
		return q, err
	case q.AccountID != from.id || q.PayeeID != in.PayeeID || q.AmountMinor != in.AmountMinor || q.Currency != in.Currency || q.Funding != in.Funding:
		return q, fmt.Errorf("%w: quote %s priced %s to %s from %s", ErrQuote, q.ID, minorString(q.AmountMinor, q.Currency), q.PayeeName, q.AccountID)
	case !q.ExpiresAt.After(time.Now()):
		return q, fmt.Errorf("%w: quote %s expired at %s — ask for a new one", ErrQuote, q.ID, q.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return q, nil
}

// payerName is who pays from workspaceID's accounts, as the partner holds them: the workspace's name.
func (s *DualTokenStore) payerName(ctx context.Context, workspaceID string) (string, error) {
	var name string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), '')`, workspaceID).Scan(&name); err != nil {
		return "", fmt.Errorf("economy: payment: %w", err)
	}
	if name == "" {
		name = workspaceID
	}
	return name, nil
}

// routeAccount is workspaceID's open account for purpose in currency under ref (a partner account's reference, or ""
// for Talyvor's revenue), opened the first time — under an advisory lock, so two payments open one.
func routeAccount(ctx context.Context, tx pgx.Tx, workspaceID, currency, purpose, ref, name string) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('money_route:' || $1 || ':' || $2 || ':' || $3 || ':' || $4, 0))`,
		workspaceID, currency, purpose, ref); err != nil {
		return "", fmt.Errorf("economy: route account: %w", err)
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND purpose = $3 AND partner_account_ref = $4
		AND agent_id IS NULL AND status = 'open' ORDER BY created_at LIMIT 1`, workspaceID, currency, purpose, ref).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		id = "macc_" + uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO money_accounts (id, workspace_id, currency, purpose, name, partner_account_ref) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, workspaceID, currency, purpose, name, ref)
	}
	if err != nil {
		return "", fmt.Errorf("economy: route account: %w", err)
	}
	return id, nil
}

// movements are the postings that take p out (hold → the partner, and Talyvor's fee → revenue) and, for a returned
// one, back (the partner → the account; the fee stays). An international payment posts the payee's currency too: what
// arrives comes in from the route's conversion account and goes out through its payments account, each a partner
// account the reconciliation (B30.11) finds no statement for.
func (s *DualTokenStore) movements(ctx context.Context, tx pgx.Tx, p OutsidePayment, partnerAccountID string) (out, back []MoneyPosting, err error) {
	toPartner := p.AmountMinor + p.PartnerCostMinor
	out = []MoneyPosting{{AccountID: p.holdAccountID, AmountMinor: -p.debit()}, {AccountID: partnerAccountID, AmountMinor: toPartner}}
	back = []MoneyPosting{{AccountID: partnerAccountID, AmountMinor: -toPartner}, {AccountID: p.AccountID, AmountMinor: toPartner}}
	if !p.international() {
		return out, back, nil
	}
	revenue, err := routeAccount(ctx, tx, p.WorkspaceID, p.Currency, MoneyRevenue, "", "Talyvor fees")
	if err != nil {
		return nil, nil, err
	}
	conversion, err := routeAccount(ctx, tx, p.WorkspaceID, p.ArrivesCurrency, MoneyPartner, "intl:conversion", "International payments: converted")
	if err != nil {
		return nil, nil, err
	}
	paid, err := routeAccount(ctx, tx, p.WorkspaceID, p.ArrivesCurrency, MoneyPartner, "intl:payments", "International payments: paid")
	if err != nil {
		return nil, nil, err
	}
	out = append(out, MoneyPosting{AccountID: revenue, AmountMinor: p.FeeMinor},
		MoneyPosting{AccountID: conversion, AmountMinor: -p.ArrivesMinor, Currency: p.ArrivesCurrency},
		MoneyPosting{AccountID: paid, AmountMinor: p.ArrivesMinor, Currency: p.ArrivesCurrency})
	back = append(back, MoneyPosting{AccountID: paid, AmountMinor: -p.ArrivesMinor, Currency: p.ArrivesCurrency},
		MoneyPosting{AccountID: conversion, AmountMinor: p.ArrivesMinor, Currency: p.ArrivesCurrency})
	return out, back, nil
}
