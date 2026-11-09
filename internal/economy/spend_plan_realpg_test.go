package economy

import (
	"context"
	"testing"
	"time"
)

// B55.45 — the month plan's currency is the one that holds the most value, each currency's minor units scaled by its
// own decimals: an agent's 2 USDC (2,000,000 millionths) never outweighs the company's $5,000.00 (500,000 cents).
func TestSpendPlan_ASmallUSDCWalletIsNeverThePlansCurrency(t *testing.T) {
	pool := supplyPool(t)
	ctx := WithUseCountry(context.Background(), "GB")
	s := screenedStore(pool)
	const ws = "ws-b5545"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreateAgent(ctx, ws, "spender", "user-b5545")
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := s.OpenMoneyAccount(ctx, MoneyAccount{WorkspaceID: ws, AgentID: agent.ID, Currency: CurrencyUSDC, Purpose: MoneyAgent})
	if err != nil {
		t.Fatal(err)
	}
	fund := func(key string, account MoneyAccount, minor int64) {
		t.Helper()
		partner := openMoney(t, s, ws, account.Currency, MoneyPartner)
		if _, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Test payer",
			Kind: "test_top_up", IdempotencyKey: key, Funding: FundingTest, Postings: []MoneyPosting{
				{AccountID: account.ID, AmountMinor: minor}, {AccountID: partner.ID, AmountMinor: -minor}}}); err != nil {
			t.Fatal(err)
		}
	}
	fund("usd", openMoney(t, s, ws, CurrencyUSD, MoneyCompany), 500_000) // $5,000.00
	fund("usdc", wallet, 2_000_000)                                      // 2 USDC

	plan, err := s.SpendPlan(ctx, ws, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Currency != CurrencyUSD {
		t.Fatalf("the plan's currency = %s; want USD, the $5,000.00 account over the 2 USDC wallet", plan.Currency)
	}
}
