package market

import (
	"context"
	"testing"
	"time"
)

// B41.176 — a subscription nobody used for 30 days is marked idle on the licences list and the schedules' tick alerts
// its buyer once, with its licence to cancel; a subscription that was used is neither. A cancel clears the mark.
func TestIdleLicences_AnUnusedSubscriptionAlertsOnceAndAUsedOneNone(t *testing.T) {
	ctx := context.Background()
	const idleBuyer, usedBuyer = "ws-b41176-idle", "ws-b41176-used"
	pool, s, bank, listing, offer := renewalsMarket(t, idleBuyer)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, usedBuyer); err != nil {
		t.Fatal(err)
	}
	meter := &stripeMeter{tried: map[string]int{}}
	base := s.now().UTC().Add(-40 * day).Truncate(time.Second)
	at := func(d time.Duration) { s.clock = func() time.Time { return base.Add(d) } }
	at(0)
	sub := map[string]string{}
	for _, buyer := range []string{idleBuyer, usedBuyer} {
		lic, _, err := s.License(ctx, LicenceDeps{Meter: meter, Capabilities: &askedCapabilities{}}, buyer, "", listing, "sub", LicenceRequest{OfferID: offer[OfferSubscribe]})
		if err != nil || !lic.AutoRenew {
			t.Fatalf("subscribe %s = %+v, %v; want a licence that renews", buyer, lic, err)
		}
		sub[buyer] = lic.ID
	}
	at(20 * day)
	if _, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, usedBuyer, "", listing, UseRequest{Variables: map[string]string{"a": "1", "b": "2"}}); err != nil {
		t.Fatal(err)
	}

	for i, want := range []int{1, 0} { // the second tick tells nobody again
		res, err := bank.RunAgentSchedules(ctx, base.Add(31*day+time.Duration(i)*time.Minute))
		if err != nil || res.IdleAlerted != want {
			t.Fatalf("tick %d = %+v, %v; want %d idle licence alerted", i+1, res, err, want)
		}
	}
	var alerts []string
	rows, err := pool.Query(ctx, `SELECT licence_id FROM market_licence_idle_alerts`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		alerts = append(alerts, id)
	}
	if len(alerts) != 1 || alerts[0] != sub[idleBuyer] {
		t.Fatalf("idle alerts = %v; want one, for the unused subscription %s", alerts, sub[idleBuyer])
	}

	at(31 * day)
	isIdle := func(buyer string) bool {
		t.Helper()
		list, err := s.Licences(ctx, buyer)
		if err != nil || len(list) != 1 || list[0].ID != sub[buyer] {
			t.Fatalf("%s's licences = %+v, %v; want its subscription", buyer, list, err)
		}
		return list[0].Idle
	}
	if !isIdle(idleBuyer) || isIdle(usedBuyer) {
		t.Fatalf("idle: unused %v, used %v; want only the unused subscription idle", isIdle(idleBuyer), isIdle(usedBuyer))
	}
	if _, err := s.CancelLicence(ctx, idleBuyer, sub[idleBuyer]); err != nil {
		t.Fatal(err)
	}
	if isIdle(idleBuyer) {
		t.Fatal("a cancelled subscription is still idle; want the mark gone once it no longer renews")
	}
}
