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

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/sellertax"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/taxprofile"
	"github.com/talyvor/lens/internal/workspace"
)

// B17.170 — the testers' self-billed-invoice, through the routes it calls, wired as main.go wires them: a test buyer's
// bill of two $30 rents from two sellers is paid and clears both; one seller saves GB individual details with VAT number
// GB123456789 and the self-billing agreement, the other the same without it. Paid by the synthetic payout run, the
// self-billing seller's newest week carries a TEST-SB invoice to TALYVOR LTD, its net the payout's gross, no VAT while
// self-billing VAT is under review, the supply_vat line under review at 0 and the lines summing to the payout's net; the
// other seller's paid week carries none.
func TestB17170_ASelfBillingSellersPaidWeekCarriesItsSelfBilledInvoice(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const syntheticKey = "b17170-synthetic-key"
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
	talyvor := market.Supplier{LegalName: "TALYVOR LTD"}
	store.SetReceipts(market.Receipts{Supplier: talyvor, Buyers: taxprofile.NewStore(pool, registry), Rates: ecbrate.New(pool, ecbrate.DailyURL)})
	store.SetSelfBilling(market.SelfBilling{Partners: registry, Customer: talyvor, VAT: false})
	bank := economy.NewDualTokenStore(nil, pool, nil)
	tax := sellertax.NewStore(pool, ring, registry)
	r := chi.NewRouter()
	mountSyntheticRoutes(r, syntheticKey, syntheticDeps{workspaces: workspace.New(pool), credits: bank, answers: storedanswers.New(pool, nil),
		audit: pool, mint: func(ws, _ string, _ []string, _ time.Duration) (string, error) { return "tok-" + ws, nil },
		due: syntheticDueDeps{db: pool, loans: bank, bills: store, payouts: store, sellerTax: tax, cards: bank}})
	mountSellerTaxRoutes(r, tax)
	mountMarketPayoutRoutes(r, store, nil, nil, marketPayoutURLs{})
	call := func(ws, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if ws == "" {
			req.Header.Set(syntheticKeyHeader, syntheticKey)
		} else {
			req = req.WithContext(auth.WithAuthContext(req.Context(),
				&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
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
	var made struct {
		Workspaces []syntheticUser `json:"workspaces"`
	}
	if code := call("", http.MethodPost, "/v1/synthetic/workspaces", `{"count":3}`, &made); code != http.StatusCreated || len(made.Workspaces) != 3 {
		t.Fatalf("make three test users = %d %+v", code, made)
	}
	biller, plain, buyer := made.Workspaces[0].WorkspaceID, made.Workspaces[1].WorkspaceID, made.Workspaces[2].WorkspaceID

	// The buyer's $30 rent of each seller's listing, metered; the bill paid now clears both.
	for _, seller := range []string{biller, plain} {
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at) VALUES ('use_' || $1, 'lst_' || $1, 1, $1, $2, 300000000, 'billed', now(), now(), now())`, seller, buyer); err != nil {
			t.Fatal(err)
		}
	}
	var bill struct {
		UsesCleared int `json:"uses_cleared"`
	}
	if code := call("", http.MethodPost, "/v1/synthetic/workspaces/"+buyer+"/marketplace/bill/pay", "", &bill); code != http.StatusOK || bill.UsesCleared != 2 {
		t.Fatalf("paying the buyer's bill of two rents = %d %+v, want both cleared", code, bill)
	}

	const details = `"seller_type":"individual","first_name":"Nightly","last_name":"Seller","address":"2 Test Street, London","country":"GB",
		"tins":[{"jurisdiction":"GB","number":"1234567890"}],"date_of_birth":"1985-04-12","account_identifier":"GB33BUKB20201555555555","account_holder":"Nightly Seller"`
	var agreed sellertax.Details
	if code := call(biller, http.MethodPut, "/v1/workspaces/"+biller+"/marketplace/seller-tax",
		`{`+details+`,"vat_number":"GB123456789","self_billing_agreed_version":"draft-2026-10"}`, &agreed); code != http.StatusOK ||
		!agreed.Complete || !agreed.VATValid || agreed.SelfBillingAgreedVersion != "draft-2026-10" {
		t.Fatalf("the self-billing seller's details = %d %+v, want complete, the number valid and the agreement kept", code, agreed)
	}
	var saved sellertax.Details
	if code := call(plain, http.MethodPut, "/v1/workspaces/"+plain+"/marketplace/seller-tax", `{`+details+`}`, &saved); code != http.StatusOK || !saved.Complete {
		t.Fatalf("the other seller's details = %d %+v, want complete", code, saved)
	}

	paidWeek := func(seller string) (*market.Payout, market.Statement) {
		t.Helper()
		var run struct {
			Payout *market.Payout `json:"payout"`
		}
		if code := call("", http.MethodPost, "/v1/synthetic/workspaces/"+seller+"/marketplace/payouts/run", "", &run); code != http.StatusOK || run.Payout == nil {
			t.Fatalf("the payout run for %s = %d %+v, want a payout", seller, code, run)
		}
		var list struct {
			Statements []market.StatementSummary `json:"statements"`
		}
		if code := call(seller, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements", "", &list); code != http.StatusOK ||
			len(list.Statements) == 0 || list.Statements[0].PayoutID != run.Payout.ID {
			t.Fatalf("%s's statements = %d %+v, want the newest naming %s", seller, code, list, run.Payout.ID)
		}
		var st market.Statement
		if code := call(seller, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements?period="+list.Statements[0].Period, "", &st); code != http.StatusOK {
			t.Fatalf("%s's statement of %s = %d", seller, run.Payout.Period, code)
		}
		return run.Payout, st
	}
	p, st := paidWeek(biller)
	inv := st.SelfBilledInvoice
	var sum, supplyVAT int64
	for _, l := range st.Lines {
		sum += l.AmountUSDMicros
		if l.Kind == market.LineSupplyVAT {
			supplyVAT = l.AmountUSDMicros
		}
	}
	if inv == nil || inv.PayoutID != p.ID || inv.Period != st.Period || inv.NetUSDMicros != p.GrossUSDMicros || inv.VATUSDMicros != 0 || p.VATUSDMicros != 0 ||
		inv.GrossUSDMicros != inv.NetUSDMicros || inv.VATEnabled || inv.Treatment != market.TaxUnderReview || inv.Note != market.VATUnderReview ||
		!strings.HasPrefix(inv.Number, "TEST-SB-") || !inv.Preview || inv.AgreementVersion != "draft-2026-10" || inv.Customer.Name != "TALYVOR LTD" ||
		inv.Supplier.Name != "Nightly Seller" || inv.Supplier.Country != "GB" || inv.Supplier.VATNumber != "GB123456789" ||
		supplyVAT != 0 || sum != st.NetUSDMicros || st.NetUSDMicros != p.NetUSDMicros {
		t.Fatalf("the self-billing seller paid %+v: statement %+v with invoice %+v (lines sum to %d)", p, st, inv, sum)
	}
	if _, st := paidWeek(plain); st.SelfBilledInvoice != nil {
		t.Fatalf("the seller without the agreement's paid week carries an invoice: %+v", st.SelfBilledInvoice)
	}
}
