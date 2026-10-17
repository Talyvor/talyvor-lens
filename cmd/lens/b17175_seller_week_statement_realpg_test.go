package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/sellertax"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// B17.175 — the testers' seller-week-statement, through the routes it calls, wired as main.go wires them: a test
// buyer's bill of one $30 rent is paid and clears it, and the release job releases the seller's share. Not yet paid,
// the payout page lists no payout, the list of statements is empty, and this week's statement names no payout, holds
// the one sale and carries the share forward, its lines summing to a net of 0; ?period=2026-W54 is 400 and the
// seller's agent key is 403. Paid by the synthetic payout run, the payout page reads paid this week, the list starts
// with this week naming the payout, and that week's lines sum to its net — the listed net and the payout's — with
// its Stripe fees the payout's.
func TestB17175_ASellersWeekStatementSumsToItsPayout(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const syntheticKey = "b17175-synthetic-key"
	kek := make([]byte, envelope.KEKLen)
	if _, err := rand.Read(kek); err != nil {
		t.Fatal(err)
	}
	ring, err := envelope.NewKeyring(kek)
	if err != nil {
		t.Fatal(err)
	}
	registry := partners.NewRegistry(nil)
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	tax := sellertax.NewStore(pool, ring, registry)
	r := chi.NewRouter()
	mountSyntheticRoutes(r, syntheticKey, syntheticDeps{workspaces: workspace.New(pool), credits: bank, answers: storedanswers.New(pool, nil),
		audit: pool, mint: func(ws, _ string, _ []string, _ time.Duration) (string, error) { return "tok-" + ws, nil },
		due: syntheticDueDeps{db: pool, loans: bank, bills: store, payouts: store, sellerTax: tax, cards: bank}})
	mountSellerTaxRoutes(r, tax)
	mountMarketPayoutRoutes(r, store, func(string) (market.ConnectStripe, error) { return nil, errNoTestMode }, nil, marketPayoutURLs{})
	const owner, agentKey = "owner", "agent"
	call := func(ws, as, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		switch as {
		case owner:
			req = req.WithContext(auth.WithAuthContext(req.Context(),
				&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		case agentKey:
			req = req.WithContext(auth.WithAuthContext(req.Context(),
				&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-agent-" + ws, Scopes: []string{"chat"}}))
		default:
			req.Header.Set(syntheticKeyHeader, syntheticKey)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		if w.Code >= 300 {
			t.Logf("%s %s = %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Code
	}
	sumOf := func(st market.Statement) (sum int64, by map[string]int64) {
		by = map[string]int64{}
		for _, l := range st.Lines {
			sum += l.AmountUSDMicros
			by[l.Kind] += l.AmountUSDMicros
		}
		return sum, by
	}

	var made struct {
		Workspaces []syntheticUser `json:"workspaces"`
	}
	if code := call("", "", http.MethodPost, "/v1/synthetic/workspaces", `{"count":2}`, &made); code != http.StatusCreated || len(made.Workspaces) != 2 {
		t.Fatalf("make two test users = %d %+v", code, made)
	}
	seller, buyer := made.Workspaces[0].WorkspaceID, made.Workspaces[1].WorkspaceID

	// The buyer's $30 rent, metered; the bill paid now clears it, and the release job releases the seller's share.
	if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
		charge, used_at, ran_at, metered_at) VALUES ('use_' || $1, 'lst_' || $1, 1, $1, $2, 300000000, 'billed', now(), now(), now())`, seller, buyer); err != nil {
		t.Fatal(err)
	}
	var bill struct {
		UsesCleared int `json:"uses_cleared"`
	}
	if code := call("", "", http.MethodPost, "/v1/synthetic/workspaces/"+buyer+"/marketplace/bill/pay", "", &bill); code != http.StatusOK || bill.UsesCleared != 1 {
		t.Fatalf("paying the buyer's bill of one rent = %d %+v, want it cleared", code, bill)
	}
	if n, err := store.ReleaseDue(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("the release job released %d (%v), want the one sale", n, err)
	}
	const rent, share = 30_000_000, 25_500_000

	// Not yet paid: no payout anywhere, and this week's lines sum to a net of 0.
	y, wk := time.Now().UTC().ISOWeek()
	week := fmt.Sprintf("%04d-W%02d", y, wk)
	var page market.Payouts
	if code := call(seller, owner, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/payouts", "", &page); code != http.StatusOK || page.PaidThisWeek || len(page.Payouts) != 0 {
		t.Fatalf("the payout page, not yet paid = %d %+v", code, page)
	}
	var list struct {
		Statements []market.StatementSummary `json:"statements"`
	}
	if code := call(seller, owner, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements", "", &list); code != http.StatusOK || len(list.Statements) != 0 {
		t.Fatalf("the list of statements, not yet paid = %d %+v", code, list)
	}
	var st0 market.Statement
	if code := call(seller, owner, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements?period="+week, "", &st0); code != http.StatusOK {
		t.Fatalf("the statement of %s, not yet paid = %d", week, code)
	}
	if sum, by := sumOf(st0); st0.Payout != nil || st0.NetUSDMicros != 0 || sum != 0 || st0.Sales != 1 || by[market.LineSales] != rent || by[market.LineCarriedForward] != -share {
		t.Fatalf("not yet paid, %s should hold the one sale and carry %d forward to a net of 0: %+v", week, share, st0)
	}
	if code := call(seller, owner, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements?period=2026-W54", "", nil); code != http.StatusBadRequest {
		t.Fatalf("2026 has no week 54, but it answered %d", code)
	}
	for _, q := range []string{"", "?period=" + week} {
		if code := call(seller, agentKey, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements"+q, "", nil); code != http.StatusForbidden {
			t.Fatalf("the seller's agent key read statements%s = %d, want 403", q, code)
		}
	}

	// Paid by the payout run, the seller's tax details complete.
	var saved sellertax.Details
	if code := call(seller, owner, http.MethodPut, "/v1/workspaces/"+seller+"/marketplace/seller-tax", `{"seller_type":"individual","first_name":"Nightly",
		"last_name":"Seller","address":"2 Test Street, London","country":"GB","tins":[{"jurisdiction":"GB","number":"1234567890"}],"date_of_birth":"1985-04-12",
		"account_identifier":"GB33BUKB20201555555555","account_holder":"Nightly Seller"}`, &saved); code != http.StatusOK || !saved.Complete {
		t.Fatalf("the seller's details = %d %+v, want complete", code, saved)
	}
	var run struct {
		Withheld bool           `json:"withheld"`
		Payout   *market.Payout `json:"payout"`
	}
	if code := call("", "", http.MethodPost, "/v1/synthetic/workspaces/"+seller+"/marketplace/payouts/run", "", &run); code != http.StatusOK || run.Withheld || run.Payout == nil {
		t.Fatalf("the payout run = %d %+v, want a payout", code, run)
	}
	paid := run.Payout
	if code := call(seller, owner, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/payouts", "", &page); code != http.StatusOK || !page.PaidThisWeek ||
		len(page.Payouts) != 1 || page.Payouts[0].ID != paid.ID || page.Payouts[0].Period != week {
		t.Fatalf("the payout page, paid %s in %s = %d %+v", paid.ID, week, code, page)
	}
	if code := call(seller, owner, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements", "", &list); code != http.StatusOK ||
		len(list.Statements) == 0 || list.Statements[0].Period != week || list.Statements[0].PayoutID != paid.ID {
		t.Fatalf("the list of statements, paid %s in %s = %d %+v", paid.ID, week, code, list)
	}
	var st1 market.Statement
	if code := call(seller, owner, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements?period="+week, "", &st1); code != http.StatusOK {
		t.Fatalf("the statement of %s, paid = %d", week, code)
	}
	sum, by := sumOf(st1)
	if st1.Payout == nil || st1.Payout.ID != paid.ID || sum != st1.NetUSDMicros || st1.NetUSDMicros != paid.NetUSDMicros || st1.Payout.NetUSDMicros != paid.NetUSDMicros ||
		list.Statements[0].NetUSDMicros != paid.NetUSDMicros || page.Payouts[0].NetUSDMicros != paid.NetUSDMicros ||
		by[market.LineStripeFees] != -(paid.AccountFeeUSDMicros+paid.PayoutFeeUSDMicros) {
		t.Fatalf("paid %+v, the statement of %s (lines sum to %d) reads %+v", paid, week, sum, st1)
	}
	if code := call(seller, agentKey, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements?period="+week, "", nil); code != http.StatusForbidden {
		t.Fatalf("the seller's agent key read the paid week = %d, want 403", code)
	}
}
