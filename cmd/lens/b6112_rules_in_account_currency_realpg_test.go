package main

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// b6112Rates are the Test FX partner's rates with the euro worth eurUSD dollars.
type b6112Rates struct{ eurUSD *big.Rat }

func (r b6112Rates) PerUSD(_ context.Context, currency string) (*big.Rat, error) {
	switch currency {
	case "EUR":
		return new(big.Rat).Inv(r.eurUSD), nil
	case "GBP":
		return big.NewRat(4, 5), nil
	}
	return big.NewRat(1, 1), nil
}

// b6112TopUp puts minor more test euros in the agent's account.
func b6112TopUp(t *testing.T, pool *pgxpool.Pool, b b468Payer, minor int64) {
	t.Helper()
	ctx := context.Background()
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = $2`,
		b.acct.WorkspaceID, b.acct.Currency).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: b.acct.WorkspaceID, Capability: economy.CapabilityPaymentsIn,
		Kind: "test_top_up", IdempotencyKey: "top-up-b6112", Funding: economy.FundingTest, Counterparty: "Test payer",
		Postings: []economy.MoneyPosting{{AccountID: partnerAccount, AmountMinor: -minor}, {AccountID: b.acct.ID, AmountMinor: minor}}}); err != nil {
		t.Fatal(err)
	}
}

// B61.12 — a euro account's agent with "per payment €500.00" pays €500.00 and is refused €500.01, today and with the
// euro moved by 5%: the rules are held in euros and a payment in euros is judged on its own amount. A dollar payment
// from it is judged at the rate, rounded up.
func TestB6112_AnAgentsRulesHoldInTheAccountsOwnCurrency(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6112"
	var reg *partners.Registry
	b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
			reg = r
			return sandboxMoneyLayer(t, pool, store, r)
		})
	b6112TopUp(t, pool, b, 400_000)
	// A payee saved a day ago: the new-payee wait (B50.21) is not what this judges.
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '2 days' WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	agent := b.acct.AgentID
	rules, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{Currency: "EUR", MaxPerPaymentMinor: 50_000})
	if err != nil {
		t.Fatal(err)
	}
	if rules.Currency != "EUR" || rules.MaxPerPaymentMinor != 50_000 {
		t.Fatalf("the rules read back as %s %d, want EUR 50000", rules.Currency, rules.MaxPerPaymentMinor)
	}
	refused := func(key string, minor int64) {
		t.Helper()
		_, err := b.store.PayOutside(ctx, ws, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: b.payee.ID,
			Currency: "EUR", AmountMinor: minor, Reference: "Invoice " + key, IdempotencyKey: key})
		if !errors.Is(err, economy.ErrAgentRule) {
			t.Fatalf("pay %s of %d: %v, want refused by the €500.00 limit per payment", key, minor, err)
		}
	}
	for _, eurUSD := range []*big.Rat{big.NewRat(11, 10), big.NewRat(1155, 1000)} { // the fixed test rate, then the euro up 5%
		reg.UseFXRates(b6112Rates{eurUSD})
		key := eurUSD.FloatString(3)
		b.pay(t, "at-"+key, 50_000)
		refused("over-"+key, 50_001)
	}
	// A dollar payment, at $1.155 to the euro: $577.49 is €499.99…, within; $577.51 is €500.008…, which rounds up to
	// €500.01 and is refused (rounded down it would pass).
	for minor, want := range map[int64]string{57_749: "allowed", 57_751: "refused"} {
		sim, err := b.store.SimulateAgentRules(ctx, ws, agent, economy.SimulatedRequest{AmountMinor: minor, Currency: "USD"})
		if err != nil || sim.Verdict != want {
			t.Errorf("a payment of %d US cents: %+v, %v; want %s", minor, sim, err, want)
		}
	}
	if !t.Failed() {
		t.Logf("€500.00 paid twice, €500.01 refused twice, at both rates")
	}
	// The payments the rules let through are on the ledger: two of €500.00 out of the agent's account.
	var paid int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(-sum(p.amount_minor), 0)::bigint FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		WHERE p.account_id = $1 AND e.idempotency_key LIKE 'payment_out%'`, b.acct.ID).Scan(&paid); err != nil || paid != 100_000 {
		t.Fatalf("the agent's account paid out %d (%v), want 100000", paid, err)
	}
}

// B61.12 — rules saved before, in µLXC, read back in dollars at the peg, limit for limit; a boost in the rules'
// currency raises the limit it names; a µLXC save on euro rules is refused rather than read as dollars.
func TestB6112_RulesInMicroLXCMoveToDollarsAtThePeg(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6112-peg"
	b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry { return sandboxMoneyLayer(t, pool, store, r) })
	agent := b.acct.AgentID
	hourly := int64(20_000_000) // LXC 20 = $2.00
	r, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{MaxPerRequestULXC: 5_000_000, HourlyLimitULXC: &hourly,
		DailyLimitULXC: 200_000_000, MonthlyLimitULXC: 4_000_000_000, ApprovalAboveULXC: 50_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if r.Currency != "USD" || r.MaxPerPaymentMinor != 50 || *r.HourlyLimitMinor != 200 || r.DailyLimitMinor != 2_000 ||
		r.MonthlyLimitMinor != 40_000 || r.ApprovalAboveMinor != 500 || *r.WeeklyLimitMinor != 0 {
		t.Fatalf("µLXC rules read back as %s %d/%d/%d/%d/%d, want USD 50/200/2000/40000/500", r.Currency, r.MaxPerPaymentMinor,
			*r.HourlyLimitMinor, r.DailyLimitMinor, r.MonthlyLimitMinor, r.ApprovalAboveMinor)
	}
	// A boost in the rules' currency: $25.00 is over a $20.00 day, and within it raised to $30.00.
	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{Currency: "USD", DailyLimitMinor: 2_000, HourlyLimitMinor: new(int64)}); err != nil {
		t.Fatal(err)
	}
	judge := func(want string) {
		t.Helper()
		sim, err := b.store.SimulateAgentRules(ctx, ws, agent, economy.SimulatedRequest{AmountMinor: 2_500, Currency: "USD"})
		if err != nil || sim.Verdict != want {
			t.Fatalf("$25.00: %+v, %v; want %s", sim, err, want)
		}
	}
	judge("refused")
	if _, err := b.store.BoostAgentRule(ctx, ws, agent, economy.AgentRuleBoost{Rule: "daily_limit_minor", Value: 3_000,
		Until: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	judge("allowed")
	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{Currency: "EUR", MaxPerPaymentMinor: 50_000, HourlyLimitMinor: new(int64)}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{MaxPerRequestULXC: 5_000_000}); !errors.Is(err, economy.ErrRulesCurrency) {
		t.Fatalf("a µLXC save on euro rules: %v, want refused", err)
	}
}
