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

// everyWorkspace is one Connect client for every workspace, as before B25.6 gave test workspaces their own.
func everyWorkspace(c market.ConnectStripe) connectByWorkspace {
	return func(string) (market.ConnectStripe, error) { return c, nil }
}

// connectFake is Stripe Connect in test mode: one Express account per seller whose onboarding finishes when
// the test says so, the transfers it was asked for, and the invoice each charge paid.
type connectFake struct {
	accounts  map[string]*billing.ConnectAccount
	transfers []connectTransfer
	charges   map[string]connectCharge
}

type connectTransfer struct {
	account, payout, workspace string
	cents                      int64
}

type connectCharge struct {
	invoice string
	cents   int64
}

func (f *connectFake) CreateConnectedAccount(_ context.Context, ws, country, _ string) (billing.ConnectAccount, error) {
	a := &billing.ConnectAccount{ID: "acct_" + ws, Country: country, CurrentlyDue: []string{"external_account", "individual.verification.document"}}
	f.accounts[a.ID] = a
	return *a, nil
}
func (f *connectFake) OnboardingLink(_ context.Context, id, refresh, ret string) (string, error) {
	return "https://connect.stripe.com/setup/e/" + id + "?return=" + ret, nil
}
func (f *connectFake) ConnectedAccount(_ context.Context, id string) (billing.ConnectAccount, error) {
	return *f.accounts[id], nil
}
func (f *connectFake) TransferToSeller(_ context.Context, account string, cents int64, payout, ws string) (string, error) {
	for i, t := range f.transfers {
		if t.payout == payout { // Stripe answers a transfer group it has already paid with that transfer
			return fmt.Sprintf("tr_%d", i+1), nil
		}
	}
	f.transfers = append(f.transfers, connectTransfer{account, payout, ws, cents})
	return fmt.Sprintf("tr_%d", len(f.transfers)), nil
}
func (f *connectFake) ChargeInvoice(_ context.Context, charge string) (string, int64, error) {
	c := f.charges[charge]
	return c.invoice, c.cents, nil
}

