package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
)

// B50.54 — a conversion screens its partner postings on a connection of its own before its transaction begins, so
// as many conversions at once as the pool has connections still finish: none holds one connection while it waits for
// a second.
func TestB5054_EightConversionsAtOnceOnAFourConnectionPoolAllFinish(t *testing.T) {
	ctx := context.Background()
	cfg := agentRoutesDB(t).Config()
	cfg.MaxConns = 4 // CI's size
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const ws = "ws-b5054-pool"
	store := allowanceStore(pool)
	confirmedAccounts(t, pool, ws)
	allowanceWorkspace(t, pool, ws, "business", "business_company")
	allowanceAccount(t, pool, store, ws, "", "GBP", 0)
	allowanceAccount(t, pool, store, ws, "", "USD", 0)
	type racer struct{ agent, quote string }
	var racers []racer
	for i := range 8 {
		agent, err := store.CreateAgent(ctx, ws, fmt.Sprintf("Converter %d", i), "owner")
		if err != nil {
			t.Fatal(err)
		}
		allowanceAccount(t, pool, store, ws, agent.ID, "GBP", 10_00)
		allowanceAccount(t, pool, store, ws, agent.ID, "USD", 0)
		q, err := store.QuoteConversion(ctx, ws, agent.ID, economy.Conversion{SellMinor: 10_00, SellCurrency: "GBP", BuyCurrency: "USD"})
		if err != nil {
			t.Fatalf("converter %d's quote: %v", i, err)
		}
		racers = append(racers, racer{agent.ID, q.ID})
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, len(racers))
	start := make(chan struct{})
	for i, r := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = store.Convert(cctx, ws, r.agent, r.quote, "b5054-"+r.quote)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("converter %d: %v; want converted", i, err)
		}
	}
	for _, r := range racers {
		var entries int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'fx_conversion' AND idempotency_key = $2`,
			ws, "fx_convert:"+r.quote).Scan(&entries); err != nil {
			t.Fatal(err)
		}
		if entries != 1 {
			t.Errorf("quote %s posted %d times on the ledger; want once", r.quote, entries)
		}
	}
}
