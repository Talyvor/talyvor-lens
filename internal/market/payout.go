package market

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/billing"
)

// payout.go — B20.5: SELLERS ARE PAID IN MONEY, THROUGH STRIPE CONNECT.
//
//   - A seller connects a Stripe Express account through Stripe's onboarding link; Stripe collects their
//     identity, bank and tax details, and Lens records what Stripe says of the account (market_sellers).
//   - A seller's AVAILABLE balance is every share they earned whose 14-day holdback has passed and that no
//     refund reversed, less every payout. Once a week (B32.42), a connected seller whose available balance has
//     reached US$25 is paid it: one market_payouts row, then one Stripe transfer of it less Stripe's fees
//     at cost — $2 on the seller's first payout of a calendar month, and 0.25% + $0.25 on every payout — every
//     fee shown. A seller under US$25 is carried to the next week. Each payout has a statement of its week
//     (statement.go).
//   - A seller may instead take their available balance as Talyvor credits, 1:1, at any time: one
//     market_payouts row and one lxc_ledger row, committed together.
//   - A buyer's refund or chargeback reverses the earnings of the uses it paid for (a market_refunds row
//     each). Inside the holdback, the earning never becomes available; after it, the balance falls by it,
//     and a balance that falls below zero is owed and recovered from the seller's future earnings.
//   - An earning an open hold names (a dispute, an IP claim: escrow.go) stays in the holdback past its 14 days.
//   - Every payout is journalled with its row (B32.17): a Stripe payout moves it from the seller's available to
//     stripe:clearing (the net) and stripe:connect_fees (Stripe's fees); credits move it to credits:issued.
//   - A seller whose tax details are still incomplete after the last request for them (B32.41, internal/sellertax)
//     is withheld: the payout run skips them and they cannot take credits, while their earnings keep clearing, until
//     the details are complete.
//   - To a seller who agreed to self-billing, each payout in money is also a self-billed invoice from them to Talyvor
//     for the earnings it pays, and the VAT they charge on it is paid on top (B32.43, selfbill.go).

const (
	// PayoutMinimumUSDMicros: a seller is paid in money once their available balance reaches US$25.
	PayoutMinimumUSDMicros = 25_000_000
	// Stripe's Connect fees, deducted at cost: $2 per account paid in a month — on its first payout of the month —
	// and 0.25% + $0.25 per payout.
	stripeAccountFeeCents   = 200
	stripePayoutFixedCents  = 25
	stripePayoutBasisPoints = 25

	PayoutStripe  = "stripe"
	PayoutCredits = "credits"

	usdMicrosPerCent = 10_000
)

// ErrNotConnected: the seller has no connected Stripe account yet.
var ErrNotConnected = errors.New("market: connect a Stripe account to be paid")

// ErrNothingAvailable: no earnings are past their holdback and unpaid.
var ErrNothingAvailable = errors.New("market: no earnings are available yet — they become available 14 days after the buyer's payment clears")

// ErrTaxHold: the seller's payouts are held until their tax details are complete (B32.41).
var ErrTaxHold = errors.New("market: your payouts are on hold until your tax details are complete")

// taxHeldSQL is true of a seller s withheld until their tax details are complete (B32.41).
const taxHeldSQL = `EXISTS (SELECT 1 FROM seller_tax_profiles t WHERE t.workspace_id = s.workspace_id AND t.withheld_since IS NOT NULL)`

// ErrNoContactEmail: the person connecting has no email address, and Stripe opens a seller's account only
// with one to reach them at (B35.4).
var ErrNoContactEmail = invalid("Add an email address to your sign-in to connect with Stripe — Stripe needs one to reach you about your payouts.")

// ConnectStripe is Stripe Connect as the payouts use it. *billing.LiveStripe satisfies it.
type ConnectStripe interface {
	CreateConnectedAccount(ctx context.Context, workspaceID, country, contactEmail string) (billing.ConnectAccount, error)
	OnboardingLink(ctx context.Context, accountID, refreshURL, returnURL string) (string, error)
	ConnectedAccount(ctx context.Context, accountID string) (billing.ConnectAccount, error)
	TransferToSeller(ctx context.Context, accountID string, cents int64, payoutID, workspaceID string) (transferID string, err error)
}

