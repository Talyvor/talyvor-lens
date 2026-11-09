package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

// B55.31 — an agent's licence, prize and renewal above its approval amount file an approval that reads as a sentence,
// under the fingerprint it had before, so an approval waiting from then still matches its retry. Approved, a licence
// and a prize go through once.
func TestAgentPurchaseMemos_ALicencePrizeAndRenewalReadInWords(t *testing.T) {
	ctx := context.Background()
	const buyer = "ws-b5531-buyer"
	pool, s, bank, listing, offer := renewalsMarket(t, buyer)
	deps := LicenceDeps{Meter: &stripeMeter{tried: map[string]int{}}, Agents: bank, Capabilities: bank}
	yes := true
	agent, err := bank.CreateAgent(ctx, buyer, "researcher", "owner-"+buyer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MaySubscribe: &yes, ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	filed := func(what, wantMemo string) string {
		t.Helper()
		fp := sha256.Sum256([]byte("purchase\x00" + agent.ID + "\x00" + what))
		var id, memo string
		if err := pool.QueryRow(ctx, `SELECT id, memo FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2`,
			agent.ID, hex.EncodeToString(fp[:])).Scan(&id, &memo); err != nil {
			t.Fatalf("no approval under the fingerprint of %q: %v", what, err)
		}
		if memo != wantMemo {
			t.Errorf("the approval's memo = %q, want %q", memo, wantMemo)
		}
		return id
	}
	approve := func(id string) {
		t.Helper()
		if _, err := bank.DecideAgentApproval(ctx, buyer, id, true); err != nil {
			t.Fatal(err)
		}
	}
	var need *economy.ApprovalNeededError

	// A rent: 1,000,000 µUSD is 10 LXC.
	rent := LicenceRequest{OfferID: offer[OfferRent]}
	if _, _, err := s.License(ctx, deps, buyer, agent.ID, listing, "rent", rent); !errors.As(err, &need) {
		t.Fatalf("a rent above the approval amount = %v; want an approval filed", err)
	}
	approve(filed(fmt.Sprintf("licence:%s:%s:%s:10000000", agent.ID, listing, offer[OfferRent]),
		"Rent Adder, commercial licence, $1.00 (10 LXC) on your monthly marketplace bill"))
	if lic, _, err := s.License(ctx, deps, buyer, agent.ID, listing, "rent", rent); err != nil || lic.Charge != ChargeBilled {
		t.Fatalf("the approved rent = %+v, %v; want it billed", lic, err)
	}
	if _, _, err := s.License(ctx, deps, buyer, agent.ID, listing, "rent-2", rent); !errors.As(err, &need) {
		t.Fatalf("a second rent on the one approval = %v; want it to need another", err)
	}

	// A prize of $2.00.
	award := PrizeAward{ListingID: listing, Version: 1, AmountUSDMicros: 2_000_000}
	if _, err := s.AwardPrize(ctx, deps, buyer, agent.ID, award, nil); !errors.As(err, &need) {
		t.Fatalf("a prize above the approval amount = %v; want an approval filed", err)
	}
	approve(filed(fmt.Sprintf("prize:%s:%s:1:20000000", agent.ID, listing),
		"A prize for Adder, $2.00 (20 LXC) on your monthly marketplace bill"))
	if lic, err := s.AwardPrize(ctx, deps, buyer, agent.ID, award, nil); err != nil || lic.Charge != ChargeBilled {
		t.Fatalf("the approved prize = %+v, %v; want it billed", lic, err)
	}
	if _, err := s.AwardPrize(ctx, deps, buyer, agent.ID, award, nil); !errors.As(err, &need) {
		t.Fatalf("a second prize on the one approval = %v; want it to need another", err)
	}

	// A subscription's renewal: 400,000 µUSD is 4 LXC. Its first period is taken under no approval amount.
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MaySubscribe: &yes}); err != nil {
		t.Fatal(err)
	}
	sub, _, err := s.License(ctx, deps, buyer, agent.ID, listing, "sub", LicenceRequest{OfferID: offer[OfferSubscribe]})
	if err != nil || sub.EndsAt == nil {
		t.Fatalf("the subscription = %+v, %v", sub, err)
	}
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MaySubscribe: &yes, ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := bank.RunAgentSchedules(ctx, sub.EndsAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	filed(fmt.Sprintf("renewal:%s:%s:4000000", sub.ID, sub.EndsAt.UTC().Format(time.RFC3339)),
		"Adder renews: $0.40 (4 LXC) on your monthly marketplace bill")
}
