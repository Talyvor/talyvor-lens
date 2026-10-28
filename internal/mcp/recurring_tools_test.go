package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

type fakeRecurring struct{ prepared int }

func (f *fakeRecurring) PrepareRecurringPayments(context.Context, string, string, string, economy.RecurringPlanIn) (economy.RecurringPlan, bool, error) {
	f.prepared++
	return economy.RecurringPlan{ID: "rpp_1"}, false, nil
}

// B45.13: only the owner or an admin prepares recurring payments — an agent's or a session key is refused — and bank
// details in the list are refused before anything is prepared.
func TestMCP_RecurringPaymentsOwnerOnly(t *testing.T) {
	s := newServer(nil, nil, nil, nil, nil, "test")
	f := &fakeRecurring{}
	s.SetRecurringPayments(f)
	call := func(actx *auth.AuthContext, args string) string {
		t.Helper()
		resp := s.dispatch(auth.WithAuthContext(context.Background(), actx), rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"),
			Method: "tools/call", Params: json.RawMessage(`{"name":"prepare_recurring_payments","arguments":` + args + `}`)})
		raw, _ := json.Marshal(resp)
		return string(raw)
	}
	list := `{"idempotency_key":"k","owner_name":"Ada","until":"2030-01-01T00:00:00Z","payments":[{"payee_id":"p","currency":"GBP","amount_minor":1,"every":"week"}]}`
	if out := call(&auth.AuthContext{WorkspaceID: "ws", APIKeyID: "key-agent"}, list); f.prepared != 0 || !strings.Contains(out, "only the workspace's owner") {
		t.Errorf("an agent's key prepared recurring payments: %s", out)
	}
	owner := &auth.AuthContext{WorkspaceID: "ws", AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	if out := call(owner, strings.Replace(list, `"every":"week"`, `"every":"week","sort_code":"200000"`, 1)); f.prepared != 0 ||
		!strings.Contains(out, "payee details supplied by the assistant") {
		t.Errorf("bank details in the list were not refused: %s", out)
	}
	if out := call(owner, list); f.prepared != 1 || !strings.Contains(out, "rpp_1") {
		t.Errorf("the owner's list was not prepared: %s", out)
	}
}
