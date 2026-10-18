package dashboard

// The status page must be TRUE, then useful, then handsome — in that order.
//
// It replaced a 41-panel dashboard whose ten authenticated panels rendered a
// permanent em-dash because the page never sent a credential. The lesson is not
// "show fewer panels"; it is "show only what you can prove". So the assertions
// that matter most here are the NEGATIVE ones: no credentialed value, no
// aggregate that answers a question the visitor did not ask, and — the trap that
// produced this whole cleanup — no money constant copied out of Go with nothing
// to keep the copy honest.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var styleScriptRE = regexp.MustCompile(`(?is)<(style|script)\b.*?</(style|script)>`)

// stripStyleAndScript leaves only what a reader actually sees.
func stripStyleAndScript(s string) string { return styleScriptRE.ReplaceAllString(s, "") }

func render(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	New("0.1.0").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status page: got %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

/* ── What it must say ─────────────────────────────────────────────────────── */

func TestPageNamesTheServiceAndItsVersion(t *testing.T) {
	body := render(t)
	if !strings.Contains(body, "Talyvor Lens") {
		t.Error("the page must name the service")
	}
	if !strings.Contains(body, "0.1.0") {
		t.Error("the page must carry the version it was built with")
	}
}

// TestPageSaysItIsAnAPINotAUI: the whole reason a visitor is confused here is
// that they typed an API hostname expecting an app. Say so, first.
func TestPageSaysItIsAnAPINotAUI(t *testing.T) {
	body := strings.ToLower(render(t))
	if !strings.Contains(body, "api") {
		t.Error("the page must say this host is an API")
	}
	if !strings.Contains(body, "not a") && !strings.Contains(body, "isn’t") && !strings.Contains(body, "rather than") {
		t.Error("the page must distinguish itself from the app, not merely mention the word API")
	}
}

// TestPagePointsAtTheDetailedStatusPage: /status is a SEPARATE, pre-existing and
// fully working public page (internal/status) — per-component health, provider
// latencies, all unauthenticated and all real. A page whose job is to point
// onward must not omit the most useful destination on its own host.
func TestPagePointsAtTheDetailedStatusPage(t *testing.T) {
	if !strings.Contains(render(t), "/status") {
		t.Error("the page must link to /status, the detailed public health page on this same host")
	}
}

// B28.12: the page says what Lens is for — Agent Wallets — and points at the API reference that lists them.
func TestPageLeadsWithAgentWallets(t *testing.T) {
	body := stripStyleAndScript(render(t))
	if !strings.Contains(body, "Agent Wallets and the gateway that enforces them") {
		t.Error("the page must describe Lens as Agent Wallets and the gateway that enforces them")
	}
	if !strings.Contains(body, `href="/openapi.json"`) {
		t.Error("the page must link to /openapi.json, where the wallet routes are listed")
	}
}

func TestPagePointsAtTheDashboardAndTheDocs(t *testing.T) {
	body := render(t)
	if !strings.Contains(body, "app.talyvor.com") {
		t.Error("the page must point at where the dashboard actually is")
	}
	if !strings.Contains(body, "docs.talyvor.com") {
		t.Error("the page must point at the docs")
	}
}

// TestPageReadsHealthFromTheUnauthenticatedEndpoint: /healthz already answers
// without a credential, so health is the ONE live reading this page can honestly
// show. It must actually fetch it rather than paint a decorative green dot.
func TestPageReadsHealthFromHealthz(t *testing.T) {
	body := render(t)
	if !strings.Contains(body, "/healthz") {
		t.Error("health must be read from /healthz — the endpoint that answers unauthenticated")
	}
	if !strings.Contains(body, "fetch(") {
		t.Error("health must be fetched live, not baked into the HTML")
	}
}

/* ── What it must NOT say ─────────────────────────────────────────────────── */

// TestPageShowsNoCredentialedData is the core regression guard. Every path below
// requires an API key; the page holds none, so requesting any of them could only
// ever render a 401 as an em-dash — the exact failure that emptied the old
// dashboard.
func TestPageShowsNoCredentialedData(t *testing.T) {
	body := render(t)
	credentialed := []string{
		"/v1/api/", "/v1/workspaces/", "/v1/api/catalog", "/v1/api/workspaces",
		"/v1/api/distill/summary", "/v1/api/routing/intelligence", "/v1/api/local/status",
		"/v1/api/anomalies/scan", "/v1/api/alerts/circuits", "/v1/api/povi/",
		"/v1/api/modality/capabilities",
	}
	for _, p := range credentialed {
		if strings.Contains(body, p) {
			t.Errorf("the page references %s, which requires an API key it does not have — "+
				"it could only ever render a 401", p)
		}
	}
}

// TestPageShowsNoEconomyAggregates: /v1/economy/stats, /v1/oracle/stats and
// /v1/tokens/rates need a credential since B39.3, so the page could only render
// a 401. Before that they were public and were excluded on editorial grounds:
// they are global rather than the visitor's, they
// exist only when the economy is enabled (so the page would differ per
// deployment), and on a young install "circulating supply 2,000 µLENS · 0 oracle
// tasks" reproduces the exact "this product is dead" impression this page was
// built to correct — with accurate data.
func TestPageShowsNoEconomyAggregates(t *testing.T) {
	body := render(t)
	for _, p := range []string{"/v1/economy/", "/v1/oracle/stats", "/v1/tokens/rates", "/v1/marketplace/"} {
		if strings.Contains(body, p) {
			t.Errorf("the page reads %s, which requires an API key it does not have", p)
		}
	}
	for _, w := range []string{"circulating", "total supply", "Total Supply"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(w)) {
			t.Errorf("the page renders %q", w)
		}
	}
}

