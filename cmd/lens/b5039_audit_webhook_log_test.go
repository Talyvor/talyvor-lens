package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/auth"
)

type failingAuditWebhook struct{}

func (failingAuditWebhook) ExportWebhook(context.Context, string, audit.ExportFilter) error {
	return errors.New("audit: webhook returned status 500")
}

// signalHandler records log lines and closes logged on the first one, so the test can wait for the export
// goroutine without polling a shared buffer.
type signalHandler struct {
	slog.Handler
	mu     *sync.Mutex
	logged chan struct{}
	once   *sync.Once
}

func (h signalHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	err := h.Handler.Handle(ctx, r)
	h.mu.Unlock()
	h.once.Do(func() { close(h.logged) })
	return err
}

// B50.39 — a failed audit webhook export logs the workspace and the webhook's scheme and host, never its path or
// query, which is where a SIEM or chat webhook keeps its secret.
func TestB5039_FailedAuditWebhookLogsNoSecret(t *testing.T) {
	var buf bytes.Buffer
	h := signalHandler{slog.NewTextHandler(&buf, nil), &sync.Mutex{}, make(chan struct{}), &sync.Once{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	body := `{"webhook_url":"https://siem.example.com/hook/SECRETPATH?token=SECRETQ","filter":{"WorkspaceID":"ws-b5039"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audit/webhook", strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "ws-b5039", IsAdmin: true}))
	w := httptest.NewRecorder()
	newAuditWebhookHandler(failingAuditWebhook{}, nil)(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	select {
	case <-h.logged:
	case <-time.After(5 * time.Second):
		t.Fatal("the failed export was never logged")
	}
	h.mu.Lock()
	logs := buf.String()
	h.mu.Unlock()
	if strings.Contains(logs, "SECRETPATH") || strings.Contains(logs, "SECRETQ") {
		t.Fatalf("the log carries the webhook's secret:\n%s", logs)
	}
	for _, want := range []string{"siem.example.com", "ws-b5039"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log does not name %q:\n%s", want, logs)
		}
	}
}
