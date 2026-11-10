package main

// write_json_err_test.go — insecure-default #3: verbose internal errors leaking
// to clients. writeJSONErr is the one error-writing helper (~77 call sites pass
// err.Error() with a 500), so redaction is centralised here: a 5xx logs its full
// detail server-side and returns a GENERIC body; 4xx client messages pass
// through. Centralising means no 500 call site can forget to redact.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
)

func TestWriteJSONErr_5xxRedacted(t *testing.T) {
	// A realistic leak: a raw pgx/driver error carrying an internal host + user.
	const secret = `pq: password authentication failed for user "lens" at 10.0.0.5:5432`
	rec := httptest.NewRecorder()
	writeJSONErr(rec, http.StatusInternalServerError, secret)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(body["error"], "password") || strings.Contains(body["error"], "10.0.0.5") {
		t.Fatalf("5xx response leaked the internal error detail to the client: %q", body["error"])
	}
	if body["error"] != "internal server error" {
		t.Errorf("5xx body = %q, want generic \"internal server error\"", body["error"])
	}
}

func TestWriteJSONErr_4xxPassthrough(t *testing.T) {
	// 4xx messages are legitimately client-facing and must NOT be redacted.
	const clientMsg = "workspace_id is required"
	rec := httptest.NewRecorder()
	writeJSONErr(rec, http.StatusBadRequest, clientMsg)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != clientMsg {
		t.Errorf("4xx body = %q, want passthrough %q", body["error"], clientMsg)
	}
}

// B55.51: a refusal a person reads starts with no package name. Lens's own error text is the input,
// so the web and the phone no longer strip "economy: " one screen at a time.
func TestWriteJSONErr_4xxDropsThePackageName(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONErr(rec, http.StatusNotFound, fmt.Errorf("economy: payment: %w", economy.ErrAgentNotFound).Error())

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := "No such agent in this workspace"; body["error"] != want {
		t.Errorf("4xx body = %q, want %q", body["error"], want)
	}
}
