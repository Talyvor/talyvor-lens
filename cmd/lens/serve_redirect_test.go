package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A forged Host header must not choose where port 80 redirects to.
func TestHTTPSRedirectIgnoresHostHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/v1/x?y=1", nil)
	req.Host = "evil.example:80"
	rec := httptest.NewRecorder()
	httpsRedirectHandler("lens.talyvor.com").ServeHTTP(rec, req)
	if got := rec.Header().Get("Location"); got != "https://lens.talyvor.com/v1/x?y=1" {
		t.Fatalf("Location = %q, want the configured domain", got)
	}
}
