package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// b5042Bank is the store, with the test's agent key named as its agent's.
type b5042Bank struct {
	*economy.DualTokenStore
	ws, agentID string
}

func (b b5042Bank) AgentOfKey(_ context.Context, key string) (string, string, error) {
	if key == "key-b5042-agent" {
		return b.agentID, b.ws, nil
	}
	return "", "", nil
}

// B50.42 — an agent's mandate request is named by where its money goes, never in the agent's own words: a Talyvor
// company by its workspace's name, an outside business by the owner's saved payee its pulls are paid out to. The
// owner's own grant keeps the name the owner typed.
func TestB5042_AgentMandateRequestIsNamedByItsPayeeNotTheAgent(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, payeeWS = "ws-b5042-payer", "ws-b5042-payee"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screener)
	store.SetPayeeScreener(screener)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, 'Bolt Cloud', $1)`, payeeWS); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_b5042', $1, 'Northwind Supplies', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, ws, "Subscriber", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	agentGBP, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, payeeWS, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMandateRoutes(r, b5042Bank{store, ws, agent.ID})
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	agentKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-b5042-agent"}
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	grant := func(who *auth.AuthContext, payee string) string {
		t.Helper()
		body := `{"grant":{"account_id":"` + agentGBP.ID + `",` + payee + `,"payee_name":"Acme Hosting",` +
			`"max_per_pull_minor":500,"max_per_month_minor":2000,"expires_at":"` + expires + `"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/money/mandates", strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), who)))
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		id, _ := m["id"].(string)
		if w.Code != http.StatusCreated || id == "" {
			t.Fatalf("the grant to %s = %d %s, want 201", payee, w.Code, w.Body.String())
		}
		return id
	}
	stored := func(id string) string {
		t.Helper()
		var name string
		if err := pool.QueryRow(ctx, `SELECT payee_name FROM money_mandates WHERE id = $1`, id).Scan(&name); err != nil {
			t.Fatal(err)
		}
		return name
	}
	listed := func(id string) string {
		t.Helper()
		granted, _, err := store.Mandates(ctx, ws, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range granted {
			if m.ID == id {
				return m.PayeeName
			}
		}
		t.Fatalf("mandate %s is not listed", id)
		return ""
	}

	for _, c := range []struct {
		who         *auth.AuthContext
		payee, want string
	}{
		{agentKey, `"payee_workspace_id":"` + payeeWS + `"`, "Bolt Cloud"},
		{agentKey, `"payee_business_id":"northwind","payee_id":"payee_b5042"`, "Northwind Supplies"},
		{owner, `"payee_workspace_id":"` + payeeWS + `"`, "Acme Hosting"},
	} {
		id := grant(c.who, c.payee)
		if got, list := stored(id), listed(id); got != c.want || list != c.want {
			t.Errorf("the mandate to %s is stored as %q and listed as %q, want %q", c.payee, got, list, c.want)
		}
	}
}
