package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/controlplane"
	"github.com/talyvor/lens/internal/proxy"
	"github.com/talyvor/lens/internal/quality"
)

// B42.2 on a migrated schema: workspace B naming workspace A's node or prompt hash is refused with
// 404 and nothing of A's changes (last_seen_at, the Redis heartbeat, hit_count); B's own still works.

func TestNodeHeartbeats_OtherWorkspacesNodeIs404AndStaysAsItWas(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	mr := miniredis.RunT(t)
	hb := controlplane.NewHeartbeatStore(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	r := chi.NewRouter()
	r.Post("/v1/workspaces/{wsID}/nodes/{nodeID}/heartbeat", newInferenceHeartbeatHandler(pool, hb))
	r.Post("/v1/workspaces/{wsID}/cache-nodes/{nodeID}/heartbeat", newCacheHeartbeatHandler(pool, hb))
	r.Post("/v1/workspaces/{wsID}/embedding-nodes/{nodeID}/heartbeat", newEmbeddingHeartbeatHandler(controlplane.NewNodeStore(pool, hb), hb))

	long := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	for _, c := range []struct{ kind, table, route, insert string }{
		{"inference", "inference_nodes", "nodes", `INSERT INTO inference_nodes (id, workspace_id, url, provider, last_seen_at) VALUES ($1, $2, 'http://n', 'ollama', $3)`},
		{"cache", "cache_nodes", "cache-nodes", `INSERT INTO cache_nodes (id, workspace_id, url, last_seen_at) VALUES ($1, $2, 'http://n', $3)`},
		{"embedding", "embedding_nodes", "embedding-nodes", `INSERT INTO embedding_nodes (id, workspace_id, url, model, last_seen_at) VALUES ($1, $2, 'http://n', 'm', $3)`},
	} {
		t.Run(c.kind, func(t *testing.T) {
			nodeA, nodeB := c.kind+"-node-a", c.kind+"-node-b"
			for _, n := range [][2]string{{nodeA, "ws-a"}, {nodeB, "ws-b"}} {
				if _, err := pool.Exec(ctx, c.insert, n[0], n[1], long); err != nil {
					t.Fatalf("seed %s: %v", n[0], err)
				}
			}
			beat := func(node string) int {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-b/"+c.route+"/"+node+"/heartbeat", strings.NewReader(`{"uptime_seconds":5}`)))
				return w.Code
			}
			lastSeen := func(node string) time.Time {
				var at time.Time
				if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM `+c.table+` WHERE id = $1`, node).Scan(&at); err != nil {
					t.Fatal(err)
				}
				return at.UTC()
			}

			if code := beat(nodeA); code != http.StatusNotFound {
				t.Errorf("ws-b beating ws-a's node: status %d, want 404", code)
			}
			if got := lastSeen(nodeA); !got.Equal(long) {
				t.Errorf("ws-a's node last_seen_at moved to %v", got)
			}
			if fresh, _, _ := hb.IsFresh(ctx, c.kind, nodeA); fresh {
				t.Errorf("ws-a's node got a fresh Redis heartbeat from ws-b")
			}

			if code := beat(nodeB); code != http.StatusOK {
				t.Fatalf("ws-b beating its own node: status %d, want 200", code)
			}
			if got := lastSeen(nodeB); !got.After(long) {
				t.Errorf("own node last_seen_at did not move")
			}
			if fresh, _, _ := hb.IsFresh(ctx, c.kind, nodeB); !fresh {
				t.Errorf("own node has no fresh Redis heartbeat")
			}
		})
	}
}

func TestFeedback_PromptHashOfAnotherWorkspaceIs404AndHitCountUnchanged(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	for _, row := range [][2]string{{"hash-a", "ws-a"}, {"hash-b", "ws-b"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO prompt_embeddings (provider, model, prompt_hash, response, hit_count, workspace_id) VALUES ('openai', 'gpt-4', $1, 'answer', 5, $2)`, row[0], row[1]); err != nil {
			t.Fatalf("seed %s: %v", row[0], err)
		}
	}
	scorer := quality.New(pool)
	h := proxy.AnswerFeedbackHandler(nil, pool, func(ctx context.Context, wsID, promptHash, signal string) (bool, error) {
		return scorer.RecordFeedback(ctx, wsID, promptHash, quality.FeedbackSignal(signal))
	})
	send := func(hash string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/feedback", strings.NewReader(`{"prompt_hash":"`+hash+`","signal":"positive"}`))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "ws-b", AuthMethod: "api_key", APIKeyID: "key-b"}))
		w := httptest.NewRecorder()
		h(w, req)
		return w.Code
	}
	hits := func(hash string) (n int) {
		if err := pool.QueryRow(ctx, `SELECT hit_count FROM prompt_embeddings WHERE prompt_hash = $1`, hash).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if code := send("hash-a"); code != http.StatusNotFound {
		t.Errorf("ws-b scoring ws-a's prompt hash: status %d, want 404", code)
	}
	if n := hits("hash-a"); n != 5 {
		t.Errorf("ws-a's hit_count = %d, want it unchanged at 5", n)
	}
	if code := send("hash-b"); code != http.StatusOK {
		t.Fatalf("ws-b scoring its own prompt hash: status %d, want 200", code)
	}
	if n := hits("hash-b"); n != 6 {
		t.Errorf("own hit_count = %d, want 6", n)
	}
}