// Crediter writes LXC credits on the caller's transaction. *economy.DualTokenStore satisfies it.
type Crediter interface {
	CreditLXCTx(ctx context.Context, tx pgx.Tx, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error)
}

// PayoutFees splits a payout of grossCents into Stripe's fees and what the seller receives. The account fee,
// $2, is taken on the seller's first payout of a calendar month (firstOfMonth) only. The payout fee is 0.25% of
// what is paid out plus $0.25, so net is solved for: net + 0.25%·net = gross − account fee − $0.25, rounded
// down to the cent (the fee carries the fraction).
func PayoutFees(grossCents int64, firstOfMonth bool) (accountFeeCents, payoutFeeCents, netCents int64) {
	if firstOfMonth {
		accountFeeCents = stripeAccountFeeCents
	}
	netCents = (grossCents - accountFeeCents - stripePayoutFixedCents) * 10_000 / (10_000 + stripePayoutBasisPoints)
	if netCents < 0 {
		netCents = 0
	}
	return accountFeeCents, grossCents - accountFeeCents - netCents, netCents
}

// Payout is one market_payouts row.
type Payout struct {
	ID                  string     `json:"id"`
	Method              string     `json:"method"`
	Month               string     `json:"month"`
	Period              string     `json:"period"` // the ISO week it was made in, such as 2026-W41 (B32.42)
	GrossUSDMicros      int64      `json:"gross_usd_micros"`
	VATUSDMicros        int64      `json:"vat_usd_micros"` // the seller's VAT on their self-billed invoice, paid on top (B32.43)
	AccountFeeUSDMicros int64      `json:"account_fee_usd_micros"`
	PayoutFeeUSDMicros  int64      `json:"payout_fee_usd_micros"`
	NetUSDMicros        int64      `json:"net_usd_micros"`
	CreditsULXC         int64      `json:"credits_ulxc,omitempty"`
	StripeTransferID    string     `json:"stripe_transfer_id,omitempty"`
	PaidAt              *time.Time `json:"paid_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// PayoutQuote is what paying the available balance out now would come to, fees shown.
type PayoutQuote struct {
	GrossUSDMicros      int64 `json:"gross_usd_micros"`
	AccountFeeUSDMicros int64 `json:"account_fee_usd_micros"`
	PayoutFeeUSDMicros  int64 `json:"payout_fee_usd_micros"`
	NetUSDMicros        int64 `json:"net_usd_micros"`
}

// Payouts is a seller's payout page.
type Payouts struct {
	Account             *billing.ConnectAccount `json:"account"` // nil: not connected
	InHoldbackUSDMicros int64                   `json:"in_holdback_usd_micros"`
	AvailableUSDMicros  int64                   `json:"available_usd_micros"`
	OwedUSDMicros       int64                   `json:"owed_usd_micros"` // reversed after being paid: recovered from future earnings
	PaidOutUSDMicros    int64                   `json:"paid_out_usd_micros"`
	MinimumUSDMicros    int64                   `json:"minimum_usd_micros"`
	PaidThisMonth       bool                    `json:"paid_this_month"` // Stripe's account fee is already taken this month
	PaidThisWeek        bool                    `json:"paid_this_week"`  // the seller is next paid next week (B32.42)
	Quote               PayoutQuote             `json:"quote"`           // a money payout of the available balance
	Payouts             []Payout                `json:"payouts"`
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// heldSQL is true of an earning e whose use an open hold names: it stays in the holdback (B32.17).
const heldSQL = `EXISTS (SELECT 1 FROM market_holds h WHERE h.use_id = e.use_id AND h.released_at IS NULL)`

// sellerBalance reads a seller's earnings past the holdback and still inside it — or kept in it by a hold —
// (neither counting what a refund reversed), and everything paid out.
func sellerBalance(ctx context.Context, q queryRower, workspaceID string, now time.Time) (released, inHoldback, paid int64, err error) {
	err = q.QueryRow(ctx, `SELECT
		COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e
			WHERE e.seller_workspace_id = $1 AND e.payable_at <= $2 AND NOT `+heldSQL+` AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)), 0)::bigint,
		COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e
			WHERE e.seller_workspace_id = $1 AND (e.payable_at > $2 OR `+heldSQL+`) AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)), 0)::bigint,
		COALESCE((SELECT sum(gross_usd_micros) FROM market_payouts WHERE workspace_id = $1), 0)::bigint`,
		workspaceID, now).Scan(&released, &inHoldback, &paid)
	return released, inHoldback, paid, err
}

// sellerFunds splits what a seller has available by what paid for it (B22.1): earnings are test or live as
// the invoice that paid them was, and each Stripe payout as the key that made it was. Earnings taken as credits
// draw on test earnings first. Test earnings never reach a live payout: a live key pays at most liveLeft. A test
// workspace's earnings (marked test, B25.1) are test earnings whatever paid them.
func sellerFunds(ctx context.Context, q queryRower, workspaceID string, now time.Time) (testLeft, liveLeft int64, err error) {
	var testReleased, liveReleased, testPaid, livePaid, credits int64
	err = q.QueryRow(ctx, `SELECT
		COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e WHERE e.seller_workspace_id = $1 AND e.payable_at <= $2 AND NOT `+heldSQL+`
			AND (NOT e.livemode OR e.test) AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)), 0)::bigint,
		COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e WHERE e.seller_workspace_id = $1 AND e.payable_at <= $2 AND NOT `+heldSQL+`
			AND e.livemode AND NOT e.test AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)), 0)::bigint,
		COALESCE((SELECT sum(gross_usd_micros) FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe' AND NOT livemode), 0)::bigint,
		COALESCE((SELECT sum(gross_usd_micros) FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe' AND livemode), 0)::bigint,
		COALESCE((SELECT sum(gross_usd_micros) FROM market_payouts WHERE workspace_id = $1 AND method = 'credits'), 0)::bigint`,
		workspaceID, now).Scan(&testReleased, &liveReleased, &testPaid, &livePaid, &credits)
	if err != nil {
		return 0, 0, fmt.Errorf("market: seller funds: %w", err)
	}
	fromTest := min(credits, max(testReleased-testPaid, 0))
	return max(testReleased-testPaid-fromTest, 0), max(liveReleased-livePaid-(credits-fromTest), 0), nil
}

// liveKey reports whether api pays out with a live Stripe key.
func liveKey(api ConnectStripe) bool {
	l, ok := api.(interface{ Livemode() bool })
	return ok && l.Livemode()
}

func lockSeller(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('market_seller:' || $1, 0))`, workspaceID)
	return err
}

func monthOf(t time.Time) string { return t.UTC().Format("2006-01") }

// weekOf is t's ISO week, UTC, such as 2026-W41: the period of a payout made at t (B32.42).
func weekOf(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// ConnectSeller gives the seller a link to Stripe's onboarding for their connected account, creating the
// account the first time. country (ISO 3166-1 alpha-2, "" for the platform's) is fixed once it exists.
// email is the signed-in person's, which Stripe is given as the account's contact email (B35.4): a test
// (synthetic) workspace, whose owner has none, is given synthetic+<workspace id>@example.com, and anyone
// else without one is refused before Stripe is asked.
func (s *Store) ConnectSeller(ctx context.Context, api ConnectStripe, workspaceID, country, email, refreshURL, returnURL string) (string, billing.ConnectAccount, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country != "" && len(country) != 2 {
		return "", billing.ConnectAccount{}, invalid("country must be a two-letter code, such as GB or US")
	}
	a, err := s.sellerAccount(ctx, workspaceID)
	if errors.Is(err, ErrNotConnected) {
		contact, cerr := s.contactEmail(ctx, workspaceID, email)
		if cerr != nil {
			return "", a, cerr
		}
		created, cerr := api.CreateConnectedAccount(ctx, workspaceID, country, contact)
		if cerr != nil {
			return "", a, fmt.Errorf("market: create the seller's Stripe account: %w", cerr)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, country, details_submitted, payouts_enabled, currently_due, disabled_reason, checked_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now()) ON CONFLICT (workspace_id) DO NOTHING`,
			workspaceID, created.ID, created.Country, created.DetailsSubmitted, created.PayoutsEnabled, created.CurrentlyDue, created.DisabledReason); err != nil {
			return "", a, fmt.Errorf("market: record the seller's Stripe account: %w", err)
		}
		a, err = s.sellerAccount(ctx, workspaceID)
	}
	if err != nil {
		return "", a, err
	}
	url, err := api.OnboardingLink(ctx, a.ID, refreshURL, returnURL)
	if err != nil {
		return "", a, fmt.Errorf("market: Stripe onboarding link: %w", err)
	}
	return url, a, nil
}

// contactEmail is the address a new seller account gives Stripe: a test workspace's own test address, or
// the signed-in person's.
func (s *Store) contactEmail(ctx context.Context, workspaceID, email string) (string, error) {
	var test bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&test); err != nil {
		return "", fmt.Errorf("market: is the seller a test workspace: %w", err)
	}
	if test {
		return "synthetic+" + workspaceID + "@example.com", nil
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return "", ErrNoContactEmail
	}
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return "", invalid("email must be an address, such as you@company.com")
	}
	return email, nil
}

func (s *Store) sellerAccount(ctx context.Context, workspaceID string) (billing.ConnectAccount, error) {
	var a billing.ConnectAccount
	err := s.pool.QueryRow(ctx, `SELECT stripe_account_id, country, details_submitted, payouts_enabled, currently_due, disabled_reason
		FROM market_sellers WHERE workspace_id = $1`, workspaceID).
		Scan(&a.ID, &a.Country, &a.DetailsSubmitted, &a.PayoutsEnabled, &a.CurrentlyDue, &a.DisabledReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotConnected
	}
	if err != nil {
		return a, fmt.Errorf("market: seller account: %w", err)
	}
	return a, nil
}

// RecordSellerAccount records what Stripe says of a seller's connected account.
func (s *Store) RecordSellerAccount(ctx context.Context, a billing.ConnectAccount) error {
	if a.CurrentlyDue == nil {
		a.CurrentlyDue = []string{}
	}
	_, err := s.pool.Exec(ctx, `UPDATE market_sellers SET details_submitted = $2, payouts_enabled = $3, currently_due = $4,
		disabled_reason = $5, checked_at = now() WHERE stripe_account_id = $1`,
		a.ID, a.DetailsSubmitted, a.PayoutsEnabled, a.CurrentlyDue, a.DisabledReason)
	if err != nil {
		return fmt.Errorf("market: record seller account %s: %w", a.ID, err)
	}
	return nil
}

// SellerPayouts reads a seller's payout page. A connected account Stripe has not yet enabled for payouts
// is asked about first (api nil: what was last recorded).
func (s *Store) SellerPayouts(ctx context.Context, api ConnectStripe, workspaceID string, now time.Time) (Payouts, error) {
	p := Payouts{MinimumUSDMicros: PayoutMinimumUSDMicros, Payouts: []Payout{}}
	a, err := s.sellerAccount(ctx, workspaceID)
	switch {
	case errors.Is(err, ErrNotConnected):
	case err != nil:
		return p, err
	default:
		if !a.PayoutsEnabled && api != nil {
			if fresh, err := api.ConnectedAccount(ctx, a.ID); err == nil {
				if err := s.RecordSellerAccount(ctx, fresh); err != nil {
					return p, err
				}
				fresh.Country = a.Country
				a = fresh
			}
		}
		p.Account = &a
	}
	released, inHoldback, paid, err := sellerBalance(ctx, s.pool, workspaceID, now)
	if err != nil {
		return p, fmt.Errorf("market: seller balance: %w", err)
	}
	p.InHoldbackUSDMicros, p.PaidOutUSDMicros = inHoldback, paid
	p.AvailableUSDMicros, p.OwedUSDMicros = max(released-paid, 0), max(paid-released, 0)
	if err := s.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe' AND month = $2),
		EXISTS (SELECT 1 FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe' AND period = $3)`,
		workspaceID, monthOf(now), weekOf(now)).Scan(&p.PaidThisMonth, &p.PaidThisWeek); err != nil {
		return p, fmt.Errorf("market: payouts: %w", err)
	}
	gross := p.AvailableUSDMicros / usdMicrosPerCent
	if accountFee, payoutFee, net := PayoutFees(gross, !p.PaidThisMonth); net > 0 {
		p.Quote = PayoutQuote{gross * usdMicrosPerCent, accountFee * usdMicrosPerCent, payoutFee * usdMicrosPerCent, net * usdMicrosPerCent}
	}
	rows, err := s.pool.Query(ctx, `SELECT id, method, month, period, gross_usd_micros, vat_usd_micros, account_fee_usd_micros, payout_fee_usd_micros, net_usd_micros,
		       credits_ulxc, COALESCE(stripe_transfer_id, ''), paid_at, last_error, created_at
		FROM market_payouts WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 100`, workspaceID)
	if err != nil {
		return p, fmt.Errorf("market: payouts: %w", err)
	}
	p.Payouts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payout, error) {
		var x Payout
		return x, row.Scan(&x.ID, &x.Method, &x.Month, &x.Period, &x.GrossUSDMicros, &x.VATUSDMicros, &x.AccountFeeUSDMicros, &x.PayoutFeeUSDMicros, &x.NetUSDMicros,
			&x.CreditsULXC, &x.StripeTransferID, &x.PaidAt, &x.LastError, &x.CreatedAt)
	})
	if err != nil {
		return p, fmt.Errorf("market: payouts: %w", err)
	}
	return p, nil
}

// TakeAsCredits pays the seller's whole available balance as Talyvor credits, 1:1: one market_payouts row,
// one lxc_ledger row and its journal entry, in one transaction.
func (s *Store) TakeAsCredits(ctx context.Context, crediter Crediter, workspaceID string, now time.Time) (Payout, error) {
	var p Payout
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSeller(ctx, tx, workspaceID); err != nil {
			return err
		}
		var held bool
		if err := tx.QueryRow(ctx, `SELECT `+taxHeldSQL+` FROM (SELECT $1::text AS workspace_id) s`, workspaceID).Scan(&held); err != nil {
			return err
		}
		if held {
			return ErrTaxHold
		}
		released, _, paid, err := sellerBalance(ctx, tx, workspaceID, now)
		if err != nil {
			return err
		}
		if released-paid <= 0 {
			return ErrNothingAvailable
		}
		gross := released - paid
		testLeft, _, err := sellerFunds(ctx, tx, workspaceID, now)
		if err != nil {
			return err
		}
		p = Payout{ID: "mpo_" + uuid.NewString(), Method: PayoutCredits, Month: monthOf(now), Period: weekOf(now), GrossUSDMicros: gross, NetUSDMicros: gross,
			CreditsULXC: gross * ulxcPerUSDMicro}
		var test bool
		if err := tx.QueryRow(ctx, `INSERT INTO market_payouts (id, workspace_id, method, month, period, gross_usd_micros, net_usd_micros, credits_ulxc, paid_at, created_at)
			VALUES ($1, $2, 'credits', $3, $4, $5, $5, $6, $7, $7) RETURNING paid_at, created_at, test`,
			p.ID, workspaceID, p.Month, p.Period, gross, p.CreditsULXC, now).Scan(&p.PaidAt, &p.CreatedAt, &test); err != nil {
			return err
		}
		// B22.1: credits taken from test earnings are test-funded credits — all of them, for a test workspace (B25.1).
		testPart := min(testLeft, gross)
		if test {
			testPart = gross
		}
		if _, err := PostJournalTx(ctx, tx, JournalCredits, p.ID, "test", now,
			Posting{Account: SellerAvailable(workspaceID), AmountUSDMicros: testPart, Funding: "test"},
			Posting{Account: AccountCreditsIssued, AmountUSDMicros: -testPart, Funding: "test"},
			Posting{Account: SellerAvailable(workspaceID), AmountUSDMicros: gross - testPart, Funding: "live"},
			Posting{Account: AccountCreditsIssued, AmountUSDMicros: -(gross - testPart), Funding: "live"}); err != nil {
			return err
		}
		testULXC := testPart * ulxcPerUSDMicro
		funding := "test and live"
		switch testULXC {
		case 0:
			funding = "live"
		case p.CreditsULXC:
			funding = "test"
		}
		_, err = crediter.CreditLXCTx(ctx, tx, workspaceID, p.CreditsULXC, "marketplace earnings taken as credits",
			map[string]interface{}{"market_payout_id": p.ID, "usd_micros": gross, "funding": funding, "test_funded_ulxc": testULXC})
		return err
	})
	if err != nil && !errors.Is(err, ErrNothingAvailable) && !errors.Is(err, ErrTaxHold) {
		err = fmt.Errorf("market: take earnings as credits: %w", err)
	}
	return p, err
}

