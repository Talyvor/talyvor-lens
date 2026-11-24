package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Client-IP resolution for THIS deployment: lens sits behind a containerised
// Caddy reverse proxy on the same Docker bridge (deploy/caddy/Caddyfile is a
// bare `reverse_proxy lens:8080`, so Caddy runs its defaults). Public traffic
// reaches lens only through Caddy — compose publishes caddy :80/:443, while
// lens publishes 127.0.0.1:8080 only, for SSH-tunnel admin.
//
// Verified empirically against caddy:2-alpine (v2.11.4), the image compose
// runs: with `trusted_proxies` unset, Caddy REPLACES X-Forwarded-For with the
// true TCP peer, discarding any client-supplied chain — but passes
// True-Client-IP and X-Real-IP through untouched. So XFF is the only header
// Caddy vouches for, and the RIGHTMOST entry is always the one it wrote.
//
// That makes True-Client-IP / X-Real-IP the live spoof vector, because chi's
// deprecated RealIP consulted them BEFORE XFF — bypassing Caddy's sanitising
// entirely. These tests pin that they are now inert, that a forged XFF cannot
// win even if a future `trusted_proxies` config makes Caddy append rather
// than replace, and that honest resolution still works.

// caddyHop drives a request through the production client-IP middleware
// exactly as run() wires it, simulating the Caddy hop: RemoteAddr is Caddy's
// container address on the Docker bridge, and headers are what lens receives
// after Caddy has forwarded. Returns the IP Lens resolves via clientIP.
func caddyHop(t *testing.T, headers http.Header) (resolved, remoteAddr string) {
	t.Helper()

	const caddyBridgeAddr = "172.18.0.3:41234"

	r := chi.NewRouter()
	r.Use(clientIPMiddleware())
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		resolved = clientIP(req)
		remoteAddr = req.RemoteAddr
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = caddyBridgeAddr
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
	return resolved, remoteAddr
}

// Honest client through Caddy: Caddy appends the real peer, so XFF holds
// exactly one entry. This is the "does IP resolution still work at all" guard
// — the failure mode where a wrong choice (e.g. ClientIPFromRemoteAddr)
// collapses every client onto Caddy's bridge address.
func TestClientIP_ResolvesThroughCaddyHop(t *testing.T) {
	got, _ := caddyHop(t, http.Header{"X-Forwarded-For": {"203.0.113.7"}})
	if got != "203.0.113.7" {
		t.Errorf("client IP through Caddy hop = %q, want %q", got, "203.0.113.7")
	}
}

// Defence in depth for the classic leftmost-XFF spoof: a forged entry sits to
// the LEFT of the proxy-written one, which is what RealIP returned. Today's
// Caddy strips the client chain before lens sees it, so this shape does not
// arise in production — it becomes reachable the moment `trusted_proxies` is
// configured (which switches Caddy to appending) or another hop is added.
// Pinned so that change cannot silently reopen the hole.
func TestClientIP_ForgedXFFCannotSetIt(t *testing.T) {
	got, _ := caddyHop(t, http.Header{"X-Forwarded-For": {"9.9.9.9, 203.0.113.7"}})
	if got == "9.9.9.9" {
		t.Errorf("client IP is attacker-controlled: forged XFF value %q was accepted", got)
	}
	if got != "203.0.113.7" {
		t.Errorf("client IP = %q, want the Caddy-appended peer %q", got, "203.0.113.7")
	}
}

// Same defence in depth via a SEPARATE header line rather than a comma list —
// the variant that lets an attacker pick which value naive parsers read. Per
// RFC 2616 these merge in order received, so the proxy-written value is still
// rightmost and must still win.
func TestClientIP_ForgedDuplicateXFFHeaderCannotSetIt(t *testing.T) {
	got, _ := caddyHop(t, http.Header{"X-Forwarded-For": {"9.9.9.9", "203.0.113.7"}})
	if got != "203.0.113.7" {
		t.Errorf("client IP = %q, want the Caddy-appended peer %q", got, "203.0.113.7")
	}
}

