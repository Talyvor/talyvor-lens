package proxy

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/inference"
)

// upstream_credential_leak_test.go — THE CALLER'S TALYVOR CREDENTIAL MUST NOT REACH THE PROVIDER.
//
// forward copies EVERY inbound client header onto the upstream request (skipping only Host and
// Accept-Encoding) and then runs cfg.setAuth. The existing oracle forward_authheaders_test.go pins
// that the client's Authorization is overwritten — and its comment states that as a general property
// of the seam ("setAuth ... OVERWRITES the Authorization with the configured provider key").
//
// ⚠ THAT ORACLE DRIVES ONE PROVIDER. setAuth is a per-provider closure and only THREE of the eight
// shapes write Authorization at all: google's setAuth is an empty func (its key goes in the URL),
// vllm's is conditional on an operator key being configured, and bedrock's discards its own error
// when AWS credentials are absent. A property asserted on `openai` alone was read as holding for the
// switch.
//
// ⚠ AND AUTHORIZATION IS NOT THE ONLY CREDENTIAL HEADER. internal/auth accepts a Talyvor credential
// in THREE inbound locations (Authorization: Bearer, X-Talyvor-Key, X-API-Key) — extractCredential
// in manager.go and extractKey in middleware.go. No setAuth branch touches X-Talyvor-Key, and only
// anthropic's collides with X-API-Key. So the header a customer authenticates with is forwarded to
// the provider verbatim on every request that does not happen to reuse its name.
//
// The credential at stake is not junk. It is the caller's `tlv_` workspace key (spends their
// balance, mints keys for their workspace) or, via the X-API-Key legacy path, the GLOBAL admin key.
//
// WHAT THIS FILE ASSERTS, per provider shape: no inbound credential header value appears anywhere in
// what the upstream received — any header value, or the URL. Plus four floors, because a guard that
// can pass by measuring nothing is not a guard:
//   - the provider set is DERIVED from inference.ConfigFor's own switch and checked BOTH DIRECTIONS
//     against the table below, so a new provider fails until it is probed and a deleted one fails as
//     stale (a derived sweep cannot see what is no longer there);
//   - the credential-header set is DERIVED from internal/auth's two extractors and checked BOTH
//     DIRECTIONS, so a fourth credential location fails until it is classified;
//   - a benign client header must STILL arrive — "strip every header" would satisfy the leak
//     assertion and break the proxy;
//   - the configured provider credential must STILL arrive on every shape that has one — "strip the
//     credential headers and never re-add auth" would also satisfy it.

// credentialHeaders — the inbound header names internal/auth will accept a Talyvor credential in.
//
// PINNED BY HAND, and cross-checked against the source in TestCredentialHeaderSet_MatchesAuthSource.
// The pin exists because the derivation cannot see a deletion: if someone removes the X-API-Key
// branch from extractCredential the parse simply returns less, and a source-only guard would go
// quietly green over a narrower sweep.
var credentialHeaders = map[string]string{
	"Authorization": "Bearer <key>. auth/manager.go extractCredential + auth/middleware.go extractKey.",
	"X-Talyvor-Key": "the raw key, no scheme. Both extractors read it.",
	"X-API-Key":     "legacy location; auth/manager.go treats it as the GLOBAL admin key fallback.",
}

// leakSentinels maps each credential header to a distinct value, so a failure names WHICH location
// leaked rather than reporting that something did.
var leakSentinels = map[string]string{
	"Authorization": "Bearer tlv_SENTINEL_AUTHORIZATION",
	"X-Talyvor-Key": "tlv_SENTINEL_XTALYVORKEY",
	"X-API-Key":     "tlv_SENTINEL_XAPIKEY",
}

// benignClientHeader is forwarded on purpose — forward_authheaders_test.go pins that client headers
// reach the provider. It is the floor that stops "strip everything" from passing.
const benignClientHeader = "X-Client-Trace"

