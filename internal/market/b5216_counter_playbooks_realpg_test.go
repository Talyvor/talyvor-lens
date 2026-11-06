package market

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// b5216Playbook is a renewal playbook whose counter wording is counter.
func b5216Playbook(rounds int, counter string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"deal_type": "renewal", "rounds": rounds, "open_pct": 40, "limit_pct": 80, "wording": map[string]string{
		"ask": "Hi. We would like your best price for {what}.", "counter": counter, "walk_away": "Thanks for your time on {what}. We will leave it there."}})
	return b
}

// B52.16 — a playbook that fails Counter's gym is never listed, as a listing or as a new version; the listed ones rank
// by gym score; and running one on a negotiation is one billed use however often it is asked, whose clearing credits
// its creator the seller's share once.
func TestB5216_CounterPlaybooks(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const creator, rival, buyer = "ws-b5216-creator", "ws-b5216-rival", "ws-b5216-buyer"
	clean := "Thanks. For {what} we can do {offer}."

	// Its counter wording names a rival offer no owner allowed: every counter it words would be refused.
	_, err := s.Publish(ctx, creator, Draft{Kind: KindCounterPlaybook, Title: "Rival", Visibility: "public", PricePerUseULXC: 10_000_000,
		Artifact: b5216Playbook(3, "Thanks. Another supplier is cheaper, so we offer {offer}.")})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "gym") {
		t.Fatalf("publishing a playbook that fails the gym = %v; want it refused for the gym", err)
	}
	var listed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_listings WHERE kind = 'counter_playbook'`).Scan(&listed); err != nil || listed != 0 {
		t.Fatalf("%d playbook listings after the refusal (%v); want none", listed, err)
	}

	l, err := s.Publish(ctx, creator, Draft{Kind: KindCounterPlaybook, Title: "Steady", Visibility: "public", PricePerUseULXC: 10_000_000,
		Artifact: b5216Playbook(3, clean)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, rival, Draft{Kind: KindCounterPlaybook, Title: "One shot", Visibility: "public", Artifact: b5216Playbook(1, clean)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishVersion(ctx, creator, l.ID, b5216Playbook(3, "Thanks. Another supplier is cheaper, so we offer {offer}."), "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a new version that fails the gym = %v; want it refused", err)
	}
	ranked, err := s.CounterPlaybooks(ctx, "renewal")
	if err != nil || len(ranked) != 2 {
		t.Fatalf("the ranked playbooks = %+v, %v; want both", ranked, err)
	}
	if ranked[0].Gym.ScoreBPS < ranked[1].Gym.ScoreBPS || ranked[0].Gym.Negotiations == 0 || ranked[0].GymVersion != 1 || ranked[0].Wording.Counter != clean {
		t.Fatalf("the ranking = %+v; want the better gym score first, scored on the listed version 1", ranked)
	}
	if none, _ := s.CounterPlaybooks(ctx, "purchase"); len(none) != 0 {
		t.Fatalf("purchase playbooks = %+v; want none", none)
	}

	// A playbook is run through a negotiation alone; the marketplace's own use route cannot run one.
	meter := &b501Meter{}
	if _, err := s.Use(ctx, UseDeps{Meter: meter}, buyer, "", l.ID, UseRequest{}); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("a direct use of a playbook = %v; want it not runnable", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO negotiation_mandates (id, workspace_id, deadline, canonical, credential_id, signature, created_by)
		VALUES ('neg_b5216', $1, now() + interval '7 days', '{"task": {"kind": "renewal", "what": "Acme CRM"}}', 'cred', 'sig', 'owner')`, buyer); err != nil {
		t.Fatal(err)
	}
	cheap := int64(999_999)
	if _, err := s.RunCounterPlaybook(ctx, UseDeps{Meter: meter}, buyer, "neg_b5216", l.ID, &cheap); !errors.Is(err, ErrOverMaxPrice) {
		t.Fatalf("a run above the owner's most = %v; want it refused", err)
	}
	first, err := s.RunCounterPlaybook(ctx, UseDeps{Meter: meter}, buyer, "neg_b5216", l.ID, nil)
	if err != nil || first.Again || first.Charge != ChargeBilled || first.PriceULXC != 10_000_000 {
		t.Fatalf("the first run = %+v, %v; want one billed use at the listing's price", first, err)
	}
	again, err := s.RunCounterPlaybook(ctx, UseDeps{Meter: meter}, buyer, "neg_b5216", l.ID, nil)
	if err != nil || !again.Again || again.UseID != first.UseID {
		t.Fatalf("running it again = %+v, %v; want the same run, charged nothing more", again, err)
	}
	var uses int
	var bound string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM market_uses WHERE listing_id = $1), playbook->>'deal_type'
		FROM negotiation_playbook_runs WHERE mandate_id = 'neg_b5216'`, l.ID).Scan(&uses, &bound); err != nil || uses != 1 || bound != "renewal" {
		t.Fatalf("%d uses of the playbook, bound %q (%v); want one use and the playbook on the negotiation", uses, bound, err)
	}
	if len(meter.accepted) != 1 || meter.accepted[0] != first.UseID {
		t.Fatalf("metered %v; want the one use %s", meter.accepted, first.UseID)
	}

	paid := time.Now().UTC().Add(time.Minute)
	for range 2 { // a replayed invoice clears nothing more
		if _, err := s.ClearInvoice(ctx, buyer, "in_b5216", paid.Add(-time.Hour), paid, paid, true); err != nil {
			t.Fatal(err)
		}
	}
	j, err := s.JournalFor(ctx, first.UseID)
	if err != nil {
		t.Fatal(err)
	}
	want := []Posting{{AccountStripeClearing, 1_000_000, "USD", "live"}, {AccountMarketFee, -150_000, "USD", "live"}, {SellerHoldback(creator), -850_000, "USD", "live"}}
	if len(j) != 1 || j[0].Kind != JournalClear || !reflect.DeepEqual(j[0].Postings, want) {
		t.Fatalf("the run's journal = %+v; want one clear entry %v", j, want)
	}
	if got := journalBalance(t, s, SellerHoldback(creator)); got != -850_000 {
		t.Fatalf("the creator's holdback = %d; want -850000, credited once", got)
	}
}