// TestPageHardcodesNoMoneyConstants is the guard for the defect that survived the
// old dashboard: the LXC peg was hardcoded as "$0.10" twice and the staking APYs
// as 5/12/20% three times, duplicating economy.LXCUSDValue and marketplace.APY*
// with NOTHING linking the copies to their source. All five were correct on the
// day they were written — that is what makes the shape dangerous. This page
// carries no money constant at all, and this test fails if one appears.
func TestPageHardcodesNoMoneyConstants(t *testing.T) {
	// Check what a READER sees: CSS legitimately contains "100%" and script
	// legitimately contains numbers, and neither is a money constant. Stripping
	// them is what makes this assertion mean what it says.
	body := stripStyleAndScript(render(t))
	// Any currency amount, any percentage — on a page that has no business
	// quoting either.
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\$\s?\d`),
		regexp.MustCompile(`\d+\s?%`),
		regexp.MustCompile(`(?i)\bAPY\b`),
		regexp.MustCompile(`(?i)\bpeg\b`),
		regexp.MustCompile(`(?i)\bLXC\b`),
		regexp.MustCompile(`(?i)\bLENS\b(?i)\s*(token|per|=)`),
	} {
		if m := re.FindString(body); m != "" {
			t.Errorf("the page contains %q (matched %s) — a money constant copied from Go with "+
				"nothing to keep the copy honest is how the old dashboard went stale", m, re)
		}
	}
}

/* ── How it must look ─────────────────────────────────────────────────────── */

// TestPageUsesTheBrandTokens pins the page to brand v4 (the 4 Oct 2026 board,
// brand-v4/tokens/tokens.css), not to an approximation: the palette in both
// themes, the type stack, and the flat mark beside the name. Drift here means the
// API host stops looking like the product.
func TestPageUsesTheBrandTokens(t *testing.T) {
	body := render(t)
	brandHex := map[string]string{
		"canvas dark":   "#060A12",
		"surface dark":  "#081220",
		"ink dark":      "#E6EEF7",
		"muted dark":    "#7E93AB",
		"accent dark":   "#3AD6C0",
		"canvas light":  "#F4F7FB",
		"ink light":     "#060A12",
		"muted light":   "#46586E",
		"accent light":  "#0F7A6C",
		"positive dark": "#45C77F",
	}
	for name, hex := range brandHex {
		if !strings.Contains(body, hex) {
			t.Errorf("%s: %s missing — tokens must come from brand-v4/tokens/tokens.css", name, hex)
		}
	}
	for _, font := range []string{`"Space Grotesk"`, `"IBM Plex Mono"`} {
		if !strings.Contains(body, font) {
			t.Errorf("the brand type stack must name %s", font)
		}
	}
	if strings.Contains(body, "@font-face") {
		t.Error("the page must not request a font — the brand faces are named over the system stack")
	}
	if !strings.Contains(body, `aria-label="Talyvor mark"`) || !strings.Contains(body, `class="tv-mark"`) {
		t.Error("the brand's flat mark must sit beside the name, inlined")
	}
	// The colours this page wore before the brand: the suite's old teal and its
	// tokens, and the amber of the old Lens pages. None may come back.
	for _, old := range []string{"#0B7A85", "#3ABDC9", "#f0a030", "#F4F5F6", "#141618", "#1D2023", "Inter,"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(old)) {
			t.Errorf("the pre-brand value %s is still on the page", old)
		}
	}
	if !strings.Contains(body, "prefers-color-scheme") {
		t.Error("the page must honour the viewer's light/dark preference")
	}
	if !strings.Contains(body, "prefers-reduced-motion") {
		t.Error("the reduced-motion floor must be honoured")
	}
}

// TestPageIsSelfContained: no CDN, no external font, no analytics. The API host
// must not make a visitor's browser talk to a third party.
func TestPageIsSelfContained(t *testing.T) {
	body := render(t)
	for _, bad := range []string{"http://", "//cdn", "googleapis", "unpkg", "jsdelivr", "<script src"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page references %q — it must be entirely self-contained", bad)
		}
	}
}

func TestPageEscapesNothingDynamicIntoHTML(t *testing.T) {
	// The version is the only interpolation; prove a hostile value cannot break out.
	rec := httptest.NewRecorder()
	New(`"><script>alert(1)</script>`).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Error("the version string was interpolated unescaped — an injection")
	}
}
