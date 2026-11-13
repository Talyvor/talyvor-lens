package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B58.2 — a payment held for the owner is a 202 with its approval as a field, to every caller: before, only a
// connected assistant got it, and a workspace key, the owner, a marketplace use and a room's run got 403 with the
// approval's id inside the words, which the web lost when the words changed.

// heldAnswer is the body a connected assistant has been sent for a held payment since B52.4, byte for byte.
func heldAnswer(need *economy.ApprovalNeededError) string {
	return fmt.Sprintf(`{"amount_ulxc":%d,"approval_id":%q,"message":%q,"status":"pending_approval"}`+"\n", need.AmountULXC, need.ApprovalID, need.Error())
}

// The four fields, and nothing else, from a marketplace use and a room's run, as a connected assistant's pay has them.
func TestB582_AHeldUseOrRunAnswers202WithTheFourFields(t *testing.T) {
	need := &economy.ApprovalNeededError{ApprovalID: "apr_x", AmountULXC: 3_000_000}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"a marketplace use": func(w http.ResponseWriter, err error) { writeMarketUseErr(w, err, stripeByKind{}, "ws-b582") },
		"a room's run":      writeRoomRunErr,
	} {
		w := httptest.NewRecorder()
		write(w, fmt.Errorf("market: %w", need))
		if want := heldAnswer(need); w.Code != http.StatusAccepted || w.Body.String() != want {
			t.Errorf("%s held for the owner = %d %s, want 202 %s", name, w.Code, w.Body.String(), want)
		}
	}
}

// A workspace key's pay above the approval amount: 202 naming the approval it filed, and nothing is posted.
func TestB582_AWorkspaceKeysHeldPayAnswers202(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b582"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	buyer, err := store.CreateAgent(ctx, ws, "buyer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	seller, err := store.CreateAgent(ctx, ws, "seller", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, buyer.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, buyer.ID, "key-b582-buyer"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, buyer.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+buyer.ID+"/pay",
		strings.NewReader(`{"to_agent_id":"`+seller.ID+`","amount_ulxc":3000000,"memo":"invoice 20"}`))
	req = req.WithContext(auth.WithAuthContext(req.Context(),
		&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-b582-buyer", Scopes: []string{auth.ScopeProxy}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var filed string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending' AND amount_ulxc = 3000000`, buyer.ID).Scan(&filed); err != nil {
		t.Fatalf("the approval's row: %v (the answer was %d %s)", err, w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); w.Code != http.StatusAccepted || err != nil || len(body) != 4 ||
		body["status"] != "pending_approval" || body["approval_id"] != filed || body["amount_ulxc"] != float64(3_000_000) ||
		!strings.Contains(fmt.Sprint(body["message"]), filed) {
		t.Fatalf("a workspace key's 3 LXC pay above the 1 LXC approval amount = %d %s, want 202 naming %s in its four fields", w.Code, w.Body.String(), filed)
	}
	var postings, approvals int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE kind = 'pay'`).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE agent_id = $1`, buyer.ID).Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if postings != 0 || approvals != 1 {
		t.Errorf("the held pay left %d pay postings and %d approvals; want none posted and the approval filed once", postings, approvals)
	}
}
