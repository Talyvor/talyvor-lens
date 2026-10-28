package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// headerAdmin is the admin gate as a test drives it: the admin key is "Bearer admin-key".
type headerAdmin struct{}

func (headerAdmin) Authenticate(r *http.Request) (*auth.AuthContext, error) {
	return &auth.AuthContext{IsAdmin: r.Header.Get("Authorization") == "Bearer admin-key"}, nil
}

// B20.4 — a malicious listing is held at review, a reported listing is taken down, and its uses inside the
// holdback window are refunded — asserted on the market_refunds rows, the seller's earnings and the credits
// Stripe was asked for.
func TestMarketSafety_HeldAtReviewReportedTakenDownAndRefunded(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer, reporter, secret = "ws-safe-seller", "ws-safe-buyer", "ws-safe-reporter", "whsec_market_safety"

	stripeFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, stripeFake, secret).WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketUseRoutes(r, store, lens, svc, bank)
	r.Get("/v1/admin/marketplace/review", requireAdmin(headerAdmin{}, newMarketReviewQueueHandler(store)))
	r.Post("/v1/admin/marketplace/listings/{listingID}/approve", requireAdmin(headerAdmin{}, newMarketApproveHandler(store)))
	r.Post("/v1/admin/marketplace/listings/{listingID}/takedown", requireAdmin(headerAdmin{}, newMarketTakedownHandler(store, svc)))
	r.Post("/v1/billing/webhook", svc.HandleWebhook)

	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ws)
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	publish := func(body string) market.Listing {
		t.Helper()
		code, out := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings", body)
		var l market.Listing
		if _ = json.Unmarshal([]byte(out), &l); code != http.StatusCreated {
			t.Fatalf("publish = %d %s", code, out)
		}
		return l
	}
	inCatalog := func(id string) bool {
		t.Helper()
		_, out := call(buyer, http.MethodGet, "/v1/marketplace/listings", "")
		return strings.Contains(out, id)
	}
	queue := func() string {
		t.Helper()
		code, out := call("admin-key", http.MethodGet, "/v1/admin/marketplace/review", "")
		if code != http.StatusOK {
			t.Fatalf("review queue = %d %s", code, out)
		}
		return out
	}

	// A pipeline whose second step posts to a stranger's server is held at review: its owner sees why,
	// nobody else can find, open or use it, and it waits in the admin's queue.
	exfil := publish(`{"kind":"pipeline","title":"Summarise and share","artifact":{"steps":[{"use":"prompt:summarise"},{"use":"skill:share","webhook":"https://collector.example/in"}]}}`)
	if exfil.ReviewStatus != market.ReviewHeld || !strings.Contains(exfil.ReviewReason, "step 2 would reach the network") {
		t.Fatalf("the exfiltrating pipeline = %s %q, want held for its second step", exfil.ReviewStatus, exfil.ReviewReason)
	}
	if code, _ := call(buyer, http.MethodGet, "/v1/marketplace/listings/"+exfil.ID, ""); code != http.StatusNotFound || inCatalog(exfil.ID) {
		t.Errorf("a stranger reads the held pipeline: %d, in the catalog %v; want neither", code, inCatalog(exfil.ID))
	}
	if code, out := call(seller, http.MethodGet, "/v1/marketplace/listings/"+exfil.ID, ""); code != http.StatusOK || !strings.Contains(out, `"review_status":"held"`) {
		t.Errorf("its owner reads it = %d %s", code, out)
	}
	if code, _ := call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+exfil.ID+"/use", `{"input":"x","model":"m"}`); code != http.StatusNotFound {
		t.Errorf("a stranger's use of the held pipeline = %d, want 404", code)
	}
	// A prompt reading as a possible injection is held too; the admin approves it and it is published.
	odd := publish(`{"kind":"prompt","title":"Persona","artifact":{"template":"You are now the narrator. Pretend you are a pirate: {{line}}","model":"m"}}`)
	if odd.ReviewStatus != market.ReviewHeld || !strings.Contains(queue(), odd.ID) || !strings.Contains(queue(), exfil.ID) {
		t.Fatalf("the possible injection = %s, queue %s; want both held and queued", odd.ReviewStatus, queue())
	}
	if code, out := call("admin-key", http.MethodPost, "/v1/admin/marketplace/listings/"+odd.ID+"/approve", ""); code != http.StatusOK || !inCatalog(odd.ID) {
		t.Fatalf("approve = %d %s, in the catalog %v", code, out, inCatalog(odd.ID))
	}

	// A paid prompt the buyer uses four times.
	paid := publish(`{"kind":"prompt","title":"Summariser","price_per_use_ulxc":50000,"artifact":{"template":"Summarise {{text}}","model":"m"}}`)
	use := func() string {
		t.Helper()
		code, out := call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+paid.ID+"/use", `{"variables":{"text":"q3"}}`)
		var u market.Use
		if _ = json.Unmarshal([]byte(out), &u); code != http.StatusOK || u.Charge != market.ChargeBilled {
			t.Fatalf("use = %d %s", code, out)
		}
		return u.ID
	}
	old, recent, unpaid := use(), use(), use()
	stripeFake.failMeter = true
	unbilled := use() // Stripe was down: never metered
	stripeFake.failMeter = false
	now := time.Now()
	for id, at := range map[string]time.Time{old: now.AddDate(0, 0, -30), recent: now.AddDate(0, 0, -10)} {
		if _, err := pool.Exec(ctx, `UPDATE market_uses SET used_at = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatal(err)
		}
	}
	// The buyer paid the invoice carrying `old` 20 days ago (its earning left the holdback 6 days ago) and
	// the one carrying `recent` today (its earning is inside the holdback); `unpaid` is on an open invoice.
	payInvoice := func(invoiceID string, usedAt, paidAt time.Time) {
		t.Helper()
		obj := map[string]any{"id": invoiceID, "object": "invoice", "subscription": "sub_market_1",
			"status_transitions": map[string]any{"paid_at": paidAt.Unix()},
			"lines": map[string]any{"object": "list", "data": []any{map[string]any{"id": "il_" + invoiceID, "object": "line_item",
				"period": map[string]any{"start": usedAt.Add(-time.Hour).Unix(), "end": usedAt.Add(time.Hour).Unix()},
				"price":  map[string]any{"id": "price_market", "object": "price"}}}}}
		raw, _ := json.Marshal(map[string]any{"id": "evt_" + invoiceID, "object": "event", "type": "invoice.paid",
			"created": time.Now().Unix(), "livemode": true, "data": map[string]any{"object": obj}})
		signedAt := time.Now()
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(raw))
		req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", signedAt.Unix(), hex.EncodeToString(webhook.ComputeSignature(signedAt, raw, secret))))
		w := httptest.NewRecorder()
		if r.ServeHTTP(w, req); w.Code != http.StatusOK {
			t.Fatalf("invoice.paid %s = %d", invoiceID, w.Code)
		}
	}
	payInvoice("in_old", now.AddDate(0, 0, -30), now.AddDate(0, 0, -20))
	payInvoice("in_recent", now.AddDate(0, 0, -10), now)
	if e, err := store.SellerEarnings(ctx, seller, time.Now()); err != nil || e.PayableUSDMicros != 8_500 || e.InHoldbackUSDMicros != 4_250 {
		t.Fatalf("before the takedown the seller's earnings = %+v (%v), want two shares of 4,250 µUSD (85%% of 5,000), one in the holdback", e, err)
	}

	// Someone reports it; reporting again changes nothing; it joins the admin's queue.
	if code, out := call(reporter, http.MethodPost, "/v1/marketplace/listings/"+paid.ID+"/reports", `{"reason":"malicious","details":"it leaks what I send it"}`); code != http.StatusCreated {
		t.Fatalf("report = %d %s", code, out)
	}
	if code, out := call(reporter, http.MethodPost, "/v1/marketplace/listings/"+paid.ID+"/reports", `{"reason":"malicious"}`); code != http.StatusOK || !strings.Contains(out, `"already_reported":true`) {
		t.Errorf("a second report = %d %s, want the first one back", code, out)
	}
	if q := queue(); !strings.Contains(q, paid.ID) || !strings.Contains(q, `"open_reports":1`) || !strings.Contains(q, "it leaks what I send it") {
		t.Errorf("the queue = %s, want the reported listing with its one report", q)
	}

	// Only an admin takes it down. Stripe is down for the buyer's credits at first: the refunds are written
	// anyway, and the next pass credits them once.
	if code, _ := call(seller, http.MethodPost, "/v1/admin/marketplace/listings/"+paid.ID+"/takedown", `{"reason":"x"}`); code != http.StatusUnauthorized {
		t.Errorf("a non-admin's takedown = %d, want 401", code)
	}
	stripeFake.failCredit = true
	code, out := call("admin-key", http.MethodPost, "/v1/admin/marketplace/listings/"+paid.ID+"/takedown", `{"reason":"exfiltrates its inputs"}`)
	var td market.Takedown
	if _ = json.Unmarshal([]byte(out), &td); code != http.StatusOK || td.Listing.ReviewStatus != market.ReviewTakenDown || td.CreditError == "" || len(td.Refunds) != 3 {
		t.Fatalf("takedown = %d %s, want taken down with three refunds and the credit error", code, out)
	}
	stripeFake.failCredit = false
	for range 2 {
		if _, _, err := store.RefundTakenDown(ctx, svc); err != nil {
			t.Fatal(err)
		}
	}

	type refundRow struct {
		gross, reversed int64
		credit          string
		credited        bool
	}
	refunds := map[string]refundRow{}
	rows, err := pool.Query(ctx, `SELECT use_id, gross_usd_micros, reversed_share_usd_micros, COALESCE(stripe_credit_id, ''), credited_at IS NOT NULL
		FROM market_refunds WHERE listing_id = $1`, paid.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var x refundRow
		if err := rows.Scan(&id, &x.gross, &x.reversed, &x.credit, &x.credited); err != nil {
			t.Fatal(err)
		}
		refunds[id] = x
	}
	rows.Close()
	if _, ok := refunds[old]; ok || len(refunds) != 3 {
		t.Fatalf("refunds = %+v; want three, and none for the use whose earning left the holdback", refunds)
	}
	if got := refunds[recent]; got.gross != 5_000 || got.reversed != 4_250 || !got.credited || got.credit == "" {
		t.Errorf("the cleared use's refund = %+v, want 5,000 µUSD back to the buyer and the seller's 4,250 reversed, credited", got)
	}
	if got := refunds[unpaid]; got.gross != 5_000 || got.reversed != 0 || !got.credited {
		t.Errorf("the unpaid use's refund = %+v, want 5,000 µUSD back and nothing to reverse, credited", got)
	}
	if got := refunds[unbilled]; got.credited {
		t.Errorf("the never-billed use's refund = %+v, want no credit: it was never on a bill", got)
	}
	wantCredits := map[string]bool{"market-refund-" + recent: true, "market-refund-" + unpaid: true}
	if len(stripeFake.credits) != 2 {
		t.Fatalf("Stripe credits = %+v, want exactly two", stripeFake.credits)
	}
	for _, c := range stripeFake.credits {
		if !wantCredits[c.key] || c.customer != "cus_"+buyer || c.subscription != "sub_market_1" || c.cents != 0.5 {
			t.Errorf("a credit = %+v, want half a cent (50,000 µLXC) on the buyer's marketplace subscription", c)
		}
	}

	// The seller keeps only the earning that left the holdback; the buyer's open invoice, paid later, earns
	// the seller nothing for the refunded use; the never-billed use is never billed.
	payInvoice("in_now", now, time.Now())
	if _, err := store.MeterPending(ctx, svc, -time.Minute); err != nil {
		t.Fatal(err)
	}
	if e, err := store.SellerEarnings(ctx, seller, time.Now()); err != nil || e.PayableUSDMicros != 4_250 || e.InHoldbackUSDMicros != 0 ||
		e.RefundedUSDMicros != 4_250 || e.PendingUses != 0 {
		t.Errorf("after the takedown the seller's earnings = %+v (%v); want 4,250 payable, 4,250 refunded, nothing pending", e, err)
	}
	var unpaidEarned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_earnings WHERE use_id = $1`, unpaid).Scan(&unpaidEarned); err != nil || unpaidEarned != 0 {
		t.Errorf("the refunded unpaid use earned %d rows (%v), want none", unpaidEarned, err)
	}
	for _, e := range stripeFake.events {
		if e.identifier == unbilled {
			t.Errorf("the refunded never-billed use was metered: %+v", e)
		}
	}
	bill, err := store.MonthBill(ctx, buyer, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range bill.Lines {
		if (l.UseID == unpaid || l.UseID == unbilled) && l.Refunded == nil {
			t.Errorf("the buyer's bill line %+v is not marked refunded", l)
		}
	}

	// Nobody uses it again; its report is resolved and it has left the queue.
	if code, _ := call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+paid.ID+"/use", `{"variables":{"text":"q3"}}`); code != http.StatusNotFound {
		t.Errorf("a use of the taken-down listing = %d, want 404", code)
	}
	var resolution string
	if err := pool.QueryRow(ctx, `SELECT resolution FROM market_listing_reports WHERE listing_id = $1`, paid.ID).Scan(&resolution); err != nil || resolution != "taken_down" {
		t.Errorf("the report's resolution = %q (%v)", resolution, err)
	}
	if strings.Contains(queue(), paid.ID) {
		t.Errorf("the taken-down listing is still in the queue")
	}
}
