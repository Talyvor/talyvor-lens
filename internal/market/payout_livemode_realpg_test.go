package market

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/testschema"
	"github.com/talyvor/lens/migrations"
)

// migratedDB is a fresh database with every migration applied.
func migratedDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_market_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	// B61.52: the accounts these tests move money for have confirmed who they are.
	if _, err := mc.Exec(ctx, testschema.ConfirmedAccountsSQL); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	return pool
}

// connectLive is Stripe Connect with a live or a test key: every seller's account is enabled, and it records
// the transfers it was asked for.
type connectLive struct {
	live      bool
	transfers []string
}

func (c *connectLive) Livemode() bool { return c.live }
func (c *connectLive) CreateConnectedAccount(context.Context, string, string, string) (billing.ConnectAccount, error) {
	return billing.ConnectAccount{}, nil
}
func (c *connectLive) OnboardingLink(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (c *connectLive) ConnectedAccount(_ context.Context, id string) (billing.ConnectAccount, error) {
	return billing.ConnectAccount{ID: id, DetailsSubmitted: true, PayoutsEnabled: true}, nil
}
func (c *connectLive) TransferToSeller(_ context.Context, _ string, _ int64, payoutID, _ string) (string, error) {
	c.transfers = append(c.transfers, payoutID)
	return "tr_" + payoutID, nil
}

// B22.1 — test money never reaches a live payout: a seller whose earnings were paid on a test-mode invoice
// gets nothing from a live key, and when a live invoice pays them too, a live key pays out exactly the live
// earnings; a test payout left unsent is never sent with a live key. All on the migrated schema.
func TestPayOut_ALiveKeyPaysOutOnlyLiveEarnings(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const seller, buyer = "ws-b221-seller", "ws-b221-buyer"
	if _, err := pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, details_submitted, payouts_enabled)
		VALUES ($1, 'acct_b221', true, true)`, seller); err != nil {
		t.Fatal(err)
	}
	paid := time.Now().Add(-30 * 24 * time.Hour) // both invoices were paid before the 14-day holdback
	use := func(id string, ulxc int64, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at) VALUES ($1, 'lst_b221', 1, $2, $3, $4, 'billed', $5, $5, $5)`, id, seller, buyer, ulxc, at); err != nil {
			t.Fatal(err)
		}
	}
	// $100 of uses on a TEST-mode invoice, and a test payout of $20 from last month that Stripe never accepted.
	use("use_test", 1_000_000_000, paid.Add(-3*time.Hour))
	if n, err := s.ClearInvoice(ctx, buyer, "in_test", paid.Add(-4*time.Hour), paid.Add(-2*time.Hour), paid, false); err != nil || n != 1 {
		t.Fatalf("clear the test invoice = %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO market_payouts (id, workspace_id, method, month, gross_usd_micros, account_fee_usd_micros,
		net_usd_micros, stripe_account_id, created_at, livemode) VALUES ('mpo_test_unsent', $1, 'stripe', $2, 20000000, 1000000, 19000000,
		'acct_b221', $3, false)`,
		seller, monthOf(paid), paid); err != nil {
		t.Fatal(err)
	}
	live := &connectLive{live: true}
	if _, err := s.PayOut(ctx, live, time.Now()); err != nil {
		t.Fatal(err)
	}
	var livePayouts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_payouts WHERE workspace_id = $1 AND livemode`, seller).Scan(&livePayouts); err != nil {
		t.Fatal(err)
	}
	if livePayouts != 0 || len(live.transfers) != 0 {
		t.Fatalf("a live key paid %d payouts and sent %v out of test earnings; want nothing", livePayouts, live.transfers)
	}

	// $50 of uses on a LIVE invoice: a live key pays out exactly the seller's 85% of them, $42.50 (B32.8).
	use("use_live", 500_000_000, paid.Add(-1*time.Hour))
	if n, err := s.ClearInvoice(ctx, buyer, "in_live", paid.Add(-2*time.Hour), paid, paid, true); err != nil || n != 1 {
		t.Fatalf("clear the live invoice = %d, %v", n, err)
	}
	if _, err := s.PayOut(ctx, live, time.Now()); err != nil {
		t.Fatal(err)
	}
	var id string
	var gross int64
	if err := pool.QueryRow(ctx, `SELECT id, gross_usd_micros FROM market_payouts WHERE workspace_id = $1 AND livemode`, seller).Scan(&id, &gross); err != nil {
		t.Fatalf("the live payout: %v", err)
	}
	if gross != 42_500_000 || len(live.transfers) != 1 || live.transfers[0] != id {
		t.Fatalf("the live payout is $%d.%02d with transfers %v; want $42.50, sent once as %s", gross/1_000_000, gross/10_000%100, live.transfers, id)
	}
	var unsent bool
	if err := pool.QueryRow(ctx, `SELECT paid_at IS NULL FROM market_payouts WHERE id = 'mpo_test_unsent'`).Scan(&unsent); err != nil || !unsent {
		t.Fatalf("the unsent test payout was sent with a live key (%v)", err)
	}
}
