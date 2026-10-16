package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// B23.1 — A THUMBS-DOWN REMOVES A WRONG ANSWER. wsTDA's answer is served to wsTDB from the shared pool;
// wsTDB marks it wrong through the real POST /v1/feedback handler; the answer is gone from
// prompt_embeddings and the exact cache (wsTDA's own copies too), the next identical question goes to
// the model and wsTDB is charged for it on lxc_ledger, and the audit row names who marked it.
// Buffered and streamed, through HandleAnthropic.

var thumbsDownMigrateOnce sync.Once

func thumbsDownDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG thumbs-down test")
	}
	const schema = "proxy_b231_thumbs_down"
	ctx := context.Background()
	thumbsDownMigrateOnce.Do(func() {
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		cfg.RuntimeParams["search_path"] = schema + ",public"
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				t.Fatalf("reset schema: %v", err)
			}
		}
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	})
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	db, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, `DELETE FROM prompt_embeddings`); err != nil {
		t.Fatal(err)
	}
	return db
}

type thumbsDownRig struct {
	p     *Proxy
	db    *pgxpool.Pool
	mr    *miniredis.Miniredis
	calls *int64
}

// newThumbsDownRig is the B21.3 stored-answers rig — pooling on, both workspaces opted in, funded and
// earn-verified, the exact cache in miniredis and the semantic cache in Postgres — with an upstream
// that answers a streamed question as a stream and a buffered one as a message.
func newThumbsDownRig(t *testing.T) *thumbsDownRig {
	t.Helper()
	ctx := context.Background()
	db := thumbsDownDB(t)
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	p.router = nil
	store := economy.NewDualTokenStore(nil, db, nil)
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.SetLXCSpendSink(store, func() bool { return false })
	p.SetLXCGate(store, func() bool { return false })
	p.SetPoolConsumerDiscount(0.30)

	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSE("The capital of the UK is Paris.", true))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"The capital of the UK is Paris."}],`+
			`"model":"claude-haiku-4-5","usage":{"input_tokens":16,"output_tokens":9}}`)
	}))
	t.Cleanup(up.Close)
	p.anthropicURL = up.URL

	exact, mr := newExactCacheForTest(t)
	p.exact = exact
	sem := cache.NewSemanticCache(db, fpEmbedder{}, 0.98, 0)
	sem.SetPairVerifier(yesVerifier{})
	p.semantic, p.embedder = sem, fpEmbedder{}

	wsm := p.workspaceManager
	for _, id := range []string{"wsTDA", "wsTDB"} {
		if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: id, Name: id, Active: true, LoggingPolicy: workspace.LoggingMetadata}); err != nil {
			t.Fatal(err)
		}
		if err := wsm.SetCachePoolable(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)
			ON CONFLICT (id) DO UPDATE SET earn_verified = true`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, $2, $2)
			ON CONFLICT (workspace_id) DO UPDATE SET balance = EXCLUDED.balance, cash_backed_ulxc = EXCLUDED.cash_backed_ulxc`,
			id, costWireFunded); err != nil {
			t.Fatal(err)
		}
	}
	p.SetPoolGate(cache_pooling.New(func() bool { return true }, wsm.GetCachePoolable))
	ledger := mining.NewLedgerStore(db)
	ledger.SetMintVerifier(earnverify.New(false))
	p.SetRoyaltyMinter(poolroyalty.NewMinter(db, ledger, 0.5, func() bool { return true }))
	return &thumbsDownRig{p: p, db: db, mr: mr, calls: &calls}
}

// chat is the browser chat's credential for ws: a session key belonging to "user-"+ws.
func chat(ctx context.Context, ws string) context.Context {
	return auth.WithAuthContext(ctx, &auth.AuthContext{WorkspaceID: ws, UserID: "user-" + ws, SessionKeyID: "sk-" + ws,
		AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}})
}

// ask sends the question for ws under requestID and reports whether the model answered it.
func (g *thumbsDownRig) ask(t *testing.T, ws, requestID string, stream bool) (askedModel bool) {
	t.Helper()
	body := `{"model":"claude-haiku-4-5","max_tokens":4096,"messages":[{"role":"user","content":"what is the capital of the UK?"}]}`
	if stream {
		body = chatQuestion
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", ws)
	req.Header.Set("X-Talyvor-Request-ID", requestID)
	req = req.WithContext(chat(req.Context(), ws))
	before := atomic.LoadInt64(g.calls)
	w := newFlushRecorder()
	g.p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ws=%s status=%d body=%s", ws, w.Code, w.Body.String())
	}
	return atomic.LoadInt64(g.calls) > before
}

func (g *thumbsDownRig) count(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := g.db.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (g *thumbsDownRig) exactCopies() int {
	n := 0
	for _, k := range g.mr.Keys() {
		if strings.HasPrefix(k, "lens:exact:") && !strings.HasSuffix(k, ":owner") {
			n++
		}
	}
	return n
}

func (g *thumbsDownRig) thumbsDown(t *testing.T, ws, requestID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback",
		strings.NewReader(`{"request_id":"`+requestID+`","signal":"negative"}`))
	req = req.WithContext(chat(req.Context(), ws))
	w := httptest.NewRecorder()
	AnswerFeedbackHandler(g.p, g.db, func(context.Context, string, string, string) (bool, error) { return true, nil })(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("thumbs-down %s: %d %s", requestID, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestThumbsDown_AnAnswerServedFromThePoolAndMarkedWrongIsNeverServedAgain(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := map[bool]string{true: "streamed", false: "buffered"}[stream]
		t.Run(name, func(t *testing.T) {
			g := newThumbsDownRig(t)
			if !g.ask(t, "wsTDA", name+"-a1", stream) {
				t.Fatal("setup: the first question must reach the model")
			}
			if g.ask(t, "wsTDB", name+"-b1", stream) {
				t.Fatal("setup: wsTDB was not served wsTDA's answer from the pool")
			}
			if g.count(t, `SELECT count(*) FROM prompt_embeddings`) == 0 || g.exactCopies() == 0 {
				t.Fatal("setup: nothing was stored — the removal below would prove nothing")
			}
			modelCharges := `SELECT count(*) FROM lxc_ledger WHERE workspace_id = 'wsTDB' AND amount < 0 AND description <> 'chat: pooled answer'`
			chargedBefore := g.count(t, modelCharges)

			out := g.thumbsDown(t, "wsTDB", name+"-b1")

			if n := g.count(t, `SELECT count(*) FROM prompt_embeddings`); n != 0 {
				t.Errorf("%d stored rows of the answer remain in prompt_embeddings after it was marked wrong (%s)", n, out)
			}
			if n := g.exactCopies(); n != 0 {
				t.Errorf("%d exact-cache copies of the answer remain after it was marked wrong (%s)", n, out)
			}
			var markedBy, servedFrom string
			if err := g.db.QueryRow(context.Background(), `SELECT marked_by, served_from FROM answer_removals
				WHERE workspace_id = 'wsTDB' AND request_id = $1 AND signal = 'negative'`, name+"-b1").Scan(&markedBy, &servedFrom); err != nil {
				t.Fatalf("no audit row for the thumbs-down: %v", err)
			}
			if !strings.Contains(markedBy, "user:user-wsTDB") || servedFrom != "cache_hit_pooled" {
				t.Errorf("audit row says marked_by %q, served_from %q; want wsTDB's user and cache_hit_pooled", markedBy, servedFrom)
			}

			if !g.ask(t, "wsTDB", name+"-b2", stream) {
				t.Error("wsTDB was served the answer it marked wrong")
			}
			if got := g.count(t, modelCharges); got != chargedBefore+1 {
				t.Errorf("wsTDB has %d model-call charges after asking again, want %d — the repeat was not charged a model call",
					got, chargedBefore+1)
			}
		})
	}
}

// A thumbs-down on a fresh answer removes what it was stored as, so it is never served.
func TestThumbsDown_AFreshAnswerMarkedWrongIsNotKept(t *testing.T) {
	g := newThumbsDownRig(t)
	if !g.ask(t, "wsTDA", "fresh-a1", true) {
		t.Fatal("setup: the first question must reach the model")
	}
	g.thumbsDown(t, "wsTDA", "fresh-a1")
	if n := g.count(t, `SELECT count(*) FROM prompt_embeddings`); n != 0 || g.exactCopies() != 0 {
		t.Errorf("%d rows and %d exact copies of a fresh answer marked wrong were kept", n, g.exactCopies())
	}
	if !g.ask(t, "wsTDB", "fresh-b1", true) {
		t.Error("a fresh answer marked wrong was served from the pool")
	}
}
