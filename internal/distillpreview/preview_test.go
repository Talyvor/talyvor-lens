package distillpreview_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/distillpreview"
)

// fakeConverter is the ONLY collaborator the handler is allowed to convert
// through. It records calls so tests can prove (a) it's the isolated path and
// (b) it's not invoked when auth/format gates fail (side-effect-freeness).
type fakeConverter struct {
	calls    int
	gotBytes []byte
	gotFmt   distill.Format
	res      distill.Result
	err      error
}

func (f *fakeConverter) Convert(_ context.Context, in []byte, format distill.Format) (distill.Result, error) {
	f.calls++
	f.gotBytes = append([]byte(nil), in...)
	f.gotFmt = format
	return f.res, f.err
}

func adminYes(*http.Request) bool { return true }
func adminNo(*http.Request) bool  { return false }

func post(t *testing.T, h http.Handler, contentType string, body []byte, query string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	target := "/v1/admin/distill/preview"
	if query != "" {
		target += "?" + query
	}
	r := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w, got
}

// Happy path: an HTML doc is converted THROUGH the injected (isolated) converter
// and returned as a dry-run. The converter sees the raw body + derived format.
func TestPreview_ConvertsViaIsolatedConverter(t *testing.T) {
	fc := &fakeConverter{res: distill.Result{Markdown: "# Hi\n\nthere", Format: distill.FormatHTML}}
	h := &distillpreview.Handler{Converter: fc, IsAdmin: adminYes}

	body := []byte("<html><body><h1>Hi</h1><p>there</p></body></html>")
	w, got := post(t, h, "text/html; charset=utf-8", body, "")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fc.calls != 1 {
		t.Fatalf("converter must be called exactly once; calls=%d", fc.calls)
	}
	if fc.gotFmt != distill.FormatHTML {
		t.Errorf("converter got format %q, want html (derived from Content-Type)", fc.gotFmt)
	}
	if !bytes.Equal(fc.gotBytes, body) {
		t.Error("converter must receive the raw request body")
	}
	if got["markdown"] != "# Hi\n\nthere" || got["format"] != "html" {
		t.Errorf("response shape wrong: %v", got)
	}
	if got["needs_vision"] != false || got["tier"] != "faithful" {
		t.Errorf("defaults wrong: needs_vision=%v tier=%v", got["needs_vision"], got["tier"])
	}
	if _, ok := got["savings"].(map[string]any); !ok {
		t.Errorf("response must carry a savings object; got %v", got["savings"])
	}
}

// A text-less / scanned input (NeedsVision) is reported HONESTLY: needs_vision
// true, NO fabricated markdown, zero savings, no error. Preview never OCRs.
func TestPreview_NeedsVisionHonest(t *testing.T) {
	fc := &fakeConverter{res: distill.Result{NeedsVision: true, Markdown: "", Format: distill.FormatPDF}}
	h := &distillpreview.Handler{Converter: fc, IsAdmin: adminYes}

	w, got := post(t, h, "application/pdf", []byte("%PDF-1.4 scanned no text layer"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	if got["needs_vision"] != true {
		t.Errorf("text-less PDF must report needs_vision=true; got %v", got["needs_vision"])
	}
	if got["markdown"] != "" {
		t.Errorf("must NOT fabricate text for a NeedsVision doc; got %q", got["markdown"])
	}
	sav := got["savings"].(map[string]any)
	if sav["tokens_saved"].(float64) != 0 {
		t.Errorf("NeedsVision must report 0 tokens saved; got %v", sav["tokens_saved"])
	}
}

// Non-admin is rejected 403 and the document is NEVER converted (no work, no
// resource spend on an unauthorized caller).
func TestPreview_AdminRequired(t *testing.T) {
	fc := &fakeConverter{res: distill.Result{Markdown: "should not run"}}
	h := &distillpreview.Handler{Converter: fc, IsAdmin: adminNo}

	w, got := post(t, h, "text/html", []byte("<h1>x</h1>"), "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin must get 403; got %d", w.Code)
	}
	if got["error"] != "admin credentials required" {
		t.Errorf("error message = %q, want the admin message", got["error"])
	}
	if fc.calls != 0 {
		t.Errorf("a rejected request must NOT convert; calls=%d", fc.calls)
	}
}

// A nil authorizer is fail-closed (treated as not-admin) — never accidentally open.
func TestPreview_NilAuthorizerFailsClosed(t *testing.T) {
	fc := &fakeConverter{}
	h := &distillpreview.Handler{Converter: fc, IsAdmin: nil}
	w, _ := post(t, h, "text/html", []byte("<h1>x</h1>"), "")
	if w.Code != http.StatusForbidden {
		t.Errorf("nil authorizer must fail closed (403); got %d", w.Code)
	}
	if fc.calls != 0 {
		t.Errorf("must not convert when auth is unset; calls=%d", fc.calls)
	}
}

// The tier is applied PARENT-SIDE on the (faithful) converter output: outline
// drops body, keeping only headings. Proves ApplyTier runs after Convert.
func TestPreview_TierAppliedParentSide(t *testing.T) {
	faithful := "# Title\n\nintro body to drop\n\n## Section\n\nmore body to drop"
	fc := &fakeConverter{res: distill.Result{Markdown: faithful, Format: distill.FormatHTML}}
	h := &distillpreview.Handler{Converter: fc, IsAdmin: adminYes}

	w, got := post(t, h, "text/html", []byte("<h1>Title</h1>"), "tier=outline")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got["tier"] != "outline" {
		t.Errorf("tier not recorded as outline; got %v", got["tier"])
	}
	md := got["markdown"].(string)
	if !strings.Contains(md, "# Title") || strings.Contains(md, "intro body to drop") {
		t.Errorf("outline must keep headings + drop body; got %q", md)
	}
}

// An unknown / missing Content-Type is a 400 and never converts (we don't sniff
// untrusted bytes in-process; the caller declares the media type).
func TestPreview_UnknownContentType(t *testing.T) {
	for _, ct := range []string{"application/zip", ""} {
		fc := &fakeConverter{}
		h := &distillpreview.Handler{Converter: fc, IsAdmin: adminYes}
		w, _ := post(t, h, ct, []byte("data"), "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("content-type %q must be 400; got %d", ct, w.Code)
		}
		if fc.calls != 0 {
			t.Errorf("content-type %q must not convert; calls=%d", ct, fc.calls)
		}
	}
}

// Oversized bodies are rejected by the cap (resource bound) before conversion.
func TestPreview_BodyTooLarge(t *testing.T) {
	fc := &fakeConverter{}
	h := &distillpreview.Handler{Converter: fc, IsAdmin: adminYes, MaxBytes: 8}
	w, _ := post(t, h, "text/html", bytes.Repeat([]byte("A"), 100), "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body must be 413; got %d", w.Code)
	}
	if fc.calls != 0 {
		t.Errorf("oversized body must not convert; calls=%d", fc.calls)
	}
}

