package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// B28.186 — two questions asked in a chat attached to a paid skill are two billed uses on the buyer's marketplace
// bill, each asked with the skill's instructions and each answer naming its charge; a model call that fails charges
// nothing, and a request naming no listing passes through untouched — asserted on market_uses and the meter events.
func TestChatListing_TwoQuestionsAreTwoBilledUses(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer = "ws-chat-seller", "ws-chat-buyer"

	stripeFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, stripeFake, "whsec_chat_listing").WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)

	// The proxy, as the chat's request reaches it: it streams one frame, or fails when asked to.
	var sent [][]market.Message
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []market.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		sent = append(sent, in.Messages)
		if strings.Contains(in.Messages[len(in.Messages)-1].Content, "fail") {
			http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {}\n\ndata: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	r.Post("/v1/proxy/openai/*", chatListing(store, svc, bank, chatShapeOpenAI)(proxy))

	call := func(ws, path, listing, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if listing != "" {
			req.Header.Set(listingHeader, listing)
		}
		who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	pub := call(seller, "/v1/workspaces/"+seller+"/marketplace/listings", "",
		`{"kind":"skill","title":"Terse","price_per_use_ulxc":10000000,"artifact":{"instructions":"Answer in one word.","model":"gpt-5-mini"}}`)
	if pub.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", pub.Code, pub.Body)
	}
	var listing market.Listing
	_ = json.Unmarshal(pub.Body.Bytes(), &listing)
	ask := func(q string) *httptest.ResponseRecorder {
		return call(buyer, "/v1/proxy/openai/v1/chat/completions", listing.ID,
			`{"model":"gpt-5-mini","stream":true,"messages":[{"role":"user","content":"`+q+`"}]}`)
	}
	billed := func() (n int, price int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(price_ulxc), 0) FROM market_uses
			WHERE listing_id = $1 AND buyer_workspace_id = $2 AND charge = 'billed' AND ran_at IS NOT NULL AND metered_at IS NOT NULL`,
			listing.ID, buyer).Scan(&n, &price); err != nil {
			t.Fatal(err)
		}
		return n, price
	}

	for _, q := range []string{"yes?", "no?"} {
		w := ask(q)
		if w.Code != http.StatusOK || w.Header().Get(listingChargeHeader) != market.ChargeBilled || !strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatalf("%q asked through the skill = %d, charge %q, body %q; want the stream, marked billed", q, w.Code, w.Header().Get(listingChargeHeader), w.Body)
		}
		got := sent[len(sent)-1]
		if len(got) != 2 || got[0] != (market.Message{Role: "system", Content: "Answer in one word."}) || got[1].Content != q {
			t.Errorf("the model was sent %+v, want the skill's instructions and then the question", got)
		}
	}
	if n, price := billed(); n != 2 || price != 20_000_000 || len(stripeFake.events) != 2 {
		t.Fatalf("two questions: %d billed uses for %d µLXC, %d meter events; want 2 for 20,000,000 and 2 events", n, price, len(stripeFake.events))
	}

	// A model call that fails is relayed as it is, and charges nothing.
	if w := ask("fail"); w.Code != http.StatusBadGateway || w.Header().Get(listingChargeHeader) != "" {
		t.Errorf("a failed call = %d, charge %q; want 502 and no charge", w.Code, w.Header().Get(listingChargeHeader))
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE listing_id = $1`, listing.ID).Scan(&rows); err != nil || rows != 2 || len(stripeFake.events) != 2 {
		t.Errorf("after a failed call: %d use rows (err %v), %d meter events; want still 2 and 2", rows, err, len(stripeFake.events))
	}

	// A request naming no listing is the proxy's alone.
	if w := call(buyer, "/v1/proxy/openai/v1/chat/completions", "", `{"model":"gpt-5-mini","messages":[{"role":"user","content":"hi"}]}`); w.Code != http.StatusOK ||
		len(sent[len(sent)-1]) != 1 || w.Header().Get(listingChargeHeader) != "" {
		t.Errorf("no listing: %d, sent %+v; want the request untouched", w.Code, sent[len(sent)-1])
	}
}
