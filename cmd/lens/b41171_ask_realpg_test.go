package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

// B41.171 — asking "translate 500 product descriptions into German" returns up to three listings, each with an
// estimated total for 500 uses on its cheapest commercial offer and why it fits; a listing that does something else
// is not among them, and a volume given beats the number in the words.
func TestMarketAsk_TopThreeWithEstimates(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	store := market.NewStore(pool)
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketDiscoveryRoutes(r, store)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	const seller, buyer = "ws-b41171-seller", "ws-b41171-buyer"
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(title, description, offers string, caps ...string) string {
		t.Helper()
		capsJSON, _ := json.Marshal(append([]string{}, caps...))
		code, body := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
			fmt.Sprintf(`{"kind":"prompt","title":%q,"description":%q,"artifact":{"template":"%s {{text}}"},"offers":%s,"capabilities":%s}`,
				title, description, title, offers, capsJSON))
		var l market.Listing
		if err := json.Unmarshal([]byte(body), &l); code != http.StatusCreated || err != nil {
			t.Fatalf("publish %s = %d %s", title, code, body)
		}
		return l.ID
	}
	// The estimate for 500 uses each should give.
	want := map[string]int64{
		publish("German product translator", "Translates product descriptions into German.",
			`[{"kind":"per_use","licence":"commercial","price_usd_micros":2000}]`, "translate"): 1_000_000, // 500 × $0.002
		publish("Catalogue translator", "Translate product descriptions for a shop catalogue.",
			`[{"kind":"per_use","licence":"commercial","price_usd_micros":5000},{"kind":"buy","licence":"commercial","price_usd_micros":300000}]`,
			"translate"): 300_000, // the purchase is cheaper than 500 uses
		publish("Description translator", "Translations of product descriptions, German included.",
			`[{"kind":"rent","licence":"commercial","price_usd_micros":100000,"period_days":30,"included_uses":200}]`,
			"translate"): 300_000, // three rentals of 200 uses
	}
	unrelated := publish("Invoice extractor", "Reads the totals off an invoice.", `[]`, "extract")

	ask := func(query string) market.AskAnswer {
		t.Helper()
		code, body := call(buyer, http.MethodGet, "/v1/marketplace/ask?"+query, "")
		var a market.AskAnswer
		if err := json.Unmarshal([]byte(body), &a); code != http.StatusOK || err != nil {
			t.Fatalf("ask %s = %d %s", query, code, body)
		}
		return a
	}
	a := ask("q=" + url.QueryEscape("translate 500 product descriptions into German"))
	if a.Volume != 500 || a.VolumeFrom != market.VolumeInWords {
		t.Fatalf("volume %d from %q; want 500 from the words", a.Volume, a.VolumeFrom)
	}
	if len(a.Picks) != 3 {
		t.Fatalf("%d picks; want the three translators: %+v", len(a.Picks), a.Picks)
	}
	for _, p := range a.Picks {
		total, ok := want[p.ID]
		if p.ID == unrelated || !ok {
			t.Fatalf("picked %s (%s); want only the translators", p.ID, p.Title)
		}
		if p.Estimate == nil || p.Estimate.Volume != 500 || p.Estimate.TotalUSDMicros != total {
			t.Fatalf("%s: estimate %+v; want %d µUSD for 500 uses", p.Title, p.Estimate, total)
		}
		why := strings.Join(p.Why, " ")
		if !strings.Contains(why, "It declares it can Translate.") || !strings.Contains(why, "product") {
			t.Fatalf("%s: why %q; want its Translate capability and the job's words it has", p.Title, why)
		}
	}
	if b := ask("q=" + url.QueryEscape("translate 500 product descriptions into German") + "&volume=1000"); b.Volume != 1000 ||
		b.VolumeFrom != market.VolumeAsked || b.Picks[0].Estimate.Volume != 1000 {
		t.Fatalf("volume=1000: %d from %q, first estimate %+v; want 1000 as asked", b.Volume, b.VolumeFrom, b.Picks[0].Estimate)
	}
	if code, body := call(buyer, http.MethodGet, "/v1/marketplace/ask?q=", ""); code != http.StatusBadRequest {
		t.Fatalf("an empty job = %d %s; want 400", code, body)
	}
}