// PayOut is the weekly payout run (B32.42), made on the payout weekday: it releases every earning past its holdback
// onto the journal, so that each payout's statement shows what it paid; then it pays every connected seller not yet
// paid in money this ISO week whose available balance has reached the minimum, whose account Stripe — asked now — has
// enabled for payouts, and who is not withheld for their tax details (B32.41); and it retries every transfer Stripe
// has not yet accepted. A seller under the minimum is carried to the next week. It answers how many transfers Stripe
// accepted.
func (s *Store) PayOut(ctx context.Context, api ConnectStripe, now time.Time) (int, error) {
	return s.payOut(ctx, api, now, nil)
}

// PayOutSellers is the payout run for one kind of seller only — test (synthetic) workspaces, or real ones
// (B25.6) — so that each kind is paid with its own key: a test seller through Stripe test mode, always, even
// once Lens's key is live.
func (s *Store) PayOutSellers(ctx context.Context, api ConnectStripe, test bool, now time.Time) (int, error) {
	return s.payOut(ctx, api, now, &test)
}

// RetryPayouts asks Stripe again for every transfer of one kind of seller's payouts it has not yet accepted: what the
// payout run does on the days between its weekdays.
func (s *Store) RetryPayouts(ctx context.Context, api ConnectStripe, test bool) (int, error) {
	return s.transferUnpaid(ctx, api, &test)
}

