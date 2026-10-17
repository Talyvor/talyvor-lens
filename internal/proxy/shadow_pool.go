package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/poolshadow"
)

// shadow_pool.go — the WRITE-SIDE shadow log for cross-tenant cache pooling (W4.9). It records that
// a fresh, cacheable response was generated, so the pooled hit rate becomes computable from repeats
// while pooling itself stays off.
//
// ⚠ WHY IT IS ON THE WRITE SIDE, WHICH IS THE FINDING THAT SHAPED IT AND NOT AN IMPLEMENTATION
// CHOICE. The obvious shadow log does the pooled LOOKUP on a private miss and records the would-be
// hit without serving it. It CANNOT REPORT ANYTHING BUT ZERO: tryExactPooled and trySemanticPooled
// read a keyspace written only under poolGate.DecidePoolableOnWrite, and both sides route through
// the same PoolabilityGate.Participant predicate (global switch AND workspace opt-in). Off means
// the pooled keyspace is EMPTY, so the lookup misses every time, forever — and "0 would have
// pooled" is then read as evidence that pooling is worthless rather than as evidence that the
// off-switch also emptied the thing being measured.
//
// THE SAFETY IS STRUCTURAL, and it is the same argument shadow_lxc.go and pattern_capture.go make:
// shadowPoolObservation returns NOTHING. A void, post-serve, error-swallowed call cannot block,
// delay, fail or alter any request — there is no return value a serve path could branch on.
//
// CANNOT SERVE, CANNOT POOL, CANNOT SPEND. The sink surface is one persist-only method. It has no
// cache client, so it cannot read or write the pooled keyspace; no ledger handle, so it cannot
// mint; and it is handed FINGERPRINTS, not prompt text or response bytes, so the table it writes
// cannot become a prompt log. Turning this observation into a serve would mean writing new code,
// not flipping a flag.
//
// Inert by default: gated on PoolShadowLogEnabled (default false) AND a wired sink.

// poolShadowSink is the minimal surface the proxy depends on — one method, persist-only.
// *poolshadow.Recorder satisfies it. It deliberately exposes NO read-back and no lookup: the rate
// is computed offline by an operator, never by the serve path.
type poolShadowSink interface {
	Record(ctx context.Context, o poolshadow.Observation) error
}

// SetPoolShadowLog wires the shadow-log sink + its enable flag (read PER-CALL so the flag stays
// live rather than captured). Both are optional and nil-safe.
func (p *Proxy) SetPoolShadowLog(sink poolShadowSink, enabled func() bool) {
	p.poolShadowSink = sink
	p.poolShadowEnabled = enabled
}

// poolShadowWriteTimeout bounds the detached observation write, mirroring captureWriteTimeout.
const poolShadowWriteTimeout = 5 * time.Second

// shadowPoolObservation records that a fresh, cacheable response was produced for (provider, model,
// rawPrompt) by workspaceID. VOID by design.
//
// ⚠ THE CALLER GATES ON THE SAME shouldCache IT GATES storeCaches ON. That is deliberate: a
// response the product would not cache could never have been in the pool either, so logging it
// would put rows in the denominator that no amount of pooling could ever have converted — a rate
// deflated by construction. PII-flagged and quality-rejected responses are excluded for exactly
// that reason, not only for privacy.
//
// didPool carries whether the pooled copy was ACTUALLY written, so a reader can separate shadow
// rows from live ones without knowing how the flag was set that day.
func (p *Proxy) shadowPoolObservation(ctx context.Context, workspaceID, provider, model, rawPrompt, reqFP string, didPool bool) {
	if p == nil || p.poolShadowSink == nil || p.poolShadowEnabled == nil || !p.poolShadowEnabled() || cacheStoreOff(ctx) || poolOff(ctx) {
		return
	}
	// Shed under overload, like every other post-serve observational write: an observation is
	// observational by contract, so a saturated writer bound means skip the row rather than queue
	// another writer against an already-drowning pool.
	if !p.obsLimiter.TryAcquire() {
		if p.obsLimiter.LogDrop() {
			slog.Warn("pool shadow log: observation dropped (writer bound reached; observational, serve unaffected)",
				slog.Int64("dropped_total", p.obsLimiter.Dropped()))
		}
		return
	}
	defer p.obsLimiter.Release()

	// pooledPromptKey is the PRODUCTION pooled key rule (cache.PooledPromptKey) — the same call
	// storeCaches makes. Passing it in rather than re-deriving it inside poolshadow keeps one
	// source of truth: if the key rule changes, the shadow numbers move with the pool they
	// describe instead of silently ceasing to describe it.
	o := poolshadow.Observe(workspaceID, provider, model, cache.FingerprintedKey(pooledPromptKey(rawPrompt), reqFP), reqFP, rawPrompt, didPool)

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), poolShadowWriteTimeout)
	defer cancel()
	if err := p.poolShadowSink.Record(wctx, o); err != nil {
		slog.Warn("pool shadow log: observation write failed (observational; serve unaffected)",
			slog.String("workspace", workspaceID),
			slog.String("err", err.Error()),
		)
	}
}
