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
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/tenant"
)

// B48.43: the top-up an agent asked for with wallet_topup_link reads back by id for the agent's owner, before anything
// of it has started — what they pay, the fee, the rate, the USDC it buys and until when — and is no such move to anyone
// else.
func TestB4843_TheOwnerReadsTheTopUpTheirAgentAskedFor(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4843"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetRampPartner(&partners.TestRampPartner{})
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b4843"), Clearances: store,
		Limits: store.MoneyLimits(), Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	session := func(wsID, user string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: wsID, AuthMethod: auth.MethodJWT, UserID: user, Scopes: []string{auth.ScopeKeys}}
	}
	owner := session(ws, "owner")
	call := func(as *auth.AuthContext, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), as))
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
	var agent economy.Agent
	if code := call(owner, http.MethodPost, base, `{"name":"Ops","wallet_kind":"crypto"}`, &agent); code != http.StatusCreated {
		t.Fatalf("create the agent = %d", code)
	}
	// What wallet_topup_link asks for, by the agent's own key: a card top-up of 100 EUR into its USDC wallet.
	asked, err := store.QuoteWalletMove(ctx, ws, agent.ID, economy.WalletMoveRequest{Route: economy.WalletMoveCard, AmountMinor: 10_000,
		Currency: money.EUR}, "agent:"+agent.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var listed []economy.WalletMove
	if code := call(owner, http.MethodGet, base+"/"+agent.ID+"/wallet-moves", "", &listed); code != http.StatusOK || len(listed) != 0 {
		t.Fatalf("B48.8's list = %d %+v, want it to leave out a quote nothing of which has started", code, listed)
	}

	path := base + "/" + agent.ID + "/wallet-moves/" + asked.ID
	var got economy.WalletMove
	if code := call(owner, http.MethodGet, path, "", &got); code != http.StatusOK {
		t.Fatalf("the owner reading the top-up their agent asked for = %d, want 200", code)
	}
	if got.ID != asked.ID || got.Status != "quoted" || got.Route != economy.WalletMoveCard || got.SellMinor != 10_000 ||
		got.SellCurrency != money.EUR || got.FeeMinor != asked.FeeMinor || got.Rate != asked.Rate || got.BuyMinor != asked.BuyMinor ||
		got.BuyCurrency != money.USDC || got.BuyMinor <= 0 || got.ExpiresAt == nil || got.ExpiresAt.Sub(*asked.ExpiresAt).Abs() >= time.Microsecond { // Postgres keeps microseconds
		t.Fatalf("the owner reads %+v; want the quote as the agent asked it: %+v", got, asked)
	}

	// Anyone else is told there is no such move: another person in the workspace, another workspace's owner at their own
	// workspace, and an id the agent never asked for.
	for _, c := range []struct {
		who  string
		as   *auth.AuthContext
		path string
	}{
		{"another person in the workspace", session(ws, "someone-else"), path},
		{"another workspace's owner", session("ws-b4843-other", "owner"),
			"/v1/workspaces/ws-b4843-other/agents/" + agent.ID + "/wallet-moves/" + asked.ID},
		{"the owner, for a move never asked", owner, base + "/" + agent.ID + "/wallet-moves/wmove_never"},
	} {
		if code := call(c.as, http.MethodGet, c.path, "", nil); code != http.StatusNotFound {
			t.Errorf("%s reading the top-up = %d, want 404", c.who, code)
		}
	}
}
