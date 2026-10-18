// Package dashboard serves the single static page Lens shows to a human who
// points a browser at the API host.
//
// ── WHAT THIS REPLACED, AND WHY ─────────────────────────────────────────────
//
// Lens used to ship a 41-panel dashboard here (~2,600 lines of HTML and inline
// JS). It was retired rather than restyled, on evidence:
//
//   - Ten of its thirteen data panels called AUTHENTICATED endpoints — every
//     /v1/api/* route it used, plus every /v1/workspaces/{ws}/* one — with a
//     bare `fetch(path)`. No Authorization header was attached anywhere, and the
//     page offered no way to supply one. Verified against the deployed host:
//     each returned 401 {"error":"API key required"}. Their values rendered a
//     permanent em-dash, which read as "this product is dead" when the truth was
//     "this page never asked".
//   - So its justification — that a self-hostable product should not require a
//     second service to see anything — did not hold: it could not show a visitor
//     their own data at all.
//   - The obvious repair, a key box, was refused deliberately. It would put a
//     long-lived Lens API key in browser localStorage ON THE API HOSTNAME, which
//     is exactly the exposure the suite's BFF exists to prevent by holding the
//     key server-side behind a session. Trading that property for the
//     convenience of not running the app is a bad trade, and worst on the host
//     where the key is most useful to an attacker.
//
// ── THE RULE THIS PAGE OBEYS ────────────────────────────────────────────────
//
// It shows only what it can prove without a credential. In practice that is
// /healthz — already unauthenticated, already real — and nothing else. Economy
// aggregates (/v1/economy/stats, /v1/oracle/stats, /v1/tokens/rates) need a
// credential since B39.3; before that they were considered and left out: they are global rather than the visitor's, they
// exist only when the economy is enabled, and on a young install "circulating
// supply 2,000 µLENS · 0 oracle tasks" recreates the very impression this page
// corrects — accurately.
//
// It also carries NO money constant. The retired dashboard hardcoded the LXC peg
// twice and the staking rates three times, duplicating economy.LXCUSDValue and
// the marketplace APY constants with nothing linking copy to source — correct on
// the day written, silently wrong later. status_test.go fails if a currency
// amount or percentage reappears in the rendered text.
package dashboard

import (
	"html"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/brand"
)

// Handler serves the status page. The HTML is rendered once at construction with
// the version baked in, so each request is a memcpy: no template execution, no
// database read, no credential, nothing to fail.
type Handler struct {
	version string
	html    []byte
}

// New renders the status page for the given build version.
func New(version string) *Handler {
	// The version is the only interpolation and it comes from the build, but
	// escape it anyway — a page whose single dynamic value is unescaped is one
	// bad ldflag away from an injection.
	rendered := strings.NewReplacer("{{BRAND_CSS}}", brand.TokensCSS, "{{MARK}}", brand.Mark).Replace(statusHTML)
	rendered = strings.ReplaceAll(rendered, "{{VERSION}}", html.EscapeString(version))
	return &Handler{version: version, html: []byte(rendered)}
}

// ServeHTTP serves the status page. Registered at "/" unconditionally, and at
// "/dashboard" only when LENS_DASHBOARD_ENABLED is set (see cmd/lens/dashboard_routes.go).
func (h *Handler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Static bytes for a given build; let a proxy hold it briefly, but not so
	// long that a redeploy's version stays wrong on a refresh.
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.html)
}
