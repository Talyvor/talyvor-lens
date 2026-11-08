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

// B30.59 — the owner sets a sweep target on the company's USD account, sees the fund with its return labelled
// simulated, and withdraws from it; an agent's key, which is not the owner's, can do none of it.
func TestTreasurySweepRoutes_TheOwnerSetsReadsAndWithdraws(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3059-routes"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetTreasuryPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	acct, err := store.OpenCurrencyAccount(ctx, ws, "", economy.CurrencyUSD, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTestMoney(ctx, ws, acct.ID, 500_00, "b3059-routes-in"); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountTreasurySweepRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	agentKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-agent"}
	call := func(who *auth.AuthContext, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/money/accounts/"+acct.ID+"/treasury-sweep"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	if code, out := call(agentKey, http.MethodPut, "", `{"target_minor":100}`); code != http.StatusForbidden {
		t.Fatalf("an agent's key sets the sweep = %d %v, want 403", code, out)
	}
	if code, out := call(owner, http.MethodPut, "", `{"target_minor":10000}`); code != http.StatusOK || out["partner"] != "test" || out["return"] != "simulated" {
		t.Fatalf("the owner sets the sweep = %d %v, want 200 at the test broker, its return simulated", code, out)
	}
	if n, err := store.SweepTreasuries(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("the day's sweep moved %d (%v), want 1", n, err)
	}
	if code, out := call(owner, http.MethodGet, "", ""); code != http.StatusOK || out["fund_minor"] != float64(400_00) {
		t.Fatalf("the owner reads the sweep = %d %v, want 200 and $400.00 in the fund", code, out)
	}
	if code, out := call(agentKey, http.MethodPost, "/withdraw", `{"amount_minor":100,"idempotency_key":"k"}`); code != http.StatusForbidden {
		t.Fatalf("an agent's key withdraws = %d %v, want 403", code, out)
	}
	if code, out := call(owner, http.MethodPost, "/withdraw", `{"amount_minor":50000,"idempotency_key":"over"}`); code != http.StatusConflict {
		t.Fatalf("the owner withdraws more than the fund holds = %d %v, want 409", code, out)
	}
	if code, out := call(owner, http.MethodPost, "/withdraw", `{"amount_minor":15000,"idempotency_key":"back"}`); code != http.StatusCreated || out["kind"] != "treasury_withdrawal" {
		t.Fatalf("the owner withdraws $150.00 = %d %v, want 201, a treasury_withdrawal", code, out)
	}
	if b, err := store.Balance(ctx, ws, acct.ID); err != nil || b.AmountMinor != 250_00 {
		t.Fatalf("the account holds %+v (%v), want $250.00", b, err)
	}
}
