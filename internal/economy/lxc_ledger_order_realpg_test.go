package economy

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// B58.4: the history lists rows in the order their balances were written. created_at is the transaction's
// start, but balance_after is written under the lxc_balances row lock, in commit order, so a transaction
// that starts first and locks second lists below a row whose balance it built on.

// requireBalanceChain fails unless every row's balance_after is the next older row's plus its own amount,
// and the oldest row's is its own amount (the workspace started at zero).
func requireBalanceChain(t *testing.T, h []LXCLedgerEntry, wantRows int) {
	t.Helper()
	if len(h) != wantRows {
		t.Fatalf("history: got %d rows, want %d", len(h), wantRows)
	}
	for i := 0; i < len(h)-1; i++ {
		if h[i].BalanceAfter != h[i+1].BalanceAfter+h[i].Amount {
			t.Fatalf("row %d reads %d after %d, but the row below ends on %d", i, h[i].BalanceAfter, h[i].Amount, h[i+1].BalanceAfter)
		}
	}
	if last := h[len(h)-1]; last.BalanceAfter != last.Amount {
		t.Fatalf("oldest row reads %d after %d, want it to start from zero", last.BalanceAfter, last.Amount)
	}
}

// spendStartedEarly is SpendLXC's write, in a transaction begun before start() returns and locked only
// after proceed is closed: the earlier start, the later commit.
func spendStartedEarly(ctx context.Context, dt *DualTokenStore, ws string, amount int64, started chan<- struct{}, proceed <-chan struct{}) error {
	tx, err := dt.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT now()`); err != nil {
		return err
	}
	close(started)
	<-proceed
	bal, minted, spent, err := readLXCBalance(ctx, tx, ws)
	if err != nil {
		return err
	}
	if err := insertLXCLedger(ctx, tx, ws, -amount, bal-amount, LXCTypeSpend, "early", nil); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, ws, bal-amount, minted, spent+amount); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestLXCHistory_OverlappingSpendsListInBalanceOrder(t *testing.T) {
	ctx := context.Background()
	dt := NewDualTokenStore(nil, lxcHistPool(t), nil)
	const ws = "ws-b584-overlap"
	if _, err := dt.GrantLXC(ctx, ws, 1_000_000_000, "seed", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}

	started, proceed := make(chan struct{}), make(chan struct{})
	errc := make(chan error, 1)
	go func() { errc <- spendStartedEarly(ctx, dt, ws, 20_130, started, proceed) }()
	<-started
	time.Sleep(20 * time.Millisecond) // the second spend's transaction starts measurably later
	if err := dt.SpendLXC(ctx, ws, 40_000, "late"); err != nil {
		t.Fatalf("second spend: %v", err)
	}
	close(proceed)
	if err := <-errc; err != nil {
		t.Fatalf("first spend: %v", err)
	}

	h, err := dt.GetLXCHistory(ctx, ws, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	requireBalanceChain(t, h, 3)
	if h[0].Description != "early" {
		t.Fatalf("newest row is %q; the spend that locked last must list first", h[0].Description)
	}
}

func TestLXCHistory_TwentyConcurrentWritersKeepTheChain(t *testing.T) {
	ctx := context.Background()
	dt := NewDualTokenStore(nil, lxcHistPool(t), nil)
	const ws = "ws-b584-twenty"
	if _, err := dt.GrantLXC(ctx, ws, 1_000_000_000, "seed", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}

	const writers, each = 20, 5
	var wg sync.WaitGroup
	errc := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				amount := int64(1_000 + w*each + i)
				if (w+i)%2 == 0 {
					errc <- dt.SpendLXC(ctx, ws, amount, fmt.Sprintf("w%d-%d", w, i))
					continue
				}
				started, proceed := make(chan struct{}), make(chan struct{})
				go func() { <-started; time.Sleep(time.Duration(w%4) * 3 * time.Millisecond); close(proceed) }()
				errc <- spendStartedEarly(ctx, dt, ws, amount, started, proceed)
			}
		}(w)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatalf("spend: %v", err)
		}
	}

	h, err := dt.GetLXCHistory(ctx, ws, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	requireBalanceChain(t, h, writers*each+1)
}
