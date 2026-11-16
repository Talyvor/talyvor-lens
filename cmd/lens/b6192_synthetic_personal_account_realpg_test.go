package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// B61.92 — the testers make a synthetic Personal account: "account_type": "personal" makes each workspace a Personal
// account on Solo, which GET /v1/workspaces/{ws}/plan reads back; "plan" names Crew or Fleet and is refused 400 in
// words for a business plan, before any workspace is made; and a call with no account_type still makes a Business on
// Studio.
func TestB6192_TheTestersMakeASyntheticPersonalAccount(t *testing.T) {
	pool := syntheticDB(t)
	r := chi.NewRouter()
	mountSyntheticRoutes(r, "the-key", syntheticDeps{workspaces: workspace.New(pool), credits: economy.NewDualTokenStore(nil, pool, nil),
		answers: storedanswers.New(pool, nil), audit: pool, plans: pool,
		mint: func(workspaceID, _ string, _ []string, _ time.Duration) (string, error) {
			return "tok-" + workspaceID, nil
		}})
	r.Get("/v1/workspaces/{wsID}/plan", newWorkspacePlanHandler(pool))
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(syntheticKeyHeader, "the-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	create := func(body string) (string, map[string]any) {
		t.Helper()
		code, out := call(http.MethodPost, "/v1/synthetic/workspaces", body)
		users, _ := out["workspaces"].([]any)
		if code != http.StatusCreated || len(users) != 1 {
			t.Fatalf("create %s = %d %v, want 201 with one workspace", body, code, out)
		}
		return users[0].(map[string]any)["workspace_id"].(string), out
	}
	readPlan := func(ws, accountType, plan string) {
		t.Helper()
		code, out := call(http.MethodGet, "/v1/workspaces/"+ws+"/plan", "")
		if code != http.StatusOK || out["account_type"] != accountType || out["plan"] != plan {
			t.Fatalf("GET %s's plan = %d %v, want %s on %s", ws, code, out, accountType, plan)
		}
	}

	ws, out := create(`{"count":1,"account_type":"personal"}`)
	if out["account_type"] != "personal" || out["plan"] != "personal_solo" {
		t.Fatalf("a personal account's create = %v, want account_type personal on personal_solo", out)
	}
	readPlan(ws, "personal", "personal_solo")

	ws, _ = create(`{"count":1,"account_type":"personal","plan":"personal_fleet"}`)
	readPlan(ws, "personal", "personal_fleet")

	var before int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workspaces WHERE synthetic`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	code, refused := call(http.MethodPost, "/v1/synthetic/workspaces", `{"count":1,"account_type":"personal","plan":"business_company"}`)
	if msg, _ := refused["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "personal_solo") {
		t.Fatalf("a personal account on business_company = %d %v, want 400 naming the personal ladder", code, refused)
	}
	var after int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workspaces WHERE synthetic`).Scan(&after); err != nil || after != before {
		t.Fatalf("the refused call left %d synthetic workspaces (%v), want %d: none made", after, err, before)
	}
	if code, out := call(http.MethodPost, "/v1/synthetic/workspaces", `{"count":1,"account_type":"robot"}`); code != http.StatusBadRequest {
		t.Fatalf("account_type robot = %d %v, want 400", code, out)
	}

	ws, out = create(`{"count":1}`)
	if out["account_type"] != "business" || out["plan"] != "business_studio" {
		t.Fatalf("a create with no account_type = %v, want a business on business_studio", out)
	}
	readPlan(ws, "business", "business_studio")
}
