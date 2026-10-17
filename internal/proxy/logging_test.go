package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// recordingAlertSink counts RecordSpend calls and remembers the most
// recent prompt payload so tests can verify metadata mode strips the
// prompt body before persistence. It also captures every spend row (with its
// distill_method) so DISTILL attribution tests can assert per-row tagging.
type recordingAlertSink struct {
	mu            sync.Mutex
	calls         int
	lastPrompt    string
	lastModality  string
	lastEstimated bool
	lastInput     int
	lastOutput    int
	spends        []recordedSpend
}

// recordedSpend is one captured token_events write (the fields DISTILL tests
// assert on).
type recordedSpend struct {
	model         string
	inputTokens   int
	outputTokens  int
	modality      string
	estimated     bool
	distillMethod string
	// serveSource is "" for upstream RecordSpend/RecordSpendWithDistill writes and the
	// cache layer label for RecordCacheServe writes — the sink-level mirror of
	// token_events.serve_source (the store maps "" → 'upstream' via the column DEFAULT).
	serveSource string
	// tare is the Tare metering record (B6.4) on a RecordSpendWithTare write; zero otherwise.
	tare alerts.TareMeter
}

func (r *recordingAlertSink) IsCircuitOpen(string, string, string) bool { return false }
func (r *recordingAlertSink) GetDowngradeModel(_ string, model string) string {
	return model
}
func (r *recordingAlertSink) RecordSpend(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool) error {
	return r.record(model, inputTokens, outputTokens, prompt, modality, estimated, "")
}

func (r *recordingAlertSink) RecordSpendWithDistill(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string) error {
	return r.record(model, inputTokens, outputTokens, prompt, modality, estimated, distillMethod)
}

func (r *recordingAlertSink) RecordSpendWithTare(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string, tare alerts.TareMeter) error {
	if err := r.record(model, inputTokens, outputTokens, prompt, modality, estimated, distillMethod); err != nil {
		return err
	}
	r.mu.Lock()
	r.spends[len(r.spends)-1].tare = tare
	r.mu.Unlock()
	return nil
}

func (r *recordingAlertSink) RecordCacheServe(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, sessionID, requestID, modality, serveSource string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastPrompt = ""
	r.lastModality = modality
	r.lastEstimated = true
	r.lastInput = inputTokens
	r.lastOutput = outputTokens
	r.spends = append(r.spends, recordedSpend{
		model: model, inputTokens: inputTokens, outputTokens: outputTokens,
		modality: modality, estimated: true, serveSource: serveSource,
	})
	return nil
}

func (r *recordingAlertSink) RecordNodeServe(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, sessionID, requestID, modality string, _ float64) error {
	return r.RecordCacheServe(ctx, workspaceID, team, sprint, feature, model, inputTokens, outputTokens, sessionID, requestID, modality, "node")
}

// spendWithServeSource returns the first captured row with the given serve_source
// (and whether one exists). "" matches upstream RecordSpend rows.
func (r *recordingAlertSink) spendWithServeSource(source string) (recordedSpend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.spends {
		if s.serveSource == source {
			return s, true
		}
	}
	return recordedSpend{}, false
}

func (r *recordingAlertSink) record(model string, inputTokens, outputTokens int, prompt, modality string, estimated bool, distillMethod string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastPrompt = prompt
	r.lastModality = modality
	r.lastEstimated = estimated
	r.lastInput = inputTokens
	r.lastOutput = outputTokens
	r.spends = append(r.spends, recordedSpend{
		model: model, inputTokens: inputTokens, outputTokens: outputTokens,
		modality: modality, estimated: estimated, distillMethod: distillMethod,
	})
	return nil
}

// spendWithMethod returns the first captured row with the given distill_method
// (and whether one exists).
func (r *recordingAlertSink) spendWithMethod(method string) (recordedSpend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.spends {
		if s.distillMethod == method {
			return s, true
		}
	}
	return recordedSpend{}, false
}

func newLoggingProxy(t *testing.T, policy workspace.LoggingPolicy) (*Proxy, *recordingAlertSink, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-log", Name: "log-test", Active: true, LoggingPolicy: policy,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	p.openAIURL = srv.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	return p, sink, srv
}

func dispatch(t *testing.T, p *Proxy, wsID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w
}

func TestLogging_NoneSkipsRecordSpend(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingNone)
	w := dispatch(t, p, "ws-log")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sink.calls != 0 {
		t.Errorf("LoggingNone: RecordSpend called %d times, want 0", sink.calls)
	}
	if got := w.Header().Get("X-Talyvor-Logging"); got != "none" {
		t.Errorf("X-Talyvor-Logging = %q, want none", got)
	}
}

func TestLogging_MetadataCallsRecordSpendWithEmptyPrompt(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	w := dispatch(t, p, "ws-log")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sink.calls != 1 {
		t.Errorf("LoggingMetadata: RecordSpend called %d times, want 1", sink.calls)
	}
	if sink.lastPrompt != "" {
		t.Errorf("LoggingMetadata: RecordSpend prompt = %q, want empty (metadata must not persist prompt text)", sink.lastPrompt)
	}
	if got := w.Header().Get("X-Talyvor-Logging"); got != "metadata" {
		t.Errorf("X-Talyvor-Logging = %q, want metadata", got)
	}
}

func TestLogging_FullCallsRecordSpendWithPrompt(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingFull)
	w := dispatch(t, p, "ws-log")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sink.calls != 1 {
		t.Errorf("LoggingFull: RecordSpend called %d times, want 1", sink.calls)
	}
	if sink.lastPrompt == "" {
		t.Errorf("LoggingFull: RecordSpend prompt = empty, want the original prompt text")
	}
	if got := w.Header().Get("X-Talyvor-Logging"); got != "full" {
		t.Errorf("X-Talyvor-Logging = %q, want full", got)
	}
}

func TestLogging_UnknownWorkspaceDefaultsToMetadata(t *testing.T) {
	// No workspace registered → GetLoggingPolicy returns metadata.
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	w := dispatch(t, p, "ws-unknown-id")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Talyvor-Logging"); got != "metadata" {
		t.Errorf("unknown workspace logging policy = %q, want metadata (safe default)", got)
	}
}
