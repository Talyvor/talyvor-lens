package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// B43.5 (CodeQL go/reflected-xss #42): a recorded answer that echoes markup is never replayed as a page.
func TestWriteRunCodeAnswerNeverReplaysHTML(t *testing.T) {
	for _, tc := range []struct{ name, setType, want string }{
		{"sniffed", "", "text/plain; charset=utf-8"},
		{"html", "text/html; charset=utf-8", "text/plain; charset=utf-8"},
		{"json", "application/json", "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if tc.setType != "" {
				rec.Header().Set("Content-Type", tc.setType)
			}
			rec.WriteHeader(http.StatusBadRequest)
			_, _ = rec.Write([]byte("<html><script>alert(1)</script></html>"))

			out := httptest.NewRecorder()
			writeRunCodeAnswer(out, rec, false)
			if got := out.Header().Get("Content-Type"); got != tc.want {
				t.Fatalf("Content-Type = %q, want %q", got, tc.want)
			}
			if got := out.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if out.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", out.Code)
			}
		})
	}
}