func TestPreview_EmptyBody(t *testing.T) {
	fc := &fakeConverter{}
	h := &distillpreview.Handler{Converter: fc, IsAdmin: adminYes}
	w, _ := post(t, h, "text/html", nil, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body must be 400; got %d", w.Code)
	}
	if fc.calls != 0 {
		t.Errorf("empty body must not convert; calls=%d", fc.calls)
	}
}

// End-to-end through the REAL ProcessIsolator subprocess (not a fake): proves
// the preview path actually converts an uploaded document via the killable
// worker. On linux the worker runs under the prod RLIMIT_AS (proven sufficient
// in PR #0.5).
func TestPreview_RealIsolatorEndToEnd(t *testing.T) {
	workerBin := buildRealWorker(t)
	iso := &distill.ProcessIsolator{WorkerBin: workerBin}
	h := &distillpreview.Handler{Converter: iso, IsAdmin: adminYes}

	w, got := post(t, h, "text/html", []byte("<html><body><h1>Hi</h1><p>there</p></body></html>"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("real-isolator preview status=%d; body=%s", w.Code, w.Body.String())
	}
	if md, _ := got["markdown"].(string); !strings.Contains(md, "# Hi") {
		t.Errorf("real isolator should convert HTML→Markdown; got %q", md)
	}
}

// B39.10: a real scanned PDF through the real preview path warns that a preview
// runs no vision model — never that the vision converter "is a later PR".
func TestPreview_ScannedPDFWarningSaysNoVisionRuns(t *testing.T) {
	scanned, err := os.ReadFile("../distill/testdata/vision/memo.pdf") // image-only, no text layer
	if err != nil {
		t.Fatal(err)
	}
	h := &distillpreview.Handler{Converter: &distill.ProcessIsolator{WorkerBin: buildRealWorker(t)}, IsAdmin: adminYes}

	w, got := post(t, h, "application/pdf", scanned, "")
	if w.Code != http.StatusOK || got["needs_vision"] != true {
		t.Fatalf("status=%d needs_vision=%v; body=%s", w.Code, got["needs_vision"], w.Body.String())
	}
	warn, _ := json.Marshal(got["warnings"])
	if strings.Contains(string(warn), "later PR") {
		t.Errorf("preview warning must not call vision OCR a later PR; got %s", warn)
	}
	if !strings.Contains(string(warn), "a preview runs no vision model") {
		t.Errorf("preview warning must say no vision model ran; got %s", warn)
	}
}

func buildRealWorker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "distill-worker")
	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/talyvor/lens/cmd/distill-worker").CombinedOutput()
	if err != nil {
		t.Fatalf("build distill-worker: %v\n%s", err, out)
	}
	return bin
}