func (s *Store) payOut(ctx context.Context, api ConnectStripe, now time.Time, test *bool) (int, error) {
	if _, err := s.ReleaseDue(ctx, now); err != nil {
		return 0, err
	}
	q, args := `SELECT workspace_id, stripe_account_id FROM market_sellers s
		WHERE NOT EXISTS (SELECT 1 FROM market_payouts p WHERE p.workspace_id = s.workspace_id AND p.method = 'stripe' AND p.period = $1)
		  AND NOT `+taxHeldSQL,
		[]any{weekOf(now)}
	if test != nil {
		q, args = q+` AND COALESCE((SELECT synthetic FROM workspaces w WHERE w.id = s.workspace_id), false) = $2`, append(args, *test)
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY workspace_id`, args...)
	if err != nil {
		return 0, fmt.Errorf("market: sellers to pay: %w", err)
	}
	type seller struct{ ws, account string }
	sellers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (seller, error) {
		var x seller
		return x, row.Scan(&x.ws, &x.account)
	})
	if err != nil {
		return 0, fmt.Errorf("market: sellers to pay: %w", err)
	}
	var askErr error
	live := liveKey(api)
	// payable is what the seller may be paid now: with a live key, only what live earnings paid for (B22.1).
	payable := func(q queryRower, ws string) (int64, error) {
		released, _, paid, err := sellerBalance(ctx, q, ws, now)
		if err != nil || !live {
			return released - paid, err
		}
		_, liveLeft, err := sellerFunds(ctx, q, ws, now)
		return min(released-paid, liveLeft), err
	}
	for _, x := range sellers {
		due, err := payable(s.pool, x.ws)
		if err != nil {
			return 0, fmt.Errorf("market: payout for %s: %w", x.ws, err)
		}
		if due < PayoutMinimumUSDMicros {
			continue
		}
		account, err := api.ConnectedAccount(ctx, x.account)
		if err != nil {
			if askErr == nil {
				askErr = fmt.Errorf("market: payout for %s: ask Stripe about %s: %w", x.ws, x.account, err)
			}
			continue // the next pass asks again
		}
		if err := s.RecordSellerAccount(ctx, account); err != nil {
			return 0, err
		}
		if !account.PayoutsEnabled {
			continue
		}
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			_, err := s.payOutTx(ctx, tx, x.ws, x.account, live, payable, now)
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("market: payout for %s: %w", x.ws, err)
		}
	}
	n, err := s.transferUnpaid(ctx, api, test)
	if err == nil {
		err = askErr
	}
	return n, err
}

// payOutTx makes one seller's Stripe payout of what payable leaves them now, inside tx: the market_payouts row, its
// self-billed invoice when the seller agreed to one, and its journal entry. It answers the payout's id, or "" when
// the seller is under the minimum or was paid this ISO week already. The transfer is asked of Stripe after.
func (s *Store) payOutTx(ctx context.Context, tx pgx.Tx, ws, account string, live bool, payable func(queryRower, string) (int64, error), now time.Time) (string, error) {
	if err := lockSeller(ctx, tx, ws); err != nil {
		return "", err
	}
	due, err := payable(tx, ws)
	if err != nil {
		return "", err
	}
	if due < PayoutMinimumUSDMicros {
		return "", nil
	}
	// Stripe's account fee is taken on the seller's first payout of the calendar month only.
	var paidThisMonth bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe' AND month = $2)`,
		ws, monthOf(now)).Scan(&paidThisMonth); err != nil {
		return "", err
	}
	gross := due / usdMicrosPerCent
	id := "mpo_" + uuid.NewString()
	// B32.43: to a seller who agreed to self-billing the payout is also a self-billed invoice for the earnings it
	// pays, and the VAT the seller charges on them is paid on top.
	bill, err := s.selfBillFor(ctx, tx, ws, id, gross*usdMicrosPerCent, now)
	if err != nil {
		return "", err
	}
	var vat int64
	if bill != nil {
		vat = bill.VATUSDMicros / usdMicrosPerCent
	}
	accountFee, payoutFee, net := PayoutFees(gross+vat, !paidThisMonth)
	var test bool
	err = tx.QueryRow(ctx, `INSERT INTO market_payouts (id, workspace_id, method, month, period, gross_usd_micros, vat_usd_micros,
		account_fee_usd_micros, payout_fee_usd_micros, net_usd_micros, stripe_account_id, created_at, livemode)
		VALUES ($1, $2, 'stripe', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (workspace_id, period) WHERE method = 'stripe' AND period <> '' DO NOTHING
		RETURNING test`,
		id, ws, monthOf(now), weekOf(now), gross*usdMicrosPerCent, vat*usdMicrosPerCent, accountFee*usdMicrosPerCent,
		payoutFee*usdMicrosPerCent, net*usdMicrosPerCent, account, now, live).Scan(&test)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // paid this week already
	}
	if err != nil {
		return "", err
	}
	if bill != nil {
		if err := issueSelfBillTx(ctx, tx, ws, bill, live, test); err != nil {
			return "", err
		}
	}
	// B32.17: the payout leaves the seller's available balance — the net to Stripe, Stripe's fees at cost.
	_, err = PostJournalTx(ctx, tx, JournalPayout, id, funding(live, test), now,
		Posting{Account: SellerAvailable(ws), AmountUSDMicros: (gross + vat) * usdMicrosPerCent},
		Posting{Account: AccountStripeClearing, AmountUSDMicros: -net * usdMicrosPerCent},
		Posting{Account: AccountConnectFees, AmountUSDMicros: -(accountFee + payoutFee) * usdMicrosPerCent})
	return id, err
}

