package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// marketStripe is Stripe in test mode for the marketplace bill: it keeps what it was sent.
type marketStripe struct {
	subs       []string
	events     []meterEvent
	failMeter  bool
	credits    []marketCredit // B20.4: negative lines on a buyer's marketplace bill
	failCredit bool
	items      []string // B32.39: the tax price added to a subscription
}

type marketCredit struct {
	customer, subscription, key string
	cents                       float64
}

type meterEvent struct {
	name, customer, identifier string
	value                      int64
}

func (f *marketStripe) CreateCustomer(_ context.Context, ws string) (string, error) {
	return "cus_" + ws, nil
}
func (f *marketStripe) CreateCheckoutSession(context.Context, billing.CheckoutParams) (string, string, error) {
	return "", "", errors.New("not in this test")
}
func (f *marketStripe) CardFingerprint(context.Context, string) (string, error) { return "", nil }
func (f *marketStripe) CreateMarketSubscription(_ context.Context, customer, price, ws string) (string, error) {
	id := fmt.Sprintf("sub_market_%d", len(f.subs)+1)
	f.subs = append(f.subs, id+" "+customer+" "+price)
	return id, nil
}
func (f *marketStripe) CreditMarketUse(_ context.Context, customer, subscription string, cents float64, _, key string) (string, error) {
	if f.failCredit {
		return "", errors.New("stripe: 503")
	}
	for i, c := range f.credits {
		if c.key == key { // Stripe answers a retried idempotency key with the first item
			return fmt.Sprintf("ii_%d", i+1), nil
		}
	}
	f.credits = append(f.credits, marketCredit{customer, subscription, key, cents})
	return fmt.Sprintf("ii_%d", len(f.credits)), nil
}
func (f *marketStripe) AddMarketSubscriptionItem(_ context.Context, subscription, price string) error {
	f.items = append(f.items, subscription+" "+price)
	return nil
}
func (f *marketStripe) SendMeterEvent(_ context.Context, name, customer, identifier string, value int64, _ time.Time) error {
	if f.failMeter {
		return errors.New("stripe: 503")
	}
	f.events = append(f.events, meterEvent{name, customer, identifier, value})
	return nil
}

