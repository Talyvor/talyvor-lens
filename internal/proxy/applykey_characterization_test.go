package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestApplyKey_CredentialSubstitution_Characterization pins the CURRENT observable behavior of applyKey
// (proxy.go#applyKey — was cited as line 2466, which had decayed 577 lines) — the key-pool
// credential override — BEFORE PR-3c relocates its logic to
// inference.ConfigForKey. Recon found applyKey is a credential-path BLIND SPOT (no test asserted its
// key-substitution), so this is the behavior oracle for the move, exactly as #255 pinned forward's
// header/auth ordering.
//
// It builds each source config the way the proxy builds it (p.configForProvider), applies applyKey with a
// sentinel POOLED key against a *Proxy whose CONFIGURED keys are distinct sentinels, then OBSERVES the
// result by running the returned setAuth on a fresh request (and, for google, calling upstreamURLFn). The
// assertions encode what applyKey ACTUALLY does today — including the surprising truths (anthropic also
// re-sets anthropic-version; google puts the key in the URL query, not a header; mistral/groq/vllm/bedrock
// are pass-through — the pooled key is NOT applied). Do NOT "fix" applyKey to match an expectation.
//
// This test stays in package proxy through the PR-3c move: proxy.applyKey becomes a one-line delegation to
// inference.ConfigForKey with the SAME observable result, so this must remain green unedited. If step 2
// can't keep it green without editing it, that is the stop signal.
func TestApplyKey_CredentialSubstitution_Characterization(t *testing.T) {
	// CONFIGURED keys are deliberately distinct from the POOLED key so a leak of the configured value is
	// detectable — applyKey must substitute the pooled key, never the proxy's own.
	p := &Proxy{
		openAIURL:    "https://api.openai.example",
		openAIKey:    "CONFIGURED-openai",
		anthropicURL: "https://api.anthropic.example",
		anthropicKey: "CONFIGURED-anthropic",
		googleURL:    "https://generativelanguage.example",
		googleKey:    "CONFIGURED-google",
		mistralURL:   "https://api.mistral.example",
		mistralKey:   "CONFIGURED-mistral",
	}
	const pooled = "POOLKEY-sentinel"

	// authHeaders runs a config's auth on a fresh request and returns the resulting header set. Observed
	// via the exported ApplyAuth method (= nil-guarded setAuth) since the providerConfig fields moved to
	// internal/inference and are unexported (PR-3c). Observation-only: every assertion below is unchanged.
	authHeaders := func(cfg providerConfig) http.Header {
		req := httptest.NewRequest(http.MethodPost, "http://upstream/", nil)
		cfg.ApplyAuth(req)
		return req.Header
	}

	// 1 · openai → Authorization: Bearer <pooled> (the pooled key, NOT the configured one).
	t.Run("openai_Authorization_Bearer_pooled", func(t *testing.T) {
		got := authHeaders(p.applyKey(context.Background(), p.configForProvider("openai"), pooled)).Get("Authorization")
		t.Logf("PINNED openai   Authorization = %q", got)
		if got != "Bearer "+pooled {
			t.Fatalf("openai Authorization = %q, want %q", got, "Bearer "+pooled)
		}
		if strings.Contains(got, "CONFIGURED") {
			t.Fatalf("openai Authorization leaked the configured key: %q", got)
		}
	})

	// 2 · anthropic → x-api-key: <pooled>, and (surprising truth) applyKey ALSO re-sets anthropic-version;
	// it sets NO Authorization header (anthropic auth is x-api-key, not Bearer).
	t.Run("anthropic_xapikey_pooled_plus_version", func(t *testing.T) {
		h := authHeaders(p.applyKey(context.Background(), p.configForProvider("anthropic"), pooled))
		t.Logf("PINNED anthropic x-api-key = %q  anthropic-version = %q  Authorization = %q",
			h.Get("x-api-key"), h.Get("anthropic-version"), h.Get("Authorization"))
		if got := h.Get("x-api-key"); got != pooled {
			t.Fatalf("anthropic x-api-key = %q, want %q", got, pooled)
		}
		if got := h.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q, want 2023-06-01 (applyKey re-sets it in the rewritten closure)", got)
		}
		if got := h.Get("Authorization"); got != "" {
			t.Fatalf("anthropic must not set Authorization, got %q", got)
		}
	})

	// 3 · google → the pooled key goes in the URL QUERY (key=<pooled>), not a header; setAuth stays a no-op.
	t.Run("google_url_query_pooled_no_header", func(t *testing.T) {
		cfg := p.applyKey(context.Background(), p.configForProvider("google"), pooled)
		gotURL := cfg.UpstreamURL("gemini-2.5-flash")
		t.Logf("PINNED google   upstreamURL = %q", gotURL)
		if !strings.Contains(gotURL, "key="+pooled) {
			t.Fatalf("google URL = %q, want it to contain key=%s (pooled key in the query string)", gotURL, pooled)
		}
		if strings.Contains(gotURL, "CONFIGURED") {
			t.Fatalf("google URL leaked the configured key: %q", gotURL)
		}
		if got := authHeaders(cfg).Get("Authorization"); got != "" {
			t.Fatalf("google must set no Authorization header (key is in the URL), got %q", got)
		}
	})

	// 4 · pass-through (mistral) → applyKey leaves the config UNCHANGED; the pooled key is NOT applied, the
	// CONFIGURED key still flows. (groq/vllm/bedrock are likewise absent from applyKey's switch.)
	t.Run("mistral_passthrough_pooled_NOT_applied", func(t *testing.T) {
		base := p.configForProvider("mistral")
		after := p.applyKey(context.Background(), base, pooled)
		beforeAuth := authHeaders(base).Get("Authorization")
		afterAuth := authHeaders(after).Get("Authorization")
		t.Logf("PINNED mistral  before=%q  after=%q (pass-through: pooled key NOT applied)", beforeAuth, afterAuth)
		if afterAuth != beforeAuth {
			t.Fatalf("mistral auth changed by applyKey: before=%q after=%q (pass-through expected)", beforeAuth, afterAuth)
		}
		if !strings.Contains(afterAuth, "CONFIGURED-mistral") {
			t.Fatalf("mistral must keep the configured key, got %q", afterAuth)
		}
		if strings.Contains(afterAuth, pooled) {
			t.Fatalf("mistral wrongly applied the pooled key: %q", afterAuth)
		}
	})
}
