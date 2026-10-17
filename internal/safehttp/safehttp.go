// Package safehttp hardens outbound HTTP against SSRF: its dialer resolves the target host and refuses
// to connect to loopback, private (RFC1918 / ULA), link-local (169.254/16 incl. the cloud metadata
// endpoint 169.254.169.254, fe80::/10), unspecified, and multicast addresses, re-validating every
// redirect hop. Ported from talyvor-track's SEC-6 fix. Use for every fetch of a caller/config-supplied
// URL — node registration, the audit webhook, node inference/attestation/challenge/benchprobe calls.
//
// NOTE (deployment): this blocks ALL private ranges. If Lens mining nodes legitimately register on a
// private cluster network (10/192.168), that needs a per-deployment allowlist — a documented follow-on.
// The loopback / link-local / metadata blocks are unambiguous and never a legitimate node target.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrBlockedAddress is returned when a fetch targets a non-public (SSRF-sensitive) address.
var ErrBlockedAddress = errors.New("safehttp: refusing to connect to a non-public address")

// ErrInvalidURL is returned for a URL AllowedURL refuses.
var ErrInvalidURL = errors.New("safehttp: URL must be an absolute http(s) URL with a public host and no credentials")

// urlShape: http(s), a host[:port] with no userinfo, and no whitespace or control characters anywhere.
var urlShape = regexp.MustCompile(`^https?://[^/?#@\s\x00-\x1f\x7f]+(?:[/?#][^\s\x00-\x1f\x7f]*)?$`)

// AllowedURL reports whether a caller-supplied URL may be requested: it must be http(s) with a plain
// host, and the host must not be a link-local (incl. cloud metadata), unspecified or multicast IP
// literal. Loopback, private and DNS-named hosts are left to the dial guard, which resolves them and
// honours the operator allowlist — check AllowedURL in addition to a guarded client, never instead.
// A single regexp-led return is the shape CodeQL's go/request-forgery accepts as a URL guard.
func AllowedURL(raw string) bool {
	return urlShape.MatchString(raw) && hostAllowed(raw)
}

func hostAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip == nil || (!ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast())
}

// metadataIP is the cloud metadata endpoint — NEVER allowlistable (see blocked).
var metadataIP = net.ParseIP("169.254.169.254")

// blocked reports whether ip must never be dialed. An operator-supplied CIDR
// allowlist (Phase-0 Item E) may un-block PRIVATE (RFC1918 / ULA) ranges ONLY —
// for the internal node network on a private subnet. It can NEVER un-block
// loopback, link-local (incl. the cloud metadata endpoint 169.254.169.254 and
// fe80::/10), multicast, or unspecified addresses: those are checked FIRST and
// are never a legitimate node target.
//
// SECURITY INVARIANT: cloud metadata stays blocked regardless of the allowlist —
// 169.254.169.254 is LINK-LOCAL, not private, so it never reaches the allowlist
// branch. ParseCIDRAllowlist additionally refuses at config time to accept any
// CIDR containing it (defense in depth + fail-loud).
func blocked(ip net.IP, allow []*net.IPNet) bool {
	// Never-allowlistable ranges, checked first.
	if ip.IsLoopback() || // 127.0.0.0/8, ::1
		ip.IsLinkLocalUnicast() || // 169.254.0.0/16 (incl. metadata 169.254.169.254), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() || // 0.0.0.0, ::
		ip.IsMulticast() {
		return true
	}
	// Private (RFC1918 / ULA) is blocked UNLESS the operator explicitly allowlisted
	// a CIDR that contains it (the internal node network on a private subnet).
	if ip.IsPrivate() { // 10/8, 172.16/12, 192.168/16, fc00::/7 (ULA)
		for _, n := range allow {
			if n.Contains(ip) {
				return false // operator opted this private CIDR in
			}
		}
		return true
	}
	return false // public — the normal case
}

// ParseCIDRAllowlist parses a comma-separated list of CIDRs into networks (Phase-0
// Item E). Empty ⇒ nil (no allowlist; today's all-private-blocked behavior EXACTLY,
// zero change). It REFUSES any CIDR containing the cloud metadata endpoint
// 169.254.169.254 — that range is never allowlistable, fail-loud at boot.
func ParseCIDRAllowlist(csv string) ([]*net.IPNet, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("safehttp: invalid CIDR %q in allowlist: %w", part, err)
		}
		if n.Contains(metadataIP) {
			return nil, fmt.Errorf("safehttp: CIDR %q contains the cloud metadata endpoint 169.254.169.254 — never allowlistable", part)
		}
		out = append(out, n)
	}
	return out, nil
}

// SafeDialContext resolves the host, rejects any resolved address in a blocked range, and dials the
// resolved IP directly (so a DNS name can't rebind to an internal IP between the check and the connect).
// Exported so callers that need their own Transport (e.g. one carrying a node TLS config) can install
// the SSRF guard without losing that config.
func SafeDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return SafeDialContextAllow(base, nil)
}

// SafeDialContextAllow is SafeDialContext with an operator CIDR allowlist (Phase-0
// Item E). A nil/empty allowlist is byte-identical to SafeDialContext (all private
// ranges blocked). Only allowlisted PRIVATE CIDRs are dialable; metadata/link-local
// stay blocked regardless (see blocked).
func SafeDialContextAllow(base *net.Dialer, allow []*net.IPNet) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		base = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("safehttp: no addresses for %q", host)
		}
		for _, ip := range ips {
			if blocked(ip, allow) {
				return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, ip)
			}
		}
		return base.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
}

// CheckRedirect re-validates each redirect hop (the new connection re-dials through SafeDialContext) and
// caps the chain. Install it on any client that follows redirects to a caller-supplied URL.
func CheckRedirect(_ *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("safehttp: stopped after 10 redirects")
	}
	return nil
}

// Client returns an *http.Client with the SSRF dial guard on the initial request and every redirect hop.
func Client(timeout time.Duration) *http.Client { return ClientAllow(timeout, nil) }

// ClientAllow is Client with an operator CIDR allowlist (Phase-0 Item E). nil/empty
// ⇒ byte-identical to Client (today's behavior, zero change).
func ClientAllow(timeout time.Duration, allow []*net.IPNet) *http.Client {
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           SafeDialContextAllow(base, allow),
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: CheckRedirect,
	}
}
