package market

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// failEvery answers, but fails every nth run.
type failEvery struct {
	n    int64
	runs atomic.Int64
}

func (f *failEvery) Run(context.Context, string, []Message) (string, error) {
	if f.runs.Add(1)%f.n == 0 {
		return "", errors.New("model down")
	}
	return "3", nil
}

// B41.173 — a listing with 100 uses, 5 of them failed, shows 95% success on its trust panel, computed from the use
// records, with its median latency and cost per use; the seller's own failing uses do not count.
func TestPerformance_SuccessRateFromUseRecords(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, buyer = "ws-b41173-seller", "ws-b41173-buyer"
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)
	l, err := s.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`),
		Offers:   []Offer{{Kind: OfferPerUse, Licence: LicenceCommercial, PriceUSDMicros: 40_000}}})
	if err != nil {
		t.Fatal(err)
	}
	add := UseRequest{Variables: map[string]string{"a": "1", "b": "2"}}

	tr, err := s.Trust(ctx, buyer, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p := tr.Performance; p.Uses != 0 || p.SuccessRate != nil || p.MedianLatencyMS != nil || p.CostPerUseUSDMicros != nil {
		t.Fatalf("a listing nobody used = %+v; want no uses and null figures", p)
	}

	deps := UseDeps{Runner: &failEvery{n: 20}, Meter: &meterCalls{}}
	failed := 0
	for range 100 {
		if _, err := s.Use(ctx, deps, buyer, "", l.ID, add); err != nil {
			failed++
		}
	}
	// The seller trying its own listing while it is down: never in its record.
	for range 3 {
		if _, err := s.Use(ctx, UseDeps{Runner: &failEvery{n: 1}}, seller, "", l.ID, add); err == nil {
			t.Fatal("a run that fails answered")
		}
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE listing_id = $1`, l.ID).Scan(&kept); err != nil || failed != 5 || kept != 95 {
		t.Fatalf("uses = %d failed, %d kept (%v); want 5 failed and their use rows removed", failed, kept, err)
	}

	if tr, err = s.Trust(ctx, buyer, l.ID); err != nil {
		t.Fatal(err)
	}
	p := tr.Performance
	if p.Days != 30 || p.Uses != 100 || p.Failed != 5 || p.SuccessRate == nil || *p.SuccessRate != 0.95 {
		t.Fatalf("performance = %+v; want 100 uses, 5 failed, 0.95", p)
	}
	if p.MedianLatencyMS == nil || p.CostPerUseUSDMicros == nil || *p.CostPerUseUSDMicros != 40_000 {
		t.Fatalf("median latency %v, cost per use %v; want a latency and 40000 µUSD", p.MedianLatencyMS, p.CostPerUseUSDMicros)
	}
	if b, _ := json.Marshal(tr); !strings.Contains(string(b), `"performance":{"days":30,"uses":100,"failed":5,"success_rate":0.95,`) {
		t.Fatalf("GET …/trust gives %s; want its performance", b)
	}

	// Older than 30 days drops out.
	if _, err := pool.Exec(ctx, `UPDATE market_use_outcomes SET ran_at = now() - interval '31 days' WHERE listing_id = $1`, l.ID); err != nil {
		t.Fatal(err)
	}
	if tr, err = s.Trust(ctx, buyer, l.ID); err != nil || tr.Performance.Uses != 0 {
		t.Fatalf("performance after 31 days = %+v, %v; want no uses", tr.Performance, err)
	}
}
