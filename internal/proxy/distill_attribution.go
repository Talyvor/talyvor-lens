package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/workspace"
)

// distill_attribution.go — the S1 distill attribution WRITE: a durable,
// MINT-FREE record of a CONSENTED cross-tenant pooled-distill serve (who
// contributed the artifact, who was served, the content hash).
//
// THE SAFETY IS STRUCTURAL (same shape as capturePattern / the pooled mint):
// recordDistillServes returns NOTHING — a void, post-serve, error-swallowed,
// detached call cannot block, delay, fail, or alter any request. The sink
// interface exposes ONLY RecordDistillServe — there is no credit/earn method, so
// this path cannot mint by construction.
//
// GATED on ALL of: (1) the three-switch distill-pooling consent already
// authorized the cross-tenant serve (the fact is emitted ONLY on the consented
// branch of tryConvertBlock), (2) owner != requester (self-serve skipped
// upstream), and (3) the requester's logging policy is not None (the row records
// a content hash). Default-off: a nil sink ⇒ no call at all.

// distillServeFact is a consented cross-tenant distill serve surfaced up from
// MaybeDistill so serve() can record it post-flush. owner is the artifact's
// contributing workspace; the requester is the serving request's workspace.
type distillServeFact struct {
	owner string
	hash  string
	// basis (L2/S4 PR2): present (visionModel != "") ONLY for a cross-tenant OCR
	// serve — the avoided-COGS A's cached OCR transcription saved B, snapshotted at
	// serve time, plus the provenance (model + token split) that makes the figure
	// re-derivable. Conversion serves carry no basis (zero values). Recorded
	// descriptively into distill_royalty_basis; the gated mint is PR3.
	avoidedCOGSUSD     float64
	visionModel        string
	visionInputTokens  int
	visionOutputTokens int
}

// distillAttributionSink is the minimal write surface the proxy depends on — one
// method, persist-only. *distillattrib.Store satisfies it. Deliberately exposes
// NO credit/earn method (cannot mint by construction).
type distillAttributionSink interface {
	RecordDistillServe(ctx context.Context, owner, requester, contentHash string) error
	// RecordRoyaltyBasisFunded records the avoided-COGS basis PLUS the consumer's SETTLED charge for ONE
	// cross-tenant OCR reuse relationship, once (ON CONFLICT DO NOTHING). settledChargeUSD is the USD the
	// reuser ACTUALLY paid for the request (the clamped reservation settle) — the funding basis the async
	// DistillMinter mints s × of, skipping any row whose charge is ≤ 0. It is a descriptive money FIGURE,
	// NOT a credit/earn method — *distillattrib.Store satisfies it with a ledger-free INSERT, so this sink
	// still cannot mint by construction.
	RecordRoyaltyBasisFunded(ctx context.Context, owner, requester, contentHash string, avoidedCOGS, settledChargeUSD float64, visionModel string, inTokens, outTokens int) error
}

// recordDistillServes writes S1 attribution rows for the consented cross-tenant
// serves in `facts`. VOID by design — it cannot affect the response. Called
// POST-SERVE on a detached context (so client cancellation can't drop the write,
// and the write can't touch the serve). Errors are logged-and-swallowed.
//
// SUPPRESSED under LoggingNone: the row records a content hash, so it honors the
// same no-logging posture as the spend/capture writes. Empty-owner facts are
// skipped defensively (a pre-feature pooled entry with no owner stamp — though
// the consent gate already refuses to serve those cross-tenant).
// settledChargeUSD is what the consumer ACTUALLY paid for this reuse request (the clamped reservation
// settle from proxy.go — never the larger delivered cost). It is stamped onto the OCR basis so the async
// DistillMinter mints s × the CHARGE; 0 (reservations off / no settle) writes a row the sweeper skips —
// fail-closed, we never fund a royalty we cannot prove the consumer paid.
func (p *Proxy) recordDistillServes(ctx context.Context, requester string, loggingPolicy workspace.LoggingPolicy, facts []distillServeFact, settledChargeUSD float64) {
	if p == nil || p.distillAttribSink == nil || len(facts) == 0 {
		return
	}
	if loggingPolicy == workspace.LoggingNone {
		return // the row records a content hash; honor the no-logging posture
	}
	dctx, cancel := detachBounded(ctx) // B39.4: and bounded, so it cannot hang on a locked row
	defer cancel()
	for _, f := range facts {
		if f.owner == "" {
			continue // no owner stamp ⇒ nothing to attribute (defensive)
		}
		if err := p.distillAttribSink.RecordDistillServe(dctx, f.owner, requester, f.hash); err != nil {
			slog.Warn("distill attribution: write failed (observational; serve unaffected)",
				slog.String("owner", f.owner),
				slog.String("requester", requester),
				slog.String("err", err.Error()),
			)
		}
		// L2/S4 PR2: a cross-tenant OCR serve (visionModel != "") ALSO records its
		// avoided-COGS basis, once per relationship (ON CONFLICT DO NOTHING).
		// Conversion serves carry no basis. Still descriptive — the sink has no
		// credit/earn method, so this records a figure, never a mint.
		if f.visionModel != "" {
			if err := p.distillAttribSink.RecordRoyaltyBasisFunded(dctx, f.owner, requester, f.hash,
				f.avoidedCOGSUSD, settledChargeUSD, f.visionModel, f.visionInputTokens, f.visionOutputTokens); err != nil {
				slog.Warn("distill royalty basis: write failed (observational; serve unaffected)",
					slog.String("owner", f.owner),
					slog.String("requester", requester),
					slog.String("err", err.Error()),
				)
			}
		}
	}
}
