package economy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
)

// money_credits.go — B30.25: BUY CREDITS FROM A CURRENCY ACCOUNT, WITH NO CARD FEE.
//
// The company's account in pounds, euros or dollars pays Talyvor's revenue account in the same currency, and the
// credits land in the workspace at list — $0.10 a credit, pounds and euros at the conversion partner's rate for
// dollars — with no fee on top. One transaction writes the money entry (kind credit_purchase) and one lxc_ledger
// purchase row naming it, funded test or live as the money was; live money is asked of currency_accounts first.

// ErrCreditPurchase: a purchase asked for in an amount, currency or funding it cannot have.
var ErrCreditPurchase = errors.New("economy: invalid credit purchase")

// CreditPurchase is credits bought from a company's currency account.
type CreditPurchase struct {
	EntryID        string `json:"entry_id"` // the money entry that paid for them
	AccountID      string `json:"account_id"`
	AmountMinor    int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
	Funding        string `json:"funding"` // test ("" too) or live
	IdempotencyKey string `json:"idempotency_key"`
	CreditsULXC    int64  `json:"credits_ulxc"`
	BalanceULXC    int64  `json:"balance_ulxc"` // the workspace's credits after it
}

// BuyCredits buys credits for workspaceID with in.AmountMinor of in.Currency from its company account. The same
// idempotency key again answers the purchase it first made and buys nothing twice.
func (s *DualTokenStore) BuyCredits(ctx context.Context, workspaceID string, in CreditPurchase) (CreditPurchase, error) {
	p := in
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if p.Funding == "" {
		p.Funding = FundingTest
	}
	switch {
	case p.Currency != CurrencyGBP && p.Currency != CurrencyEUR && p.Currency != CurrencyUSD:
		return CreditPurchase{}, fmt.Errorf("%w: credits are bought from an account in GBP, EUR or USD, not %q", ErrCreditPurchase, p.Currency)
	case p.AmountMinor <= 0 || p.AmountMinor > math.MaxInt64/100_000:
		return CreditPurchase{}, fmt.Errorf("%w: amount_minor is a positive number of pence or cents", ErrCreditPurchase)
	case p.Funding != FundingTest && p.Funding != FundingLive:
		return CreditPurchase{}, fmt.Errorf("%w: funding is test or live, not %q", ErrCreditPurchase, p.Funding)
	case strings.TrimSpace(p.IdempotencyKey) == "":
		return CreditPurchase{}, fmt.Errorf("%w: a purchase needs an idempotency_key", ErrCreditPurchase)
	}
	cents := p.AmountMinor
	if p.Currency != CurrencyUSD {
		if s.accountPartners == nil {
			return CreditPurchase{}, fmt.Errorf("%w: no account partner is set", ErrAccountNotOpened)
		}
		conv, err := s.accountPartners.FX(ctx, CapabilityCurrencyAccounts)
		if err != nil {
			return CreditPurchase{}, fmt.Errorf("economy: price the credits: %w", err)
		}
		q, err := conv.Quote(partners.WithWorkspace(ctx, workspaceID), partners.FXQuoteRequest{
			ID:   "credits:" + workspaceID + ":" + p.IdempotencyKey + ":" + strconv.FormatInt(p.AmountMinor, 10) + p.Currency,
			Sell: partners.Money{Minor: p.AmountMinor, Currency: p.Currency}, Buy: CurrencyUSD})
		if err != nil {
			return CreditPurchase{}, fmt.Errorf("economy: price the credits: %w", err)
		}
		cents = q.Buy.Minor
	}
	p.CreditsULXC = cents * 10_000 * ULXCPerUSDMicro // a cent is 10,000 µUSD

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreditPurchase{}, fmt.Errorf("economy: buy credits: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The company's account, held while its balance is read and spent: two purchases from it take turns.
	err = tx.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND purpose = 'company'
		AND agent_id IS NULL AND status = 'open' ORDER BY created_at LIMIT 1 FOR UPDATE`, workspaceID, p.Currency).Scan(&p.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditPurchase{}, fmt.Errorf("%w: the company has no open %s account", ErrMoneyAccountNotFound, p.Currency)
	}
	if err != nil {
		return CreditPurchase{}, fmt.Errorf("economy: buy credits: %w", err)
	}
	revenue, err := routeAccount(ctx, tx, workspaceID, p.Currency, MoneyRevenue, "", "Talyvor fees")
	if err != nil {
		return CreditPurchase{}, err
	}
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityCurrencyAccounts, Kind: "credit_purchase",
		IdempotencyKey: "credits:" + p.IdempotencyKey, Funding: p.Funding, Memo: "credits", Postings: []MoneyPosting{
			{AccountID: p.AccountID, AmountMinor: -p.AmountMinor}, {AccountID: revenue, AmountMinor: p.AmountMinor}}}, s.screener)
	if err != nil {
		return CreditPurchase{}, err
	}
	p.EntryID = e.ID
	// A replay: the entry was posted before, and so were its credits.
	var credited int64
	err = tx.QueryRow(ctx, `SELECT amount, balance_after FROM lxc_ledger WHERE workspace_id = $1 AND type = $2
		AND metadata->>'money_entry_id' = $3 LIMIT 1`, workspaceID, LXCTypePurchase, e.ID).Scan(&credited, &p.BalanceULXC)
	if err == nil {
		p.CreditsULXC = credited
		return p, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CreditPurchase{}, fmt.Errorf("economy: buy credits: %w", err)
	}
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		p.AccountID, p.Funding).Scan(&held); err != nil {
		return CreditPurchase{}, fmt.Errorf("economy: buy credits: %w", err)
	}
	if held < 0 { // the balance read is after this entry's postings
		return CreditPurchase{}, fmt.Errorf("%w: the account holds %s of %s money, and these credits cost %s", ErrAgentFunds,
			minorString(held+p.AmountMinor, p.Currency), p.Funding, minorString(p.AmountMinor, p.Currency))
	}
	if p.BalanceULXC, err = s.CreditLXCTx(ctx, tx, workspaceID, p.CreditsULXC, "bought from the company's "+p.Currency+" account",
		map[string]interface{}{"funding": p.Funding, "money_entry_id": e.ID, "amount_minor": p.AmountMinor, "currency": p.Currency}); err != nil {
		return CreditPurchase{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CreditPurchase{}, fmt.Errorf("economy: buy credits: %w", err)
	}
	return p, nil
}