// transferUnpaid asks Stripe for the transfer of every payout it has not yet accepted.
func (s *Store) transferUnpaid(ctx context.Context, api ConnectStripe, test *bool) (int, error) {
	// A payout is transferred only with a key of its own mode (B22.1): a test payout left unpaid when the key
	// went live is never sent as real money. With test set, only that kind of seller's payouts (B25.6).
	q, args := `SELECT id, workspace_id, stripe_account_id, net_usd_micros FROM market_payouts
		WHERE paid_at IS NULL AND livemode = $1`, []any{liveKey(api)}
	if test != nil {
		q, args = q+` AND test = $2`, append(args, *test)
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY created_at, id LIMIT 200`, args...)
	if err != nil {
		return 0, fmt.Errorf("market: payouts to transfer: %w", err)
	}
	type due struct {
		id, ws, account string
		net             int64
	}
	todo, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.id, &d.ws, &d.account, &d.net)
	})
	if err != nil {
		return 0, fmt.Errorf("market: payouts to transfer: %w", err)
	}
	n := 0
	var firstErr error
	for _, d := range todo {
		transferID, err := api.TransferToSeller(ctx, d.account, d.net/usdMicrosPerCent, d.id, d.ws)
		if err != nil {
			if _, uerr := s.pool.Exec(ctx, `UPDATE market_payouts SET last_error = $2 WHERE id = $1 AND paid_at IS NULL`, d.id, err.Error()); uerr != nil {
				return n, fmt.Errorf("market: payout %s: %w", d.id, uerr)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("market: transfer payout %s: %w", d.id, err)
			}
			continue
		}
		if _, err := s.pool.Exec(ctx, `UPDATE market_payouts SET stripe_transfer_id = $2, paid_at = now(), last_error = ''
			WHERE id = $1 AND paid_at IS NULL`, d.id, transferID); err != nil {
			return n, fmt.Errorf("market: payout %s: %w", d.id, err)
		}
		n++
	}
	return n, firstErr
}

// ReverseInvoice reverses the earnings of the uses a paid marketplace invoice carried, because its buyer
// was refunded ("buyer_refund") or charged back ("chargeback") by the Stripe object ref: a market_refunds
// row per use, oldest first, until the uses reversed for refunds and chargebacks of this invoice cover
// amountUSDMicros (every use when all). The buyer has their money back from Stripe, so nobody is credited;
// the seller's share comes out of their balance — inside the holdback before it was ever available, after
// it out of what is available, or owed from their future earnings.
func (s *Store) ReverseInvoice(ctx context.Context, invoiceID, cause, ref string, amountUSDMicros int64, all bool) (int, bool, error) {
	reason := map[string]string{"buyer_refund": "the buyer's payment was refunded", "chargeback": "the buyer's payment was charged back"}[cause]
	if reason == "" {
		return 0, false, fmt.Errorf("market: reverse invoice %s: unknown cause %q", invoiceID, cause)
	}
	rows, err := s.pool.Query(ctx, `SELECT u.id, r.use_id IS NOT NULL, COALESCE(r.cause, ''), u.price_ulxc,
		       CASE WHEN u.tax_metered_at IS NOT NULL THEN COALESCE(t.tax_usd_micros, 0) ELSE 0 END
		FROM market_uses u LEFT JOIN market_refunds r ON r.use_id = u.id LEFT JOIN market_tax_lines t ON t.use_id = u.id
		WHERE u.cleared_invoice_id = $1 ORDER BY u.used_at, u.id`, invoiceID)
	if err != nil {
		return 0, false, fmt.Errorf("market: uses of invoice %s: %w", invoiceID, err)
	}
	type use struct {
		id          string
		refunded    bool
		refundCause string
		priceULXC   int64
		tax         int64 // µUSD the invoice collected beside the price (B32.39)
	}
	uses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (use, error) {
		var u use
		return u, row.Scan(&u.id, &u.refunded, &u.refundCause, &u.priceULXC, &u.tax)
	})
	if err != nil {
		return 0, false, fmt.Errorf("market: uses of invoice %s: %w", invoiceID, err)
	}
	if len(uses) == 0 {
		return 0, false, nil
	}
	var covered int64
	for _, u := range uses {
		if u.refundCause == "buyer_refund" || u.refundCause == "chargeback" {
			covered += u.priceULXC/ulxcPerUSDMicro + u.tax
		}
	}
	n := 0
	for _, u := range uses {
		if u.refunded {
			continue
		}
		if !all && covered >= amountUSDMicros {
			break
		}
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `INSERT INTO market_refunds (use_id, listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc,
				gross_usd_micros, reversed_share_usd_micros, reason, cause, stripe_ref)
				SELECT u.id, u.listing_id, u.buyer_workspace_id, u.seller_workspace_id, u.price_ulxc, u.price_ulxc / $2,
				       COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e WHERE e.use_id = u.id), 0), $3, $4, $5
				FROM market_uses u
				WHERE u.id = $1
				ON CONFLICT (use_id) DO NOTHING`, u.id, ulxcPerUSDMicro, reason, cause, ref)
			n += int(tag.RowsAffected())
			return err
		})
		if err != nil {
			return n, true, fmt.Errorf("market: reverse use %s: %w", u.id, err)
		}
		covered += u.priceULXC/ulxcPerUSDMicro + u.tax
	}
	return n, true, nil
}
