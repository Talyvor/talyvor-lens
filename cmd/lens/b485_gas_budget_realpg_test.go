package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/tenant"
)

type b485Pushes chan nativepush.Message

func (p b485Pushes) Send(_ context.Context, _, _ string, m nativepush.Message) error {
	p <- m
	return nil
}

// B48.5 — a workspace's budget for the network fees a paymaster pays for its agents: each sponsored send is one row
// held at its quoted fee and charged what the chain took; a send the budget cannot cover is refused with a clear error
// and the owner asked to top up once, however many sends are refused; a top-up lets the next one through. The
// paymaster and the chain themselves are in internal/money/smartaccount's devnet test.
func TestB485_TheWalletPaysNetworkFeesWithinTheWorkspacesBudget(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, base = "ws-b485", int64(8453)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetGasBudgetDefault(1_000)
	push := make(b485Pushes, 4)
	store.SetNativePush(push)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation, push_token)
		VALUES ('dev-b485', $1, 'test', '\x00', '\x00', 'tok')`, ws); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	admin := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "talyvor-ops", IsAdmin: true, Scopes: []string{auth.ScopeKeys}}
	as := owner
	call := func(method, path, body string) (int, economy.GasBudget) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/workspaces/"+ws+"/agents/gas-budget"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), as)))
		var b economy.GasBudget
		if w.Code == http.StatusOK {
			_ = json.Unmarshal(w.Body.Bytes(), &b)
		}
		return w.Code, b
	}
	row := func(hash string) (held int64, charged *int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT held_wei, charged_wei FROM gas_sponsorships WHERE chain_id = $1 AND user_op_hash = $2 AND workspace_id = $3`,
			base, hash, ws).Scan(&held, &charged); err != nil {
			t.Fatal(err)
		}
		return held, charged
	}

	gas := store.SponsoredGas(ws, base)
	// A send quoted at 600 wei is held, and charged the 420 the chain took: one row, and a second charge changes nothing.
	if err := gas.Hold(ctx, "0xop1", big.NewInt(600)); err != nil {
		t.Fatal(err)
	}
	if err := gas.Hold(ctx, "0xop1", big.NewInt(600)); err != nil {
		t.Fatal(err)
	}
	if code, b := call(http.MethodGet, "?chain_id=8453", ""); code != http.StatusOK || b.HeldWei != 600 || b.LeftWei != 400 {
		t.Fatalf("while it is on its way = %d %+v", code, b)
	}
	for range 2 {
		if err := gas.Charge(ctx, "0xop1", big.NewInt(420)); err != nil {
			t.Fatal(err)
		}
	}
	if held, charged := row("0xop1"); held != 600 || charged == nil || *charged != 420 {
		t.Fatalf("the sponsorship's row holds %d and is charged %v, want 600 and 420", held, charged)
	}

	// A send the bundler refused is released and costs nothing.
	if err := gas.Hold(ctx, "0xop2", big.NewInt(100)); err != nil {
		t.Fatal(err)
	}
	if err := gas.Release(ctx, "0xop2"); err != nil {
		t.Fatal(err)
	}
	if code, b := call(http.MethodGet, "?chain_id=8453", ""); code != http.StatusOK || b.ChargedWei != 420 || b.HeldWei != 0 || b.LeftWei != 580 || b.TopUpAsked {
		t.Fatalf("after one charge and one release = %d %+v", code, b)
	}

	// 580 left and three sends that may cost 600 — the released one sent again among them: all are refused, nothing
	// is held, and the owner is asked once.
	for _, h := range []string{"0xop3", "0xop4", "0xop2"} {
		err := gas.Hold(ctx, h, big.NewInt(600))
		if !errors.Is(err, smartaccount.ErrGasBudgetSpent) || !strings.Contains(err.Error(), "580 wei left") || !strings.Contains(err.Error(), "top it up") {
			t.Fatalf("a send over the budget: %v", err)
		}
	}
	var holds int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gas_sponsorships WHERE workspace_id = $1`, ws).Scan(&holds); err != nil || holds != 2 {
		t.Fatalf("%d sponsorship rows, want 2 (%v)", holds, err)
	}
	if code, b := call(http.MethodGet, "?chain_id=8453", ""); code != http.StatusOK || !b.TopUpAsked {
		t.Fatalf("after the refusals = %d %+v, want the owner asked", code, b)
	}
	select {
	case m := <-push:
		if m.Data["type"] != "gas_budget_spent" || !strings.Contains(m.Body, "Top it up") {
			t.Fatalf("the owner was asked %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the owner was never asked to top up")
	}
	select {
	case m := <-push:
		t.Fatalf("the owner was asked again: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}

	// Talyvor pays what the budget sponsors, so the owner cannot top it up; Talyvor's admin does, the ask is cleared and
	// the send goes through.
	if code, _ := call(http.MethodPost, "/top-up", `{"chain_id":8453,"wei":5000}`); code != http.StatusForbidden {
		t.Fatalf("the owner's top-up = %d, want 403", code)
	}
	as = admin
	if code, _ := call(http.MethodPost, "/top-up", `{"chain_id":8453,"wei":0}`); code != http.StatusBadRequest {
		t.Fatalf("a top-up of nothing = %d, want 400", code)
	}
	code, b := call(http.MethodPost, "/top-up", `{"chain_id":8453,"wei":5000}`)
	if code != http.StatusOK || b.BudgetWei != 6_000 || b.LeftWei != 5_580 || b.TopUpAsked {
		t.Fatalf("top-up = %d %+v", code, b)
	}
	var by string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM gas_budget_top_ups WHERE workspace_id = $1 AND wei = 5000`, ws).Scan(&by); err != nil || by != "talyvor-ops" {
		t.Fatalf("the top-up's row names %q (%v)", by, err)
	}
	for _, h := range []string{"0xop3", "0xop2"} {
		if err := gas.Hold(ctx, h, big.NewInt(600)); err != nil {
			t.Fatalf("after the top-up, %s: %v", h, err)
		}
	}
	if code, b := call(http.MethodGet, "?chain_id=8453", ""); code != http.StatusOK || b.HeldWei != 1_200 || b.LeftWei != 4_380 {
		t.Fatalf("two sends held after the top-up = %d %+v", code, b)
	}

	// Another chain's budget and another workspace's are their own.
	if code, b := call(http.MethodGet, "?chain_id=10", ""); code != http.StatusOK || b.LeftWei != 1_000 {
		t.Fatalf("another chain = %d %+v", code, b)
	}
	if code, _ := call(http.MethodGet, "", ""); code != http.StatusBadRequest {
		t.Fatalf("no chain = %d, want 400", code)
	}
	if err := store.SponsoredGas("ws-other", base).Charge(ctx, "0xop3", big.NewInt(1)); err == nil {
		t.Fatal("another workspace charged this workspace's hold")
	}
}
