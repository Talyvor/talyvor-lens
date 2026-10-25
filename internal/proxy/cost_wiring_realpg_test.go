package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// Real-PG proof that the cache-aware cost basis (CostUSDDetailed) is WIRED into the reservation SETTLE —
// the customer's actual bill. A response reporting mostly-CACHED input must settle at the cache-aware
// basis, dramatically below the flat rate; a response with NO cached counts must settle byte-identically
// to today. Asserted on the workspace balance + ledger + agent ceiling, never a status code.

const costWireFunded = int64(100_000_000) // $10 in µLXC

// settleULXC mirrors settleReservation's exact USD→µLXC conversion (ceil), so the expected charge is
// computed the same way the code computes it — no float drift in the assertion.
func settleULXC(usd float64) int64 { return economy.ChargeULXC(usd) }

// costWireProxy wires a full proxy (buffered serve path) onto a real-PG reservation store, funded and
// reservation-active, with a deterministic served model. The caller sets p.openAIURL to a usage upstream.
func costWireProxy(t *testing.T) (*Proxy, *economy.DualTokenStore, *pgxpool.Pool) {
	t.Helper()
	_, store, pool := seamProxy(t) // real-PG store + reservation schema (the bare proxy is discarded)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil // deterministic served model — no legacy router override at proxy.go:1184
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	seamFund(t, pool, "ws-log", costWireFunded)
	return p, store, pool
}

// usageUpstream serves one buffered OpenAI response carrying the given usage JSON (or none when "").
func usageUpstream(t *testing.T, usageJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"choices":[{"message":{"role":"assistant","content":"ok"}}]`
		if usageJSON != "" {
			body += `,"usage":` + usageJSON
		}
		_, _ = io.WriteString(w, body+`}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// driveAgentRequest runs one buffered agent request (a ~10k-token prompt so the output-aware HOLD, at full
// input price, comfortably exceeds any settle — no clamp masks the effect) and returns the recorder.
func driveAgentRequest(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	prompt := strings.Repeat("x", 40000)
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, prompt)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "agent-1", WorkspaceID: "ws-log"}))
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w
}

func ledgerNet(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var net int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount),0) FROM lxc_ledger WHERE workspace_id=$1`, ws).Scan(&net); err != nil {
		t.Fatal(err)
	}
	return net
}

func costWireAgentSpent(t *testing.T, pool *pgxpool.Pool, key string) int64 {
	t.Helper()
	var spent int64
	if err := pool.QueryRow(context.Background(),
		`SELECT spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id=$1`, key).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	return spent
}

// TestCostWiring_BufferedSettle_CacheAware: THE money proof. A provider response of 9,000 CACHED + 1,000
// uncached input must SETTLE at the cache-aware basis — dramatically less than 10,000 uncached at the flat
// rate — and the balance, ledger net, and agent ceiling must reflect that lower delivered charge with the
// hold's excess REFUNDED. Before the wiring fix the settle uses flat CostUSD and this fails on the balance.
func TestCostWiring_BufferedSettle_CacheAware(t *testing.T) {
	p, store, pool := costWireProxy(t)
	p.openAIURL = usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":9000}}`).URL

	w := driveAgentRequest(t, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ov := w.Header().Get("X-Talyvor-Model-Override"); ov != "" {
		t.Fatalf("served model was overridden to %q — the expected-cost calc assumes gpt-4o", ov)
	}

	detailedULXC := settleULXC(alerts.CostUSDDetailed("gpt-4o", 1000, 9000, 0, 100))
	fullULXC := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100)) // the flat basis the OLD wiring charged
	if detailedULXC*10 >= fullULXC*6 {                           // detailed must be < 60% of the flat basis (dramatic)
		t.Fatalf("precondition: detailed %d not dramatically < full %d", detailedULXC, fullULXC)
	}

	// (1) BALANCE reflects the cache-aware charge — the customer paid the detailed basis, NOT the flat one.
	if got := seamBalance(t, store, "ws-log"); got != costWireFunded-detailedULXC {
		t.Errorf("balance after settle = %d, want %d (funded − detailed %d). %d would mean flat CostUSD is still wired.",
			got, costWireFunded-detailedULXC, detailedULXC, costWireFunded-fullULXC)
	}
	// (2) LEDGER net = the delivered charge only (hold − release + spend = −detailed): the hold's excess
	//     was REFUNDED (a larger release), so a cache-heavy response over-holds then settles down correctly.
	if net := ledgerNet(t, pool, "ws-log"); net != -detailedULXC {
		t.Errorf("ledger net = %d, want %d (the DELIVERED cache-aware charge)", net, -detailedULXC)
	}
	// (3) CEILING holds: the agent sub-budget's spent nets to exactly the delivered charge, not the hold.
	if spent := costWireAgentSpent(t, pool, "agent-1"); spent != detailedULXC {
		t.Errorf("agent spent = %d, want %d (ceiling accounts the delivered charge, not the hold)", spent, detailedULXC)
	}
}

// TestCostWiring_BufferedSettle_NoCacheIdenticalToToday: a provider response with NO cached counts must
// settle at EXACTLY the flat CostUSD — byte-identical to today. Guards the additive property: the fix
// only changes the bill when the provider actually reports caching.
func TestCostWiring_BufferedSettle_NoCacheIdenticalToToday(t *testing.T) {
	p, store, pool := costWireProxy(t)
	p.openAIURL = usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100}`).URL // no prompt_tokens_details

	w := driveAgentRequest(t, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	flatULXC := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	if got := seamBalance(t, store, "ws-log"); got != costWireFunded-flatULXC {
		t.Errorf("no-cache balance = %d, want %d (flat CostUSD — identical to today)", got, costWireFunded-flatULXC)
	}
	if net := ledgerNet(t, pool, "ws-log"); net != -flatULXC {
		t.Errorf("no-cache ledger net = %d, want %d (flat CostUSD)", net, -flatULXC)
	}
}
