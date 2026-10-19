package mcp

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/economy"
)

type fakeMandates struct{ workspace string }

func (f *fakeMandates) PermissionMandates(_ context.Context, workspaceID, _ string) ([]economy.PermissionMandate, error) {
	f.workspace = workspaceID
	m := economy.PermissionMandate{Status: "active"}
	m.ID = "m-1"
	return []economy.PermissionMandate{m}, nil
}

// B40.2: get_mandates is listed once mandates are set and reads the caller's own workspace's mandates.
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
	got := toolResult(t, rpcCall(t, s, "tools/call", map[string]any{"name": "get_mandates", "arguments": map[string]any{}}))
	list, _ := got["mandates"].([]any)
	if f.workspace != "default" || len(list) != 1 || list[0].(map[string]any)["id"] != "m-1" {
		t.Errorf("get_mandates read workspace %q and answered %v", f.workspace, got)
	}
}
