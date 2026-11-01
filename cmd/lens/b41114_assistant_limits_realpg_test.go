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
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B41.114 — with Claude capped at £200 a day, a third £100 payment prepared through Claude that day is refused and
// not kept, while the same payment from the app is prepared; amounts that would wrap int64 past the limit are refused
// too; and the limit lists what Claude has prepared today.
func TestAssistantLimits_ClaudeCappedAppNot(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, claude = "ws-b41114-limits", "mcp:tlv_oc_claude"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	r := chi.NewRouter()
	mountPaymentRequestRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, key, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_plumber', $1, 'Ada Plumbing', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}

	if code, out := call(http.MethodPut, "/v1/money/assistant-limits", "", `{"client":"`+claude+`","currency":"gbp","per_day_minor":20000}`); code != http.StatusOK {
		t.Fatalf("setting Claude's limit = %d %v", code, out)
	}
	pay := func(client, key string) (int, map[string]any) {
		return call(http.MethodPost, "/v1/money/payment-requests", key,
			`{"client":"`+client+`","items":[{"payee_id":"payee_plumber","currency":"GBP","amount_minor":10000,"reference":"`+key+`"}]}`)
	}
	if code, out := call(http.MethodPost, "/v1/money/payment-requests", "claude-wrap", `{"client":"`+claude+`","items":[
		{"payee_id":"payee_plumber","currency":"GBP","amount_minor":9223372036854775807},{"payee_id":"payee_plumber","currency":"GBP","amount_minor":2}]}`); code != http.StatusForbidden {
		t.Fatalf("amounts that wrap past the int64 limit through Claude = %d %v, want 403", code, out)
	}
	for _, key := range []string{"claude-1", "claude-2"} {
		if code, out := pay(claude, key); code != http.StatusCreated {
			t.Fatalf("%s through Claude = %d %v, want 201", key, code, out)
		}
	}
	code, out := pay(claude, "claude-3")
	if code != http.StatusForbidden || !strings.Contains(out["error"].(string), "may prepare up to GBP 200.00 a day and has prepared GBP 200.00 today") {
		t.Fatalf("a third £100 through Claude = %d %v, want 403 over its limit", code, out)
	}
	if code, out := pay(economy.ClientChat, "app-1"); code != http.StatusCreated {
		t.Fatalf("the same payment from the app = %d %v, want 201", code, out)
	}

	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_requests WHERE workspace_id = $1 AND client = $2`, ws, claude).Scan(&kept); err != nil || kept != 2 {
		t.Fatalf("Claude's requests kept = %d (%v), want 2: the refused one is not kept", kept, err)
	}
	code, out = call(http.MethodGet, "/v1/money/assistant-limits", "", "")
	limits, _ := out["limits"].([]any)
	if code != http.StatusOK || len(limits) != 1 {
		t.Fatalf("the limits = %d %v", code, out)
	}
	if l := limits[0].(map[string]any); l["client"] != claude || l["currency"] != "GBP" || l["per_day_minor"] != 20000.0 || l["prepared_today_minor"] != 20000.0 {
		t.Errorf("Claude's limit reads %v, want GBP 20000 a day with 20000 prepared today", l)
	}
}
