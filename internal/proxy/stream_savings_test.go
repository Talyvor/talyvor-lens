package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/keypool"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/routingbrain"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/workspace"
)

// B15.3 — a streamed request gets the savings a buffered one gets. Every test here drives the real
// handler with "stream": true and reads what reached the provider, what the client was sent, and what
// was recorded afterwards. (Compression on a stream is TestCompressionGate_AStreamingRequestIsCompressedAndMeasured.)

// streamEcho is an OpenAI-compatible upstream that answers every request as a stream naming the model it
// was SENT — as a provider does, and as the chat reads to name the model that answered — and keeps each
// request's body and Authorization header.
type streamEcho struct {
	mu     sync.Mutex
	bodies [][]byte
	auths  []string
}

func newStreamEcho(t *testing.T, p *Proxy) *streamEcho {
	t.Helper()
	e := &streamEcho{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.bodies = append(e.bodies, b)
		e.auths = append(e.auths, r.Header.Get("Authorization"))
		e.mu.Unlock()
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"model":"`+req.Model+`","choices":[{"delta":{"content":"Paris is the capital of France."}}]}`+"\n\n"+
			`data: {"model":"`+req.Model+`","choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":40}}`+"\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	p.openAIURL = srv.URL
	return e
}

// sentModel is the model field of the n-th request the provider received.
func (e *streamEcho) sentModel(t *testing.T, n int) string {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.bodies) <= n {
		t.Fatalf("the provider received %d request(s); request %d never reached it", len(e.bodies), n)
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(e.bodies[n], &req); err != nil {
		t.Fatalf("request %d body: %v", n, err)
	}
	return req.Model
}

// streamChat sends one streamed chat request through HandleOpenAI.
func streamChat(t *testing.T, p *Proxy, model, content string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":` + content + `}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	return w
}

// A workspace that turned on cost-optimised routing gets it on a stream: the provider is sent the cheaper
// model, the client is told (header) and sees the answering model in the stream (what the chat's footer
// names), and the spend row bills the model that answered. A workspace that did not opt in is sent the
// model it named — the floor that makes the opt-in the variable.
func TestStreamSavings_CostOptimisedRoutingRoutesAStream(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	up := newStreamEcho(t, p)

	streamChat(t, p, "gpt-4o", `"What is the capital of France?"`, nil)
	if got := up.sentModel(t, 0); got != "gpt-4o" {
		t.Fatalf("floor: a stream from a workspace that did not opt in was sent %q, want the named gpt-4o", got)
	}

	if err := p.workspaceManager.SetCostOptimizeRouting(context.Background(), "ws-log", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	w := streamChat(t, p, "gpt-4o", `"What is the capital of Spain?"`, nil)
	if got := up.sentModel(t, 1); got != "gpt-4o-mini" {
		t.Fatalf("an opted-in workspace's stream was sent %q, want the routed gpt-4o-mini", got)
	}
	if h := w.Header().Get("X-Talyvor-Routed"); h != "gpt-4o→gpt-4o-mini" {
		t.Errorf("X-Talyvor-Routed = %q, want gpt-4o→gpt-4o-mini", h)
	}
	if !strings.Contains(w.Body.String(), `"model":"gpt-4o-mini"`) {
		t.Errorf("the stream the client received does not name the model that answered:\n%s", w.Body.String())
	}
	if len(sink.spends) != 2 || sink.spends[1].model != "gpt-4o-mini" {
		t.Fatalf("spend rows = %+v, want the routed stream billed as gpt-4o-mini", sink.spends)
	}
}

// circuitOpenSink is the recording sink with its circuit tripped: every request is to be downgraded.
type circuitOpenSink struct{ *recordingAlertSink }

func (circuitOpenSink) IsCircuitOpen(string, string, string) bool { return true }
func (circuitOpenSink) GetDowngradeModel(string, string) string   { return "gpt-4o-mini" }

// A tripped circuit breaker downgrades a stream, as it downgrades a buffered request, and says so.
func TestStreamSavings_CircuitBreakerDowngradesAStream(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	p.setAlertSink(circuitOpenSink{sink})
	up := newStreamEcho(t, p)

	w := streamChat(t, p, "gpt-4o", `"Summarise the plot of Hamlet."`, nil)
	if got := up.sentModel(t, 0); got != "gpt-4o-mini" {
		t.Fatalf("a stream under an open circuit was sent %q, want the downgrade gpt-4o-mini", got)
	}
	if w.Header().Get("X-Talyvor-Circuit-Open") != "true" {
		t.Error("X-Talyvor-Circuit-Open is not set on the downgraded stream")
	}
	if len(sink.spends) != 1 || sink.spends[0].model != "gpt-4o-mini" {
		t.Fatalf("spend rows = %+v, want one billed as gpt-4o-mini", sink.spends)
	}
}

// An auto-routed stream carrying an image to a text-only model is served by a model that can see it —
// the capability redirect a buffered auto request gets — where it used to be refused with a 422.
func TestStreamSavings_AutoRoutedImageStreamIsServedByACapableModel(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	up := newStreamEcho(t, p)

	image := `[{"type":"text","text":"What is in this picture?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]`
	streamChat(t, p, "gpt-4", image, map[string]string{"X-Talyvor-Auto-Route": "true"})
	sent := up.sentModel(t, 0)
	if !modality.Supports(sent, modality.ModalitySet{HasImage: true}) {
		t.Fatalf("the auto-routed image stream was sent %q, a model that cannot see images", sent)
	}
}

// B17.116 — Chat's "Auto (cheapest good)" streams model "auto" with routing intelligence off: the provider is
// sent the model the router chose, never "auto" (a 404 at OpenAI), the stream names it, and the one spend row bills it.
func TestStreamSavings_AutoStreamWithoutRoutingIntelligenceIsServedByTheRoutersPick(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	up := newStreamEcho(t, p)

	w := streamChat(t, p, "auto", `"Name the smallest planet in one word."`, nil)
	if got := up.sentModel(t, 0); got != "gpt-4o-mini" {
		t.Fatalf("the auto stream was sent %q, want the router's cheap pick gpt-4o-mini", got)
	}
	if h := w.Header().Get("X-Talyvor-Routed"); h != "auto→gpt-4o-mini" {
		t.Errorf("X-Talyvor-Routed = %q, want auto→gpt-4o-mini", h)
	}
	if !strings.Contains(w.Body.String(), `"model":"gpt-4o-mini"`) {
		t.Errorf("the stream the client received does not name the model that answered:\n%s", w.Body.String())
	}
	if len(sink.spends) != 1 || sink.spends[0].model != "gpt-4o-mini" {
		t.Fatalf("spend rows = %+v, want one billed as gpt-4o-mini", sink.spends)
	}
}

// The quieter half of the buffered post-flush seam, on a stream: the session turn, the routing corpus,
// the work tier and the route decision are all written, against the model that answered.
func TestStreamSavings_PostServeRecordsAreWrittenForAStream(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	up := newStreamEcho(t, p)
	p.sessionTracker = session.New(nil)
	p.scorer = quality.New(nil)
	patterns, tiers, routes := &fakeCaptureSink{}, &fakeWorkTierSink{}, &fakeRouteSink{}
	p.SetPatternCapture(patterns, func() bool { return true })
	p.SetWorkTier(tiers, func() bool { return true })
	p.SetRouteDecision(routes, func() bool { return true })
	p.SetRoutingAdvisor(routing.New(noCohorts{}, nil, routing.Config{Enabled: true}))

	streamChat(t, p, "auto", `"What is the capital of France?"`, map[string]string{"X-Talyvor-Session": "sess-b153"})
	served := up.sentModel(t, 0)
	if served == "" || served == "auto" {
		t.Fatalf("the auto stream was sent %q, not a concrete model", served)
	}

	s, ok := p.sessionTracker.GetSession("sess-b153")
	if !ok || s.TurnCount != 1 || s.TotalCostUSD <= 0 || s.TotalInputTokens != 1200 || s.TotalOutputTokens != 40 {
		t.Errorf("session after one streamed answer = %+v (found %v), want 1 turn with its cost and the provider's 1200 / 40 tokens", s, ok)
	}
	if len(patterns.pats) != 1 || patterns.pats[0].ModelUsed != served {
		t.Errorf("routing corpus rows = %+v, want one for %q", patterns.pats, served)
	}
	if tiers.calls != 1 {
		t.Errorf("work-tier rows = %d, want 1", tiers.calls)
	}
	if routes.calls != 1 || routes.last.ActualModel != served {
		t.Errorf("route decisions = %d (last %+v), want 1 naming %q", routes.calls, routes.last, served)
	}
}

type noCohorts struct{}

func (noCohorts) AggregateCohorts(context.Context) ([]mining.CohortStat, error) { return nil, nil }
func (noCohorts) AggregateCohortsTiered(context.Context) ([]mining.CohortStat, error) {
	return nil, nil
}

// A workspace that logs nothing still has its streamed spend counted against its budgets, as its
// buffered spend is: the budget feed is in memory and was never the logging policy's to switch off.
func TestStreamSavings_ALoggingNoneStreamStillReachesItsBudget(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingNone)
	newStreamEcho(t, p)
	b := &recordingBudget{}
	p.budgetService = b

	streamChat(t, p, "gpt-4o", `"What is the capital of France?"`, nil)
	if b.calls != 1 || b.spent <= 0 {
		t.Fatalf("budget feed = %d call(s), $%v, want one positive cost for the streamed answer", b.calls, b.spent)
	}
}

