package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// pattern_earn.go — the Phase-3 routing-pattern EARNING wire-up (S4): the FIRST
// pattern-earning stage that touches the serve path. It routes an opted-in,
// authenticated workspace's served request into RecordPattern (which credits
// LENS via the S1/S2/S3-hardened earn path) INSTEAD of the mint-free capture
// write — earn and capture are mutually exclusive (earnPattern returns whether
// it took the corpus row; the caller runs capture only when it didn't).
//
// SERVE-NEUTRAL WHEN OFF: the flag check is the FIRST guard, before any auth
// read / IsOptedIn DB read / hash. Flag-off ⇒ earnPattern returns false at that
// guard with zero work, and the caller's capturePattern runs exactly as today.
//
// SAME ERROR BELT as capturePattern: detached context.WithoutCancel, void
// effect on serving, logged-and-swallowed write error. Called POST-FLUSH at the
// same seam as capturePattern — the IsOptedIn read and the credit run on the
// serve goroutine AFTER the client response is flushed, on a detached context,
// so nothing the client awaits is held on them.
//
// The earn sink is DELIBERATELY SEPARATE from the capture sink: the capture
// sink stays one mint-free method (cannot mint by construction); this is the
// mint-capable surface, gated by LENS_PATTERN_EARNING_ENABLED.

// patternEarnSink is the earn surface the proxy depends on: the opt-in read
// (sourced at the seam, AND'd into the gate) + the crediting earn write.
// *mining.PatternMiner satisfies both.
type patternEarnSink interface {
	IsOptedIn(ctx context.Context, workspaceID string) (bool, error)
	RecordPattern(ctx context.Context, workspaceID string, p mining.RoutingPattern, optedIn bool, requestID string) error
}

// SetPatternEarn wires the earn sink + its enable flag (read per-call).
func (p *Proxy) SetPatternEarn(sink patternEarnSink, enabled func() bool) {
	p.patternEarnSink = sink
	p.patternEarnEnabled = enabled
}

// earnPattern attempts the earning write for a just-served request. It returns
// TRUE when it took responsibility for this request's routing_patterns row (so
// the caller MUST skip capture — even if the write then errored, to avoid a
// double-write), and FALSE in every non-earning state (caller runs capture
// exactly as today). VOID effect on serving: post-flush, detached context,
// error swallowed.
//
// Gate: PatternEarningEnabled AND a real authenticated non-default workspace
// AND opted-in. The economic unit is the WORK PRODUCT — requestID is a
// content hash over (model, prompt, response), so identical work earns once.
func (p *Proxy) earnPattern(ctx context.Context, piiDetected, guardrailFired bool, loggingPolicy workspace.LoggingPolicy, feature, model, provider, prompt string, response []byte, inputTokens, outputTokens int, quality float64, scored bool, latencyMs int64, complexityBucket string) bool {
	// FLAG-OFF: first, cheapest guard — no auth read, no DB read, no hash.
	if p == nil || p.patternEarnSink == nil || p.patternEarnEnabled == nil || !p.patternEarnEnabled() {
		return false
	}
	// CORPUS EXCLUSION (money-path self-protection, shared guard): a sensitive
	// request never mints. Decline BEFORE the auth read / opt-in DB read /
	// content hash / RecordPattern — sensitive ⇒ no earn, no credit, no mint.
	// sensitiveForCorpus is the single shared predicate (also gates
	// capturePattern); its loggingPolicy==none term is belt-and-suspenders (see
	// pattern_corpus_sensitive.go).
	if sensitiveForCorpus(piiDetected, guardrailFired, loggingPolicy) {
		return false
	}
	if !scored {
		return false // no real quality score — let capture's own !scored guard drop it
	}
	// Gate (b): the credited workspace must be the AUTHENTICATED principal, not
	// the caller-asserted header. Read auth.GetAPIKey — the validated credential
	// the AuthMiddleware stamps on BOTH auth paths (the DB workspace-key fast
	// path AND the global-key/JWT fallback); GetAuthContext is stamped only on
	// the fallback, so it is nil for normal workspace keys (the dominant path)
	// and would make earning a no-op. Exclude the global-admin key (WorkspaceID
	// "") and the unauthenticated 'default' fallback — operational traffic
	// doesn't earn. A nil APIKey (in-process/test call without the middleware) ⇒
	// no earn, no panic (fail-safe).
	ak := auth.GetAPIKey(ctx)
	if ak == nil || ak.WorkspaceID == "" || ak.WorkspaceID == "default" {
		return false
	}
	earnWS := ak.WorkspaceID
	// Detached: post-serve, must outlive client cancellation and cannot affect
	// the served response (the IsOptedIn read + the credit both ride this).
	dctx, cancel := detachBounded(ctx) // B39.4: and bounded, so it cannot hang on a locked row
	defer cancel()
	optedIn, err := p.patternEarnSink.IsOptedIn(dctx, earnWS)
	if err != nil || !optedIn {
		return false // not opted-in, or read error (fail-safe) — caller runs capture
	}
	// Gate (a): the request_id is the WORK PRODUCT content hash — derived,
	// deterministic, NOT a caller-supplied header. Composed as the hash of the
	// three components' FIXED-LENGTH digests (not raw NUL-joined text), so a
	// caller-controlled prompt containing a literal NUL cannot shift a boundary
	// and collide distinct (model, prompt, response) triples. Identical work ⇒
	// same hash ⇒ the S3 claim earns it once.
	rid := poolroyalty.SHA256Hex([]byte(
		poolroyalty.SHA256Hex([]byte(model)) +
			poolroyalty.SHA256Hex([]byte(prompt)) +
			poolroyalty.SHA256Hex(response),
	))
	pat := mining.ExtractPattern(feature, model, provider, inputTokens, outputTokens, quality, latencyMs, false)
	pat.ComplexityBucket = complexityBucket // tier-cohort dimension; earned rows carry it too (parity with capture)
	if err := p.patternEarnSink.RecordPattern(dctx, earnWS, pat, true, rid); err != nil {
		// Logged-and-swallowed, like the capture/shadow paths — never touches
		// the served response. The earn still "took" the row decision (we return
		// true below), so capture does not double-write on a transient failure.
		slog.Warn("pattern earn: write failed (observational; serve unaffected)",
			slog.String("workspace", earnWS),
			slog.String("err", err.Error()),
		)
	}
	return true
}
