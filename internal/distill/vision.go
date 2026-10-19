package distill

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/talyvor/lens/internal/metrics"
)

// vision.go is the vision-OCR fallback for text-less documents (scanned,
// image-only, or encrypted PDFs that set NeedsVision). It is the EXPENSIVE path
// — the honest opposite of distillation's savings: OCR'ing a document with a
// vision model SPENDS tokens, it does not save them. This file holds the
// decision, the request shaping, the honest cost accounting, and the dispatch
// SEAM. The LIVE dispatch (building provider content blocks, calling a
// vision-capable model over HTTP) is internal/proxy/vision_dispatch.go, and the
// proxy books the cost as a 'vision_ocr' spend row — here we depend only on the
// VisionDispatcher interface, exercised in tests by a mock.

// VisionDispatcher sends a text-less document to a vision-capable model and
// returns the OCR'd text plus the call's token cost. The implementation lives
// in the request path (it needs the provider HTTP client + model selection);
// internal/distill only DECIDES to call it, SHAPES the request, and accounts
// for the cost honestly. A nil dispatcher disables the fallback.
//
// The dispatcher owns everything VisionRequest deliberately does NOT carry:
// model/provider selection (e.g. via modality.CapableModel), workspace
// allow-lists and policy, and ctx-cancellation handling. It is expected to read
// those from ctx values or its own config. The seam stays request-path-agnostic
// on purpose. Implementations SHOULD NOT panic (distill recovers it as an error
// regardless) and MUST treat req.Bytes as read-only.
type VisionDispatcher interface {
	DispatchVision(ctx context.Context, req VisionRequest) (VisionResult, error)
}

// ModelPlanner is an OPTIONAL capability a VisionDispatcher may implement: it
// reports, WITHOUT dispatching, the vision model this request WOULD be OCR'd by
// (the SAME selection DispatchVision uses). The orchestrator needs it to build
// the OCR-cache key before dispatch — the model is a determining input (a
// different model produces different text for the same bytes), and is otherwise
// only known AFTER the call. A dispatcher that does not implement this, or returns
// ok=false (unsupported provider / no capable model), makes the orchestrator SKIP
// the OCR cache and dispatch as normal: fail-safe by construction — no planned
// model ⇒ no cache ⇒ never a wrong-model serve.
type ModelPlanner interface {
	PlannedVisionModel(ctx context.Context) (model string, ok bool)
}

// VisionRequest is the provider-agnostic description of "OCR this document".
// The dispatcher decides how to present Bytes to the model (render to images,
// send as a document block, etc.) — that provider-specific shaping is its job.
type VisionRequest struct {
	// Bytes is the original document (e.g. the scanned PDF) to be OCR'd.
	Bytes []byte
	// MediaType is the document's MIME type (e.g. "application/pdf").
	MediaType string
	// Format is the distill format the document was detected/declared as.
	Format Format
	// Prompt is the OCR instruction for the vision model.
	Prompt string
}

// VisionResult is what the dispatcher returns: the recovered Markdown and the
// token cost the vision call incurred (reported by the provider, or estimated
// by the dispatcher). InputTokens covers the image/document input; OutputTokens
// covers the generated text.
type VisionResult struct {
	Markdown     string
	InputTokens  int
	OutputTokens int
	// Model is the vision model that served the OCR. A dispatcher MUST set it on
	// a successful result: the request path prices the durable 'vision_ocr' spend
	// row on it, so an empty/unknown model yields an UNPRICED (cost_usd=0) row —
	// the OCR cost would then not count against budgets. The proxy logs a warning
	// if it is missing so the gap is observable rather than silent.
	Model string
}

// DefaultVisionPrompt instructs the vision model to transcribe a document into
// clean Markdown, preserving structure and inventing nothing.
const DefaultVisionPrompt = "Transcribe this document into clean Markdown. Preserve headings, tables, and lists. Output only the document's text — do not summarize, comment, or add anything that is not in the document."

