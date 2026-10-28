package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/safehttp"
	"github.com/talyvor/lens/internal/webpush"
)

// B50.2: every outbound call a caller can steer refuses loopback, private, link-local and the cloud metadata
// address — the agent key directory (the origin is the calling agent's), a node's attestation (the URL is the
// node's), the audit webhook (the URL is the caller's) and a push subscription (the endpoint is the browser's).
// Partner base URLs come only from the operator's environment, and Lens fetches no link previews.
func TestOutboundCallsRefuseNonPublicAddresses(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer internal.Close()
	const metadata = "http://169.254.169.254"
	ctx := context.Background()

	for _, origin := range []string{internal.URL, metadata} {
		if _, err := newAgentKeyDirectory().Key(ctx, origin, "kid"); !errors.Is(err, safehttp.ErrBlockedAddress) {
			t.Errorf("agent key directory at %s: err %v, want ErrBlockedAddress", origin, err)
		}
		_, priv, _ := ed25519.GenerateKey(nil)
		if _, err := newAttestationClient(&config.Config{}, priv).Fetch(ctx, origin, 1); !errors.Is(err, safehttp.ErrBlockedAddress) {
			t.Errorf("node attestation at %s: err %v, want ErrBlockedAddress", origin, err)
		}
	}
	if err := audit.New(nil).ExportWebhook(ctx, metadata+"/latest/meta-data", audit.ExportFilter{}); !errors.Is(err, safehttp.ErrInvalidURL) {
		t.Errorf("audit webhook to the metadata address: err %v, want ErrInvalidURL", err)
	}
	for _, endpoint := range []string{internal.URL, "https://169.254.169.254/push", "https://127.0.0.1/push"} {
		if u, _ := url.Parse(endpoint); webpush.PushServiceEndpoint(u) {
			t.Errorf("push subscription endpoint %s accepted", endpoint)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the loopback server was reached %d times", n)
	}
}