// providerProbe is one shape of the setAuth switch, configured to talk to a local upstream.
type providerProbe struct {
	// provider is the name inference.ConfigFor switches on.
	provider string
	// label distinguishes two probes of the SAME provider (vllm keyed vs unkeyed).
	label string
	// configure points this provider at the test upstream and sets its credential.
	configure func(p *Proxy, upstreamURL string)
	// wantCredential is a substring that MUST appear in what the upstream received (a header value
	// or the URL) — the configured provider credential. Empty means this shape deliberately sends
	// none, and that is recorded rather than skipped.
	wantCredential string
	// why records the reason a shape carries no credential.
	why string
}

func probes() []providerProbe {
	return []providerProbe{
		{
			provider: "openai", label: "openai",
			configure:      func(p *Proxy, u string) { p.openAIURL = u },
			wantCredential: "openai-key",
		},
		{
			provider: "anthropic", label: "anthropic",
			configure:      func(p *Proxy, u string) { p.anthropicURL = u },
			wantCredential: "anthropic-key",
		},
		{
			provider: "google", label: "google",
			configure:      func(p *Proxy, u string) { p.googleURL = u },
			wantCredential: "google-key", // in the URL query, not a header
		},
		{
			provider: "mistral", label: "mistral",
			configure:      func(p *Proxy, u string) { p.mistralURL = u; p.mistralKey = "mistral-key" },
			wantCredential: "mistral-key",
		},
		{
			provider: "groq", label: "groq",
			configure:      func(p *Proxy, u string) { p.groqURL = u; p.groqKey = "groq-key" },
			wantCredential: "groq-key",
		},
		{
			provider: "vllm", label: "vllm-keyed",
			configure:      func(p *Proxy, u string) { p.vllmURL = u; p.vllmKey = "vllm-key" },
			wantCredential: "vllm-key",
		},
		{
			provider: "vllm", label: "vllm-unkeyed",
			configure:      func(p *Proxy, u string) { p.vllmURL = u; p.vllmKey = "" },
			wantCredential: "",
			why:            "vLLM commonly runs unauthenticated on a private network; setAuth attaches nothing when no operator key is configured.",
		},
		{
			provider: "bedrock", label: "bedrock-signed",
			configure: func(p *Proxy, u string) {
				p.bedrockURL = u
				p.bedrockConfig = inference.BedrockConfig{
					AccessKeyID:     "AKIA-TEST-ACCESS-KEY",
					SecretAccessKey: "test-secret",
					Region:          "us-east-1",
				}
			},
			wantCredential: "AKIA-TEST-ACCESS-KEY",
		},
		{
			provider: "bedrock", label: "bedrock-unsigned",
			configure:      func(p *Proxy, u string) { p.bedrockURL = u; p.bedrockConfig = inference.BedrockConfig{} },
			wantCredential: "",
			why:            "SignRequest returns an error when AWS credentials are absent and ConfigFor DISCARDS it (`_ = SignRequest(...)`), so no auth header is written at all.",
		},
	}
}

// upstreamCapture is everything the provider side of the wire saw.
type upstreamCapture struct {
	header http.Header
	url    string
}

// contains reports whether needle appears in any received header value or in the URL.
func (c upstreamCapture) contains(needle string) (where string, found bool) {
	for name, values := range c.header {
		for _, v := range values {
			if strings.Contains(v, needle) {
				return "header " + name, true
			}
		}
	}
	if strings.Contains(c.url, needle) {
		return "URL", true
	}
	return "", false
}

