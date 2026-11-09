package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// Real-PG proof that a STREAMING agent request SETTLES its reservation in-band (closing the revenue hole
// where the streaming settle ran on context.Background(), never fired, and the hold was swept + refunded —
// i.e. streaming was FREE) — and settles at the cache-aware delivered cost, like the buffered path.

// streamCachedSSE: an OpenAI SSE stream whose final usage chunk reports 9,000 cached + 1,000 uncached input.
const streamCachedSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100,\"prompt_tokens_details\":{\"cached_tokens\":9000}}}\n\n" +
	"data: [DONE]\n\n"

func reservationStatus(t *testing.T, pool *pgxpool.Pool, ws string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(MAX(status),'<none>') FROM lxc_reservations WHERE workspace_id=$1`, ws).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// reservationLedgerRows sums the ledger amounts by type for a workspace.
func reservationLedgerRows(t *testing.T, pool *pgxpool.Pool, ws string) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT type, SUM(amount) FROM lxc_ledger WHERE workspace_id=$1 GROUP BY type`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var typ string
		var amt int64
		if err := rows.Scan(&typ, &amt); err != nil {
			t.Fatal(err)
		}
		out[typ] = amt
	}
	return out
}

// driveStreamAgentRequest runs one STREAMING agent request (stream:true; a ~10k-token prompt so the
// output-aware HOLD comfortably exceeds the settle — no clamp) and returns the recorder.
func driveStreamAgentRequest(t *testing.T, p *Proxy) *flushRecorder {
	t.Helper()
	prompt := strings.Repeat("x", 40000)
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}],"stream":true}`, prompt)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "agent-1", WorkspaceID: "ws-log"}))
	w := newFlushRecorder()
	p.HandleOpenAI(w, req)
	return w
}

// TestStreamSettle_CacheAware_SettlesInBand: THE revenue-hole proof. A streaming agent request must SETTLE
// in-band at the cache-aware delivered cost. Before the fix the settle runs on context.Background(), the
// reservation stays 'held' (stranded → swept → refunded), and the request is FREE.
func TestStreamSettle_CacheAware_SettlesInBand(t *testing.T) {
	p, store, pool := costWireProxy(t)
	p.openAIURL = sseUpstream(t, streamCachedSSE).URL

	w := driveStreamAgentRequest(t, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	detailedULXC := settleULXC(alerts.CostUSDDetailed("gpt-4o", 1000, 9000, 0, 100))
	fullULXC := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	if detailedULXC*10 >= fullULXC*6 { // detailed must be < 60% of flat (cache-aware, dramatic)
		t.Fatalf("precondition: detailed %d not dramatically < full %d", detailedULXC, fullULXC)
	}

	// (1) SETTLED, not held — the revenue hole is closed (a 'held' reservation is swept → refunded → free).
	if st := reservationStatus(t, pool, "ws-log"); st != "settled" {
		t.Errorf("reservation status = %q, want 'settled' (a stranded 'held' reservation means the streaming request was FREE)", st)
	}
	// (2) BALANCE = the delivered cache-aware cost — not the full hold (stranded), not a refund (funded).
	if bal := seamBalance(t, store, "ws-log"); bal != costWireFunded-detailedULXC {
		t.Errorf("balance = %d, want %d (funded − delivered detailed %d)", bal, costWireFunded-detailedULXC, detailedULXC)
	}
	// (3) BOTH ledger rows the settle adds: a reservation_release (+hold, undo) and a spend (−detailed, the
	//     bill). Net over hold − release + spend = −detailed.
	rows := reservationLedgerRows(t, pool, "ws-log")
	if rows[economy.LXCTypeReservationRelease] <= 0 {
		t.Errorf("missing the reservation_release (+hold) refund row: %+v", rows)
	}
	if rows[economy.LXCTypeSpend] != -detailedULXC {
		t.Errorf("spend row = %d, want %d (the delivered cache-aware bill)", rows[economy.LXCTypeSpend], -detailedULXC)
	}
	if net := ledgerNet(t, pool, "ws-log"); net != -detailedULXC {
		t.Errorf("ledger net = %d, want %d", net, -detailedULXC)
	}
}

// TestStreamSettle_NoCacheIdenticalToFlat: a streamed response with NO cached counts settles at the flat
// CostUSD — the extended SSE parser + detailed wiring leave the no-cache case byte-identical to before.
func TestStreamSettle_NoCacheIdenticalToFlat(t *testing.T) {
	p, store, pool := costWireProxy(t)
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100}}\n\n" +
		"data: [DONE]\n\n"
	p.openAIURL = sseUpstream(t, sse).URL
	if w := driveStreamAgentRequest(t, p); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	flatULXC := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	if st := reservationStatus(t, pool, "ws-log"); st != "settled" {
		t.Errorf("status = %q, want 'settled' (no-cache must still settle in-band)", st)
	}
	if bal := seamBalance(t, store, "ws-log"); bal != costWireFunded-flatULXC {
		t.Errorf("no-cache streaming balance = %d, want %d (flat CostUSD, identical to buffered no-cache)", bal, costWireFunded-flatULXC)
	}
}

// TestStreamSettle_CrossTenantPooledHit_FundsViaResolve answers the funding-invariant question on the
// STREAMING lane: a streamed cross-tenant POOLED cache hit is served by the SSE-replay path (serve() —
// BEFORE the stream-handler dispatch), which calls settlePooledServe → mintPooledRoyalty(funded)
// exactly like the buffered path. So the streamed pooled hit does NOT bypass settlePooledServe: the
// consumer's reservation is SETTLED at avoided_COGS (a positive charge), which is the mint's funding basis
// (#351 proves settlePooledServe's return funds the mint). A bypass would leave the hold stranded.
func TestStreamSettle_CrossTenantPooledHit_FundsViaResolve(t *testing.T) {
	p, _, pool := costWireProxy(t) // full serve proxy + reservation store; ws-log is the consumer (agent)
	// A COUNTING upstream so we can PROVE the streamed request was served from the pool (no upstream call) —
	// i.e. it took the cache-hit lane (settlePooledServe), not a miss (my recordStreamSpend, which
	// would also settle and thus not distinguish the funding path).
	var upstreamCalls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"pooled answer"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(up.Close)
	p.openAIURL = up.URL

	wsm := p.workspaceManager
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "wsA", Name: "wsA", Active: true, LoggingPolicy: workspace.LoggingMetadata,
	}); err != nil {
		t.Fatal(err)
	}
	// Pooling ON, both workspaces poolable, so wsA's entry is cross-tenant readable by ws-log.
	p.SetPoolGate(cache_pooling.New(func() bool { return true }, wsm.GetCachePoolable))
	if err := wsm.SetCachePoolable(context.Background(), "wsA", true); err != nil {
		t.Fatal(err)
	}
	if err := wsm.SetCachePoolable(context.Background(), "ws-log", true); err != nil {
		t.Fatal(err)
	}

	const prompt = "cross-tenant pooled prompt zzz"
	// (1) wsA (contributor) seeds the private + pooled cache — a plain buffered request.
	seed := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, prompt)
	sreq := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(seed))
	sreq.Header.Set("Content-Type", "application/json")
	sreq.Header.Set("X-Talyvor-Workspace", "wsA")
	sw := httptest.NewRecorder()
	p.HandleOpenAI(sw, sreq)
	if sw.Code != http.StatusOK {
		t.Fatalf("seed status = %d, body=%s", sw.Code, sw.Body.String())
	}
	beforeStream := atomic.LoadInt64(&upstreamCalls) // wsA hit the upstream once; the streamed hit must not

	// (2) ws-log (consumer, AGENT, STREAMING) requests the SAME prompt → cross-tenant POOLED hit via
	//     replayAsSSE (served in serve() before the stream dispatch).
	streamBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}],"stream":true}`, prompt)
	streq := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(streamBody))
	streq.Header.Set("Content-Type", "application/json")
	streq.Header.Set("X-Talyvor-Workspace", "ws-log")
	streq = streq.WithContext(auth.WithAuthContext(streq.Context(), &auth.AuthContext{APIKeyID: "agent-1", WorkspaceID: "ws-log"}))
	stw := newFlushRecorder()
	p.HandleOpenAI(stw, streq)
	if stw.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body=%s", stw.Code, stw.Body.String())
	}

	// PROVE the streamed request took the POOLED-HIT lane: it served from the pool with NO upstream call.
	// (Else it was a miss served live, and the settle would be recordStreamSpend, not settlePooledServe.)
	if delta := atomic.LoadInt64(&upstreamCalls) - beforeStream; delta != 0 {
		t.Fatalf("streamed request made %d upstream calls — it was NOT a pooled cache hit, so this doesn't exercise settlePooledServe", delta)
	}

	// THE ANSWER — no bypass: the streamed pooled hit SETTLED the consumer's reservation (only
	// settlePooledServe does that on this path), so the funding tie fired.
	if st := reservationStatus(t, pool, "ws-log"); st != "settled" {
		t.Errorf("streamed cross-tenant pooled hit left reservation %q, want 'settled' — the streaming lane must invoke settlePooledServe (which funds the mint), not bypass it (which would strand the hold → free)", st)
	}
	// The consumer was actually CHARGED avoided_COGS (a spend row) — that positive charge is exactly the
	// mint's funding basis, so a streamed pooled hit funds a real royalty (never mints on $0).
	rows := reservationLedgerRows(t, pool, "ws-log")
	if rows[economy.LXCTypeSpend] >= 0 {
		t.Errorf("no spend charge on the streamed pooled hit (rows=%+v) — avoided_COGS must be billed, else the mint is unfunded", rows)
	}
}

