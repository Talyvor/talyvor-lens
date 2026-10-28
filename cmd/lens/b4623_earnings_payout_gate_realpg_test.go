package main

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B46.23 — a pending earnings payout passes the money layer's gate only to its own recipient's verified payout details
// from its own paying account: the same key and amount to other details, or from another account, is judged by the
// agent's rules. And no live money is paid out until payments_out is cleared for it — neither a new payout nor one
// already held.
func TestB4623_AHeldEarningsPayoutPassesTheGateOnlyToItsOwnRecipientAndNoLiveMoneyBeforeClearance(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, seller, other = "ws-b4623", "ws-b4623-seller", "ws-b4623-other"
	f := &b4611Payouts{paid: map[string]int{}}
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, ""), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapPayouts, Partner: "fakepay"}: true}})
			if err != nil {
				t.Fatal(err)
			}
			if err := layer.Register(&money.Adapter{Name: "fakepay", Payouts: f}); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	agent := b.acct.AgentID
	// Two payouts held for recipients who have not verified their details yet.
	sale := b4611Pay(t, b.store, ws, economy.Earnings{Kind: economy.EarningsSale, Source: "sale:lic_b4623", AgentID: agent, Currency: "GBP",
		Shares: []economy.EarningsShare{{WorkspaceID: seller, AmountMinor: 4200}}})[0]
	later := b4611Pay(t, b.store, ws, economy.Earnings{Kind: economy.EarningsSale, Source: "sale:lic_b4623_later", AgentID: agent, Currency: "GBP",
		Shares: []economy.EarningsShare{{WorkspaceID: other, AmountMinor: 100}}})[0]
	if sale.Status != economy.PayoutHeld || later.Status != economy.PayoutHeld {
		t.Fatalf("the payouts = %+v and %+v, want both held", sale, later)
	}
	// From now on the agent may move at most £1.00 at a time.
	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{MaxPerRequestULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}

	// The seller verifies: the held £42.00 goes through the gate to their details, as PayEarnings judged and held it.
	b4611Verify(t, b.store, seller, "Sam Seller")
	list, err := b.store.EarningsPayouts(ctx, seller)
	if err != nil || len(list) != 1 || list[0].Status != economy.PaymentPending || f.paid[sale.ID] != 1 {
		t.Fatalf("the seller's payouts = %+v, %v, the partner asked %d times; want %s sent once, pending", list, err, f.paid[sale.ID], sale.ID)
	}
	var from string
	if err := pool.QueryRow(ctx, `SELECT c.partner_account_ref FROM money_accounts a
		JOIN money_accounts c ON c.id = COALESCE(a.parent_account_id, a.id) WHERE a.id = $1`, b.acct.ID).Scan(&from); err != nil || from == "" {
		t.Fatalf("the partner reference the agent pays from = %q, %v", from, err)
	}
	own := money.Payee{Name: "Sam Seller", IBAN: "GB82WEST12345698765432"} // the seller's verified details, as sendEarningsPayout sends them
	op := func(ref string, to money.Payee) money.Op {
		return money.Op{Route: money.Route{Workspace: ws, Agent: agent, Capability: economy.CapabilityPaymentsOut}, Partner: "fakepay",
			Capability: money.CapPayouts, Method: "pay", Key: sale.ID, Ref: ref, Amount: money.Amount{Minor: 4200, Asset: "GBP"}, Payee: to.Name, To: to}
	}
	limits := b.store.MoneyLimits()
	if err := limits.Allow(ctx, op(from, own)); err != nil {
		t.Fatalf("the held payout itself: %v, want it through the gate", err)
	}
	for name, o := range map[string]money.Op{
		"another IBAN":           op(from, money.Payee{Name: own.Name, IBAN: "GB33BUKB20201555555555"}),
		"another holder":         op(from, money.Payee{Name: "Mallory", IBAN: own.IBAN}),
		"a USDC address":         op(from, money.Payee{Name: own.Name, Network: "base", Address: "0x00000000000000000000000000000000000000b4"}),
		"another paying account": op(from+"-other", own),
	} {
		if err := limits.Allow(ctx, o); !errors.Is(err, money.ErrLimit) {
			t.Errorf("%s under the held payout's key: %v, want refused by the agent's £1.00 limit", name, err)
		}
	}

	// Live money is refused until payments_out is cleared for it: nothing is held, and the partner is asked nothing.
	if p, err := b.store.PayEarnings(ctx, ws, economy.Earnings{Kind: economy.EarningsRoyalty, Source: "royalty:live", AgentID: agent,
		Currency: "GBP", Funding: economy.FundingLive, Shares: []economy.EarningsShare{{WorkspaceID: seller, AmountMinor: 10}}}); !errors.Is(err, economy.ErrCapabilityNotCleared) {
		t.Fatalf("live earnings before payments_out is cleared = %+v, %v; want refused as not cleared", p, err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM money_earnings_payouts WHERE source = 'royalty:live'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d live payouts made before clearance, want none", n)
	}
	// A live payout already held — made while payments_out was cleared — is not sent once the clearance is gone.
	if _, err := pool.Exec(ctx, `UPDATE money_earnings_payouts SET funding = 'live' WHERE id = $1`, later.ID); err != nil {
		t.Fatal(err)
	}
	b4611Verify(t, b.store, other, "Olive Other")
	if list, err := b.store.EarningsPayouts(ctx, other); err != nil || len(list) != 1 || list[0].Status != economy.PayoutHeld || f.paid[later.ID] != 0 {
		t.Fatalf("the held live payout = %+v, %v, the partner asked %d times; want it still held and not sent", list, err, f.paid[later.ID])
	}
}
