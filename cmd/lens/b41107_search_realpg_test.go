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
)

// B41.107 — the route B41.7's search box reads: searching a supplier's name returns its payee, its bills (one read
// under another spelling but matched to the payee) and the payment made to it, grouped and each with its own id; an
// agent's rules come back under its name; another workspace's Acme never does.
func TestSearch_ASuppliersNameFindsItsPayeeBillsAndPayments(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, other = "ws-search", "ws-search-other"
	store := economy.NewDualTokenStore(nil, pool, nil)
	buyer, err := store.CreateAgent(ctx, ws, "Acme buyer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, buyer.ID, economy.AgentRules{ApprovalAboveULXC: 10_000_000}); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO money_payees (id, workspace_id, name, country, check_result, checked_by) VALUES
			('payee_acme', 'ws-search', 'Acme Supplies Ltd', 'GB', 'exact_match', 'test'),
			('payee_bolt', 'ws-search', 'Bolt Hire', 'GB', 'exact_match', 'test'),
			('payee_theirs', 'ws-search-other', 'Acme Supplies Ltd', 'GB', 'exact_match', 'test')`,
		`INSERT INTO money_bills (id, workspace_id, source, document_sha256, supplier, invoice_number, amount_minor, currency,
			due_date, payee_id, payee_match) VALUES
			('bill_1', 'ws-search', 'upload', 'd1', 'ACME SUPPLIES', 'A-101', 4500, 'GBP', '2026-11-05', 'payee_acme', 'matched'),
			('bill_2', 'ws-search', 'upload', 'd2', 'A.S. Ltd', 'A-102', 1200, 'GBP', '2026-11-20', 'payee_acme', 'matched'),
			('bill_3', 'ws-search', 'upload', 'd3', 'Bolt Hire', 'B-1', 900, 'GBP', '2026-11-06', 'payee_bolt', 'matched'),
			('bill_4', 'ws-search-other', 'upload', 'd4', 'Acme Supplies', 'X-1', 100, 'GBP', '2026-11-06', '', 'new_payee')`,
		`INSERT INTO money_accounts (id, workspace_id, currency, purpose) VALUES ('macc_co', 'ws-search', 'GBP', 'company'),
			('macc_hold', 'ws-search', 'GBP', 'hold')`,
		`INSERT INTO money_payments (id, workspace_id, idempotency_key, account_id, hold_account_id, payee_id, payee_name,
			amount_minor, currency, funding, reference, status) VALUES
			('mpay_acme', 'ws-search', 'k1', 'macc_co', 'macc_hold', 'payee_acme', 'Acme Supplies Ltd', 3000, 'GBP', 'test', 'Oct order', 'completed'),
			('mpay_bolt', 'ws-search', 'k2', 'macc_co', 'macc_hold', 'payee_bolt', 'Bolt Hire', 500, 'GBP', 'test', '', 'completed')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	r := chi.NewRouter()
	mountMoneySearchRoutes(r, store)
	search := func(q string, actx *auth.AuthContext) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/money/search?q="+q, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), actx))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}

	w := search("acme", owner)
	if w.Code != http.StatusOK {
		t.Fatalf("search = %d %s", w.Code, w.Body.String())
	}
	var got economy.SearchResults
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	ids := map[string][]string{}
	var kinds []string
	for _, g := range got.Groups {
		kinds = append(kinds, g.Kind)
		for _, res := range g.Results {
			ids[g.Kind] = append(ids[g.Kind], res.ID)
		}
	}
	want := map[string][]string{"agent": {buyer.ID}, "payee": {"payee_acme"}, "payment": {"mpay_acme"}, "bill": {"bill_1", "bill_2"}, "rule": {buyer.ID}}
	if len(kinds) != 5 || kinds[0] != "agent" || kinds[1] != "payee" || kinds[2] != "payment" || kinds[3] != "bill" || kinds[4] != "rule" {
		t.Errorf("groups = %v, want agent, payee, payment, bill, rule in that order", kinds)
	}
	for k, v := range want {
		if len(ids[k]) != len(v) || (len(v) > 0 && ids[k][0] != v[0]) || (len(v) == 2 && ids[k][1] != v[1]) {
			t.Errorf("%s results = %v, want %v", k, ids[k], v)
		}
	}
	if b := got.Groups[3].Results[0]; b.AmountMinor != 4500 || b.Currency != "GBP" || b.Title != "ACME SUPPLIES" {
		t.Errorf("first bill = %+v, want ACME SUPPLIES £45.00", b)
	}

	if w := search("a", owner); w.Code != http.StatusBadRequest {
		t.Errorf("a one-letter search = %d, want 400", w.Code)
	}
	if w := search("acme", &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-agent"}); w.Code != http.StatusForbidden {
		t.Errorf("a key without the keys scope searched the workspace: %d", w.Code)
	}
}
