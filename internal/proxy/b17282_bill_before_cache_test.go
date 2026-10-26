package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// cacheWatchingSpender is fundingSpender that notes how many cache keys exist when the hold is settled.
type cacheWatchingSpender struct {
	fundingSpender
	mr            *miniredis.Miniredis
	keysAtSettle  *int
	settlesCalled *int
}

func (s cacheWatchingSpender) SettleLXCReservation(ctx context.Context, id string, finalLXC int64, m economy.AgentDebitMeta) (int64, int64, error) {
	*s.keysAtSettle = len(s.mr.Keys())
	*s.settlesCalled++
	return s.fundingSpender.SettleLXCReservation(ctx, id, finalLXC, m)
}

// B17.282 — an agent's served request is billed before Lens stores its answer: the cache writes after the answer
// can take seconds, and a client reading its ledger in that time found the hold and no spend row.
func TestB17282_AServedAgentRequestIsSettledBeforeItsAnswerIsCached(t *testing.T) {
	for _, stream := range []bool{false, true} {
		off := false
		p, _, _, _, _ := newPoolingProxy(t, &off)
		exact, mr := newExactCacheForTest(t)
		p.exact = exact
		if stream {
			p.openAIURL = sseUpstream(t, openAISSEBody).URL
		}
		keys, settles := -1, 0
		p.SetAgentSpender(cacheWatchingSpender{mr: mr, keysAtSettle: &keys, settlesCalled: &settles}, func() bool { return true })
		p.SetReservation(func() bool { return true }, func() int { return 4096 })

		body := `{"model":"gpt-4o","messages":[{"role":"user","content":"what is 1992 + 1484"}]}`
		if stream {
			body = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"what is 1992 + 1484"}]}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "wsA")
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "wsA", APIKeyID: "agent-wsA"}))
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("stream=%v: status %d %s", stream, w.Code, w.Body.String())
		}
		if settles != 1 || len(mr.Keys()) == 0 {
			t.Fatalf("stream=%v: %d settle(s) and %d cache key(s) after the request, want one settle and the answer cached", stream, settles, len(mr.Keys()))
		}
		if keys != 0 {
			t.Fatalf("stream=%v: the hold was settled with %d cache key(s) already written, want the bill before the cache", stream, keys)
		}
	}
}
