package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// B32.102 (for B17.164's market-trust) — the synthetic card link. Two test workspaces linked by it read as one party:
// the linked buyer, with a billed use of the seller's listing, is refused reviewing it and never counted by the trust
// panel, while an unlinked paying buyer is. A real workspace on either side is 403 with no row written. Wired as
// main.go wires it, on the migrated schema.
func TestB32102_ASyntheticCardLinkMakesATestBuyerOneWithTheSeller(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const syntheticKey = "b32102-synthetic-key"
	marketStore := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountSyntheticRoutes(r, syntheticKey, syntheticDeps{workspaces: workspace.New(pool), credits: bank, answers: storedanswers.New(pool, nil),
		audit: pool, mint: func(ws, _ string, _ []string, _ time.Duration) (string, error) { return "tok-" + ws, nil },
		due: syntheticDueDeps{db: pool, loans: bank, bills: marketStore, payouts: marketStore, cards: bank}})
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
	if code := synthetic("/v1/synthetic/workspaces", `{"count":3}`, &made); code != http.StatusCreated || len(made.Workspaces) != 3 {
		t.Fatalf("make three test users = %d %+v", code, made)
	}
	seller, linked, honest := made.Workspaces[0].WorkspaceID, made.Workspaces[1].WorkspaceID, made.Workspaces[2].WorkspaceID
	fingerprints := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_card_fingerprints`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if code := synthetic("/v1/synthetic/workspaces/"+linked+"/card-link", `{"workspace_id":"`+linked+`"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("linking a workspace to itself = %d, want 400", code)
	}
	const real = "ws-real-b32102"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, real); err != nil {
		t.Fatal(err)
	}
	if code := synthetic("/v1/synthetic/workspaces/"+linked+"/card-link", `{"workspace_id":"`+real+`"}`, nil); code != http.StatusForbidden {
		t.Fatalf("linking a test workspace to a real one = %d, want 403", code)
	}
	if code := synthetic("/v1/synthetic/workspaces/"+real+"/card-link", `{"workspace_id":"`+seller+`"}`, nil); code != http.StatusForbidden {
		t.Fatalf("linking a real workspace to a test one = %d, want 403", code)
	}
	if n := fingerprints(); n != 0 {
		t.Fatalf("the refused links wrote %d fingerprints, want none", n)
	}
	var link struct {
		WorkspaceID string `json:"workspace_id"`
		LinkedTo    string `json:"linked_to"`
	}
	if code := synthetic("/v1/synthetic/workspaces/"+linked+"/card-link", `{"workspace_id":"`+seller+`"}`, &link); code != http.StatusOK ||
		link.WorkspaceID != linked || link.LinkedTo != seller || fingerprints() != 2 {
		t.Fatalf("linking the buyer to the seller = %d %+v with %d fingerprints; want 200 naming both, one on each", code, link, fingerprints())
	}

	// Both buyers paid for a use of the seller's listing; only the unlinked one may review it, and only its review counts.
	if _, err := pool.Exec(ctx, `INSERT INTO market_listings (id, workspace_id, kind, title) VALUES ('lst_b32102', $1, 'prompt', 'Trusted')`, seller); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256) VALUES ('lst_b32102', 1, '{"template": "x"}', 'sha')`); err != nil {
		t.Fatal(err)
	}
	for _, buyer := range []string{linked, honest} {
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc, charge, used_at, ran_at)
			VALUES ($1, 'lst_b32102', 1, $2, $3, 1000000, 'billed', now(), now())`, "use_b32102_"+buyer, seller, buyer); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := marketStore.Review(ctx, linked, "lst_b32102", market.ReviewInput{Rating: 5, Text: "My own listing."}); !errors.Is(err, market.ErrNotReviewer) {
		t.Fatalf("the linked buyer reviewing = %v; want ErrNotReviewer (403)", err)
	}
	if _, err := marketStore.Review(ctx, honest, "lst_b32102", market.ReviewInput{Rating: 3, Text: "Fine."}); err != nil {
		t.Fatalf("the unlinked paying buyer reviewing: %v", err)
	}
	tr, err := marketStore.Trust(ctx, real, "lst_b32102")
	if err != nil {
		t.Fatal(err)
	}
	if rv := tr.Reviews; rv.Count != 1 || rv.Average != 3 || len(rv.Recent) != 1 {
		t.Fatalf("the trust panel = %+v; want the unlinked buyer's 3 alone", rv)
	}
}
