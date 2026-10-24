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
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/market"
)

// B32.46 — a 97%-identical copy of another seller's listing is held naming it; declaring that listing as a parent, with
// a remix grant, publishes the copy approved with its edge; the publisher's own near-copy is not held; and a new version
// that copies another seller's listing holds its listing too. CI fingerprints with the offline hashed embedder.
func TestMarketSimilarity_AnUndeclaredCopyIsHeldNamingTheOriginal(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, copier, stranger = "ws-sim-seller", "ws-sim-copier", "ws-sim-stranger"
	store := market.NewStore(pool)
	store.SetSimilarity(embedder.NewHashedEmbedder(), market.DefaultSimilarityHold)
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	call := func(ws, method, path string, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	const prompt = "You are Harbourmaster, a careful logistics planner for small shipping firms. Read the manifest the user sends, " +
		"check every container weight against the vessel limits, flag hazardous cargo by its UN number, and propose a loading " +
		"order that keeps the heaviest boxes low and central. Explain each decision in one plain sentence. When a figure is " +
		"missing, ask for it instead of guessing. Never invent a port, a tariff or a customs rule. Finish with a short " +
		"checklist the crew can tick off at the quay, and a single line estimating how long loading will take in hours."
	// Three words of a hundred changed.
	edited := func(pairs ...string) string { return strings.NewReplacer(pairs...).Replace(prompt) }
	copied := edited("careful", "meticulous", "small", "regional", "short", "brief")
	publish := func(ws, title, systemPrompt string, extra map[string]any) market.Listing {
		t.Helper()
		body := map[string]any{"kind": "agent", "title": title, "description": "Plans how a ship is loaded.", "visibility": "public",
			"artifact": map[string]any{"system_prompt": systemPrompt, "model": "gpt-4o-mini"}}
		for k, v := range extra {
			body[k] = v
		}
		code, out := call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/marketplace/listings", body)
		var l market.Listing
		if err := json.Unmarshal([]byte(out), &l); code != http.StatusCreated || err != nil {
			t.Fatalf("publish %q = %d %s", title, code, out)
		}
		return l
	}

	original := publish(seller, "Harbourmaster", prompt, map[string]any{"remix_policy": "royalty", "remix_share_bps": 1000})
	if original.ReviewStatus != market.ReviewApproved {
		t.Fatalf("the original = %s %q, want approved", original.ReviewStatus, original.ReviewReason)
	}

	// The undeclared copy is held, naming the original, its score, and that it may be remixed.
	held := publish(copier, "Cargo planner", copied, nil)
	sim := held.Versions[0].Scan.Similar
	if held.ReviewStatus != market.ReviewHeld || sim == nil || sim.ListingID != original.ID || sim.Score < market.DefaultSimilarityHold ||
		!sim.Remixable || !strings.Contains(held.ReviewReason, original.ID) || !strings.Contains(held.ReviewReason, "declaring it as a parent") {
		t.Fatalf("the copy = %s %q similar %+v, want held naming %s as remixable", held.ReviewStatus, held.ReviewReason, sim, original.ID)
	}

	// Declared as a parent under a remix grant, the copy publishes approved with its edge at the locked share.
	if code, out := call(copier, http.MethodPost, "/v1/workspaces/"+copier+"/marketplace/listings/"+original.ID+"/remix", map[string]any{"version": 1}); code != http.StatusOK {
		t.Fatalf("remix = %d %s", code, out)
	}
	remix := publish(copier, "Cargo planner, declared", copied, map[string]any{"parents": []map[string]any{{"listing_id": original.ID}}})
	var edge int
	if err := pool.QueryRow(ctx, `SELECT share_bps FROM market_lineage WHERE child_listing_id = $1 AND parent_listing_id = $2`,
		remix.ID, original.ID).Scan(&edge); remix.ReviewStatus != market.ReviewApproved || remix.Versions[0].Scan.Similar != nil || err != nil || edge != 1000 {
		t.Fatalf("the declared copy = %s %q, edge %d (%v), want approved with a 1000 bps edge", remix.ReviewStatus, remix.ReviewReason, edge, err)
	}

	// The seller's own near-copy is not held — not by its original, nor by the remix of it.
	own := publish(seller, "Quaymaster", edited("Harbourmaster", "Quaymaster", "plain", "simple", "hours", "minutes"), nil)
	if own.ReviewStatus != market.ReviewApproved || own.Versions[0].Scan.Similar != nil {
		t.Fatalf("the seller's own near-copy = %s %q, want approved", own.ReviewStatus, own.ReviewReason)
	}

	// A new version that copies another seller's listing is held too; version 1 stays live (B17.206).
	other := publish(stranger, "Recipe converter", "Convert every quantity in the recipe the user sends into grams and millilitres.", nil)
	code, out := call(stranger, http.MethodPost, "/v1/workspaces/"+stranger+"/marketplace/listings/"+other.ID+"/versions",
		map[string]any{"artifact": map[string]any{"system_prompt": edited("careful", "thorough", "small", "coastal", "short", "concise"), "model": "gpt-4o-mini"}})
	var v market.Version
	if err := json.Unmarshal([]byte(out), &v); code != http.StatusCreated || err != nil || v.Scan.Similar == nil ||
		v.Scan.Similar.ListingID != original.ID || !strings.Contains(v.Scan.Held, original.ID) {
		t.Fatalf("version 2 copying the original = %d %s, want held naming %s", code, out, original.ID)
	}
	var status, reason string
	var heldFrom int
	if err := pool.QueryRow(ctx, `SELECT review_status, review_reason, held_version FROM market_listings WHERE id = $1`, other.ID).Scan(&status, &reason, &heldFrom); err != nil ||
		status != market.ReviewApproved || heldFrom != 2 || !strings.HasPrefix(reason, "version 2: it is ") {
		t.Fatalf("the listing after version 2 = %s %q held from %d (%v), want version 2 held", status, reason, heldFrom, err)
	}
}