// A stream is sent with a key from the operator's key pool when one is configured, as a buffered request
// is — not always with the deployment's single key.
func TestStreamSavings_AStreamUsesThePooledKey(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	up := newStreamEcho(t, p)
	p.keyPool = keypool.New()
	if _, err := p.keyPool.Add("openai", "pooled-key-b153", "pool-1", 0); err != nil {
		t.Fatalf("add pooled key: %v", err)
	}

	streamChat(t, p, "gpt-4o", `"What is the capital of France?"`, nil)
	if got := up.auths[0]; got != "Bearer pooled-key-b153" {
		t.Fatalf("the stream was sent with Authorization %q, want the pooled key", got)
	}
}

// A workspace whose Routing Brain runs autonomously has its brain's pick applied to an auto-routed
// stream, as to an auto-routed buffered request.
func TestStreamSavings_RoutingBrainAppliesToAStream(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	up := newStreamEcho(t, p)
	// The brain's pick must be on the workspace's allow-list — its hard floor.
	if err := p.workspaceManager.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-log", Name: "log-test", Active: true, LoggingPolicy: workspace.LoggingMetadata,
		AllowedModels: []string{"gpt-4o", "gpt-4o-mini", "brain-pick"},
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	const prompt = "What is the capital of France?"
	p.SetRoutingBrain(buildProxyBrain(t, []routingbrain.Recommendation{{
		WorkspaceID: "ws-log", Difficulty: brainDifficulty(len(prompt)/4, 0), Model: "brain-pick", Verified: true, Reason: "cheapest verified",
	}}, []string{"ws-log"}))

	w := streamChat(t, p, "gpt-4o", `"`+prompt+`"`, map[string]string{"X-Talyvor-Auto-Route": "true"})
	if got := up.sentModel(t, 0); got != "brain-pick" {
		t.Fatalf("the auto-routed stream was sent %q, want the brain's pick", got)
	}
	if h := w.Header().Get("X-Talyvor-Brain"); h != "applied" {
		t.Errorf("X-Talyvor-Brain = %q, want applied", h)
	}
}
