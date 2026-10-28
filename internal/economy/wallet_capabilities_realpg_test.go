package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// companyPaymentsFake is the marketplace a payment to another company's agent goes through: it refuses no one
// and records the payment as a use.
type companyPaymentsFake struct{ uses int }

func (f *companyPaymentsFake) CompanyPayeeRefusal(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *companyPaymentsFake) ChargeAgentPayment(context.Context, pgx.Tx, string, string, string, string, string, int64, string, time.Time) (string, error) {
	f.uses++
	return "use_b221", nil
}

// B22.1 — on the migrated schema: a RED capability (cards, paid in credits) and an AMBER one (paying another
// owner's agent, on the company's Stripe bill) refuse live money without a clearance and take test money; the
// operator's clearance lets live money through, and revoking it stops it again; each clear and revoke is a row.
func TestWalletClasses_AmberAndRedTakeTestMoneyUntilCleared(t *testing.T) {
	pool := supplyPool(t)
	ctx := WithUseCountry(context.Background(), "GB") // B30.1: a clearance reaches only the countries it lists
	const ws, other = "ws-b221", "ws-b221-other"
	for _, id := range []string{ws, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	s := NewDualTokenStore(nil, pool, nil)
	const lxc = int64(1_000_000)
	// Real money only: a live top-up of 1,000 LXC, 900 of it given to the agent.
	if _, err := s.CreditLXC(ctx, ws, 1_000*lxc, "stripe top-up", map[string]interface{}{"funding": FundingLive}); err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreateAgent(ctx, ws, "buyer", "user-nicolai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, agent.ID, 900*lxc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveAgentCard(ctx, ws, AgentCard{ID: "ic_b221", AgentID: agent.ID, StripeCardholderID: "ich_b221", Last4: "4242",
		ExpMonth: 12, ExpYear: 2030, Currency: "usd"}); err != nil {
		t.Fatal(err)
	}
	buy := func(evt string, dollars int64) CardDecision {
		t.Helper()
		d, err := s.AuthorizeAgentCard(ctx, CardAuthorization{EventID: evt, AuthorizationID: "iauth_" + evt, CardID: "ic_b221",
			AmountMinor: dollars * 100, Currency: "usd", MerchantName: "Paper Co", MerchantID: "m-" + evt, At: time.Now(), USDMicros: dollars * 1_000_000})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	testFunded := func() int64 {
		t.Helper()
		v, err := s.TestFundedLXC(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cardRows := func() (n int, sum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'agent_card'`,
			ws).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}

	// RED, with live-funded credits only and no clearance: declined, naming the class; nothing debited.
	if d := buy("evt_live", 10); d.Approved || !strings.Contains(d.Reason, "class RED") {
		t.Fatalf("a card purchase on live credits = %+v, want declined as class RED", d)
	}
	if n, _ := cardRows(); n != 0 {
		t.Fatalf("a declined purchase wrote %d ledger rows", n)
	}
	// Test money arrives (a Stripe test-mode top-up of 200 LXC): a $10 purchase (100 LXC) takes it.
	if _, err := s.CreditLXC(ctx, ws, 200*lxc, "stripe top-up", map[string]interface{}{"funding": FundingTest}); err != nil {
		t.Fatal(err)
	}
	if d := buy("evt_test", 10); !d.Approved {
		t.Fatalf("a card purchase on test credits = %+v, want approved", d)
	}
	if n, sum := cardRows(); n != 1 || sum != -100*lxc || testFunded() != 100*lxc {
		t.Fatalf("after the test purchase: %d rows summing %d, test-funded %d; want 1 row of −100 LXC and 100 LXC test-funded", n, sum, testFunded())
	}
	// $15 is more than the 100 LXC of test money left: declined.
	if d := buy("evt_over", 15); d.Approved || !strings.Contains(d.Reason, "class RED") {
		t.Fatalf("a purchase past the test money = %+v, want declined as class RED", d)
	}
	// The operator clears cards on a partner's reference: live credits go through, test money is left alone — on a
	// plan with live money (B32.12; free keeps it on test money even cleared).
	planGatesOnPlan(t, pool, ws, "team", false)
	liveVerified(t, pool, ws) // B30.4: cards need L2 for live money
	if _, err := s.ClearCapability(ctx, CapabilityAgentCard, "nicolai", ClearanceTerms{Reference: "issuing partner PA-1",
		Licence: "EMI-900001", Partner: "Issuer Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if d := buy("evt_cleared", 15); !d.Approved {
		t.Fatalf("a live purchase once cleared = %+v, want approved", d)
	}
	if n, sum := cardRows(); n != 2 || sum != -250*lxc || testFunded() != 100*lxc {
		t.Fatalf("after the cleared purchase: %d rows summing %d, test-funded %d; want 2 rows, −250 LXC, 100 LXC test-funded", n, sum, testFunded())
	}
	// Revoked: live money stops again from the next purchase.
	if err := s.RevokeClearance(ctx, CapabilityAgentCard, "nicolai", "partner paused"); err != nil {
		t.Fatal(err)
	}
	if d := buy("evt_revoked", 15); d.Approved || !strings.Contains(d.Reason, "class RED") {
		t.Fatalf("a live purchase after the revoke = %+v, want declined as class RED", d)
	}
	// Stripe reverses the test purchase: the agent is credited, and the credits are test money again.
	if _, err := s.SettleAgentCard(ctx, CardSettlement{EventID: "evt_rev", Kind: CardRelease, AuthorizationID: "iauth_evt_test",
		CardID: "ic_b221", Status: "reversed", Currency: "usd", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if testFunded() != 200*lxc {
		t.Fatalf("after the reversal the test-funded credits = %d, want 200 LXC", testFunded())
	}

	// AMBER: paying another owner's agent goes on the company's Stripe bill, which is real money once the key is live.
	payee, err := s.CreateAgent(ctx, other, "seller", "user-other")
	if err != nil {
		t.Fatal(err)
	}
	market := &companyPaymentsFake{}
	s.SetCompanyPayments(market)
	pay := func() error {
		_, err := s.PayAgent(ctx, ws, agent.ID, payee.ID, lxc, "")
		return err
	}
	s.SetLiveStripe(true)
	if err := pay(); !errors.Is(err, ErrCapabilityNotCleared) || !strings.Contains(err.Error(), "class AMBER") {
		t.Fatalf("paying another owner with a live key = %v, want refused as class AMBER", err)
	}
	s.SetLiveStripe(false)
	if err := pay(); err != nil {
		t.Fatalf("paying another owner with a test key = %v, want paid", err)
	}
	s.SetLiveStripe(true)
	if _, err := s.ClearCapability(ctx, CapabilityPayAnotherOwner, "nicolai", ClearanceTerms{Reference: "counsel opinion LNE-2026-09",
		Licence: "API-900002", Partner: "Payments Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := pay(); err != nil {
		t.Fatalf("paying another owner once cleared = %v, want paid", err)
	}
	if err := s.RevokeClearance(ctx, CapabilityPayAnotherOwner, "nicolai", "FCA notification pending"); err != nil {
		t.Fatal(err)
	}
	if err := pay(); !errors.Is(err, ErrCapabilityNotCleared) {
		t.Fatalf("paying another owner after the revoke = %v, want refused", err)
	}
	if market.uses != 2 {
		t.Fatalf("the marketplace bill carries %d payments, want 2 (test key, and cleared)", market.uses)
	}

	// Every clear and revoke is in the audit log; the screens see each class and what takes real money.
	log, err := s.ClearanceLog(ctx, 10)
	if err != nil || len(log) != 4 || log[0].Action != "revoke" || log[0].Operator != "nicolai" || log[3].Reference != "issuing partner PA-1" {
		t.Fatalf("clearance log = %+v, %v; want 4 rows, newest first", log, err)
	}
	caps, err := s.WalletCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c.RealMoney != (c.Class == ClassGreen) {
			t.Errorf("%s (%s) real money = %v after every clearance was revoked", c.Key, c.Class, c.RealMoney)
		}
	}
}
