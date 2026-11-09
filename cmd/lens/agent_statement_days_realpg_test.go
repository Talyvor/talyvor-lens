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
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B57.13 — ?group=day counts an agent's days in its rules' time zone and says which, the workspace's in UTC; ?limit=
// pages; the CSV stays one row per posting.
func TestAgentStatementRoutes_GroupByDayInTheAgentsZone(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5713-routes"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	get := func(path string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	a, err := store.CreateAgent(ctx, ws, "Research", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, a.ID, economy.AgentRules{Timezone: "Europe/London"}); err != nil {
		t.Fatal(err)
	}
	// Two questions to one model at 23:30 UTC on 19 October: 20 October in London.
	if _, err := pool.Exec(ctx, `WITH e AS (SELECT gen_random_uuid() AS id, g FROM generate_series(1, 2) g)
		INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref, model, created_at)
		SELECT e.id, $1, l.account, l.amount, 'spend', 'req' || e.g, 'claude-sonnet-5', '2026-10-19T23:30:00Z'::timestamptz + e.g * interval '1 second'
		  FROM e CROSS JOIN (VALUES ('agent:' || $2, -700), ('spend', 700)) AS l(account, amount)`, ws, a.ID); err != nil {
		t.Fatal(err)
	}
	period := "?from=2026-10-19&to=2026-10-22"
	var st economy.Statement
	read := func(path, zone, day string) {
		t.Helper()
		code, body := get(path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
		st = economy.Statement{}
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		if st.TimeZone != zone || len(st.Lines) == 0 || st.Lines[0].Day != day || st.Lines[0].Requests != 2 {
			t.Fatalf("GET %s: zone %q, lines %+v; want %s, first line %s with 2 requests", path, st.TimeZone, st.Lines, zone, day)
		}
	}
	read("/v1/workspaces/"+ws+"/agents/"+a.ID+"/statement"+period+"&group=day&limit=1", "Europe/London", "2026-10-20")
	if len(st.Lines) != 1 || st.NextCursor != "" {
		t.Fatalf("the agent's one line came with cursor %q", st.NextCursor)
	}
	read("/v1/workspaces/"+ws+"/agents/statement"+period+"&group=day", "UTC", "2026-10-19")
	if code, body := get("/v1/workspaces/" + ws + "/agents/" + a.ID + "/statement" + period + "&group=day&format=csv"); code != http.StatusOK || strings.Count(body, "\n") != 5 {
		t.Fatalf("CSV by day = %d\n%s\nwant a header, the opening, both postings and the closing", code, body)
	}
	if code, _ := get("/v1/workspaces/" + ws + "/agents/" + a.ID + "/statement" + period + "&group=week"); code != http.StatusBadRequest {
		t.Fatalf("group=week = %d; want 400", code)
	}
}