// THE LIVE VECTOR. RealIP preferred True-Client-IP, then X-Real-IP, then XFF.
// Caddy passes the first two through untouched, so a client could set
// True-Client-IP and have it adopted verbatim — Caddy's XFF sanitising never
// got a look in. Verified against caddy:2-alpine v2.11.4. They must now be
// inert even when a well-formed XFF is also present.
func TestClientIP_ForgedTrueClientIPAndXRealIPIgnored(t *testing.T) {
	got, _ := caddyHop(t, http.Header{
		"True-Client-IP":  {"6.6.6.6"},
		"X-Real-Ip":       {"7.7.7.7"},
		"X-Forwarded-For": {"9.9.9.9, 203.0.113.7"},
	})
	for _, forged := range []string{"6.6.6.6", "7.7.7.7", "9.9.9.9"} {
		if got == forged {
			t.Errorf("client IP is attacker-controlled: forged value %q was accepted", got)
		}
	}
	if got != "203.0.113.7" {
		t.Errorf("client IP = %q, want the Caddy-appended peer %q", got, "203.0.113.7")
	}
}

// The core of GO-2026-5774/5775/5777: RealIP MUTATED r.RemoteAddr to the
// header value, so every downstream reader of RemoteAddr — loggers, audit
// trails, future IP heuristics — silently inherited attacker-controlled data.
// RemoteAddr must remain the true TCP peer (Caddy on the bridge).
func TestClientIP_RemoteAddrNotMutated(t *testing.T) {
	_, remote := caddyHop(t, http.Header{
		"True-Client-IP":  {"6.6.6.6"},
		"X-Forwarded-For": {"9.9.9.9, 203.0.113.7"},
	})
	if remote != "172.18.0.3:41234" {
		t.Errorf("r.RemoteAddr = %q, want the untouched TCP peer %q — header data must never reach RemoteAddr",
			remote, "172.18.0.3:41234")
	}
}

// Loopback admin path (compose publishes 127.0.0.1:8080 for SSH-tunnel
// access): the request never traversed Caddy, so there is no XFF and no
// trustworthy client IP. Resolve to "" rather than inventing one — and in
// particular never fall back to a client-supplied header.
func TestClientIP_NoXFFFailsClosed(t *testing.T) {
	got, _ := caddyHop(t, http.Header{})
	if got != "" {
		t.Errorf("client IP without a Caddy hop = %q, want \"\" (fail closed)", got)
	}

	forged, _ := caddyHop(t, http.Header{"True-Client-IP": {"6.6.6.6"}, "X-Real-Ip": {"7.7.7.7"}})
	if forged != "" {
		t.Errorf("client IP without a Caddy hop = %q, want \"\" — client headers must not be a fallback", forged)
	}
}

// B67.2 (GHSA for chi's ClientIPFromXFF, fixed in v5.4.0): a caller that reaches lens without passing through Caddy —
// any peer outside the private and loopback ranges our proxy sits in — is its own TCP peer. Whatever X-Forwarded-For it
// sends, including a chain that looks like it came through a trusted hop, cannot choose the IP lens sees.
func TestClientIP_DirectCallerIsItsTCPPeer(t *testing.T) {
	for _, xff := range []string{"10.0.0.7", "10.0.0.7, 172.18.0.3", "9.9.9.9", "9.9.9.9, 198.51.100.42"} {
		r := chi.NewRouter()
		r.Use(clientIPMiddleware())
		var got string
		r.Get("/", func(w http.ResponseWriter, req *http.Request) { got = clientIP(req) })
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "198.51.100.42:5000"
		req.Header.Set("X-Forwarded-For", xff)
		r.ServeHTTP(httptest.NewRecorder(), req)
		if got != "198.51.100.42" {
			t.Errorf("direct caller forging X-Forwarded-For %q: client IP = %q, want its TCP peer %q", xff, got, "198.51.100.42")
		}
	}
}