const (
	visionOCRWarning    = "distill: text recovered via vision-OCR fallback (the expensive path — this SPENT tokens, it did not save them)"
	visionFailedWarning = "distill: vision-OCR fallback did not recover text; document remains NeedsVision"
)

// mediaTypeFor maps a distill Format to the MIME type the dispatcher should
// label the bytes with. Only PDF produces NeedsVision today; the default keeps
// the seam honest for any future text-less binary format.
func mediaTypeFor(f Format) string {
	switch f {
	case FormatPDF:
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

// dispatchSafely calls the dispatcher under a panic-recover so a buggy
// VisionDispatcher (a stage-3 implementation that panics) degrades to a normal
// error instead of crashing the caller — upholding distill's fail-safe-by-
// construction contract, the same way convert() recovers a panicking converter.
func dispatchSafely(ctx context.Context, d VisionDispatcher, req VisionRequest) (vr VisionResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			vr = VisionResult{}
			err = fmt.Errorf("vision dispatcher panicked: %v", r)
		}
	}()
	return d.DispatchVision(ctx, req)
}

// visionFallback runs the OCR fallback on a NeedsVision result. On success it
// returns an honest Result (Markdown = OCR text, Method = vision_ocr, NeedsVision
// resolved) and a Savings whose VisionTokensCost carries the OCR spend while
// TokensSaved stays 0 — the cost is NEVER presented as a saving.
//
// On failure (dispatcher error, panic, or empty output) it degrades gracefully:
// the result stays NeedsVision, no fake cost, NO Go error — a vision outage must
// not fail the conversion. The ONLY signal of an attempted-but-failed OCR is a
// warning appended to res.Warnings (NeedsVision alone can't distinguish "tried
// and failed" from "no dispatcher configured"). Stage 3 should log/alert on
// these warnings.
func visionFallback(ctx context.Context, input []byte, res Result, d VisionDispatcher) (Result, Savings) {
	req := VisionRequest{
		// Clone: the dispatcher gets its OWN copy so it can never mutate the
		// caller's input (which is also what was cached / is returned). Cheap on
		// this already-expensive, rarely-taken path.
		Bytes:     bytes.Clone(input),
		MediaType: mediaTypeFor(res.Format),
		Format:    res.Format,
		Prompt:    DefaultVisionPrompt,
	}

	vr, err := dispatchSafely(ctx, d, req)
	if err != nil || strings.TrimSpace(vr.Markdown) == "" {
		// No usable text recovered — stay honest: still NeedsVision, zero cost.
		reason := "empty"
		warn := visionFailedWarning
		if err != nil {
			reason = "error"
			warn = fmt.Sprintf("%s (%v)", visionFailedWarning, err)
		}
		metrics.DistillVisionFallback(reason)
		res.Warnings = append(res.Warnings, warn)
		return res, Savings{InputTokensRaw: len(input) / 4, InputBytes: len(input)}
	}

	cost := vr.InputTokens + vr.OutputTokens
	metrics.DistillVisionFallback("ok")
	metrics.DistillVisionTokensCost(cost)

	res.Markdown = vr.Markdown
	res.NeedsVision = false
	res.Method = MethodVisionOCR
	res.Warnings = append(res.Warnings, visionOCRWarning)

	sav := Savings{
		InputTokensRaw:       len(input) / 4,
		InputTokensDistilled: estTokens(vr.Markdown),
		TokensSaved:          0, // EXPENSIVE path: vision OCR never saves tokens.
		VisionTokensCost:     cost,
		VisionInputTokens:    vr.InputTokens,  // the split, for a durable model-priced 'vision_ocr' spend row
		VisionOutputTokens:   vr.OutputTokens, //
		VisionModel:          vr.Model,        // which vision model to price the cost against
		InputBytes:           len(input),
		OutputBytes:          len(vr.Markdown),
		CacheHit:             false,
	}
	return res, sav
}