// B20.2 — a buyer uses a paid listing, it goes once onto their test-mode monthly marketplace bill, the
// seller's payable rises by exactly their share only after that bill is paid, and a seller using their
// own listing (or a buyer linked to them) earns nothing — asserted on the ledger rows. A buyer's agent
// uses a listing within its rules, and its charge lands on its company's bill.
func TestMarketUse_PayPerUseMeteredThenClearedAndTheSellerEarns(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer, linkedBuyer, secret = "ws-seller", "ws-buyer", "ws-seller-alt", "whsec_market_test"

	stripeFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, stripeFake, secret).WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)

	// Lens's own proxy, as the use reaches it: it answers with the prompt it was sent and says who asked.
	var ranAs []string
	lens := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model    string           `json:"model"`
			Messages []market.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		ranAs = append(ranAs, r.URL.Path+" "+r.Header.Get("Authorization")+" "+in.Model)
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{
			"role": "assistant", "content": "summary of: " + in.Messages[len(in.Messages)-1].Content}}}})
		_, _ = w.Write(out)
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketUseRoutes(r, store, lens, svc, bank)
	r.Post("/v1/billing/webhook", svc.HandleWebhook)

	call := func(who *auth.AuthContext, credential, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+credential)
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	person := func(ws string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
	}
	code, body := call(person(seller), "seller-jwt", http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
		`{"kind":"prompt","title":"Summariser","price_per_use_ulxc":10000000,"artifact":{"template":"Summarise {{text}} in three bullets.","model":"gpt-5-mini"}}`)
	if code != http.StatusCreated {
		t.Fatalf("publish = %d %s", code, body)
	}
	var listing market.Listing
	_ = json.Unmarshal([]byte(body), &listing)
	use := func(who *auth.AuthContext, credential, ws string) (int, market.Use, string) {
		t.Helper()
		code, body := call(who, credential, http.MethodPost, "/v1/workspaces/"+ws+"/marketplace/listings/"+listing.ID+"/use",
			`{"variables":{"text":"the Q3 report"}}`)
		var u market.Use
		_ = json.Unmarshal([]byte(body), &u)
		return code, u, body
	}
	earnings := func() market.Earnings {
		t.Helper()
		_, body := call(person(seller), "seller-jwt", http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/earnings", "")
		var e market.Earnings
		_ = json.Unmarshal([]byte(body), &e)
		return e
	}
	type useRow struct {
		charge, agent         string
		price                 int64
		ran, metered, cleared bool
	}
	row := func(id string) useRow {
		t.Helper()
		var u useRow
		if err := pool.QueryRow(ctx, `SELECT charge, agent_id, price_ulxc, ran_at IS NOT NULL, metered_at IS NOT NULL, cleared_at IS NOT NULL
			FROM market_uses WHERE id = $1`, id).Scan(&u.charge, &u.agent, &u.price, &u.ran, &u.metered, &u.cleared); err != nil {
			t.Fatalf("use %s: %v", id, err)
		}
		return u
	}

	// The buyer uses it: the listing ran through Lens as the buyer, and one meter event carries its price.
	code, first, body := use(person(buyer), "buyer-jwt", buyer)
	if code != http.StatusOK || first.Output != "summary of: Summarise the Q3 report in three bullets." || first.Charge != market.ChargeBilled {
		t.Fatalf("buyer's use = %d %s", code, body)
	}
	if len(ranAs) != 1 || ranAs[0] != "/v1/proxy/openai/v1/chat/completions Bearer buyer-jwt gpt-5-mini" {
		t.Errorf("the listing ran as %q, want once through the proxy with the buyer's credential", ranAs)
	}
	if got := row(first.ID); got != (useRow{charge: "billed", price: 10_000_000, ran: true, metered: true}) {
		t.Errorf("the use's row = %+v", got)
	}
	want := meterEvent{"talyvor_marketplace_use", "cus_" + buyer, first.ID, 10_000_000}
	if len(stripeFake.events) != 1 || stripeFake.events[0] != want || len(stripeFake.subs) != 1 {
		t.Fatalf("Stripe got events %+v and subscriptions %v, want one event %+v on one subscription", stripeFake.events, stripeFake.subs, want)
	}
	if e := earnings(); e.PayableUSDMicros != 0 || e.PendingUses != 1 || e.PendingUSDMicros != 850_000 {
		t.Fatalf("before the buyer paid, the seller's earnings = %+v; want nothing payable and one use pending, the seller's 850,000 µUSD of it", e)
	}

	// A second use, after the period the first invoice covers (Stripe's periods are whole seconds); and Stripe
	// down for it — it is billed on the next pass.
	periodEnd := time.Unix(first.UsedAt.Unix()+1, 0)
	time.Sleep(time.Until(periodEnd) + 10*time.Millisecond)
	stripeFake.failMeter = true
	code, second, body := use(person(buyer), "buyer-jwt", buyer)
	stripeFake.failMeter = false
	if code != http.StatusOK || row(second.ID).metered {
		t.Fatalf("a use while Stripe is down = %d %s, metered %v; want answered and not yet billed", code, body, row(second.ID).metered)
	}
	if _, err := pool.Exec(ctx, `UPDATE market_uses SET ran_at = ran_at - interval '2 minutes' WHERE id = $1`, second.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := store.MeterPending(ctx, svc, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if len(stripeFake.events) != 2 || stripeFake.events[1].identifier != second.ID || !row(second.ID).metered {
		t.Fatalf("after two passes Stripe has %+v, want the second use billed exactly once", stripeFake.events)
	}

	// The seller uses their own listing; a workspace sharing the seller's card uses it: neither is billed.
	if code, u, body := use(person(seller), "seller-jwt", seller); code != http.StatusOK || u.Charge != market.ChargeOwn {
		t.Errorf("the seller's own use = %d %s", code, body)
	}
	for _, ws := range []string{seller, linkedBuyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'card-1')`, ws); err != nil {
			t.Fatal(err)
		}
	}
	if code, u, body := use(person(linkedBuyer), "alt-jwt", linkedBuyer); code != http.StatusOK || u.Charge != market.ChargeLinked {
		t.Errorf("a linked workspace's use = %d %s, want charged nothing as a wash trade", code, body)
	}
	if len(stripeFake.events) != 2 {
		t.Errorf("Stripe has %d meter events, want only the buyer's two", len(stripeFake.events))
	}

	// The buyer pays the invoice covering the first use only: that use clears — 10 LXC = $1.00, the seller's
	// first sale — and Talyvor keeps 15% of it from the first dollar (B32.8): the seller earns 850,000 µUSD and
	// the fee is 150,000. A replay changes nothing.
	paid := func(invoiceID, subscription, reason string, end time.Time) int {
		t.Helper()
		obj := map[string]any{"id": invoiceID, "object": "invoice", "subscription": subscription, "billing_reason": reason,
			"status_transitions": map[string]any{"paid_at": time.Now().Unix()},
			"lines": map[string]any{"object": "list", "data": []any{map[string]any{"id": "il_" + invoiceID, "object": "line_item",
				"period": map[string]any{"start": end.Add(-24 * time.Hour).Unix(), "end": end.Unix()},
				"price":  map[string]any{"id": "price_market", "object": "price"}}}}}
		raw, _ := json.Marshal(map[string]any{"id": "evt_" + invoiceID, "object": "event", "type": "invoice.paid",
			"created": time.Now().Unix(), "livemode": true, "data": map[string]any{"object": obj}})
		now := time.Now()
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(raw))
		req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(webhook.ComputeSignature(now, raw, secret))))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := paid("in_other", "sub_someone_elses_plan", "subscription_cycle", periodEnd); code != http.StatusOK || earnings().PayableUSDMicros != 0 {
		t.Fatalf("a paid invoice that is not a marketplace bill = %d and moved the seller's payable", code)
	}
	// B17.159: the $0 opening invoice Stripe pays the moment the first use subscribes the buyer spans the first use,
	// and clears nothing: that use is billed on the next.
	if code := paid("in_0", "sub_market_1", "subscription_create", periodEnd); code != http.StatusOK || row(first.ID).cleared || earnings().PayableUSDMicros != 0 {
		t.Fatalf("the subscription's opening invoice paid = %d, cleared the first use %v; want it acknowledged and nothing cleared", code, row(first.ID).cleared)
	}
	for range 2 {
		if code := paid("in_1", "sub_market_1", "subscription_cycle", periodEnd); code != http.StatusOK {
			t.Fatalf("invoice.paid = %d", code)
		}
	}
	var n int
	var gross, share, fee int64
	var payableAt, clearedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(gross_usd_micros), 0), COALESCE(sum(share_usd_micros), 0), COALESCE(sum(fee_usd_micros), 0),
		max(payable_at), max(cleared_at) FROM market_earnings WHERE seller_workspace_id = $1`, seller).Scan(&n, &gross, &share, &fee, &payableAt, &clearedAt); err != nil {
		t.Fatal(err)
	}
	if n != 1 || gross != 1_000_000 || share != 850_000 || fee != 150_000 || !payableAt.Equal(clearedAt.Add(market.Holdback)) {
		t.Fatalf("earnings = %d rows, gross %d, share %d, fee %d, payable %v after clearing %v; want one of gross 1,000,000, share 850,000 and fee 150,000 µUSD payable after 14 days",
			n, gross, share, fee, payableAt, clearedAt)
	}
	if !row(first.ID).cleared || row(second.ID).cleared {
		t.Errorf("cleared: first %v, second %v; want only the use the paid invoice carried", row(first.ID).cleared, row(second.ID).cleared)
	}
	if e := earnings(); e.PayableUSDMicros != 850_000 || e.InHoldbackUSDMicros != 850_000 || e.AvailableUSDMicros != 0 || e.PendingUses != 1 ||
		len(e.Earnings) != 1 || e.Earnings[0].FeeUSDMicros != 150_000 {
		t.Errorf("the seller's earnings = %+v, want 850,000 µUSD payable, all of it in the holdback, Talyvor's 150,000 named, and the second use still pending", e)
	}
	// B32.17: the seller reads the same 850,000 µUSD in holdback on the journal, and it reconciles.
	if _, body := call(person(seller), "seller-jwt", http.MethodGet, "/v1/workspaces/"+seller+"/marketplace/journal", ""); body !=
		`{"available_usd_micros":0,"due_for_release_usd_micros":0,"holdback_usd_micros":850000,"reconciled":true}`+"\n" {
		t.Errorf("the seller's journal = %s; want 850,000 µUSD in holdback, nothing available, reconciled", body)
	}

	// A buyer's agent: its rules judge the use, and its charge goes on its company's bill.
	agent, err := bank.CreateAgent(ctx, buyer, "researcher", "owner-"+buyer)
	if err != nil {
		t.Fatal(err)
	}
	if err := bank.AttachAgentKey(ctx, buyer, agent.ID, "key-researcher"); err != nil {
		t.Fatal(err)
	}
	agentKey := &auth.AuthContext{WorkspaceID: buyer, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-researcher", Scopes: []string{auth.ScopeProxy}}
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MaxPerRequestULXC: 8_000_000}); err != nil {
		t.Fatal(err)
	}
	if code, _, body := use(agentKey, "tlv_agent", buyer); code != http.StatusForbidden || !strings.Contains(body, "limit per request") {
		t.Errorf("an agent's use beyond its limit per request = %d %s, want 403", code, body)
	}
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MaxPerRequestULXC: 20_000_000, ApprovalAboveULXC: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	code, _, body = use(agentKey, "tlv_agent", buyer)
	var refused struct {
		ApprovalID string `json:"approval_id"`
	}
	if _ = json.Unmarshal([]byte(body), &refused); code != http.StatusForbidden || refused.ApprovalID == "" {
		t.Fatalf("an agent's use above its approval amount = %d %s, want 403 naming the approval", code, body)
	}
	if _, err := bank.DecideAgentApproval(ctx, buyer, refused.ApprovalID, true); err != nil {
		t.Fatal(err)
	}
	code, byAgent, body := use(agentKey, "tlv_agent", buyer)
	if code != http.StatusOK || row(byAgent.ID) != (useRow{charge: "billed", agent: agent.ID, price: 10_000_000, ran: true, metered: true}) {
		t.Fatalf("the agent's approved use = %d %s, row %+v", code, body, row(byAgent.ID))
	}
	if last := stripeFake.events[len(stripeFake.events)-1]; len(stripeFake.events) != 3 || last.customer != "cus_"+buyer || last.identifier != byAgent.ID {
		t.Errorf("the agent's use went on %+v, want its company's bill", stripeFake.events)
	}
	var agentUses int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE agent_id = $1`, agent.ID).Scan(&agentUses); err != nil || agentUses != 1 {
		t.Errorf("the agent has %d uses recorded, want only the approved one (%v)", agentUses, err)
	}
	if b, err := economy.NewDualTokenStore(nil, pool, nil).AgentBook(ctx, buyer); err != nil || len(b.Agents) != 1 || b.Agents[0].BalanceULXC != 0 {
		t.Errorf("the agent's own balance moved: %+v (%v)", b.Agents, err)
	}
	// This month's bill lists each billed use once: the buyer's two and their agent's one.
	_, body = call(person(buyer), "buyer-jwt", http.MethodGet, "/v1/workspaces/"+buyer+"/marketplace/bill", "")
	var bill market.Bill
	if _ = json.Unmarshal([]byte(body), &bill); len(bill.Lines) != 3 || bill.TotalULXC != 30_000_000 || bill.Lines[0].Title != "Summariser" {
		t.Errorf("the buyer's bill = %s, want three uses of 10,000,000 µLXC", body)
	}
}