// runProbe drives p.forward once for one provider shape and returns what the upstream received.
func runProbe(t *testing.T, pr providerProbe) upstreamCapture {
	t.Helper()

	got := make(chan upstreamCapture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		got <- upstreamCapture{header: rr.Header.Clone(), url: rr.URL.String()}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()

	p := newProxyWithFallback(t, "", "", "")
	pr.configure(p, upstream.URL)
	cfg := p.configForProvider(pr.provider)
	if cfg.ProviderName() == "" {
		t.Fatalf("%s: configForProvider(%q) returned the zero config — the probe would measure nothing", pr.label, pr.provider)
	}

	body := `{"model":"m","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/"+pr.provider+"/chat/completions", strings.NewReader(body))
	for name, value := range leakSentinels {
		r.Header.Set(name, value)
	}
	r.Header.Set(benignClientHeader, "trace-123")

	// FLOOR 0 — the probe really is carrying the credentials it claims to look for. A probe that
	// stopped setting them would find nothing and report the product clean; this makes that edit red
	// instead of green.
	for name, value := range leakSentinels {
		if got := r.Header.Get(name); got != value {
			t.Fatalf("%s: the probe did not put %s on the outbound request (got %q) — it would be measuring nothing", pr.label, name, got)
		}
	}

	resp, _, _, err := p.forward(context.Background(), r, []byte(body), "m", cfg)
	if err != nil {
		t.Fatalf("%s: forward: %v", pr.label, err)
	}
	_ = resp.Body.Close()

	select {
	case c := <-got:
		return c
	default:
		t.Fatalf("%s: the upstream was never reached — this probe measured nothing", pr.label)
		return upstreamCapture{}
	}
}

// TestForward_ClientCredentialNeverReachesUpstream is the guard. Every provider shape, every
// credential location.
func TestForward_ClientCredentialNeverReachesUpstream(t *testing.T) {
	all := probes()
	if len(all) == 0 {
		t.Fatal("no probes — this test cannot pass by having nothing to check")
	}

	ran := 0
	for _, pr := range all {
		pr := pr
		t.Run(pr.label, func(t *testing.T) {
			c := runProbe(t, pr)

			// FLOOR 1 — the client headers really were copied. Without this, a forward that dropped
			// every inbound header would satisfy the leak assertion while breaking the proxy.
			if c.header.Get(benignClientHeader) != "trace-123" {
				t.Fatalf("%s: benign client header %s did not reach the upstream (got %q) — this probe is not observing the header-copy path",
					pr.label, benignClientHeader, c.header.Get(benignClientHeader))
			}

			// FLOOR 2 — the configured provider credential still arrives.
			if pr.wantCredential != "" {
				if _, ok := c.contains(pr.wantCredential); !ok {
					t.Errorf("%s: the CONFIGURED provider credential %q never reached the upstream — auth is broken, not merely tightened",
						pr.label, pr.wantCredential)
				}
			} else {
				t.Logf("%s: carries no configured credential by design — %s", pr.label, pr.why)
			}

			// THE ASSERTION.
			for name := range credentialHeaders {
				sentinel, ok := leakSentinels[name]
				if !ok {
					t.Fatalf("credential header %q has no sentinel — the tables have drifted", name)
				}
				// Compare on the VALUE, not the scheme prefix: "Bearer " is not secret.
				needle := strings.TrimPrefix(sentinel, "Bearer ")
				if where, found := c.contains(needle); found {
					t.Errorf("%s: THE CALLER'S CREDENTIAL FROM %s REACHED THE PROVIDER — %q found in %s",
						pr.label, name, needle, where)
				}
			}
			ran++
		})
	}

	if ran != len(all) {
		t.Errorf("ran %d of %d probes — a skipped shape is an unmeasured shape", ran, len(all))
	}
}

// streamProbe is one shape of the SECOND header-copy seam.
//
// ⚠ THE STREAMING PATH BUILDS ITS OWN UPSTREAM REQUEST. stream.go:serve does not go through
// forward/RunUpstream at all — it has its own `for name, values := range r.Header` loop followed by
// its own applyAuth. A fix that stopped at forward would have been INERT on every streamed request,
// and streaming is not a corner: it is the lane the prompt compressor is deliberately skipped on.
// These probes drive the whole handler (HandleOpenAI / HandleAnthropic with "stream":true), so they
// observe the product as served rather than a helper in isolation.
type streamProbe struct {
	label          string
	configure      func(p *Proxy, upstreamURL string)
	handle         func(p *Proxy, w http.ResponseWriter, r *http.Request)
	path           string
	body           string
	sse            string
	wantCredential string
}

func streamProbes() []streamProbe {
	return []streamProbe{
		{
			label:          "stream-openai",
			configure:      func(p *Proxy, u string) { p.openAIURL = u },
			handle:         func(p *Proxy, w http.ResponseWriter, r *http.Request) { p.HandleOpenAI(w, r) },
			path:           "/v1/proxy/openai/v1/chat/completions",
			body:           `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			sse:            openAISSEBody,
			wantCredential: "openai-key",
		},
		{
			label:          "stream-anthropic",
			configure:      func(p *Proxy, u string) { p.anthropicURL = u },
			handle:         func(p *Proxy, w http.ResponseWriter, r *http.Request) { p.HandleAnthropic(w, r) },
			path:           "/v1/proxy/anthropic/v1/messages",
			body:           `{"model":"claude-3-opus-20240229","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			sse:            anthropicSSEBody,
			wantCredential: "anthropic-key",
		},
	}
}

// TestStream_ClientCredentialNeverReachesUpstream — the same assertion, on the streaming seam,
// driven through the real handler.
func TestStream_ClientCredentialNeverReachesUpstream(t *testing.T) {
	all := streamProbes()
	if len(all) == 0 {
		t.Fatal("no streaming probes — this test cannot pass by having nothing to check")
	}

	for _, sp := range all {
		sp := sp
		t.Run(sp.label, func(t *testing.T) {
			got := make(chan upstreamCapture, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
				got <- upstreamCapture{header: rr.Header.Clone(), url: rr.URL.String()}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, sp.sse)
			}))
			defer upstream.Close()

			p := newProxyWithFallback(t, "", "", "")
			sp.configure(p, upstream.URL)

			r := httptest.NewRequest(http.MethodPost, sp.path, strings.NewReader(sp.body))
			r.Header.Set("Content-Type", "application/json")
			for name, value := range leakSentinels {
				r.Header.Set(name, value)
			}
			r.Header.Set(benignClientHeader, "trace-123")
			for name, value := range leakSentinels {
				if v := r.Header.Get(name); v != value {
					t.Fatalf("%s: the probe did not put %s on the outbound request (got %q)", sp.label, name, v)
				}
			}

			w := newFlushRecorder()
			sp.handle(p, w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: handler status = %d, want 200 (body=%q) — this probe did not reach the streaming path", sp.label, w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
				t.Fatalf("%s: Content-Type = %q — the request did not take the STREAMING path, so this probe measured the wrong seam", sp.label, ct)
			}

			var c upstreamCapture
			select {
			case c = <-got:
			default:
				t.Fatalf("%s: the upstream was never reached — this probe measured nothing", sp.label)
			}

			if c.header.Get(benignClientHeader) != "trace-123" {
				t.Fatalf("%s: benign client header did not reach the upstream — this probe is not observing the header-copy path", sp.label)
			}
			if _, ok := c.contains(sp.wantCredential); !ok {
				t.Errorf("%s: the CONFIGURED provider credential %q never reached the upstream — auth is broken, not merely tightened", sp.label, sp.wantCredential)
			}

			for name := range credentialHeaders {
				needle := strings.TrimPrefix(leakSentinels[name], "Bearer ")
				if where, found := c.contains(needle); found {
					t.Errorf("%s: THE CALLER'S CREDENTIAL FROM %s REACHED THE PROVIDER — %q found in %s",
						sp.label, name, needle, where)
				}
			}
		})
	}
}

// TestLeakDetector_SeesALeak — THE CONTROL, SHIPPED RATHER THAN RUN BY HAND ONCE.
//
// Every assertion above is an ABSENCE, and an absence reported by an instrument that reads nothing
// is indistinguishable from an absence that is real. This drives the SAME capture path against
// inference.RunUpstream directly, handing it the raw inbound headers the way forward did before the
// fix, and requires the sentinel to BE FOUND. If the capture, the sentinel plumbing or
// upstreamCapture.contains ever stops working, this goes red — so the clean result next door cannot
// be a clean result from a blind detector.
//
// google is the shape used on purpose: its setAuth is an empty closure, so nothing but the
// header-copy decides what the provider sees.
func TestLeakDetector_SeesALeak(t *testing.T) {
	got := make(chan upstreamCapture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		got <- upstreamCapture{header: rr.Header.Clone(), url: rr.URL.String()}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	p := newProxyWithFallback(t, "", "", upstream.URL)
	cfg := p.configForProvider("google")

	raw := http.Header{}
	for name, value := range leakSentinels {
		raw.Set(name, value)
	}

	resp, _, _, err := inference.RunUpstream(
		context.Background(), p.httpClient, p.retryConfig,
		cfg.UpstreamURL("m"), cfg.ApplyAuth, []byte(`{}`), raw,
	)
	if err != nil {
		t.Fatalf("RunUpstream: %v", err)
	}
	_ = resp.Body.Close()

	c := <-got
	for name, sentinel := range leakSentinels {
		needle := strings.TrimPrefix(sentinel, "Bearer ")
		if where, found := c.contains(needle); !found {
			t.Errorf("CONTROL FAILED: %s did not reach the upstream through the UNSANITISED path — the detector cannot see a leak, so the clean results in this file prove nothing", name)
		} else {
			t.Logf("control: %s observed leaking through the unsanitised path (%s) — the detector works", name, where)
		}
	}
}

// TestProviderProbes_CoverConfigForSwitch — the provider set is DERIVED, both directions.
//
// A hand-written provider list guards the providers someone thought of. inference.ConfigFor's switch
// IS the set of shapes setAuth can take, so it is parsed here and required to match the probe table
// exactly: a new `case` fails until it is probed, and a deleted one fails as a stale probe.
func TestProviderProbes_CoverConfigForSwitch(t *testing.T) {
	fromSource := configForCaseLiterals(t)
	if len(fromSource) == 0 {
		t.Fatal("parsed ZERO provider cases out of inference.ConfigFor — the parse is broken, and a broken parse must not read as full coverage")
	}

	probed := map[string]bool{}
	for _, pr := range probes() {
		probed[pr.provider] = true
	}

	for _, name := range fromSource {
		if !probed[name] {
			t.Errorf("provider %q is a case in inference.ConfigFor and is NOT probed by this file — classify it (probe it, or say why it cannot leak)", name)
		}
	}
	for name := range probed {
		found := false
		for _, s := range fromSource {
			if s == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("provider %q is probed here but is NOT a case in inference.ConfigFor — stale probe", name)
		}
	}
	t.Logf("provider shapes derived from inference.ConfigFor: %v", fromSource)
}

// TestCredentialHeaderSet_MatchesAuthSource — the credential-header set is DERIVED, both directions.
//
// internal/auth decides what counts as a credential. If it grows a fourth location, this file must
// learn about it BEFORE that location starts leaking, so the two extractors are parsed and their
// Header.Get literals are required to match the pinned table exactly.
func TestCredentialHeaderSet_MatchesAuthSource(t *testing.T) {
	fromSource := authExtractorHeaderLiterals(t)
	if len(fromSource) == 0 {
		t.Fatal("parsed ZERO header names out of internal/auth's extractors — the parse is broken, and a broken parse must not read as full coverage")
	}

	pinned := map[string]bool{}
	for name := range credentialHeaders {
		pinned[textproto.CanonicalMIMEHeaderKey(name)] = true
	}

	// The THIRD copy, and the one that actually decides what gets stripped at runtime.
	stripped := map[string]bool{}
	for _, name := range auth.CredentialHeaders {
		stripped[textproto.CanonicalMIMEHeaderKey(name)] = true
	}

	for _, name := range fromSource {
		if !pinned[name] {
			t.Errorf("internal/auth accepts a credential in header %q and this file does not sweep for it — add it to credentialHeaders and leakSentinels", name)
		}
		if !stripped[name] {
			t.Errorf("internal/auth accepts a credential in header %q and auth.CredentialHeaders does NOT list it — forward would send it to the provider", name)
		}
	}
	for name := range pinned {
		found := false
		for _, s := range fromSource {
			if s == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("header %q is pinned as a credential location but internal/auth no longer reads it — stale pin", name)
		}
	}
	for name := range stripped {
		found := false
		for _, s := range fromSource {
			if s == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("auth.CredentialHeaders strips %q but no extractor reads it as a credential — stale entry", name)
		}
	}
	t.Logf("credential locations derived from internal/auth: %v", fromSource)
	t.Logf("credential locations stripped at the seam (auth.CredentialHeaders): %v", auth.CredentialHeaders)
}

// runUpstreamCallSites — every non-test call of inference.RunUpstream, with the SOURCE TEXT of its
// extraHeaders argument.
//
// The behavioural probe above drives the ONE call site that carries inbound headers today. It
// cannot see a SECOND call site added tomorrow that hands RunUpstream a raw request header map —
// RunUpstream copies whatever it is given, so the sanitising is a property of the caller, not of
// the seam. This table makes a new or edited call site fail until somebody classifies it.
//
// ⚠ SAID PLAINLY RATHER THAN IMPLIED: this compares SOURCE TEXT. It cannot tell that
// StripCredentialHeaders actually strips anything — a hollowed-out implementation would satisfy it.
// That is what TestForward_ClientCredentialNeverReachesUpstream is for; this one only answers "is
// the sanitiser applied at every seam", which the behavioural test cannot answer.
var runUpstreamCallSites = map[string]string{
	"internal/inference/inferer.go": "nil",
	"internal/proxy/proxy.go":       "auth.StripCredentialHeaders(r.Header)",
}

func TestRunUpstreamCallSites_CarryNoRawInboundHeaders(t *testing.T) {
	root := "../.."
	found := map[string]string{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			// A file this package cannot parse is not a pass — say so.
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel := strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(path, root)), "/")
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if name != "RunUpstream" || len(call.Args) != 7 {
				return true
			}
			argSrc := string(src[fset.Position(call.Args[6].Pos()).Offset:fset.Position(call.Args[6].End()).Offset])
			found[rel] = argSrc
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(found) == 0 {
		t.Fatal("found ZERO RunUpstream call sites — the walk is broken, and a broken walk must not read as full coverage")
	}
	for file, arg := range found {
		want, ok := runUpstreamCallSites[file]
		if !ok {
			t.Errorf("%s calls inference.RunUpstream and is not classified — it passes %q as extraHeaders; if that is an inbound request header map it leaks the caller's credential", file, arg)
			continue
		}
		if arg != want {
			t.Errorf("%s passes %q as RunUpstream's extraHeaders, classified as %q — re-measure before widening", file, arg, want)
		}
	}
	for file := range runUpstreamCallSites {
		if _, ok := found[file]; !ok {
			t.Errorf("%s is classified as a RunUpstream call site and no such call was found — stale entry", file)
		}
	}
	t.Logf("RunUpstream call sites: %v", found)
}

// headerCopyLoops — every non-test loop in the repo that writes into an http.Header from a range,
// keyed by "<file>: <range expression>", with what makes it safe.
//
// ⚠ WHY THIS EXISTS AND WHY IT IS NOT THE RunUpstream TABLE AGAIN. The first fix for this defect
// went in at proxy.forward and was INERT on every streamed request, because stream.go carries its
// OWN copy of the same loop and never touches RunUpstream. Two seams were found by reading a grep
// output; a third would be found by nobody. This enumerates the shape — a range that fills a header
// map — from the source, and refuses to pass on one that is not classified.
//
// A new entry here is not automatically a defect. It is a question: does this loop put an INBOUND
// request's headers onto an OUTBOUND one? If yes it must range over auth.StripCredentialHeaders.
var headerCopyLoops = map[string]string{
	"internal/scan/energy.go: countries":                                                      "B52.21: builds Scan's OWN outbound request to GIE per country and sets Talyvor's GIE key on it (LENS_SCAN_GIE_KEY). No inbound request is in reach; the fetcher refuses a redirect off GIE's host so the key never follows one.",
	"internal/scan/energy.go: ecmwfSteps":                                                     "B52.21: builds Scan's OWN outbound request to ECMWF's open-data portal per forecast step and sets only a Range header from ECMWF's index. No inbound request is in reach and no credential is set.",
	"internal/inference/runupstream.go: extraHeaders":                                         "the round-trip seam. It copies whatever it is GIVEN; sanitising is the caller's job and TestRunUpstreamCallSites_CarryNoRawInboundHeaders pins every caller.",
	"internal/proxy/stream.go: auth.StripCredentialHeaders(r.Header)":                         "the streaming seam — inbound → upstream, sanitised at the range.",
	"internal/compat/helicone.go: propertyKeys":                                               "rewrites the INBOUND request in place (r.Header.Set on r itself). It builds no upstream request, so it cannot leak to a provider.",
	`cmd/lens/market_use_handler.go: []string{"Authorization", "X-Talyvor-Key", "X-Api-Key"}`: "B20.2: copies the caller's credential onto an IN-PROCESS request to Lens's own router (lens.ServeHTTP), so a listing's model calls pass the same authentication and are billed to the same buyer. It never leaves the process; the proxy handler it reaches builds the upstream request and strips credentials there like any other.",
	`internal/proxy/run_code.go: []string{"Idempotency-Key", "X-Talyvor-Request-ID"}`:         "B28.119: renames two headers on a Run code round's OWN clone of the inbound request, so each round is its own request. It copies nothing anywhere; the clone goes to serve, which builds the upstream request and strips credentials there like any other.",
	`internal/proxy/web_search.go: []string{"Idempotency-Key", "X-Talyvor-Request-ID"}`:       "B28.118: renames two headers on the web search round's OWN clone of the inbound request, so the search is its own request. It copies nothing anywhere; the clone goes to serve, which builds the upstream request and strips credentials there like any other.",
	"internal/scan/contracts.go: listed":                                                      "B52.23: builds Scan's own request to USAspending's public search, setting only Content-Type. No inbound request is in reach, so nothing of a caller's can be copied.",
}

func TestHeaderCopyLoops_AllClassified(t *testing.T) {
	found := map[string]bool{}

	err := filepath.Walk("../..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel := strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(path, "../..")), "/")
		ast.Inspect(f, func(n ast.Node) bool {
			rng, ok := n.(*ast.RangeStmt)
			if !ok || rng.Body == nil {
				return true
			}
			writes := false
			ast.Inspect(rng.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Add" && sel.Sel.Name != "Set") {
					return true
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Header" {
					writes = true
				}
				return true
			})
			if !writes {
				return true
			}
			expr := string(src[fset.Position(rng.X.Pos()).Offset:fset.Position(rng.X.End()).Offset])
			found[rel+": "+expr] = true
			// Do NOT descend: a header copy is written as an OUTER range over the source map with an
			// INNER range over each value slice, and the inner one would otherwise be recorded under
			// the meaningless key "values". The outermost matching loop is the seam.
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(found) == 0 {
		t.Fatal("found ZERO header-copy loops — the walk is broken, and a broken walk must not read as full coverage")
	}
	for key := range found {
		if _, ok := headerCopyLoops[key]; !ok {
			t.Errorf("UNCLASSIFIED header-copy loop %q — if it puts an inbound request's headers on an outbound one it leaks the caller's credential; range over auth.StripCredentialHeaders, or classify it here with the reason it cannot", key)
		}
	}
	for key := range headerCopyLoops {
		if !found[key] {
			t.Errorf("classified header-copy loop %q no longer exists — stale entry", key)
		}
	}
	t.Logf("header-copy loops found: %d", len(found))
}

// configForCaseLiterals returns the string literals of the switch inside inference.ConfigFor.
func configForCaseLiterals(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../inference/config.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ../inference/config.go: %v", err)
	}
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ConfigFor" || fn.Recv != nil {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						out = append(out, s)
					}
				}
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

// authExtractorHeaderLiterals returns the canonical header names read by internal/auth's two
// credential extractors.
func authExtractorHeaderLiterals(t *testing.T) []string {
	t.Helper()
	want := map[string][]string{
		"../auth/manager.go":    {"extractCredential"},
		"../auth/middleware.go": {"extractKey"},
	}
	seen := map[string]bool{}
	for path, fns := range want {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, name := range fns {
			fn := findFunc(f, name)
			if fn == nil {
				t.Fatalf("%s: function %s not found — the parse anchor moved, and a missing anchor must fail rather than silently sweep nothing", path, name)
			}
			for _, h := range headerGetLiterals(fn) {
				seen[textproto.CanonicalMIMEHeaderKey(h)] = true
			}
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Recv == nil {
			return fn
		}
	}
	return nil
}

// headerGetLiterals collects the string argument of every `<x>.Header.Get("…")` call in fn.
func headerGetLiterals(fn *ast.FuncDecl) []string {
	var out []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Get" {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "Header" {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}