// TestStreamSettle_SweeperDoesNotDoubleResolve: once the streaming settle fires in-band, the stranded-hold
// sweeper must NOT also release the (now 'settled') reservation — proving the two paths cannot both resolve
// the same reservation (status-CAS under FOR UPDATE: settle flips held→settled; the sweeper matches only held).
func TestStreamSettle_SweeperDoesNotDoubleResolve(t *testing.T) {
	p, store, pool := costWireProxy(t)
	p.openAIURL = sseUpstream(t, streamCachedSSE).URL
	if w := driveStreamAgentRequest(t, p); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	detailedULXC := settleULXC(alerts.CostUSDDetailed("gpt-4o", 1000, 9000, 0, 100))
	balBefore := seamBalance(t, store, "ws-log")
	if balBefore != costWireFunded-detailedULXC {
		t.Fatalf("precondition: the settle must fire in-band first (balance=%d, want %d)", balBefore, costWireFunded-detailedULXC)
	}

	// Age the reservation so the sweeper WOULD act on it IF it were still 'held'.
	if _, err := pool.Exec(context.Background(),
		`UPDATE lxc_reservations SET created_at = now() - interval '1 hour' WHERE workspace_id='ws-log'`); err != nil {
		t.Fatal(err)
	}
	n, err := store.ReleaseStrandedReservations(context.Background(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sweeper released %d reservations, want 0 (a settled reservation must not be swept)", n)
	}
	if bal := seamBalance(t, store, "ws-log"); bal != balBefore {
		t.Errorf("balance changed after sweep: %d → %d (double-resolve / double-refund)", balBefore, bal)
	}
}
