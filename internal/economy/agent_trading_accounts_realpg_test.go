package economy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/partners"
)

// B30.48 — an agent's trading account at the broker partner, funded from its USD account.

// An agent opens a trading account at the Test broker and funds it with $1,000.00 from its USD account: one entry
// for trade_equities, the agent's account -100000 and the trading account +100000, and the trading account shows that
// cash beside "simulated prices". The same key again moves nothing; more than the agent holds, or live money while
// trade_equities is uncleared, moves nothing.
func TestTradingAccount_OpensAndFundsFromTheAgentsUSDAccount(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	reg := partners.NewRegistry(nil)
	s.SetAccountPartners(reg)
	s.SetBrokerPartners(reg)
	const ws = "ws-b3048-trading"
	agent, err := s.CreateAgent(ctx, ws, "trader", "owner-"+ws)
	if err != nil {
		t.Fatal(err)
	}
	company, err := s.OpenCurrencyAccount(ctx, ws, "", CurrencyUSD, "")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := s.OpenCurrencyAccount(ctx, ws, agent.ID, CurrencyUSD, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTestMoney(ctx, ws, company.ID, 1500_00, "b3048-in"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Kind: "transfer",
		IdempotencyKey: "b3048-to-agent", Funding: FundingTest,
		Postings: []MoneyPosting{{AccountID: company.ID, AmountMinor: -1500_00}, {AccountID: usd.ID, AmountMinor: 1500_00}}}); err != nil {
		t.Fatal(err)
	}

	ta, err := s.OpenTradingAccount(ctx, ws, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ta.Partner != "test" || ta.PartnerAccountRef == "" || ta.Prices != "simulated prices" || ta.Currency != CurrencyUSD || ta.CashMinor != 0 {
		t.Fatalf("opened %+v; want an empty USD account at the test broker, simulated prices", ta)
	}
	if again, err := s.OpenTradingAccount(ctx, ws, agent.ID); err != nil || again.ID != ta.ID {
		t.Fatalf("opened again = %+v (%v); want the same account %s", again, err, ta.ID)
	}

	e, err := s.FundTradingAccount(ctx, ws, agent.ID, 1000_00, FundingTest, "b3048-fund")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		capability, kind, account, currency, funding string
		amount                                       int64
	}
	rows, err := pool.Query(ctx, `SELECT e.capability, e.kind, p.account_id, p.currency, p.funding, p.amount_minor
		FROM money_entries e JOIN money_postings p ON p.entry_id = e.id WHERE e.id = $1 ORDER BY p.line`, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.capability, &r.kind, &r.account, &r.currency, &r.funding, &r.amount); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	want := []row{
		{CapabilityTradeEquities, "trading_account_funding", usd.ID, CurrencyUSD, FundingTest, -1000_00},
		{CapabilityTradeEquities, "trading_account_funding", ta.AccountID, CurrencyUSD, FundingTest, 1000_00},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the funding posted %+v; want %+v", got, want)
	}

	if replay, err := s.FundTradingAccount(ctx, ws, agent.ID, 1000_00, FundingTest, "b3048-fund"); err != nil || replay.ID != e.ID {
		t.Fatalf("the same key again = %s (%v); want the entry %s", replay.ID, err, e.ID)
	}
	if _, err := s.FundTradingAccount(ctx, ws, agent.ID, 600_00, FundingTest, "b3048-over"); !errors.Is(err, ErrAgentFunds) {
		t.Fatalf("funding $600.00 from $500.00: %v; want ErrAgentFunds", err)
	}
	if _, err := s.FundTradingAccount(ctx, ws, agent.ID, 1_00, FundingLive, "b3048-live"); !errors.Is(err, ErrCapabilityNotCleared) {
		t.Fatalf("live funding while trade_equities is uncleared: %v; want ErrCapabilityNotCleared", err)
	}

	seen, err := s.TradingAccount(ctx, ws, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seen.ID != ta.ID || seen.CashMinor != 1000_00 || seen.TestMinor != 1000_00 || seen.LiveMinor != 0 || seen.Prices != "simulated prices" {
		t.Fatalf("the agent sees %+v; want $1,000.00 of test cash and simulated prices", seen)
	}
	if b := moneyBalance(t, s, ws, usd.ID); b.AmountMinor != 500_00 {
		t.Fatalf("the agent's USD account holds %d; want 50000", b.AmountMinor)
	}
	if _, err := s.TradingAccount(ctx, "ws-b3048-other", agent.ID); !errors.Is(err, ErrTradingAccountNotFound) {
		t.Fatalf("another workspace reads it: %v; want ErrTradingAccountNotFound", err)
	}
}
