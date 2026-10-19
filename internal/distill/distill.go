// Package distill converts documents into clean, token-efficient Markdown
// before they reach a model — a token-reduction STAGE in the gateway, not a
// general-purpose file converter. It is Go-native (no Python / MarkItDown
// sidecar) so the whole request path stays reviewable in one language.
//
// This package is the CONVERSION CORE only: format detection + one converter
// per supported format behind a small Converter seam. Adding a format later is
// a new registration, not a rewrite (mirrors the provider seam in
// internal/proxy). The semantic cache, request-path integration, savings
// attribution, and tiers are separate, later concerns and are deliberately NOT
// in this package yet.
//
// # Security
//
// Converters parse UNTRUSTED document bytes, so they are fail-safe by
// construction:
//   - bounded: input is capped (MaxInputBytes) and converters are written to
//     run in time linear in the input — no pathological backtracking;
//   - panic-proof: Distill/DistillAs recover any panic from a converter and
//     return it as an error (a malformed document must never crash the host);
//   - inert: converters never touch the network or filesystem, never resolve
//     external entities, and never execute embedded content. Active content
//     (HTML <script>/<style>, event-handler attributes, etc.) is stripped
//     during conversion.
//
// Stripping active content also makes conversion an incidental
// prompt-injection-surface reducer (a design synergy with the guardrails
// engine, exploited in a later PR). The security bar for THIS package is just
// the list above: safe parsing, no SSRF/file access, no code execution,
// bounded resources.
package distill

import (
	"context"
	"errors"
	"fmt"
)

// Format is a document format the engine can detect and convert.
type Format string

const (
	FormatHTML    Format = "html"
	FormatDOCX    Format = "docx"
	FormatXLSX    Format = "xlsx"
	FormatPPTX    Format = "pptx" // B18.13: slide decks
	FormatCSV     Format = "csv"
	FormatJSON    Format = "json"
	FormatXML     Format = "xml"
	FormatText    Format = "text" // plaintext / markdown passthrough
	FormatPDF     Format = "pdf"  // registered but deferred (see pdf.go)
	FormatUnknown Format = "unknown"
)

// IsBinaryOrigin reports whether a format's raw bytes are a BINARY container
// (PDF/DOCX/XLSX) rather than text a model could have been sent directly. It
// gates the savings basis: for binary-origin inputs, len(bytes)/4 is a PHANTOM
// token baseline (the bytes were never text tokens), so the binary→text step is
// a SIZE reduction (bytes), and real token savings come only from the tier delta
// vs the faithful-text baseline. Text-ish formats keep the raw-text baseline.
func (f Format) IsBinaryOrigin() bool {
	switch f {
	case FormatPDF, FormatDOCX, FormatXLSX, FormatPPTX:
		return true
	default:
		return false
	}
}

// MaxInputBytes bounds a single conversion: 25 MiB, the size of a document the
// upload route accepts (B18.13 — a slide deck is mostly pictures, so 10 MiB
// refused ordinary decks), while putting a hard ceiling on the work any one
// untrusted input can cause. Inline documents stay bounded by the request body cap.
const MaxInputBytes = 25 << 20

// Result is the outcome of a conversion.
type Result struct {
	// Markdown is the clean, token-efficient rendering of the input.
	Markdown string
	// Format is the source format the input was converted from.
	Format Format
	// NeedsVision reports that the input carries no extractable text layer
	// (e.g. a scanned, image-only PDF) and should be routed to the vision
	// fallback. The PDF converter sets it when text extraction yields nothing
	// (or fails); on a served request visionFallback acts on it (vision.go).
	NeedsVision bool
	// Tier is the fidelity tier applied to produce this Markdown (faithful by
	// default). Recorded so an over-aggressive tier is traceable.
	Tier Tier
	// Method records HOW this Markdown was produced. Empty == standard text
	// conversion (the cheap path). MethodVisionOCR means the text was recovered
	// by a vision model from a text-less document — the EXPENSIVE path, whose
	// cost is reported in Savings.VisionTokensCost and must never be counted as
	// a saving. Stage 3 maps this to the token_events distill_method label.
	Method Method
	// Warnings holds non-fatal structural notes (e.g. a table with ragged
	// rows). Conversion still succeeds; these are advisory.
	Warnings []string
	// FaithfulTextTokens is the token count (len/4 basis) of the FAITHFUL-tier
	// Markdown, captured before any tier reduction. It is the honest text-token
	// baseline for binary-origin formats (whose raw bytes are not text tokens),
	// so computeSavings can measure their savings as the tier delta rather than a
	// phantom len(bytes)/4 figure. Set by applyTier; carried through the cache.
	FaithfulTextTokens int
}

// Method is how a Result's Markdown was produced (its provenance).
type Method string

// MethodVisionOCR marks Markdown recovered by the vision fallback (OCR of a
// text-less document) — the expensive path. The zero value ("") means standard
// text conversion.
const MethodVisionOCR Method = "vision_ocr"

// DistillMethod returns the canonical, bounded method label for cost
// attribution: "vision_ocr" for an OCR'd result, "convert" otherwise. Stage 3
// writes this to token_events.distill_method.
func (r Result) DistillMethod() string {
	if r.Method == MethodVisionOCR {
		return string(MethodVisionOCR)
	}
	return "convert"
}

