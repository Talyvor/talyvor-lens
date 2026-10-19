package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

type fakeMandates struct{ workspace string }

func (f *fakeMandates) PermissionMandates(_ context.Context, workspaceID, _ string) ([]economy.PermissionMandate, error) {
	f.workspace = workspaceID
	m := economy.PermissionMandate{Status: "active"}
	m.ID = "m-1"
	return []economy.PermissionMandate{m}, nil
}

// B40.2: get_mandates is listed once mandates are set and reads the owner's own workspace's mandates; a key that is
// neither the owner's nor an agent's is refused, as GET /v1/money/permission-mandates refuses it.
func TestMCP_GetMandatesReadsTheCallersWorkspace(t *testing.T) {
	s := newServer(nil, nil, nil, nil, nil, "test")
	f := &fakeMandates{}
	s.SetMandates(f)
	listed := false
	for _, tl := range rpcCall(t, s, "tools/list", nil).Result.(map[string]any)["tools"].([]any) {
		listed = listed || tl.(map[string]any)["name"] == "get_mandates"
	}
	if !listed {
		t.Fatal("get_mandates is not listed")
	}
	call := map[string]any{"name": "get_mandates", "arguments": map[string]any{}}
	if res := rpcCall(t, s, "tools/call", call).Result.(map[string]any); res["isError"] != true || f.workspace != "" {
		t.Errorf("a bare workspace key got %v (read %q), want a refusal", res, f.workspace)
	}
	owner := &auth.AuthContext{WorkspaceID: "ws-owner", AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	resp := s.dispatch(auth.WithAuthContext(context.Background(), owner), rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"),
		Method: "tools/call", Params: json.RawMessage(`{"name":"get_mandates","arguments":{}}`)})
	raw, _ := json.Marshal(resp) // as it goes on the wire
	resp = rpcResponse{}
	_ = json.Unmarshal(raw, &resp)
	got := toolResult(t, resp)
	list, _ := got["mandates"].([]any)
	if f.workspace != "ws-owner" || len(list) != 1 || list[0].(map[string]any)["id"] != "m-1" {
		t.Errorf("get_mandates read workspace %q and answered %v", f.workspace, got)
	}
}
