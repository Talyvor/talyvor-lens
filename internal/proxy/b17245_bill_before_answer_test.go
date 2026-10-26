package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// wireWatchingSpender is fundingSpender that notes what the client had been sent when the hold is settled.
type wireWatchingSpender struct {
	fundingSpender
	w      **httptest.ResponseRecorder
	atBill *string
}

func (s wireWatchingSpender) SettleLXCReservation(ctx context.Context, id string, finalLXC int64, m economy.AgentDebitMeta) (int64, int64, error) {
	*s.atBill = (*s.w).Body.String()
	return s.fundingSpender.SettleLXCReservation(ctx, id, finalLXC, m)
}

// B17.245 — the SDK quickstart reads its agent's statement the moment the answer arrives, and found only the hold:
// the settle and the platform fee were written after the answer. The answer (buffered) or the stream's end
// (streamed) now reaches the client only once the bill is booked.
func TestB17245_AnAgentsAnswerEndsOnlyAfterItsBillIsBooked(t *testing.T) {
	for _, stream := range []bool{false, true} {
		off := false
		p, _, _, _, _ := newPoolingProxy(t, &off)
		if stream {
			p.openAIURL = sseUpstream(t, openAISSEBody).URL
		}
		var w *httptest.ResponseRecorder
		atBill := "not settled"
		p.SetAgentSpender(wireWatchingSpender{w: &w, atBill: &atBill}, func() bool { return true })
		p.SetReservation(func() bool { return true }, func() int { return 4096 })

		body := `{"model":"gpt-4o","messages":[{"role":"user","content":"Repeat this exact string and nothing else: lesazavuri"}]}`
		if stream {
			body = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"Repeat this exact string and nothing else: lesazavuri"}]}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "wsA")
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "wsA", APIKeyID: "agent-wsA"}))
		w = httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Fatalf("stream=%v: status %d %q", stream, w.Code, w.Body.String())
		}
		if !stream && atBill != "" {
			t.Fatalf("stream=%v: the hold was settled with %q already sent, want the bill before the answer", stream, atBill)
		}
		if stream && (atBill == "not settled" || strings.Contains(atBill, "[DONE]") || !strings.Contains(w.Body.String(), "[DONE]")) {
			t.Fatalf("stream=%v: at the bill the client had %q, and in the end %q; want the [DONE] after the bill", stream, atBill, w.Body.String())
		}
	}
}
