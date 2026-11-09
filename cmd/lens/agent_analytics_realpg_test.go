package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.326 — an agent's spend by day, model and destination: days in its rules' time zone, a hold's unused part
// returned on the day it came back, and buckets whose out_ulxc sum to the out_ulxc of its statement for the period.
func TestAgentAnalyticsRoute_BucketsSumToTheStatementsOut(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b28326-routes"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	get := func(path string, out any) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("GET %s: %v\n%s", path, err, w.Body.String())
			}
		}
		return w.Code
	}
	a, err := store.CreateAgent(ctx, ws, "Research", "owner")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateAgent(ctx, ws, "Writer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, a.ID, economy.AgentRules{Timezone: "Europe/London"}); err != nil {
		t.Fatal(err)
	}
	// One entry: amount leaves the agent for other (negative) or comes to it from other.
	post := func(at, kind, model, other string, amount int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `WITH e AS (SELECT gen_random_uuid() AS id)
			INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref, model, created_at)
			SELECT e.id, $1, l.account, l.sign * $2::bigint, $3, '', NULLIF($4, ''), $5::timestamptz
			  FROM e CROSS JOIN (VALUES ('agent:' || $6, 1), ($7, -1)) AS l(account, sign)`,
			ws, amount, kind, model, at, a.ID, other); err != nil {
			t.Fatal(err)
		}
	}
	post("2026-10-18T12:00:00Z", "hold", "gpt-5", "held", -999) // before the period
	post("2026-10-19T09:00:00Z", "fund", "", "workspace", 10_000_000)
	// Two questions at 23:30 UTC on 19 October, which is 20 October in London: each held 1,000, settled 900, fee 5.
	for _, at := range []string{"2026-10-19T23:30:00Z", "2026-10-19T23:31:00Z"} {
		post(at, "hold", "claude-sonnet-5", "held", -1_000)
		post(at, "settle", "claude-sonnet-5", "held", 100)
		post(at, economy.LXCTypePlatformFee, "claude-sonnet-5", "spend", -5)
	}
	post("2026-10-20T10:00:00Z", "pay", "", "agent:"+b.ID, -50_000)
	post("2026-10-21T12:00:00Z", "card", "", "spend", -2_000)

	period := "?from=2026-10-19&to=2026-10-22"
	var got economy.SpendAnalytics
	if code := get("/v1/workspaces/"+ws+"/agents/"+a.ID+"/analytics"+period, &got); code != http.StatusOK {
		t.Fatalf("analytics = %d", code)
	}
	want := []economy.SpendBucket{
		{Day: "2026-10-20", Destination: "agent:" + b.ID, OutULXC: 50_000, SpentULXC: 50_000, Postings: 1},
		{Day: "2026-10-20", Model: "claude-sonnet-5", Destination: "held", OutULXC: 2_000, ReturnedULXC: 200, SpentULXC: 1_800, Postings: 4},
		{Day: "2026-10-20", Model: "claude-sonnet-5", Destination: "spend", OutULXC: 10, SpentULXC: 10, Postings: 2},
		{Day: "2026-10-21", Destination: "spend", OutULXC: 2_000, SpentULXC: 2_000, Postings: 1},
	}
	if got.TimeZone != "Europe/London" || !reflect.DeepEqual(got.Buckets, want) {
		t.Fatalf("analytics in %q =\n%+v\nwant Europe/London and\n%+v", got.TimeZone, got.Buckets, want)
	}

	// The DONE line of B28.53: the buckets sum to the agent's total out on its statement for the same period.
	var st economy.Statement
	if code := get("/v1/workspaces/"+ws+"/agents/"+a.ID+"/statement"+period, &st); code != http.StatusOK || len(st.Accounts) != 1 {
		t.Fatalf("statement = %d, accounts %+v", code, st.Accounts)
	}
	var sum int64
	for _, bk := range got.Buckets {
		sum += bk.OutULXC
	}
	if out := st.Accounts[0].OutULXC; sum != out || got.OutULXC != out || got.SpentULXC != out-got.ReturnedULXC {
		t.Fatalf("buckets sum to %d, analytics say out %d spent %d returned %d; the statement's out is %d",
			sum, got.OutULXC, got.SpentULXC, got.ReturnedULXC, out)
	}

	if code := get("/v1/workspaces/"+ws+"/agents/agt_nobody/analytics"+period, &got); code != http.StatusNotFound {
		t.Fatalf("an agent the workspace does not have = %d; want 404", code)
	}
}
