package inference

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/retry"
)

// B50.3 — Google's key rides in the URL's query; when the provider cannot be reached, http.Client puts the whole URL
// into the error, and the proxy hands that error to its caller. RunUpstream's error never carries the key.
func TestRunUpstreamErrorNeverCarriesTheKeyInTheURL(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens: the call fails with a *url.Error
	const key = "AIzaB503notarealkey0000000000000000000"
	_, _, _, err = RunUpstream(context.Background(), &http.Client{}, retry.Config{MaxAttempts: 1},
		"http://"+addr+"/v1beta/models/gemini:generateContent?key="+key, func(*http.Request) {}, []byte(`{}`), nil)
	if err == nil {
		t.Fatal("want an error from an address nothing listens on")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the upstream error carries the key: %v", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("the error should still say where it failed: %v", err)
	}
}
