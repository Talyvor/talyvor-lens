package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.332 — every wallet list (an agent's transfers, the approvals, the schedules, the earnings payouts) exports to
// CSV with ?format=csv: on the migrated schema, each CSV has one row per item of the same route's JSON list, in its
// order, and a memo a spreadsheet would run as a formula is written as text.
func TestB28332_EachWalletListsCSVHasOneRowPerItemOfItsJSONList(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, earner = "ws-b28332", "ws-b28332-earner"
	store, ag := b501Agents(t, pool, ws)
	for i, memo := range []string{"=HYPERLINK(\"x\")", "lunch", "fuel"} {
		if _, err := store.SendCredits(ctx, ws, ag[0].ID, ag[1+i%2].ID, int64(1_000_000*(i+1)), memo); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SetAgentRules(ctx, ws, ag[0].ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	for _, memo := range []string{"invoice A", "invoice B"} {
		if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, memo); !errors.Is(err, economy.ErrApprovalRequired) {
			t.Fatalf("%s: %v, want ErrApprovalRequired", memo, err)
		}
	}

	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentTransferRoutes(r, store)
	call := func(as, method, path, body string) (int, http.Header, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: as, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Header(), w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	first := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, every := range []string{"day", "week", "month"} {
		if code, _, body := call(ws, http.MethodPost, base+"/"+ag[0].ID+"/schedules",
			`{"to_agent_id":"`+ag[2].ID+`","amount_ulxc":100000,"memo":"`+every+`ly","every":"`+every+`","first_run_at":"`+first+`"}`); code != http.StatusCreated {
			t.Fatalf("create the %s schedule = %d %s", every, code, body)
		}
	}

	// The earnings payouts are another workspace's, made on the money layer's sandbox: a sale and a royalty.
	b := newB468Payer(t, pool, earner, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})
	b4611Verify(t, b.store, "ws-b28332-seller", "Sam Seller")
	for _, e := range []economy.Earnings{
		{Kind: economy.EarningsSale, Source: "sale:lic_b28332", AgentID: b.acct.AgentID, Currency: "GBP",
			Shares: []economy.EarningsShare{{WorkspaceID: "ws-b28332-seller", AmountMinor: 1500}}},
		{Kind: economy.EarningsRoyalty, Source: "royalty:lst_b28332", AgentID: b.acct.AgentID, Currency: "GBP",
			Shares: []economy.EarningsShare{{WorkspaceID: "ws-b28332-seller", AmountMinor: 250}}},
	} {
		b4611Pay(t, b.store, earner, e)
	}
	mountMoneyEarningsRoutes(r, b.store, nil)

	for _, l := range []struct{ as, path, key, file string }{
		{ws, base + "/" + ag[0].ID + "/transfers", "transfers", "transfers"},
		{ws, base + "/approvals", "approvals", "approvals"},
		{ws, base + "/schedules", "schedules", "schedules"},
		{earner, "/v1/money/earnings-payouts", "payouts", "earnings-payouts"},
	} {
		code, _, body := call(l.as, http.MethodGet, l.path, "")
		var js map[string][]map[string]any
		if err := json.Unmarshal([]byte(body), &js); code != http.StatusOK || err != nil {
			t.Fatalf("GET %s = %d %s", l.path, code, body)
		}
		list := js[l.key]
		if len(list) < 2 {
			t.Fatalf("GET %s listed %d %s, want the seeded ones", l.path, len(list), l.key)
		}
		code, h, body := call(l.as, http.MethodGet, l.path+"?format=csv", "")
		if code != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") ||
			h.Get("Content-Disposition") != `attachment; filename="`+l.file+`.csv"` {
			t.Fatalf("GET %s?format=csv = %d %v", l.path, code, h)
		}
		recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
		if err != nil {
			t.Fatalf("%s's CSV does not parse: %v\n%s", l.key, err, body)
		}
		if len(recs)-1 != len(list) {
			t.Fatalf("%s: the CSV has %d rows, the JSON list %d items\n%s", l.key, len(recs)-1, len(list), body)
		}
		col := map[string]int{}
		for i, name := range recs[0] {
			col[name] = i
		}
		idCol, ok := col["id"]
		if !ok {
			t.Fatalf("%s's CSV has no id column: %v", l.key, recs[0])
		}
		for i, item := range list {
			if recs[i+1][idCol] != item["id"] {
				t.Errorf("%s row %d is %s, the JSON list's item %d is %v: not in its order", l.key, i+1, recs[i+1][idCol], i, item["id"])
			}
		}
		if l.key == "transfers" {
			memos := map[string]bool{}
			for _, rec := range recs[1:] {
				memos[rec[col["memo"]]] = true
			}
			if !memos[`'=HYPERLINK("x")`] {
				t.Errorf("a formula memo is not written as text: %v", memos)
			}
		}
		if l.key == "approvals" {
			if _, ok := col["payee_name"]; !ok {
				t.Errorf("the approvals CSV does not flatten the payee: %v", recs[0])
			}
		}
	}
}
