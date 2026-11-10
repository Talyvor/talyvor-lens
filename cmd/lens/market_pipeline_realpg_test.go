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

// B20.7 — a buyer uses a two-step pipeline listing and gets the second step's answer to the first step's
// output. The pipeline is one use at its price, covering its seller's own step; the step that is another
// seller's listing is its own billed use, so that seller is paid too.
func TestMarketUse_APipelineRunsItsStepsInOrderAndPaysEachSeller(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, translatorCo, buyer = "ws-pipe-seller", "ws-pipe-translator", "ws-pipe-buyer"

	billFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, billFake, "whsec_pipeline").WithMarketBill(billFake, "price_market", "talyvor_marketplace_use", store)
	// Lens's own proxy: each model answers model(what it was asked).
	lens := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model    string           `json:"model"`
			Messages []market.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{
			"role": "assistant", "content": in.Model + "(" + in.Messages[len(in.Messages)-1].Content + ")"}}}})
		_, _ = w.Write(out)
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketUseRoutes(r, store, lens, svc, bank)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	publish := func(ws, body string) market.Listing {
		t.Helper()
		code, out := call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/marketplace/listings", body)
		var l market.Listing
		if _ = json.Unmarshal([]byte(out), &l); code != http.StatusCreated || l.ReviewStatus != market.ReviewApproved {
			t.Fatalf("publish = %d %s", code, out)
		}
		return l
	}
	summarise := publish(seller, `{"kind":"prompt","title":"One-line summary","price_per_use_ulxc":20000,"artifact":{"template":"Summarise in one line: {{text}}","model":"sum"}}`)
	french := publish(translatorCo, `{"kind":"skill","title":"French","price_per_use_ulxc":30000,"artifact":{"instructions":"Translate into French.","model":"fr"}}`)
	pipeline := publish(seller, `{"kind":"pipeline","title":"Summary in French","price_per_use_ulxc":50000,"artifact":{"steps":[{"use":"prompt:`+summarise.ID+`"},{"use":"listing:`+french.ID+`"}]}}`)

	code, out := call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+pipeline.ID+"/use", `{"input":"the Q3 report"}`)
	var u market.Use
	if _ = json.Unmarshal([]byte(out), &u); code != http.StatusOK {
		t.Fatalf("the pipeline's use = %d %s", code, out)
	}
	const first = "sum(Summarise in one line: the Q3 report)"
	if u.Output != "fr("+first+")" || len(u.Steps) != 2 || u.Steps[0].Output != first || u.Steps[0].UseID != "" || u.Steps[1].UseID == "" {
		t.Fatalf("the pipeline answered %q with steps %+v; want the French step's answer to the summary", u.Output, u.Steps)
	}

	// One use of the pipeline at its price, one of the other seller's step at its price, none of the
	// pipeline seller's own step; each billed once.
	rows, err := pool.Query(ctx, `SELECT listing_id || ' ' || seller_workspace_id || ' ' || price_ulxc || ' ' || charge || ' ' || (ran_at IS NOT NULL)::text
		FROM market_uses WHERE buyer_workspace_id = $1 ORDER BY price_ulxc DESC`, buyer)
	if err != nil {
		t.Fatal(err)
	}
	var uses []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		uses = append(uses, s)
	}
	rows.Close()
	want := []string{pipeline.ID + " " + seller + " 50000 billed true", french.ID + " " + translatorCo + " 30000 billed true"}
	if len(uses) != 2 || uses[0] != want[0] || uses[1] != want[1] {
		t.Fatalf("market_uses = %q, want %q", uses, want)
	}
	if len(billFake.events) != 2 || billFake.events[0].identifier != u.ID || billFake.events[0].value != 50000 ||
		billFake.events[1].identifier != u.Steps[1].UseID || billFake.events[1].value != 30000 {
		t.Fatalf("meter events = %+v, want the pipeline's 50,000 and the French step's 30,000", billFake.events)
	}

	// A step names a listing by its id ("listing:lst_…"); a pipeline naming anything else cannot be run, and
	// bills nothing.
	loose := publish(seller, `{"kind":"pipeline","title":"Loose","price_per_use_ulxc":50000,"artifact":{"steps":[{"use":"prompt:summarise"}]}}`)
	if code, out := call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+loose.ID+"/use", `{"input":"x"}`); code != http.StatusBadRequest ||
		!strings.Contains(out, `A step's \"use\" is \"listing:\u003ca listing's id\u003e\"`) {
		t.Fatalf("a pipeline whose step names no listing = %d %s, want 400", code, out)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE buyer_workspace_id = $1`, buyer).Scan(&n); err != nil || n != 2 {
		t.Fatalf("after the refused use, market_uses = %d (%v), want still 2", n, err)
	}
}
