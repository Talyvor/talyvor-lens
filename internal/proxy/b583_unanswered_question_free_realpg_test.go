package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B58.3 — A QUESTION THE AGENT NEVER GOT AN ANSWER TO IS NEVER CHARGED.
//
// With reservations off an agent's question is debited its estimate before the serve, and only a settle
// followed it: a question the upstream never answered kept its debit, and a debit that committed after its
// call gave up was answered 503 "nothing was charged" with the spend row standing. A migrated schema, the
// real handler, reservations off. Asserted on lxc_ledger and agent_postings.

func b583Ask(t *testing.T, p *Proxy, id string, stream bool) *flushRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}],"stream":%t}`, b2313Content(0), stream)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", b2313WS)
	req.Header.Set("X-Talyvor-Request-ID", id)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: b2313Key, WorkspaceID: b2313WS}))
	w := newFlushRecorder()
	p.HandleOpenAI(w, req)
	return w
}

func TestB583_AQuestionTheUpstreamNeverAnsweredIsNotCharged(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	for _, tc := range []struct {
		name   string
		url    string
		stream bool
	}{
		{"upstream down, buffered", downURL, false},
		{"upstream down, streamed", downURL, true},
		{"upstream answers 500, buffered", failing.URL, false},
		{"upstream answers 500, streamed", failing.URL, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, store, pool, agentID := b2313Setup(t, tc.stream, 10000, 100, nil)
			p.openAIURL = tc.url
			if w := b583Ask(t, p, "unanswered", tc.stream); w.Code == http.StatusOK {
				t.Fatalf("status 200; the upstream was meant to fail: %s", w.Body.String())
			}
			_, net, spent, holds := b2313Books(t, pool, agentID)
			if net != 0 || spent != 0 || holds != b2313Fund {
				t.Errorf("after an unanswered question: ledger net %d, agent spent %d and holds %d — want 0, 0 and %d",
					net, spent, holds, b2313Fund)
			}
			if bal := seamBalance(t, store, b2313WS); bal != b2313Funded {
				t.Errorf("workspace balance %d, want %d: nothing was served", bal, b2313Funded)
			}
		})
	}
}

// b583LateCommit books the debit and then reports the error a call whose context ran out reports: the
// commit landed, the caller was told it failed.
type b583LateCommit struct {
	*economy.DualTokenStore
	err error
}

func (s b583LateCommit) SpendLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, requestID string, lxcAmount int64, description string, meta economy.AgentDebitMeta) error {
	if err := s.DualTokenStore.SpendLXCForAgent(context.WithoutCancel(ctx), scopedKeyID, workspaceID, requestID, lxcAmount, description, meta); err != nil {
		return err
	}
	return s.err
}

func TestB583_ADebitThatCommittedAfterItsCallGaveUpIsNeverAnsweredNothingWasCharged(t *testing.T) {
	delivered := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"lock contention", economy.ErrLockContention},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, store, pool, agentID := b2313Setup(t, false, 10000, 100, nil)
			p.SetAgentSpender(b583LateCommit{store, tc.err}, func() bool { return true })
			w := b583Ask(t, p, "late-commit", false)
			if strings.Contains(w.Body.String(), "nothing was charged") {
				t.Errorf("Lens answered %d %q while the debit stands", w.Code, w.Body.String())
			}
			_, net, spent, _ := b2313Books(t, pool, agentID)
			switch w.Code {
			case http.StatusOK: // the question was paid for and answered: charged what it cost, once
				if net != -delivered || spent != delivered {
					t.Errorf("answered: ledger net %d, agent spent %d — want −%d and %d", net, spent, delivered, delivered)
				}
			default: // not answered: charged nothing
				if net != 0 || spent != 0 {
					t.Errorf("answered %d: ledger net %d, agent spent %d — want nothing charged", w.Code, net, spent)
				}
			}
		})
	}
}
