package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// usdcNode answers eth_call of name() and version() as Base's USDC contract does.
func usdcNode(t *testing.T) string {
	t.Helper()
	abi := func(s string) string {
		return fmt.Sprintf("0x%064x%064x%s", 32, len(s), hex.EncodeToString([]byte(s))+strings.Repeat("00", 32-len(s)%32))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Params []struct{ Data string }
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := map[string]string{"0x06fdde03": abi("USD Coin"), "0x54fd4d50": abi("2")}[req.Params[0].Data]
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": out})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// B48.10 through the routes: the agent's own key asks to be paid and reads its requests; another agent's key may do
// neither; the shared link answers anyone with what to pay and nothing about whose it is. A request naming another
// workspace's agent lands in its owner's approvals with the trust Talyvor has in the requester — the name its owner's
// live check confirmed — and is answered as one naming an unknown address is. (Matching deposits to requests is
// TestB4810_RequestsToBePaidAreMatchedToTheirDeposits in internal/economy.)
func TestB4810_AnAgentAsksToBePaidThroughTheRoutes(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, payerWS = "ws-b4810-routes", "ws-b4810-routes-payer"
	store := economy.NewDualTokenStore(nil, pool, nil)
	x, err := store.NewChainIndexer([]economy.ChainNetwork{{Name: "base", Chain: smartaccount.Chain{ID: 8453, RPCURL: usdcNode(t)},
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
	idem := "b4810-routes"
	call := func(who *auth.AuthContext, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", idem)
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

	// Talyvor to Talyvor: the seller's owner passed a live identity check as Seller Ltd; the payer is another
	// workspace's agent.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
		VALUES ($1, 1, 'contact', 'kyc-partner', 'completed', 'ref-b4810-1', ''), ($1, 2, 'person', 'kyc-partner', 'completed', 'ref-b4810-2', 'Seller Ltd')`,
		ws); err != nil {
		t.Fatal(err)
	}
	payer, err := store.CreateAgent(ctx, payerWS, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	const payerWallet = "0x00000000000000000000000000000000000B481F"
	if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation,
		owner_credential_id, owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x0', '0x0', 'cred', $4, '0x0', 'coinbase', '')`,
		payer.ID, payerWS, payerWallet, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	named := func(addr string) string {
		return `{"network":"base","amount_minor":2000000,"memo":"data","payer_address":"` + addr + `","expires_at":"` +
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
	}
	answers := map[string]string{}
	for _, c := range []struct{ idem, addr string }{{"to-payer", payerWallet}, {"to-payer-again", payerWallet}, {"to-nobody", "0x00000000000000000000000000000000000Fa4e1"}} {
		idem = c.idem
		var out map[string]any
		code := call(key("Seller"), http.MethodPost, path, named(c.addr), &out)
		delete(out, "id")
		delete(out, "created_at")
		delete(out, "payer_address")
		delete(out, "x402")
		delete(out, "link")
		answers[c.idem] = fmt.Sprint(code, out)
	}
	if answers["to-payer-again"] != answers["to-nobody"] || !strings.HasPrefix(answers["to-payer"], "201 ") {
		t.Fatalf("answers differ by whose the address is:\n%s\n%s\n%s", answers["to-payer"], answers["to-payer-again"], answers["to-nobody"])
	}
	approvals, err := store.ListAgentApprovals(ctx, payerWS)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("the payer's approvals: %+v, %v; want one", approvals, err)
	}
	if a := approvals[0]; a.Trust != "Verified agent of Seller Ltd" || a.Reason != "Seller asks to be paid 2 USDC on base" {
		t.Fatalf("the payer's approval card: trust %q, %q", a.Trust, a.Reason)
	}
}
