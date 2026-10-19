package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B40.1 — the official TypeScript MCP SDK client (testdata/sdk-client) connects to /mcp over Streamable HTTP, lists
// every tool with its annotations, reads agent_balance and pays with agent_pay on an agent's own key, then does the
// same over the 2024-11-05 HTTP+SSE transport. The payment is checked on the ledger, not on the tool's answer.
// Needs node and `npm ci` in testdata/sdk-client: LENS_MCP_SDK_E2E=1 (the mcp-sdk CI job) makes their absence a failure.
func TestOfficialTypeScriptSDKClient(t *testing.T) {
	dir, _ := filepath.Abs("testdata/sdk-client")
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		if os.Getenv("LENS_MCP_SDK_E2E") != "" {
			t.Fatalf("LENS_MCP_SDK_E2E is set but %s has no node_modules: run npm ci there", dir)
		}
		t.Skip("needs the MCP SDK: npm ci in testdata/sdk-client, or LENS_MCP_SDK_E2E=1 (the mcp-sdk CI job)")
	}
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws = "ws-sdk-client"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	buyer, err := store.CreateAgent(ctx, ws, "buyer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	seller, err := store.CreateAgent(ctx, ws, "seller", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, buyer.ID, "key-buyer"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, buyer.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)

	// Mounted as cmd/lens mounts it, behind an auth stand-in that knows one key.
	keyAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer key-buyer" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAuthContext(r.Context(), &auth.AuthContext{APIKeyID: "key-buyer", WorkspaceID: ws})))
		})
	}
	r := chi.NewRouter()
	r.With(keyAuth).Post("/mcp", srv.HandleRPC)
	r.With(keyAuth).Get("/mcp/sse", srv.HandleSSE)
	lens := httptest.NewServer(r)
	t.Cleanup(lens.Close)

	cmd := exec.Command("node", filepath.Join(dir, "check.mjs"), lens.URL, "key-buyer", seller.ID)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("the SDK client failed: %v\n%s%s", err, out, stderr)
	}
	var got struct {
		ProtocolVersion string `json:"protocolVersion"`
		SessionID       string `json:"sessionId"`
		Tools           int    `json:"tools"`
		BalanceBefore   int64  `json:"balanceBefore"`
		EntryID         string `json:"entryId"`
		SSETools        int    `json:"sseTools"`
		SSEBalance      int64  `json:"sseBalance"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("the SDK client printed %q: %v", out, err)
	}
	want := len(srv.tools())
	if got.ProtocolVersion != handshakeVersions[0] || got.SessionID == "" || got.Tools != want || got.SSETools != want {
		t.Errorf("SDK client saw version %q, session %q, %d tools (SSE %d), want %s, a session and %d", got.ProtocolVersion,
			got.SessionID, got.Tools, got.SSETools, handshakeVersions[0], want)
	}
	if got.BalanceBefore != 10_000_000 || got.SSEBalance != 8_500_000 {
		t.Errorf("agent_balance read %d before and %d over SSE after paying 1.5 LXC, want 10,000,000 and 8,500,000", got.BalanceBefore, got.SSEBalance)
	}
	// The ledger: agent_pay's entry is two postings that sum to zero, 1.5 LXC from the buyer to the seller.
	var from, to int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_ulxc) FILTER (WHERE account = 'agent:' || $2), 0),
		COALESCE(SUM(amount_ulxc) FILTER (WHERE account = 'agent:' || $3), 0) FROM agent_postings WHERE entry_id = $1::uuid`,
		got.EntryID, buyer.ID, seller.ID).Scan(&from, &to); err != nil {
		t.Fatal(err)
	}
	if from != -1_500_000 || to != 1_500_000 {
		t.Errorf("agent_pay's entry %s posted %d to the buyer and %d to the seller, want -1,500,000 and +1,500,000", got.EntryID, from, to)
	}
}
