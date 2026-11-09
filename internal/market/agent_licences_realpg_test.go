package market

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
)

// B32.22 — an agent buys, rents and subscribes only within its mandate. With MaxCommitment 20 LXC it rents a 15 LXC
// licence; a 25 LXC rent is refused naming the rule, and writes no licence and no billed row; it may not subscribe
// until its rules say so. Above its approval amount a rent files an approval, and once approved goes through once. The
// rule simulator answers the same licence questions.
func TestAgentLicences_AnAgentTakesOnlyTheLicencesItsRulesAllow(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, buyer = "ws-b3222-seller", "ws-b3222-buyer"
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	n := func(n int) *int { return &n }
	// 1 µUSD is 10 µLXC: 15 LXC is $1.50.
	l, err := s.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`),
		Offers: []Offer{
			{Kind: OfferPerUse, Licence: LicenceCommercial, PriceUSDMicros: 50_000},
			{Kind: OfferRent, Licence: LicenceCommercial, PriceUSDMicros: 1_500_000, PeriodDays: n(30)},
			{Kind: OfferRent, Licence: LicenceEnterprise, PriceUSDMicros: 2_500_000, PeriodDays: n(30), Seats: n(5)},
			{Kind: OfferSubscribe, Licence: LicenceCommercial, PriceUSDMicros: 400_000, PeriodDays: n(30), IncludedUses: n(10)},
		}})
	if err != nil {
		t.Fatal(err)
	}
	offer := map[string]string{}
	for _, o := range l.Offers {
		offer[o.Kind+" "+o.Licence] = o.ID
	}
	deps := LicenceDeps{Meter: &stripeMeter{tried: map[string]int{}}, Agents: bank, Capabilities: bank}
	agentWith := func(name string, r economy.AgentRules) string {
		t.Helper()
		a, err := bank.CreateAgent(ctx, buyer, name, "owner-"+buyer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bank.SetAgentRules(ctx, buyer, a.ID, r); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	written := func(agentID string) (licences, billed int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM market_licences WHERE agent_id = $1),
				(SELECT count(*) FROM market_uses WHERE agent_id = $1 AND charge = 'billed')`, agentID).Scan(&licences, &billed); err != nil {
			t.Fatal(err)
		}
		return licences, billed
	}
	twenty := int64(20_000_000)
	buyer20 := agentWith("buyer", economy.AgentRules{MaxCommitmentULXC: &twenty})

	rent, _, err := s.License(ctx, deps, buyer, buyer20, l.ID, "rent-15", LicenceRequest{OfferID: offer["rent commercial"]})
	if err != nil || rent.Charge != ChargeBilled || rent.PriceULXC != 15_000_000 {
		t.Fatalf("a 15 LXC rent under a 20 LXC commitment = %+v, %v; want it billed at 15,000,000 µLXC", rent, err)
	}
	if licences, billed := written(buyer20); licences != 1 || billed != 1 {
		t.Fatalf("after the rent the agent has %d licences and %d billed rows; want 1 and 1", licences, billed)
	}

	_, _, err = s.License(ctx, deps, buyer, buyer20, l.ID, "rent-25", LicenceRequest{OfferID: offer["rent enterprise"]})
	if !errors.Is(err, economy.ErrAgentRule) || !strings.Contains(err.Error(), "the most one licence may commit it to is $2.00 (20 LXC)") {
		t.Fatalf("a 25 LXC rent under a 20 LXC commitment = %v; want refused naming the commitment", err)
	}
	if licences, billed := written(buyer20); licences != 1 || billed != 1 {
		t.Fatalf("after the refused rent the agent has %d licences and %d billed rows; want still 1 and 1", licences, billed)
	}

	_, _, err = s.License(ctx, deps, buyer, buyer20, l.ID, "sub", LicenceRequest{OfferID: offer["subscribe commercial"]})
	if !errors.Is(err, economy.ErrAgentRule) || !strings.Contains(err.Error(), "do not let it subscribe") {
		t.Fatalf("a subscription by an agent whose rules do not allow one = %v; want refused saying so", err)
	}
	renews := true
	_, _, err = s.License(ctx, deps, buyer, buyer20, l.ID, "rent-renews", LicenceRequest{OfferID: offer["rent commercial"], AutoRenew: &renews})
	if !errors.Is(err, economy.ErrAgentRule) || !strings.Contains(err.Error(), "renews itself") {
		t.Fatalf("a rent that renews itself, by an agent that may not subscribe = %v; want refused saying so", err)
	}
	if licences, billed := written(buyer20); licences != 1 || billed != 1 {
		t.Fatalf("after the refused subscriptions the agent has %d licences and %d billed rows; want still 1 and 1", licences, billed)
	}
	yes := true
	if _, err := bank.SetAgentRules(ctx, buyer, buyer20, economy.AgentRules{MaySubscribe: &yes}); err != nil {
		t.Fatal(err)
	}
	if rules, err := bank.GetAgentRules(ctx, buyer, buyer20); err != nil || *rules.MaxCommitmentULXC != twenty || !*rules.MaySubscribe {
		t.Fatalf("the rules after allowing subscriptions = %+v, %v; want may_subscribe on and the commitment kept", rules, err)
	}
	if sub, _, err := s.License(ctx, deps, buyer, buyer20, l.ID, "sub", LicenceRequest{OfferID: offer["subscribe commercial"]}); err != nil || sub.Kind != OfferSubscribe {
		t.Fatalf("a subscription once its rules allow one = %+v, %v; want it taken", sub, err)
	}

	ten := int64(10_000_000)
	asks := agentWith("asks", economy.AgentRules{MaxCommitmentULXC: &twenty, ApprovalAboveULXC: ten})
	_, _, err = s.License(ctx, deps, buyer, asks, l.ID, "asks-1", LicenceRequest{OfferID: offer["rent commercial"]})
	var need *economy.ApprovalNeededError
	if !errors.As(err, &need) {
		t.Fatalf("a 15 LXC rent above a 10 LXC approval amount = %v; want an approval filed", err)
	}
	if licences, billed := written(asks); licences != 0 || billed != 0 {
		t.Fatalf("while it waits the agent has %d licences and %d billed rows; want none", licences, billed)
	}
	if _, err := bank.DecideAgentApproval(ctx, buyer, need.ApprovalID, true); err != nil {
		t.Fatal(err)
	}
	if lic, _, err := s.License(ctx, deps, buyer, asks, l.ID, "asks-1", LicenceRequest{OfferID: offer["rent commercial"]}); err != nil || lic.Charge != ChargeBilled {
		t.Fatalf("the approved rent = %+v, %v; want it billed", lic, err)
	}
	if _, _, err := s.License(ctx, deps, buyer, asks, l.ID, "asks-2", LicenceRequest{OfferID: offer["rent commercial"]}); !errors.As(err, &need) {
		t.Fatalf("a second rent on the one approval = %v; want it to need another", err)
	}
	if licences, billed := written(asks); licences != 1 || billed != 1 {
		t.Fatalf("after the approval the agent has %d licences and %d billed rows; want exactly 1 and 1", licences, billed)
	}

	sim, err := bank.SimulateAgentRules(ctx, buyer, buyer20, economy.SimulatedRequest{AmountULXC: 25_000_000,
		Payee: &economy.Payee{Kind: "listing", ID: l.ID}, Licence: &economy.Commitment{Kind: OfferRent, Licence: LicenceEnterprise}})
	if err != nil || sim.Verdict != "refused" || !strings.Contains(sim.Reason, "the most one licence may commit it to is $2.00 (20 LXC)") {
		t.Fatalf("simulating the 25 LXC rent = %+v, %v; want refused naming the commitment", sim, err)
	}
}
