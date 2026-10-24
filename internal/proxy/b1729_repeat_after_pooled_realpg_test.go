package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B17.29 — an exact repeat in a new chat is the earlier answer at 0 LXC, even when the earlier answer
// came from the shared pool. Testers 311 and 391 were charged the pooled price a second time for it.
// The chat streams, so both serve points are proved.
func TestB1729_ExactRepeatOfAPooledAnswer_IsTheAccountsOwnAtNoCharge(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "streamed", false: "buffered"}[stream], func(t *testing.T) {
			exactRepeatOfAPooledAnswer(t, stream)
		})
	}
}

func exactRepeatOfAPooledAnswer(t *testing.T, stream bool) {
	p, pool, calls := anthropicPoolProxy(t)
	armProductionMinter(t, p, pool)
	store := economy.NewDualTokenStore(nil, pool, nil)
	p.SetLXCSpendSink(store, func() bool { return false })
	p.SetLXCGate(store, func() bool { return false })

	anthropicRequest(t, p, "wsPoolA") // A asks the model; its answer is pooled

	chat := func() *httptest.ResponseRecorder {
		body := `{"model":"claude-haiku-4-5","stream":` + strconv.FormatBool(stream) +
			`,"messages":[{"role":"user","content":"` + anthropicPooledPrompt + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "wsPoolB")
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: "wsPoolB", AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
		w := httptest.NewRecorder()
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("chat request status %d: %s", w.Code, w.Body.String())
		}
		return w
	}
	ledgerRows := func(sql string) int {
		var n int
		if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	charges := `SELECT count(*) FROM lxc_ledger WHERE workspace_id = 'wsPoolB' AND amount < 0`
	mints := `SELECT count(*) FROM pool_royalty_mints`

	first := chat()
	if first.Header().Get("X-Talyvor-Pool-Charged-ULXC") == "" || ledgerRows(charges) != 1 || ledgerRows(mints) != 1 {
		t.Fatalf("setup: B's first ask was not a charged pooled serve (charge rows %d, royalty claims %d)",
			ledgerRows(charges), ledgerRows(mints))
	}
	// B17.207 — the charged serve is no replay: a caller reading it as one books nothing for its spend row.
	if first.Header().Get("X-Talyvor-Cache-Replay") != "" {
		t.Errorf("the charged pooled serve is marked a replay, which says it cost nothing")
	}

	again := chat() // the same question, in a new chat
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("upstream called %d times, want 1 — the repeat asked the model", got)
	}
	if again.Body.String() != first.Body.String() {
		t.Errorf("the repeat answered %q, want the earlier answer %q", again.Body.String(), first.Body.String())
	}
	if again.Header().Get("X-Talyvor-Cache-Replay") != "true" {
		t.Errorf("the repeat is not marked a replay, so the chat cannot say \"from your earlier answer · 0 LXC\"")
	}
	if h := again.Header().Get("X-Talyvor-Pool-Charged-ULXC"); h != "" {
		t.Errorf("the repeat was priced as a pooled serve (%s µLXC), want its own earlier answer at 0", h)
	}
	if n := ledgerRows(charges); n != 1 {
		t.Errorf("wsPoolB has %d charge rows after the repeat, want 1 — the repeat was charged", n)
	}
	if n := ledgerRows(mints); n != 1 {
		t.Errorf("%d royalty claims after the repeat, want 1 — the contributor was paid twice for one answer", n)
	}
}
