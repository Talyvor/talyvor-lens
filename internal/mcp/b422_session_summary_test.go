package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/session"
)

// B42.2: get_session_summary answers only for the caller's own workspace's session; another
// workspace's session id is refused exactly like one that does not exist.
func TestMCP_GetSessionSummary_OtherWorkspacesSessionIsNotFound(t *testing.T) {
	tracker := session.New(nil)
	tracker.GetOrCreate(context.Background(), "sess-a", "ws-a", "agent-a")    // workspace A's session
	tracker.GetOrCreate(context.Background(), "sess-own", "default", "agent") // the caller's own
	s := newServer(nil, nil, nil, nil, tracker, "test-version")

	for _, id := range []string{"sess-a", "sess-missing"} {
		resp := rpcCall(t, s, "tools/call", map[string]any{
			"name": "get_session_summary", "arguments": map[string]any{"session_id": id},
		})
		if resp.Error == nil || !strings.Contains(resp.Error.Message, "session not found") {
			t.Errorf("session %q: got %+v, want the error \"session not found\"", id, resp)
		}
	}

	got := toolResult(t, rpcCall(t, s, "tools/call", map[string]any{
		"name": "get_session_summary", "arguments": map[string]any{"session_id": "sess-own"},
	}))
	if got["session_id"] != "sess-own" || got["agent_name"] != "agent" {
		t.Errorf("own session summary = %v, want sess-own / agent", got)
	}
}