// Option configures a conversion call. The zero set = faithful tier, so
// existing callers (no options) get today's behavior unchanged.
type Option func(*convOptions)

type convOptions struct {
	tier   Tier
	vision VisionDispatcher
}

// WithTier selects the conversion fidelity tier (default faithful).
func WithTier(t Tier) Option { return func(o *convOptions) { o.tier = t } }

// WithVision enables the vision-OCR fallback: when a conversion yields a
// text-less Result (NeedsVision), the document is routed to d for OCR instead
// of being returned empty. Honored by DistillWithCache (the measured path,
// where the OCR cost is attributed). Default nil = no fallback — today's
// behavior, the text-less document stays NeedsVision.
func WithVision(d VisionDispatcher) Option { return func(o *convOptions) { o.vision = d } }

func resolveOptions(opts []Option) convOptions {
	o := convOptions{tier: TierFaithful}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Converter turns one document format into Markdown. Implementations must be
// fail-safe: return an error on malformed/unconvertible input, never panic
// (the package-level recover is a backstop, not a license), and stay within
// the caller's size/time bounds — honoring ctx cancellation in long loops.
type Converter interface {
	// Format reports the format this converter handles.
	Format() Format
	// Convert parses input and returns clean Markdown.
	Convert(ctx context.Context, input []byte) (Result, error)
}

// Sentinel errors. Callers can errors.Is against these to branch (e.g. surface
// ErrTooLarge distinctly). A text-less PDF is NOT an error — it returns a
// successful Result with NeedsVision=true (see pdf.go).
var (
	ErrEmptyInput        = errors.New("distill: empty input")
	ErrTooLarge          = errors.New("distill: input exceeds size limit")
	ErrUnsupportedFormat = errors.New("distill: unsupported or undetected format")
	ErrConversionFailed  = errors.New("distill: conversion failed")
)

// registry maps a detected format to its converter. Populated by init via
// register(); adding a format is a single register() call (additive seam).
var registry = map[Format]Converter{}

func register(c Converter) { registry[c.Format()] = c }

func init() {
	register(textConverter{})
	register(htmlConverter{})
	register(csvConverter{})
	register(jsonConverter{})
	register(xmlConverter{})
	register(docxConverter{})
	register(xlsxConverter{})
	register(pptxConverter{})
	register(pdfConverter{})
}

// Distill detects the format of input and converts it to Markdown. When the
// gateway already knows the declared content type, prefer DistillAs to skip
// sniffing (content sniffing is best-effort; some formats — notably CSV vs
// plaintext — are ambiguous from bytes alone).
//
// WithTier applies here; WithVision does NOT — the vision-OCR fallback (with its
// cost accounting) is only run by DistillWithCache. A NeedsVision document
// distilled here stays NeedsVision.
func Distill(ctx context.Context, input []byte, opts ...Option) (Result, error) {
	return convert(ctx, input, nil, resolveOptions(opts))
}

// DistillAs converts input as the explicitly-supplied format (e.g. resolved
// from a request's Content-Type by the future request-path integration). As
// with Distill, WithVision is not honored here — use DistillWithCache for the
// vision-OCR fallback.
func DistillAs(ctx context.Context, input []byte, format Format, opts ...Option) (Result, error) {
	return convert(ctx, input, &format, resolveOptions(opts))
}

// convert enforces the size bound, picks the converter (detecting the format
// when none is supplied), and runs the whole thing under a panic recover. The
// recover deliberately spans DETECTION too: DetectFormat parses an untrusted
// ZIP central directory, which is itself an attacker-reachable parse — a panic
// there must never escape to the host.
func convert(ctx context.Context, input []byte, format *Format, o convOptions) (res Result, err error) {
	// Cheap, panic-free guards first.
	if len(input) == 0 {
		return Result{Format: derefFormat(format), Tier: normalizeTier(o.tier)}, ErrEmptyInput
	}
	if len(input) > MaxInputBytes {
		return Result{Format: derefFormat(format), Tier: normalizeTier(o.tier)}, fmt.Errorf("%w: %d bytes > %d", ErrTooLarge, len(input), MaxInputBytes)
	}

	f := derefFormat(format)
	defer func() {
		if r := recover(); r != nil {
			res = Result{Format: f, Tier: normalizeTier(o.tier)}
			err = fmt.Errorf("%w: recovered from panic during %s handling: %v", ErrConversionFailed, f, r)
		}
	}()

	if format == nil {
		f = DetectFormat(input) // inside recover: untrusted ZIP/XML sniffing
	}
	c, ok := registry[f]
	if !ok {
		return Result{Format: f, Tier: normalizeTier(o.tier)}, ErrUnsupportedFormat
	}
	res, err = c.Convert(ctx, input)
	if err != nil {
		res.Tier = normalizeTier(o.tier)
		return res, err
	}
	// Tier post-processing: faithful is identity (converter output unchanged),
	// so existing callers see no regression; structured/outline reduce it.
	return applyTier(res, o.tier), nil
}

func derefFormat(f *Format) Format {
	if f == nil {
		return FormatUnknown
	}
	return *f
}