// B20.5 — a test-mode seller onboards, earns, is paid out after the holdback with Stripe's fees shown, a
// buyer's refund inside the holdback reverses the earning, a chargeback after the payout is owed and
// recovered from future earnings, and the rest is taken as credits 1:1 — every step a ledger row matched
// to a Stripe test object.
func TestMarketPayouts_OnboardEarnPaidOutAfterHoldbackAndRefundsReverse(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer, secret = "ws-pay-seller", "ws-pay-buyer", "whsec_market_payouts"

	billFake := &marketStripe{}
	connect := &connectFake{accounts: map[string]*billing.ConnectAccount{}, charges: map[string]connectCharge{}}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, billFake, secret).
		WithMarketBill(billFake, "price_market", "talyvor_marketplace_use", store).
		WithMarketPayouts(connect, store)
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketUseRoutes(r, store, lens, svc, bank)
	mountMarketPayoutRoutes(r, store, everyWorkspace(connect), bank, marketPayoutURLs{refresh: "https://app.test/expired", ret: "https://app.test/selling"})
	r.Post("/v1/billing/webhook", svc.HandleWebhook)

	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	payouts := func() market.Payouts {
		t.Helper()
		code, out := call(seller, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/payouts", "")
		var p market.Payouts
		if _ = json.Unmarshal([]byte(out), &p); code != http.StatusOK {
			t.Fatalf("payouts = %d %s", code, out)
		}
		return p
	}
	event := func(typ, id string, obj map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": typ, "created": time.Now().Unix(), "livemode": true, "data": map[string]any{"object": obj}})
		signedAt := time.Now()
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(raw))
		req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", signedAt.Unix(), hex.EncodeToString(webhook.ComputeSignature(signedAt, raw, secret))))
		w := httptest.NewRecorder()
		if r.ServeHTTP(w, req); w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", typ, id, w.Code, w.Body.String())
		}
	}
	now := time.Now()

	// The seller connects: Lens creates their Express account and hands them Stripe's onboarding.
	code, out := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/payouts/connect", `{"country":"gb","email":"seller@pay.example"}`)
	var connected struct {
		URL     string                 `json:"url"`
		Account billing.ConnectAccount `json:"account"`
	}
	if _ = json.Unmarshal([]byte(out), &connected); code != http.StatusOK || !strings.HasPrefix(connected.URL, "https://connect.stripe.com/setup/e/acct_"+seller) ||
		connected.Account.PayoutsEnabled || connected.Account.Country != "GB" {
		t.Fatalf("connect = %d %s; want Stripe's onboarding for a GB account not yet enabled", code, out)
	}
	// Stripe's onboarding finishes; the payouts page asks Stripe and records it.
	*connect.accounts["acct_"+seller] = billing.ConnectAccount{ID: "acct_" + seller, Country: "GB", DetailsSubmitted: true, PayoutsEnabled: true}
	if p := payouts(); p.Account == nil || !p.Account.PayoutsEnabled || len(p.Account.CurrentlyDue) != 0 {
		t.Fatalf("after onboarding the account = %+v, want enabled with nothing due", p.Account)
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT payouts_enabled FROM market_sellers WHERE workspace_id = $1 AND stripe_account_id = $2`,
		seller, "acct_"+seller).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("market_sellers = %v (%v), want the account recorded enabled", recorded, err)
	}

	// The seller earns: a $10 prompt the buyer uses on three bills — A (three uses, paid 20 days ago, past
	// the holdback), B (two uses, paid today) and, later, C. The seller keeps $8.50 of each; Talyvor keeps 15%
	// (B32.8).
	code, out = call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
		`{"kind":"prompt","title":"Contract review","price_per_use_ulxc":100000000,"artifact":{"template":"Review {{text}}","model":"m"}}`)
	var listing market.Listing
	if _ = json.Unmarshal([]byte(out), &listing); code != http.StatusCreated {
		t.Fatalf("publish = %d %s", code, out)
	}
	useOn := func(n int, usedAt time.Time) []string {
		t.Helper()
		var ids []string
		for i := range n {
			code, out := call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+listing.ID+"/use", `{"variables":{"text":"the NDA"}}`)
			var u market.Use
			if _ = json.Unmarshal([]byte(out), &u); code != http.StatusOK || u.Charge != market.ChargeBilled {
				t.Fatalf("use = %d %s", code, out)
			}
			if _, err := pool.Exec(ctx, `UPDATE market_uses SET used_at = $2 WHERE id = $1`, u.ID, usedAt.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, u.ID)
		}
		return ids
	}
	payInvoice := func(invoiceID string, usedAt, paidAt time.Time) {
		t.Helper()
		event("invoice.paid", "evt_"+invoiceID, map[string]any{"id": invoiceID, "object": "invoice", "subscription": "sub_market_1",
			"status_transitions": map[string]any{"paid_at": paidAt.Unix()},
			"lines": map[string]any{"object": "list", "data": []any{map[string]any{"id": "il_" + invoiceID, "object": "line_item",
				"period": map[string]any{"start": usedAt.Add(-time.Hour).Unix(), "end": usedAt.Add(time.Hour).Unix()},
				"price":  map[string]any{"id": "price_market", "object": "price"}}}}})
	}
	billA := useOn(3, now.AddDate(0, 0, -25))
	payInvoice("in_a", now.AddDate(0, 0, -25), now.AddDate(0, 0, -20))
	useOn(2, now)
	payInvoice("in_b", now, now)
	if p := payouts(); p.AvailableUSDMicros != 25_500_000 || p.InHoldbackUSDMicros != 17_000_000 {
		t.Fatalf("after two paid bills the balance = %+v, want $25.50 available and $17 in the holdback", p)
	}

	// The buyer is refunded bill B, inside the holdback: its two earnings are reversed, and nobody is credited
	// on a bill — Stripe already gave the money back.
	event("charge.refunded", "evt_refund_b", map[string]any{"id": "ch_b", "object": "charge", "amount": 2000, "amount_refunded": 2000,
		"refunded": true, "invoice": "in_b", "payment_intent": "pi_b"})
	type reversal struct {
		use, cause, ref string
		share           int64
	}
	reversals := func() []reversal {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT use_id, cause, stripe_ref, reversed_share_usd_micros FROM market_refunds
			WHERE seller_workspace_id = $1 ORDER BY refunded_at, use_id`, seller)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []reversal
		for rows.Next() {
			var x reversal
			if err := rows.Scan(&x.use, &x.cause, &x.ref, &x.share); err != nil {
				t.Fatal(err)
			}
			out = append(out, x)
		}
		return out
	}
	if got := reversals(); len(got) != 2 || got[0].cause != "buyer_refund" || got[0].ref != "ch_b" || got[0].share != 8_500_000 || got[1].share != 8_500_000 {
		t.Fatalf("after the refund of bill B the reversals = %+v, want its two $8.50 earnings reversed for ch_b", got)
	}
	if _, credited, err := store.RefundTakenDown(ctx, svc); err != nil || credited != 0 || len(billFake.credits) != 0 {
		t.Fatalf("a refunded bill was credited again on the next: %d credits (%v), Stripe asked for %+v", credited, err, billFake.credits)
	}
	if p := payouts(); p.AvailableUSDMicros != 25_500_000 || p.InHoldbackUSDMicros != 0 {
		t.Fatalf("after the refund the balance = %+v, want $25.50 available and nothing in the holdback", p)
	}

	// The weekly run pays the seller the $25.50 past the holdback: one transfer of it less Stripe's fees at cost
	// ($2 for the account this month, 0.25% + $0.25 for the payout) — and only one this week (B32.42).
	if n, err := store.PayOut(ctx, connect, now); err != nil || n != 1 {
		t.Fatalf("payout run = %d, %v; want one transfer", n, err)
	}
	var id, account, transfer string
	var gross, accountFee, payoutFee, net int64
	var paid bool
	if err := pool.QueryRow(ctx, `SELECT id, stripe_account_id, COALESCE(stripe_transfer_id, ''), gross_usd_micros, account_fee_usd_micros,
		payout_fee_usd_micros, net_usd_micros, paid_at IS NOT NULL FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe'`, seller).
		Scan(&id, &account, &transfer, &gross, &accountFee, &payoutFee, &net, &paid); err != nil {
		t.Fatal(err)
	}
	if gross != 25_500_000 || accountFee != 2_000_000 || payoutFee != 310_000 || net != 23_190_000 || !paid || account != "acct_"+seller || transfer != "tr_1" {
		t.Fatalf("the payout = %s gross %d fees %d+%d net %d paid %v to %s by %s; want $25.50 − $2.00 − $0.31 = $23.19 by tr_1",
			id, gross, accountFee, payoutFee, net, paid, account, transfer)
	}
	if len(connect.transfers) != 1 || connect.transfers[0] != (connectTransfer{"acct_" + seller, id, seller, 2319}) {
		t.Fatalf("Stripe transfers = %+v, want one of 2319¢ to the seller's account for %s", connect.transfers, id)
	}
	if n, err := store.PayOut(ctx, connect, now); err != nil || n != 0 || len(connect.transfers) != 1 {
		t.Fatalf("a second run this month = %d, %v, %d transfers; want nothing more", n, err, len(connect.transfers))
	}
	if p := payouts(); p.AvailableUSDMicros != 0 || p.PaidOutUSDMicros != 25_500_000 || !p.PaidThisMonth || len(p.Payouts) != 1 || p.Payouts[0].PayoutFeeUSDMicros != 310_000 {
		t.Fatalf("after the payout the page = %+v, want nothing available, $25.50 paid out with its fees shown", p)
	}
	// B32.42: the week's statement, which the owner reads, sums to what the payout sent.
	year, week := now.UTC().ISOWeek()
	period := fmt.Sprintf("%04d-W%02d", year, week)
	code, out = call(seller, http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/statements?period="+period, "")
	var statement market.Statement
	if _ = json.Unmarshal([]byte(out), &statement); code != http.StatusOK || statement.NetUSDMicros != net || statement.Payout == nil || statement.Payout.ID != id {
		t.Fatalf("the statement of %s = %d %s; want the payout %s, its net %d", period, code, out, id, net)
	}

	// A chargeback of one $10 use on bill A, after the holdback and the payout: its $8.50 earning is reversed and
	// the seller owes it; there is nothing to take as credits.
	connect.charges["ch_a"] = connectCharge{invoice: "in_a", cents: 3000}
	event("charge.dispute.created", "evt_dp_a", map[string]any{"id": "dp_a", "object": "dispute", "amount": 1000, "charge": "ch_a"})
	if got := reversals(); len(got) != 3 || got[2] != (reversal{billA[0], "chargeback", "dp_a", 8_500_000}) {
		t.Fatalf("after the chargeback the reversals = %+v, want bill A's first use reversed for dp_a", got)
	}
	if p := payouts(); p.OwedUSDMicros != 8_500_000 || p.AvailableUSDMicros != 0 {
		t.Fatalf("after the chargeback the balance = %+v, want $8.50 owed", p)
	}
	if code, out := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/payouts/credits", ""); code != http.StatusConflict {
		t.Fatalf("credits while owing = %d %s, want 409", code, out)
	}

	// Bill C's $17 earned past the holdback recovers the $8.50 owed; the seller takes the other $8.50 as credits, 1:1.
	useOn(2, now.AddDate(0, 0, -24))
	payInvoice("in_c", now.AddDate(0, 0, -24), now.AddDate(0, 0, -20))
	code, out = call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/payouts/credits", "")
	var credits market.Payout
	if _ = json.Unmarshal([]byte(out), &credits); code != http.StatusCreated || credits.GrossUSDMicros != 8_500_000 || credits.CreditsULXC != 85_000_000 {
		t.Fatalf("credits = %d %s, want $8.50 as 85 LXC", code, out)
	}
	var amount int64
	var typ, payoutID string
	if err := pool.QueryRow(ctx, `SELECT amount, type, metadata->>'market_payout_id' FROM lxc_ledger WHERE workspace_id = $1`, seller).
		Scan(&amount, &typ, &payoutID); err != nil || amount != 85_000_000 || typ != economy.LXCTypePurchase || payoutID != credits.ID {
		t.Fatalf("the seller's lxc_ledger row = %d %s %s (%v), want 85 LXC for %s", amount, typ, payoutID, err, credits.ID)
	}
	if p := payouts(); p.AvailableUSDMicros != 0 || p.OwedUSDMicros != 0 || p.PaidOutUSDMicros != 34_000_000 {
		t.Fatalf("at the end the balance = %+v, want nothing available or owed and $34 paid out", p)
	}
}
