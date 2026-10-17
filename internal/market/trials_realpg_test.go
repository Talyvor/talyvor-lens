package market

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

// B32.21 — with trial_uses 3, three uses write 'trial' rows, three test-funded journal entries and no meter event, and
// the fourth is billed; a second workspace sharing the first's card gets no trial; no trial row ever has an earning. An
// offer cannot give more than the trial max. A pipeline's trial runs another seller's step as a trial of that listing,
// and answers trial: true.
func TestTrials_ThreeFreeUsesOnTestMoneyThenBilled(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, buyer, twin, tool = "ws-b3221-seller", "ws-b3221-buyer", "ws-b3221-twin", "ws-b3221-tool"
	for _, ws := range []string{seller, buyer, twin, tool} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	for _, ws := range []string{buyer, twin} { // one card, two workspaces: one owner
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'card-b3221')`, ws); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)
	draft := Draft{Kind: "prompt", Title: "Adder", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`),
		Offers:   []Offer{{Kind: OfferPerUse, Licence: LicenceCommercial, PriceUSDMicros: 40_000, TrialUses: 6}}}
	if _, err := s.Publish(ctx, seller, draft); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an offer of 6 trial uses = %v; want refused above the default max of %d", err, DefaultTrialMax)
	}
	draft.Offers[0].TrialUses = 3
	l, err := s.Publish(ctx, seller, draft)
	if err != nil {
		t.Fatal(err)
	}
	meter := &meterCalls{}
	deps := UseDeps{Runner: answers("3"), Meter: meter}
	add := UseRequest{Variables: map[string]string{"a": "1", "b": "2"}}
	row := func(useID string) (charge, kind string, price int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT charge, use_kind, price_ulxc FROM market_uses WHERE id = $1`, useID).Scan(&charge, &kind, &price); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Three trial uses: 'trial' rows at 0, never metered, each journalled once on test money at the would-be split.
	var trials []string
	for i := range 3 {
		u, err := s.Use(ctx, deps, buyer, "", l.ID, add)
		if err != nil {
			t.Fatal(err)
		}
		if c, k, p := row(u.ID); c != ChargeTrial || k != "trial" || p != 0 || !u.Trial || u.Output != "3" {
			t.Fatalf("trial use %d = %s %s at %d, answered %+v; want a 'trial' row at 0 answering trial: true", i+1, c, k, p, u)
		}
		if u.TrialUsesLeft == nil || *u.TrialUsesLeft != 2-i || u.WouldHaveCostUSDMicros != 40_000 {
			t.Fatalf("trial use %d says %v left, would have cost %d µUSD; want %d left and 40,000", i+1, u.TrialUsesLeft, u.WouldHaveCostUSDMicros, 2-i)
		}
		entries, err := s.JournalFor(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := []Posting{{AccountTrialWaived, 40_000, "USD", "test"}, {AccountTrialFee, -6_000, "USD", "test"}, {TrialSeller(seller), -34_000, "USD", "test"}}
		if len(entries) != 1 || entries[0].Kind != JournalTrial || entries[0].Funding != "test" || !slices.Equal(entries[0].Postings, want) {
			t.Fatalf("trial use %d's journal = %+v; want one test-funded trial entry %+v", i+1, entries, want)
		}
		trials = append(trials, u.ID)
	}
	if len(*meter) != 0 {
		t.Fatalf("metered %v; want no trial use metered", *meter)
	}

	// The fourth is billed, and metered.
	fourth, err := s.Use(ctx, deps, buyer, "", l.ID, add)
	if err != nil {
		t.Fatal(err)
	}
	if c, _, p := row(fourth.ID); c != ChargeBilled || p != 400_000 || fourth.Trial || !slices.Equal(*meter, []string{fourth.ID}) {
		t.Fatalf("the fourth use = %s at %d (trial %v), metered %v; want billed at 400,000 µLXC and metered", c, p, fourth.Trial, *meter)
	}

	// A workspace sharing the buyer's card is the same owner: its first use is billed.
	other, err := s.Use(ctx, deps, twin, "", l.ID, add)
	if err != nil {
		t.Fatal(err)
	}
	if c, _, _ := row(other.ID); c != ChargeBilled || other.Trial {
		t.Fatalf("the card-sharing workspace's first use = %s (trial %v); want billed: its owner's trials are spent", c, other.Trial)
	}

	// A pipeline's trial runs another seller's step as a trial of that listing; both are journalled, neither billed.
	tl, err := s.Publish(ctx, tool, Draft{Kind: "prompt", Title: "Shout", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"shout {{text}}","model":"claude-haiku-4-5"}`),
		Offers:   []Offer{{Kind: OfferPerUse, Licence: LicenceCommercial, PriceUSDMicros: 10_000, TrialUses: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	pl, err := s.Publish(ctx, seller, Draft{Kind: "pipeline", Title: "Add and shout", Visibility: "public",
		Artifact: json.RawMessage(`{"steps":[{"use":"listing:` + l.ID + `"},{"use":"listing:` + tl.ID + `"}],"model":"claude-haiku-4-5"}`),
		Offers:   []Offer{{Kind: OfferPerUse, Licence: LicenceCommercial, PriceUSDMicros: 20_000, TrialUses: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	stranger := "ws-b3221-stranger"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, stranger); err != nil {
		t.Fatal(err)
	}
	metered := len(*meter)
	pu, err := s.Use(ctx, deps, stranger, "", pl.ID, add)
	if err != nil {
		t.Fatal(err)
	}
	if c, _, _ := row(pu.ID); c != ChargeTrial || !pu.Trial || len(pu.Steps) != 2 || pu.Steps[1].Charge != ChargeTrial || pu.WouldHaveCostUSDMicros != 30_000 {
		t.Fatalf("the pipeline's trial = %s, %+v; want a trial whose other seller's step is a trial too, would have cost 30,000 µUSD", c, pu)
	}
	if c, k, _ := row(pu.Steps[1].UseID); c != ChargeTrial || k != "trial" || len(*meter) != metered {
		t.Fatalf("the other seller's step = %s %s, metered %v; want a trial use, nothing metered", c, k, (*meter)[metered:])
	}
	trials = append(trials, pu.ID, pu.Steps[1].UseID)
	// Its trial of the step listing is spent: the next pipeline use is no trial, and bills the step.
	pu2, err := s.Use(ctx, deps, stranger, "", pl.ID, add)
	if err != nil {
		t.Fatal(err)
	}
	if c, _, _ := row(pu2.ID); c != ChargeBilled || pu2.Trial || pu2.Steps[1].Charge != ChargeBilled {
		t.Fatalf("the second pipeline use = %s, %+v; want billed, step and all", c, pu2)
	}

	// The buyer's invoice is paid, as the testers pay it, receipt and all (B17.153: a receipt that failed to insert
	// behind PgBouncer answered 500): the billed use earns, no trial does — ever.
	s.SetReceipts(Receipts{Supplier: Supplier{LegalName: "Talyvor Ltd"}})
	if _, n, err := s.PayTestBill(ctx, buyer, time.Now()); err != nil || n != 1 {
		t.Fatalf("paying the buyer's bill = %d uses, %v; want the one billed use cleared", n, err)
	}
	var earned []string
	rows, err := pool.Query(ctx, `SELECT use_id FROM market_earnings ORDER BY use_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		earned = append(earned, id)
	}
	if !slices.Equal(earned, []string{fourth.ID}) {
		t.Fatalf("earnings for %v; want only the billed fourth use %s (trials %v never earn)", earned, fourth.ID, trials)
	}
	var trialEarnings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_earnings e JOIN market_uses u ON u.id = e.use_id WHERE u.charge = 'trial'`).Scan(&trialEarnings); err != nil || trialEarnings != 0 {
		t.Fatalf("trial rows with an earning = %d, %v; want none", trialEarnings, err)
	}
}
