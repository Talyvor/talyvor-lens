package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B54.6 — a charge on an agent's reusable card is judged by the agent's own rules as well as the card's controls: one
// inside the card's limits but over the agent's daily limit is declined, one above the agent's approval amount is
// declined as needing approval (a card charge cannot wait) and files none, and an approved one counts toward the
// agent's spend, its limit alerts included. Each approved charge posts from the agent's account to the partner.
func TestB546_AnAgentsReusableCardFollowsTheAgentsRules(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b546"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b546"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			issuer = issuerfake.New(store.AuthorizeCardCharge)
			if err := layer.Register(issuer.Adapter()); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	agent := b.acct.AgentID
	card, err := b.store.IssueReusableCard(ctx, ws, economy.ReusableCard{AgentID: agent, Currency: "GBP", PerPaymentLimitMinor: 10000,
		DailyLimitMinor: 40000, IdempotencyKey: "agent-card"})
	if err != nil || card.Status != economy.ReusableCardActive {
		t.Fatalf("issue the agent's card = %+v %v, want active", card, err)
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_ref FROM money_reusable_cards WHERE id = $1`, card.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	charge := func(minor int64, wantOK bool, wantReason string) {
		t.Helper()
		ok, reason, err := issuer.Charge(ctx, ref, "OpenAI", money.Amount{Minor: minor, Asset: "GBP"})
		if err != nil || ok != wantOK || !strings.Contains(reason, wantReason) {
			t.Fatalf("%d = %v %q %v, want %v %q", minor, ok, reason, err, wantOK, wantReason)
		}
	}
	// GBP 10.00 is USD 12.50 at the Test partner's rate.
	v := int64(1250*10_000) * economy.ULXCPerUSDMicro

	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{DailyLimitULXC: v * 5 / 2, Timezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	charge(1000, true, "")
	charge(1000, true, "")
	charge(1000, false, "the agent has spent") // the card's own daily limit of GBP 400.00 is far off: the agent's is the stricter

	// The approved charges count toward the agent's spend: its daily limit's progress shows them.
	alerts, err := b.store.CheckSpendAlerts(ctx, ws, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var spent int64 = -1
	for _, a := range alerts {
		if a.Kind == economy.SpendAlertAgentLimit && a.AgentID == agent && a.Period == "daily" {
			spent = a.SpentMinor
		}
	}
	if spent != 2*v {
		t.Fatalf("the agent's daily spend = %d (alerts %+v), want %d: its two approved card charges", spent, alerts, 2*v)
	}

	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{ApprovalAboveULXC: v / 2, Timezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	charge(1000, false, "needs your approval")
	var filed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE agent_id = $1`, agent).Scan(&filed); err != nil || filed != 0 {
		t.Fatalf("approvals filed = %d %v, want none: a card charge cannot wait for one", filed, err)
	}
	charge(400, true, "") // under the approval amount

	b.balance(t, 50000-1000-1000-400)
	if got, want := b4610Postings(t, pool, ws), "card_charge agent -1000; card_charge partner 1000; card_charge agent -1000; card_charge partner 1000; "+
		"card_charge agent -400; card_charge partner 400"; got != want {
		t.Fatalf("the card posted %q, want %q", got, want)
	}
}
