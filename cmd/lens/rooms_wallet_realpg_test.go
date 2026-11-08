package main

import (
	"context"
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
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/tenant"
)

type roomWalletRunner string

func (a roomWalletRunner) Run(context.Context, string, []market.Message) (string, error) {
	return string(a), nil
}

type roomWalletMeter []string

func (m *roomWalletMeter) MeterMarketUse(_ context.Context, _, useID string, _ int64, _ time.Time) error {
	*m = append(*m, useID)
	return nil
}

// B32.32 — creating a room creates one room-kind agent with one attached key, which does not count toward the plan's
// agents; funding it posts as for any agent; a monthly limit above the plan's room_budget_max_usd is refused naming
// rooms_plan_limits; a member without may_spend is refused a spend on the room, and one with it spends from the room's
// wallet with the room and the member on the use and in the postings' memo.
func TestRooms_TheRoomsWalletItsBudgetAndWhoMaySpendIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, member = "ws-rw-owner", "ws-rw-member"
	for _, ws := range []string{owner, member} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, owner); err != nil {
		t.Fatal(err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	keys := tenant.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetKeys(keys)
	bank.SetRoomBudgets(store)
	r := chi.NewRouter()
	mountRoomRoutes(r, store)
	mountAgentAccountRoutes(r, bank, keys)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	// Creating a room creates its wallet: one room-kind agent of the owner's, named for the room, with one proxy key.
	code, body := call(owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms",
		`{"title":"Launch copy","terms":{"spend_policy":"members_with_spend"}}`)
	var room rooms.Detail
	if err := json.Unmarshal([]byte(body), &room); code != http.StatusCreated || err != nil || room.Wallet == nil {
		t.Fatalf("create = %d %s, want 201 with the room's wallet", code, body)
	}
	wallet := room.Wallet.AgentID
	var kind, name, owned string
	var walletKeys int
	if err := pool.QueryRow(ctx, `SELECT a.kind, a.name, a.owner_user_id, (SELECT count(*) FROM agent_account_keys k WHERE k.agent_id = a.id)
		FROM agent_accounts a JOIN rooms r ON r.wallet_agent_id = a.id WHERE r.id = $1 AND a.workspace_id = $2`, room.ID, owner).
		Scan(&kind, &name, &owned, &walletKeys); err != nil {
		t.Fatalf("the room's wallet: %v", err)
	}
	var rooms_, scopes int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_accounts WHERE workspace_id = $1 AND kind = 'room'),
		(SELECT count(*) FROM workspace_api_keys k JOIN agent_account_keys a ON a.scoped_key_id = k.id::text
		  WHERE a.agent_id = $2 AND k.workspace_id = $1 AND k.scopes = '{proxy}')`, owner, wallet).Scan(&rooms_, &scopes); err != nil {
		t.Fatal(err)
	}
	if kind != "room" || name != "Room: Launch copy" || owned != "user-"+owner || walletKeys != 1 || rooms_ != 1 || scopes != 1 {
		t.Fatalf("the wallet = %s %q owned by %q with %d keys (%d proxy-scoped), %d room agents; want one room agent \"Room: Launch copy\" "+
			"owned by user-%s with one proxy key", kind, name, owned, walletKeys, scopes, rooms_, owner)
	}

	// It is not one of the agents the plan allows: the free plan's three agents can still all be created, and a fourth cannot.
	for i, want := range []int{http.StatusCreated, http.StatusCreated, http.StatusCreated, http.StatusPaymentRequired} {
		if code, body := call(owner, http.MethodPost, "/v1/workspaces/"+owner+"/agents", `{"name":"helper"}`); code != want {
			t.Fatalf("agent %d beside the room's wallet = %d %s, want %d", i+1, code, body, want)
		}
	}

	// Nor can it become one: it takes no key but the one Lens holds for it.
	if code, body := call(owner, http.MethodPost, "/v1/workspaces/"+owner+"/agents/"+wallet+"/keys", `{"name":"mine"}`); code != http.StatusConflict ||
		!strings.Contains(body, "takes no other key") {
		t.Fatalf("a key of the owner's own for the room's wallet = %d %s, want 409", code, body)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_account_keys WHERE agent_id = $1`, wallet).Scan(&walletKeys); err != nil || walletKeys != 1 {
		t.Fatalf("the wallet's keys after a refused one = %d (%v), want 1", walletKeys, err)
	}

	// Funding it posts as for any agent: one fund entry, the workspace to the wallet.
	if code, body := call(owner, http.MethodPost, "/v1/workspaces/"+owner+"/agents/"+wallet+"/fund", `{"amount_ulxc":50000000}`); code != http.StatusOK ||
		!strings.Contains(body, `"balance_ulxc":50000000`) {
		t.Fatalf("fund the room's wallet = %d %s, want 200 at 50 LXC", code, body)
	}
	var fromWorkspace, toWallet int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc) FILTER (WHERE account = 'workspace'), 0),
		COALESCE(sum(amount_ulxc) FILTER (WHERE account = 'agent:' || $2), 0) FROM agent_postings WHERE workspace_id = $1 AND kind = 'fund'`,
		owner, wallet).Scan(&fromWorkspace, &toWallet); err != nil || fromWorkspace != -50_000_000 || toWallet != 50_000_000 {
		t.Fatalf("fund postings = workspace %d, wallet %d (%v); want −50,000,000 and +50,000,000", fromWorkspace, toWallet, err)
	}

	// Until it has a monthly limit the room has no budget, and nobody spends it.
	if _, err := store.MaySpend(ctx, owner, room.ID); !errors.Is(err, rooms.ErrPlanLimit) ||
		err.Error() != "This room has no budget yet. Its owner sets one in Room settings: on Free, up to $100 (1,000 LXC) a month." {
		t.Fatalf("the owner spends a room with no budget: %v, want refused for having none", err)
	}

	// Its monthly limit is the room's budget: above the free plan's $100 (1,000 LXC) it is refused naming the setting
	// and the plan, and nothing is saved; at $100 it is saved.
	rules := func(monthly string) (int, string) {
		return call(owner, http.MethodPut, "/v1/workspaces/"+owner+"/agents/"+wallet+"/rules",
			`{"monthly_limit_ulxc":`+monthly+`,"max_per_request_ulxc":20000000,"approval_above_ulxc":10000000}`)
	}
	if code, body := rules("1000000001"); code != http.StatusPaymentRequired || !strings.Contains(body, `"setting":"rooms_plan_limits"`) ||
		!strings.Contains(body, `"plan":"free"`) || !strings.Contains(body, `"limit":"room_budget_max_usd"`) || !strings.Contains(body, `"allows":"team"`) ||
		!strings.Contains(body, `"error":"Free allows a room a budget of up to $100 (1,000 LXC) a month. Team allows $5,000."`) {
		t.Fatalf("a monthly limit above $100 = %d %s, want 402 naming rooms_plan_limits, the free plan and team", code, body)
	}
	var saved int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_rules WHERE agent_id = $1`, wallet).Scan(&saved); err != nil || saved != 0 {
		t.Fatalf("a refused budget saved %d rules (%v), want none", saved, err)
	}
	if code, body := rules("1000000000"); code != http.StatusOK || !strings.Contains(body, `"monthly_limit_ulxc":1000000000`) {
		t.Fatalf("a $100 monthly limit = %d %s, want 200", code, body)
	}
	// Any other agent's monthly limit is still its owner's own business.
	var helper string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_accounts WHERE workspace_id = $1 AND kind = 'agent' LIMIT 1`, owner).Scan(&helper); err != nil {
		t.Fatal(err)
	}
	if code, body := call(owner, http.MethodPut, "/v1/workspaces/"+owner+"/agents/"+helper+"/rules", `{"monthly_limit_ulxc":5000000000}`); code != http.StatusOK {
		t.Fatalf("an ordinary agent's $500 monthly limit = %d %s, want 200", code, body)
	}

	// A member without may_spend is refused a spend on the room, and its view of the room says why.
	if code, body := call(member, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`); code != http.StatusCreated {
		t.Fatalf("join = %d %s", code, body)
	}
	if _, err := store.MaySpend(ctx, member, room.ID); !errors.Is(err, rooms.ErrForbidden) || !strings.Contains(err.Error(), "The room’s owner has not let you spend its budget.") {
		t.Fatalf("a member without may_spend spends the room's budget: %v, want refused in plain words", err)
	}
	if _, body := call(member, http.MethodGet, "/v1/rooms/"+room.ID, ""); !strings.Contains(body, `"may_spend":false,"why_not":"The room’s owner has not let you spend its budget. Ask the owner or an editor."`) ||
		!strings.Contains(body, `"monthly_limit_ulxc":1000000000`) || !strings.Contains(body, `"budget_max_ulxc":1000000000`) {
		t.Fatalf("the member's view of the room = %s, want the budget and why it may not spend it", body)
	}

	// Given may_spend, it spends from the room's wallet: its use is the owner's, on the wallet, with the room and the
	// member recorded, and the wallet's postings name them.
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+member, `{"may_spend":true}`); code != http.StatusOK {
		t.Fatalf("give may_spend = %d %s", code, body)
	}
	sp, err := store.MaySpend(ctx, member, room.ID)
	if err != nil || sp.OwnerWorkspaceID != owner || sp.WalletAgentID != wallet || sp.ActorWorkspaceID != member {
		t.Fatalf("the member's spend = %+v, %v; want the owner's wallet %s", sp, err, wallet)
	}
	mk := market.NewStore(pool)
	l, err := mk.Publish(ctx, member, market.Draft{Kind: "prompt", Title: "Tagline", PricePerUseULXC: 1_000_000, Visibility: "public",
		Artifact: json.RawMessage(`{"template":"A tagline for {{product}}.","model":"claude-haiku-4-5"}`)})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	var metered roomWalletMeter
	u, err := mk.Use(sp.Context(ctx), market.UseDeps{Runner: roomWalletRunner("Ship it."), Meter: &metered, Agents: bank}, sp.OwnerWorkspaceID,
		sp.WalletAgentID, l.ID, market.UseRequest{Variables: map[string]string{"product": "rooms"}, RoomID: sp.RoomID, ActorWorkspaceID: sp.ActorWorkspaceID})
	if err != nil || u.Charge != market.ChargeBilled {
		t.Fatalf("the room's use = %+v, %v; want it billed", u, err)
	}
	var buyer, agent, roomID, actor string
	if err := pool.QueryRow(ctx, `SELECT buyer_workspace_id, agent_id, room_id, actor_workspace_id FROM market_uses WHERE id = $1`, u.ID).
		Scan(&buyer, &agent, &roomID, &actor); err != nil || buyer != owner || agent != wallet || roomID != room.ID || actor != member {
		t.Fatalf("the use row = buyer %s agent %s room %s actor %s (%v); want %s, %s, %s, %s", buyer, agent, roomID, actor, err,
			owner, wallet, room.ID, member)
	}
	var walletKey string
	if err := pool.QueryRow(ctx, `SELECT scoped_key_id FROM agent_account_keys WHERE agent_id = $1`, wallet).Scan(&walletKey); err != nil {
		t.Fatal(err)
	}
	if err := bank.SpendLXCForAgent(sp.Context(ctx), walletKey, owner, "req-room-1", 1_000_000, "chat", economy.AgentDebitMeta{}); err != nil {
		t.Fatalf("a model call on the room's wallet: %v", err)
	}
	var memo string
	if err := pool.QueryRow(ctx, `SELECT memo FROM agent_postings WHERE account = 'agent:' || $1 AND ref = 'req-room-1'`, wallet).Scan(&memo); err != nil ||
		memo != "room "+room.ID+" · member "+member {
		t.Fatalf("the wallet's posting memo = %q (%v), want the room and the member", memo, err)
	}
}
