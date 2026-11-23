package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/workspace"
)

// B28.242 — A CLIENT THAT HANGS UP AFTER THE ANSWER IS STILL CHARGED, ON EVERY NON-AGENT CHARGE.
//
// B27.4 detached the agent settles. The workspace's own charges — the shadow LXC debit of a plain key,
// the browser chat's metered and pooled charges, and the pooled-paid mark — still ran on the request's
// context, so a client gone the moment its answer was written cancelled the debit: the answer was free.
// A migrated schema and the real handler; asserted on lxc_ledger.
func TestB28242_AClientThatHangsUpAfterTheAnswerIsStillCharged(t *testing.T) {
	const (
		ws       = "default"
		funded   = int64(100_000_000) // 100 LXC, cash-backed
		prompt   = "what is two plus two?"
		nodeText = "two plus two is four, as the node computed"
	)
	nodeCost := func() int64 {
		usd, _ := alerts.CostUSDResolved("gpt-4o", catalog.PurposeCharge, len(prompt)/4, 0, 0, len(nodeText)/4)
		return economy.ChargeULXC(usd)
	}()

	for _, tc := range []struct {
		name string
		auth *auth.AuthContext
		desc string
	}{
		{name: "a plain key's shadow debit", auth: &auth.AuthContext{APIKeyID: "plain-b28242", WorkspaceID: ws},
			desc: "shadow: AI call billing"},
		{name: "a chat's metered charge", auth: &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey,
			SessionKeyID: "sess-b28242", Scopes: []string{auth.ScopeProxy}}, desc: "chat: metered usage"},
	} {
		t.Run("served by a node: "+tc.name, func(t *testing.T) {
			pool := agentBankDB(t)
			ctx := context.Background()
			store := economy.NewDualTokenStore(nil, pool, nil)
			p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
			if err := p.workspaceManager.RegisterWorkspace(ctx, workspace.Workspace{
				ID: ws, Name: ws, Active: true, LoggingPolicy: workspace.LoggingFull,
			}); err != nil {
				t.Fatal(err)
			}
			p.router = nil
			p.SetLXCSpendSink(store, func() bool { return true })
			node := fakeInferenceNode(t, nodeText, 3, 9)
			r := localrouter.NewRouter(nil)
			r.Register(&localrouter.LocalEndpoint{ID: "node-1", URL: node.URL, Provider: "vllm", Models: []string{"gpt-4o"}, Healthy: true})
			r.UpdateQuality("node-1", "gpt-4o", 0.9, 10)
			p.SetNodeRouter(r, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), node.Client(), true)
			seamFund(t, pool, ws, funded)

			body, _ := json.Marshal(map[string]any{"model": "gpt-4o", "messages": []map[string]string{{"role": "user", "content": prompt}}})
			clientCtx, hangUp := context.WithCancel(context.Background())
			t.Cleanup(hangUp)
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", ws)
			req = req.WithContext(auth.WithAuthContext(clientCtx, tc.auth))
			w := &hangUpRecorder{ResponseRecorder: httptest.NewRecorder(), hangUp: hangUp}
			p.HandleOpenAI(w, req)
			if w.Code != http.StatusOK || w.Header().Get("X-Talyvor-Node-Served") == "" || clientCtx.Err() == nil {
				t.Fatalf("status %d, node %q, client gone = %v — want a node-served 200 the client hung up on; body=%s",
					w.Code, w.Header().Get("X-Talyvor-Node-Served"), clientCtx.Err() != nil, w.Body.String())
			}
			if nodeCost <= 0 {
				t.Fatalf("precondition: the node answer costs %d µLXC — it must be positive to see a charge", nodeCost)
			}
			var rows, charged int64
			if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(-sum(amount), 0)::bigint FROM lxc_ledger
				WHERE workspace_id = $1 AND type = 'spend' AND description = $2`, ws, tc.desc).Scan(&rows, &charged); err != nil {
				t.Fatal(err)
			}
			if rows != 1 || charged != nodeCost {
				t.Errorf("lxc_ledger: %d %q rows totalling %d µLXC — want 1 of %d (the client hung up, so the answer went free)",
					rows, tc.desc, charged, nodeCost)
			}
		})
	}

	t.Run("a chat's pooled cache hit and its paid mark", func(t *testing.T) {
		p, pool, calls := anthropicPoolProxy(t)
		store := economy.NewDualTokenStore(nil, pool, nil)
		p.SetLXCSpendSink(store, func() bool { return false })
		anthropicRequest(t, p, "wsPoolA") // an agent key pays list price upstream; A owns the pooled entry

		body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"` + anthropicPooledPrompt + `"}]}`
		clientCtx, hangUp := context.WithCancel(context.Background())
		t.Cleanup(hangUp)
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "wsPoolB")
		req = req.WithContext(auth.WithAuthContext(clientCtx,
			&auth.AuthContext{WorkspaceID: "wsPoolB", AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w := &hangUpRecorder{ResponseRecorder: httptest.NewRecorder(), hangUp: hangUp}
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK || atomic.LoadInt64(calls) != 1 || clientCtx.Err() == nil {
			t.Fatalf("status %d, upstream calls %d, client gone = %v — want a pooled 200 the client hung up on; body=%s",
				w.Code, atomic.LoadInt64(calls), clientCtx.Err() != nil, w.Body.String())
		}
		pooledCharges := func() (rows, charged int64) {
			t.Helper()
			if err := pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(-sum(amount), 0)::bigint FROM lxc_ledger
				WHERE workspace_id = 'wsPoolB' AND type = 'spend' AND description = 'chat: pooled answer'`).Scan(&rows, &charged); err != nil {
				t.Fatal(err)
			}
			return rows, charged
		}
		if rows, charged := pooledCharges(); rows != 1 || charged <= 0 {
			t.Errorf("lxc_ledger: %d pooled chat charges totalling %d µLXC — want 1, positive (the client hung up, so the answer went free)",
				rows, charged)
		}
		// The paid mark: the same answer asked again is not charged a second time.
		again := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		again.Header.Set("Content-Type", "application/json")
		again.Header.Set("X-Talyvor-Workspace", "wsPoolB")
		again = again.WithContext(auth.WithAuthContext(context.Background(),
			&auth.AuthContext{WorkspaceID: "wsPoolB", AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w2 := httptest.NewRecorder()
		p.HandleAnthropic(w2, again)
		if w2.Code != http.StatusOK {
			t.Fatalf("repeat status %d: %s", w2.Code, w2.Body.String())
		}
		if rows, _ := pooledCharges(); rows != 1 {
			t.Errorf("lxc_ledger: %d pooled chat charges after an exact repeat — want 1 (the paid mark was not recorded)", rows)
		}
	})
}
