package main

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/holdings"
	"github.com/talyvor/lens/internal/tenant"
)

// heldBy is a network's node, by the address it is asked about.
type heldBy map[string][]holdings.Held

func (h heldBy) Holdings(_ context.Context, addr string) ([]holdings.Held, error) {
	return h[addr], nil
}

// B48.4: the owner reads an agent's balances per token and network, valued in their currency, at
// GET …/agents/{id}/crypto-balances: from its smart account's address on the EVM networks and its Solana wallet on
// Solana. A member who does not manage agents is refused.
func TestB484_TheOwnerSeesAnAgentsBalancesPerTokenAndNetwork(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b484"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	member := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "member"}
	call := func(who *auth.AuthContext, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		return w.Code
	}
	base := "/v1/workspaces/" + ws + "/agents"
	var trader, other economy.Agent
	for _, a := range []*economy.Agent{&trader, &other} {
		if code := call(owner, http.MethodPost, base, `{"name":"Trader","wallet_kind":"crypto"}`, a); code != http.StatusCreated {
			t.Fatalf("create = %d", code)
		}
	}
	path := base + "/" + trader.ID + "/crypto-balances"
	if code := call(owner, http.MethodGet, path, "", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("with no network to read = %d, want 503", code)
	}

	const evm, sol = "0x00000000000000000000000000000000000B4842", "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"
	if _, err := pool.Exec(context.Background(), `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation,
		owner_credential_id, owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x0', '0x0', 'cred', $4, '0x0', 'coinbase', $5)`,
		trader.ID, ws, evm, make([]byte, 64), sol); err != nil {
		t.Fatal(err)
	}
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	store.SetCryptoHoldings(map[string]holdings.Reader{
		"base":   heldBy{evm: {{Decimals: 18, Amount: e18}, {Address: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", Symbol: "USDC", Decimals: 6, Amount: big.NewInt(25_000000)}}},
		"solana": heldBy{sol: {{Decimals: 9, Amount: big.NewInt(1_000_000_000)}}},
	}, holdings.Verified(), holdings.TestPrices{})

	// No verified country yet: valued in dollars, and USDT, barred somewhere, is not offered. 1 ETH $3,000.00 and 25 USDC
	// $25.00 on Base; 1 SOL $150.00 on Solana.
	var st holdings.Statement
	if code := call(owner, http.MethodGet, path, "", &st); code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	if st.Currency != "USD" || st.ValueMinor != 317500 || len(st.Networks) != 2 || st.Networks[0].ID != "base" || st.Networks[1].ID != "solana" ||
		st.Networks[0].ValueMinor != 302500 || st.Networks[1].ValueMinor != 15000 {
		t.Fatalf("statement: %+v", st)
	}
	for _, nb := range st.Networks {
		for _, b := range nb.Tokens {
			if b.Symbol == "USDT" {
				t.Errorf("USDT offered on %s to an owner whose country is not known", nb.ID)
			}
		}
	}
	if code := call(owner, http.MethodGet, path+"?currency=GBP", "", &st); code != http.StatusOK || st.Currency != "GBP" || st.ValueMinor != 254000 {
		t.Errorf("in pounds = %d: %s %d, want GBP 254000", code, st.Currency, st.ValueMinor)
	}
	for _, c := range []struct {
		who  *auth.AuthContext
		path string
		want int
	}{
		{owner, path + "?currency=JPY", http.StatusBadRequest},
		{member, path, http.StatusForbidden},
		{owner, base + "/" + other.ID + "/crypto-balances", http.StatusNotFound}, // no smart account yet
	} {
		if code := call(c.who, http.MethodGet, c.path, "", nil); code != c.want {
			t.Errorf("GET %s = %d, want %d", c.path, code, c.want)
		}
	}
}
