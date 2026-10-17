package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/sellertax"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// B32.99 (for B17.161's tax-and-payouts) — the synthetic payout run. A test seller with $25.50 released and no tax
// details is withheld, payout null and nothing journalled; once their details are complete the next run pays one
// payout of the whole 25,500,000 µUSD, journalled off their available balance, and the week's statement names it and
// sums to its net. A real workspace is refused 403 with nothing changed. Wired as main.go wires it, on the migrated schema.
func TestB3299_ATestSellerIsWithheldForTaxDetailsThenPaidByTheSyntheticPayoutRun(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const syntheticKey = "b3299-synthetic-key"
	kek := make([]byte, envelope.KEKLen)
	if _, err := rand.Read(kek); err != nil {
		t.Fatal(err)
	}
	ring, err := envelope.NewKeyring(kek)
	if err != nil {
		t.Fatal(err)
	}
	marketStore := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	tax := sellertax.NewStore(pool, ring, partners.NewRegistry(nil))
	r := chi.NewRouter()
	mountSyntheticRoutes(r, syntheticKey, syntheticDeps{workspaces: workspace.New(pool), credits: bank, answers: storedanswers.New(pool, nil),
		audit: pool, mint: func(ws, _ string, _ []string, _ time.Duration) (string, error) { return "tok-" + ws, nil },
		due: syntheticDueDeps{db: pool, loans: bank, bills: marketStore, payouts: marketStore, sellerTax: tax, cards: bank}})
	synthetic := func(path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set(syntheticKeyHeader, syntheticKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("POST %s: %v in %s", path, err, w.Body.String())
			}
		}
		return w.Code
	}
	var made struct {
		Workspaces []syntheticUser `json:"workspaces"`
	}
	if code := synthetic("/v1/synthetic/workspaces", `{"count":2}`, &made); code != http.StatusCreated || len(made.Workspaces) != 2 {
		t.Fatalf("make two test users = %d %+v", code, made)
	}
	seller, buyer := made.Workspaces[0].WorkspaceID, made.Workspaces[1].WorkspaceID

	// The buyer's $30 rent of the seller's listing, metered, and the bill paid now: the seller's 85%, $25.50, is past its holdback.
	if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
		charge, used_at, ran_at, metered_at) VALUES ('use_b3299', 'lst_b3299', 1, $1, $2, 300000000, 'billed', now(), now(), now())`, seller, buyer); err != nil {
		t.Fatal(err)
	}
	if code := synthetic("/v1/synthetic/workspaces/"+buyer+"/marketplace/bill/pay", "", nil); code != http.StatusOK {
		t.Fatalf("paying the buyer's bill = %d", code)
	}

	type run struct {
		Withheld bool           `json:"withheld"`
		Hold     string         `json:"hold"`
		Payout   *market.Payout `json:"payout"`
	}
	journalled := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_journal_entries WHERE kind = 'payout'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var first run
	if code := synthetic("/v1/synthetic/workspaces/"+seller+"/marketplace/payouts/run", "", &first); code != http.StatusOK {
		t.Fatalf("the first payout run = %d", code)
	}
	if d, _ := tax.Get(ctx, seller); !first.Withheld || first.Hold != sellertax.HoldReason || first.Payout != nil || d.WithheldSince == nil || journalled() != 0 {
		t.Fatalf("a seller without tax details: the run answered %+v, their details withheld since %v, %d payout entries; want withheld, why, no payout, nothing journalled",
			first, d.WithheldSince, journalled())
	}

	dob, account := "1985-04-12", "GB33BUKB20201555555555"
	if done, err := tax.Put(ctx, seller, sellertax.Input{SellerType: sellertax.Individual, FirstName: "Nightly", LastName: "Seller",
		Address: "2 Test Street, London", Country: "GB", TINs: &[]sellertax.TIN{{Jurisdiction: "GB", Number: "1234567890"}},
		DateOfBirth: &dob, AccountIdentifier: &account, AccountHolder: "Nightly Seller"}); err != nil || !done.Complete || done.WithheldSince != nil {
		t.Fatalf("completing the details = %+v, %v; want complete and no longer withheld", done, err)
	}
	var second run
	if code := synthetic("/v1/synthetic/workspaces/"+seller+"/marketplace/payouts/run", "", &second); code != http.StatusOK {
		t.Fatalf("the next payout run = %d", code)
	}
	p := second.Payout
	if second.Withheld || p == nil || p.Method != "stripe" || p.GrossUSDMicros != 25_500_000 || p.PaidAt == nil || !strings.HasPrefix(p.StripeTransferID, "tr_synthetic_") ||
		p.NetUSDMicros != p.GrossUSDMicros-p.AccountFeeUSDMicros-p.PayoutFeeUSDMicros {
		t.Fatalf("the next run answered %+v (payout %+v); want one paid synthetic payout of 25,500,000 µUSD gross, its net that less Stripe's fees", second, p)
	}
	var out int64
	if err := pool.QueryRow(ctx, `SELECT p.amount_usd_micros FROM market_journal_postings p JOIN market_journal_entries e ON e.id = p.entry_id
		WHERE e.kind = 'payout' AND e.ref = $1 AND p.account = $2`, p.ID, market.SellerAvailable(seller)).Scan(&out); err != nil || out != p.GrossUSDMicros {
		t.Fatalf("the payout took %d µUSD from the seller's available (%v), want its gross %d", out, err, p.GrossUSDMicros)
	}
	st, err := marketStore.SellerStatement(ctx, seller, p.Period)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, l := range st.Lines {
		sum += l.AmountUSDMicros
	}
	if st.Payout == nil || st.Payout.ID != p.ID || st.NetUSDMicros != p.NetUSDMicros || sum != st.NetUSDMicros {
		t.Fatalf("the week's statement names payout %+v with net %d, lines summing to %d; want %s, its net %d", st.Payout, st.NetUSDMicros, sum, p.ID, p.NetUSDMicros)
	}
	var again run
	if code := synthetic("/v1/synthetic/workspaces/"+seller+"/marketplace/payouts/run", "", &again); code != http.StatusOK || again.Payout != nil {
		t.Fatalf("a third run the same week = %d %+v, want no second payout", code, again)
	}

	// A real workspace is refused, and nothing changes.
	const real = "ws-real-b3299"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, real); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO seller_tax_profiles (workspace_id) VALUES ($1)`, real); err != nil {
		t.Fatal(err)
	}
	if code := synthetic("/v1/synthetic/workspaces/"+real+"/marketplace/payouts/run", "", nil); code != http.StatusForbidden {
		t.Fatalf("a real workspace's payout run = %d, want 403", code)
	}
	var reminded int
	if err := pool.QueryRow(ctx, `SELECT reminders_sent FROM seller_tax_profiles WHERE workspace_id = $1`, real).Scan(&reminded); err != nil || reminded != 0 || journalled() != 1 {
		t.Fatalf("a real workspace's refused run changed rows: %d requests (%v), %d payout entries", reminded, err, journalled())
	}
}
