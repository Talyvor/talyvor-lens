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
)

// B28.73 — the owner's SDK funds an agent through an approval link. Its key files a request under an Idempotency-Key
// and gets the link; nothing is posted. The key cannot approve it. A person signed in approves it, and exactly one
// fund entry is posted, its ref the SDK's key; approving again posts nothing more. A declined request posts nothing.
// Checked on agent_postings, never on a status code alone.
func TestB2873_AnSDKFundPostsOnceUnderItsKeyAndOnlyOnceTheLinkIsUsed(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b2873"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 100000000, 100000000)`, ws); err != nil {
		t.Fatal(err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	agent, err := bank.CreateAgent(ctx, ws, "researcher", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountAgentFundRequestRoutes(r, bank)
	sdkKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-owner-sdk", Scopes: []string{auth.ScopeAdmin}}
	person := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-owner", Scopes: []string{auth.ScopeKeys}}
	call := func(as *auth.AuthContext, method, path, key, body string) (int, economy.AgentFundRequest, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(), as))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out economy.AgentFundRequest
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out, w.Body.String()
	}
	type posting struct {
		account, ref string
		amount       int64
	}
	postings := func() []posting {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT account, ref, amount_ulxc FROM agent_postings WHERE workspace_id = $1 AND kind = 'fund' ORDER BY id`, ws)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []posting
		for rows.Next() {
			var p posting
			if err := rows.Scan(&p.account, &p.ref, &p.amount); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}
	file := "/v1/workspaces/" + ws + "/agents/" + agent.ID + "/fund-requests"
	const key = "sdk-fund-7f3a"

	if code, _, body := call(sdkKey, http.MethodPost, file, "", `{"amount_ulxc":5000000}`); code != http.StatusBadRequest {
		t.Fatalf("a fund request with no Idempotency-Key = %d %s, want 400", code, body)
	}
	code, filed, body := call(sdkKey, http.MethodPost, file, key, `{"amount_ulxc":5000000}`)
	if code != http.StatusCreated || filed.Status != economy.FundRequestPending || filed.IdempotencyKey != key ||
		filed.ApprovalURL != economy.FundRequestLinkBase+agent.ID+"?fund_request="+filed.ID {
		t.Fatalf("the SDK's fund request = %d %s, want 201, pending, under its key, with the agent's approval link", code, body)
	}
	if got := postings(); len(got) != 0 {
		t.Fatalf("postings after the request is filed = %v, want none: nothing is funded before the approval link is used", got)
	}
	if code, again, body := call(sdkKey, http.MethodPost, file, key, `{"amount_ulxc":5000000}`); code != http.StatusOK || again.ID != filed.ID {
		t.Fatalf("the same key and amount again = %d %s, want 200 and request %s", code, body, filed.ID)
	}
	if code, _, body := call(sdkKey, http.MethodPost, file, key, `{"amount_ulxc":9000000}`); code != http.StatusConflict {
		t.Fatalf("the same key for another amount = %d %s, want 409", code, body)
	}

	approve := "/v1/workspaces/" + ws + "/agents/fund-requests/" + filed.ID + "/approve"
	if code, _, body := call(sdkKey, http.MethodPost, approve, "", ""); code != http.StatusForbidden {
		t.Fatalf("the SDK's key approving its own request = %d %s, want 403", code, body)
	}
	if got := postings(); len(got) != 0 {
		t.Fatalf("postings after the key's approval was refused = %v, want none", got)
	}
	code, approved, body := call(person, http.MethodPost, approve, "", "")
	if code != http.StatusOK || approved.Status != economy.FundRequestApproved || approved.BalanceULXC == nil || *approved.BalanceULXC != 5_000_000 {
		t.Fatalf("the person's approval = %d %s, want 200, approved, the agent at 5 LXC", code, body)
	}
	want := []posting{{"workspace", key, -5_000_000}, {"agent:" + agent.ID, key, 5_000_000}}
	if got := postings(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("postings after the approval = %v, want one fund entry under the SDK's key: %v", got, want)
	}
	if code, _, body := call(person, http.MethodPost, approve, "", ""); code != http.StatusOK {
		t.Fatalf("approving again = %d %s, want 200", code, body)
	}
	if got := postings(); len(got) != 2 {
		t.Fatalf("postings after a second approval = %v, want the one entry still", got)
	}
	if code, _, body := call(person, http.MethodPost, "/v1/workspaces/"+ws+"/agents/fund-requests/"+filed.ID+"/decline", "", ""); code != http.StatusConflict {
		t.Fatalf("declining an approved request = %d %s, want 409", code, body)
	}

	// A declined request moves nothing, and cannot be approved after.
	_, other, _ := call(sdkKey, http.MethodPost, file, "sdk-fund-other", `{"amount_ulxc":3000000}`)
	if code, out, body := call(person, http.MethodPost, "/v1/workspaces/"+ws+"/agents/fund-requests/"+other.ID+"/decline", "", ""); code != http.StatusOK ||
		out.Status != economy.FundRequestDeclined {
		t.Fatalf("declining = %d %s, want 200, declined", code, body)
	}
	if code, _, body := call(person, http.MethodPost, "/v1/workspaces/"+ws+"/agents/fund-requests/"+other.ID+"/approve", "", ""); code != http.StatusConflict {
		t.Fatalf("approving a declined request = %d %s, want 409", code, body)
	}
	if got := postings(); len(got) != 2 {
		t.Fatalf("postings after a declined request = %v, want the first entry only", got)
	}
}
