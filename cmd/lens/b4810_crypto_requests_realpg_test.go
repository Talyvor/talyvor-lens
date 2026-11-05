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
	"github.com/talyvor/lens/internal/money/smartaccount"
)

// B48.10 through the routes: the agent's own key asks to be paid and reads its requests; another agent's key may do
// neither; the shared link answers anyone with what to pay and nothing about whose it is. (Matching deposits to
// requests is TestB4810_RequestsToBePaidAreMatchedToTheirDeposits in internal/economy.)
func TestB4810_AnAgentAsksToBePaidThroughTheRoutes(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4810-routes"
	store := economy.NewDualTokenStore(nil, pool, nil)
	x, err := store.NewChainIndexer([]economy.ChainNetwork{{Name: "base", Chain: smartaccount.Chain{ID: 8453, RPCURL: "http://127.0.0.1:1"},
		USDC: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", Confirmations: 1, Funding: economy.FundingTest}})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountCryptoRequestRoutes(r, store, &chainIndex{x: x})
	r.Get("/v1/crypto-requests/{requestID}", serveCryptoRequest(store))
	var ids []string
	for i, name := range []string{"Seller", "Other"} {
		a, err := store.CreateAgent(ctx, ws, name, "owner")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AttachAgentKey(ctx, ws, a.ID, "key-"+name); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation,
			owner_credential_id, owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x0', '0x0', 'cred', $4, '0x0', 'coinbase', '')`,
			a.ID, ws, "0x00000000000000000000000000000000000b481"+string(rune('0'+i)), make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	seller := ids[0]
	key := func(name string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-" + name}
	}
	call := func(who *auth.AuthContext, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "b4810-routes")
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		return w.Code
	}
	path := "/v1/workspaces/" + ws + "/agents/" + seller + "/crypto-requests"
	body := `{"token":"USDC","network":"base","amount_minor":5000000,"memo":"report","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`

	if code := call(key("Other"), http.MethodPost, path, body, nil); code != http.StatusForbidden {
		t.Fatalf("another agent's key asking for the seller = %d, want 403", code)
	}
	var made economy.CryptoRequest
	if code := call(key("Seller"), http.MethodPost, path, body, &made); code != http.StatusCreated || made.Status != "open" || made.AmountMinor != 5_000000 {
		t.Fatalf("the seller's own key = %d, %+v", code, made)
	}
	if code := call(key("Other"), http.MethodGet, path+"/"+made.ID, "", nil); code != http.StatusForbidden {
		t.Fatalf("another agent's key reading the seller's request = %d, want 403", code)
	}
	var list struct{ Requests []economy.CryptoRequest }
	if code := call(key("Seller"), http.MethodGet, path, "", &list); code != http.StatusOK || len(list.Requests) != 1 || list.Requests[0].ID != made.ID {
		t.Fatalf("the seller's list = %d, %+v", code, list)
	}
	var raw map[string]any
	if code := call(nil, http.MethodGet, "/v1/crypto-requests/"+made.ID, "", &raw); code != http.StatusOK || raw["uri"] != made.URI {
		t.Fatalf("the shared link = %d, %v", code, raw)
	}
	for _, hidden := range []string{"agent_id", "payer_address", "approval_id", "payments"} {
		if _, ok := raw[hidden]; ok {
			t.Errorf("the shared link shows %s: %v", hidden, raw[hidden])
		}
	}
}
