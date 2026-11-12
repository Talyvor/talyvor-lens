package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.330 — /forecast gives the day each agent runs out as a date in the agent's own time zone, and that zone, so a
// screen shows the agent's date and never re-reads the instant in the viewer's. Both run-outs below fall on a
// different date in UTC than in the agent's zone: Sydney is a day ahead of UTC, Los Angeles a day behind; an agent
// with no zone set is in UTC.
func TestAgentForecast_GivesRunsOutOnAsTheAgentsDateNotUTCs(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-forecast-runs-out-on"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	agent := func(name string) string {
		a, err := store.CreateAgent(ctx, ws, name, "owner")
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	sydney, la, payee := agent("sydney"), agent("los-angeles"), agent("payee")
	for _, s := range []struct {
		id         string
		fund, paid int64
	}{{sydney, 5_100_000, 4_000_000}, {la, 5_000_000, 1_000_000}} {
		if _, err := store.FundAgent(ctx, ws, s.id, s.fund); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PayAgent(ctx, ws, s.id, payee, s.paid, "run it down"); err != nil {
			t.Fatal(err)
		}
	}
	// All spent on 5 April; the forecast is as of 11 April 00:00 UTC, ten days into the month.
	// sydney: 1.1 LXC left at 4 LXC per 10 days lasts 2.75 days, to 13 April 18:00 UTC = 14 April 04:00 in Sydney.
	// la: 4 LXC left at 1 LXC per 10 days lasts 40 days, to 21 May 00:00 UTC = 20 May 17:00 in Los Angeles.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`ALTER TABLE agent_postings DISABLE TRIGGER audit_no_mutation`,
		`UPDATE agent_postings SET created_at = '2026-04-05T00:00:00Z' WHERE workspace_id = '` + ws + `'`,
		`ALTER TABLE agent_postings ENABLE TRIGGER audit_no_mutation`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for id, tz := range map[string]string{sydney: "Australia/Sydney", la: "America/Los_Angeles"} {
		if _, err := pool.Exec(ctx, `INSERT INTO agent_rules (agent_id, workspace_id, timezone) VALUES ($1, $2, $3)`, id, ws, tz); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/forecast?at=2026-04-11T00:00:00Z", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET forecast = %d %s", w.Code, w.Body.String())
	}
	var f struct {
		Agents []map[string]json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		`"` + sydney + `"`: {`"2026-04-14"`, `"Australia/Sydney"`},
		`"` + la + `"`:     {`"2026-05-20"`, `"America/Los_Angeles"`},
		`"` + payee + `"`:  {`null`, `"UTC"`},
	}
	for _, a := range f.Agents {
		id := string(a["agent_id"])
		got, ok := want[id]
		if !ok {
			continue
		}
		if string(a["runs_out_on"]) != got[0] || string(a["timezone"]) != got[1] {
			t.Errorf("agent %s: runs_out_on %q in %q; want %s in %s (runs_out_at %s)", id, a["runs_out_on"], a["timezone"], got[0], got[1], a["runs_out_at"])
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("forecast does not list %v: %s", want, w.Body.String())
	}
}
