package main

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// clientIPMiddleware resolves the calling client's IP address for this
// deployment's topology. Wired in run() as the single client-IP authority;
// read it back with clientIP.
//
// Replaces chi's middleware.RealIP (GO-2026-5774 / GO-2026-5775 /
// GO-2026-5777), which set r.RemoteAddr from the LEFTMOST X-Forwarded-For
// value — the one the client supplies — or from True-Client-IP / X-Real-IP
// whether or not our infrastructure sets them. Upgrading chi does NOT fix
// this. RealIP's body is byte-for-byte unchanged at v5.3.1 — v5.3.0 "fixed"
// the advisory by adding this ClientIPFrom* family and deprecating RealIP, so
// a bump alone silences govulncheck while leaving the spoof fully open. The
// call site had to change; see TestNoRealIPAnywhere, which is now the only
// guard against reintroduction.
//
// WHY ClientIPFromXFF WITH THE PRIVATE AND LOOPBACK RANGES — this is
// topology-specific and there is no safe default. Behaviour below was verified
// empirically against caddy:2-alpine (v2.11.4), the image
// deploy/docker-compose.yaml runs, with our own bare `reverse_proxy` Caddyfile
// — not taken from documentation:
//
//   - lens sits behind a containerised Caddy on the same Docker bridge.
//     deploy/caddy/Caddyfile is a bare `reverse_proxy lens:8080`, so Caddy
//     runs its defaults, which means `trusted_proxies` is unset — it trusts
//     no inbound hop.
//   - Consequently Caddy REPLACES X-Forwarded-For outright with the true TCP
//     peer. A client-supplied chain, comma-list or duplicate header alike, is
//     discarded before lens ever sees it. XFF therefore arrives with exactly
//     one entry, and it is trustworthy.
//   - Caddy does NOT set, strip or normalise X-Real-IP or True-Client-IP.
//     Those pass through from the client verbatim, so their presence is by
//     definition a forgery attempt and they must never be read. This — not
//     the leftmost-XFF issue — was the live spoof vector here: RealIP checked
//     True-Client-IP FIRST, ahead of XFF, so Caddy's XFF sanitising was
//     bypassed entirely.
//   - Caddy terminates TLS itself via ACME, so it is the internet-facing
//     front door with no CDN in front of it. Exactly one trusted hop.
//   - Caddy's address on the compose bridge is assigned by Docker, so it is
//     named by range, not by address: the private and loopback ranges, the
//     same peers ratelimit's anonymousIP believes a forwarded IP from.
//   - chi >= v5.4.0 (B67.2, chi's "Client IP spoofing in ClientIPFromXFF"
//     advisory) reads XFF only when the TCP peer is in one of those ranges.
//     A caller that reaches lens any other way is its own TCP peer, whatever
//     XFF it sends. From a trusted peer it walks XFF right to left past
//     private entries and returns the first public one: the one Caddy wrote.
//     That holds whether Caddy replaces (today) or appends (what it would do
//     if `trusted_proxies` were ever configured). An entry that does not
//     parse resolves to no IP (fail closed).
//   - A caller whose own address is private and comes through Caddy resolves
//     to "": Caddy replaced XFF with that private address, it is skipped as a
//     hop, and nothing is left to its left. Nothing a client wrote is reached.
//
// Rejected alternatives:
//
//   - ClientIPFromRemoteAddr — would resolve every request to Caddy's bridge
//     address (e.g. 172.18.0.3), collapsing all clients onto one value.
//   - ClientIPFromHeader("X-Real-IP") — Caddy does not set it; without a
//     matching Caddyfile `header_up`, every request would silently resolve to
//     "". It would also couple this call site to a config change in another
//     deploy artifact, which can be missed.
//   - ClientIPFromXFFTrustedProxies(1) — reads only XFF and never the TCP
//     peer, so a caller that reaches lens directly chooses its IP; and it
//     silently starts trusting a forged value if a proxy level is ever added.
//   - ClientIPFromXFF() with no ranges — panics at startup from chi v5.4.0.
//
// If a second hop is ever put in front of Caddy (a CDN, another LB), the
// rightmost public entry becomes THAT hop's address and this must be
// revisited — add the new front door's CIDRs to trustedProxyRanges.
func clientIPMiddleware() func(http.Handler) http.Handler {
	return middleware.ClientIPFromXFF(trustedProxyRanges...)
}

// trustedProxyRanges are the peers lens believes X-Forwarded-For from: net.IP's IsPrivate and IsLoopback ranges.
var trustedProxyRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7", "127.0.0.0/8", "::1/128"}

// clientIP returns the client IP that Lens resolved for this request, or ""
// if none could be established (no Caddy hop, or an unparseable value —
// both fail closed).
//
// This is the ONLY sanctioned way to obtain a caller's network origin. Do not
// read r.RemoteAddr for this: behind Caddy it is the proxy's bridge address,
// not the client's. Nothing in Lens keys off a client IP today — rate limits
// key off (workspace, key ID) and the anti-Sybil ring detector keys off card
// fingerprints and owner keys, all server-derived — so this reader exists to
// make the *next* IP consumer safe by construction.
func clientIP(r *http.Request) string {
	return middleware.GetClientIP(r.Context())
}
