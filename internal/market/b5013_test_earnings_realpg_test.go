package market

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type creditsTaken struct{ ulxc int64 }

func (c *creditsTaken) CreditLXCTx(_ context.Context, _ pgx.Tx, _ string, amount int64, _ string, _ map[string]interface{}) (int64, error) {
	c.ulxc += amount
	return 0, nil
}

// B50.13 — earnings from uses billed in Stripe test mode never become credits a real seller can spend: it takes
// only its live earnings, once. A test (synthetic) seller takes its test earnings as before.
func TestB5013_ARealSellerNeverTakesTestModeEarningsAsCredits(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const real, test, buyer = "ws-b5013-seller", "s-b5013-seller", "s-b5013-buyer"
	for _, ws := range []struct {
		id        string
		synthetic bool
	}{{real, false}, {test, true}, {buyer, true}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`, ws.id, ws.synthetic); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	paid := now.Add(-30 * 24 * time.Hour)
	billedUse(t, pool, "use_b5013_testmode", real, buyer, 100_000_000, paid.Add(-time.Hour)) // $10: $8.50 to the seller, in test mode
	billedUse(t, pool, "use_b5013_synth", test, buyer, 100_000_000, paid.Add(-time.Hour))
	if _, err := s.ClearInvoice(ctx, buyer, "in_b5013_test", paid.Add(-2*time.Hour), paid, paid, false); err != nil {
		t.Fatal(err)
	}
	taken := func(seller string) (rows int, gross int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(gross_usd_micros), 0) FROM market_payouts WHERE workspace_id = $1 AND method = 'credits'`,
			seller).Scan(&rows, &gross); err != nil {
			t.Fatal(err)
		}
		return
	}

	c := &creditsTaken{}
	if _, err := s.TakeAsCredits(ctx, c, real, now); !errors.Is(err, ErrNothingAvailable) {
		t.Fatalf("a real seller with only test-mode earnings took them as credits: %v; want ErrNothingAvailable", err)
	}
	if n, gross := taken(real); n != 0 || c.ulxc != 0 {
		t.Fatalf("a real seller's test-mode earnings became %d payout row(s) of %d µUSD and %d µLXC of credits; want none", n, gross, c.ulxc)
	}

	billedUse(t, pool, "use_b5013_live", real, "ws-b5013-livebuyer", 20_000_000, paid.Add(-time.Hour)) // $2: $1.70, paid live
	if _, err := s.ClearInvoice(ctx, "ws-b5013-livebuyer", "in_b5013_live", paid.Add(-2*time.Hour), paid, paid, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeAsCredits(ctx, c, real, now); err != nil {
		t.Fatal(err)
	}
	if n, gross := taken(real); n != 1 || gross != 1_700_000 || c.ulxc != 1_700_000*ulxcPerUSDMicro {
		t.Fatalf("a real seller took %d payout row(s) of %d µUSD and %d µLXC; want one of its live $1.70 only", n, gross, c.ulxc)
	}
	if _, err := s.TakeAsCredits(ctx, c, real, now); !errors.Is(err, ErrNothingAvailable) {
		t.Fatalf("a real seller took credits a second time: %v; want ErrNothingAvailable", err)
	}

	ct := &creditsTaken{}
	if _, err := s.TakeAsCredits(ctx, ct, test, now); err != nil {
		t.Fatal(err)
	}
	if n, gross := taken(test); n != 1 || gross != 8_500_000 || ct.ulxc != 8_500_000*ulxcPerUSDMicro {
		t.Fatalf("a test seller took %d payout row(s) of %d µUSD and %d µLXC; want its test-mode $8.50", n, gross, ct.ulxc)
	}
}
