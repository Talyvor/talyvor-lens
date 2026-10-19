package distill

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// pdfConverter extracts a PDF's TEXT LAYER to Markdown using the pure-Go,
// zero-dep, BSD-3-Clause github.com/ledongthuc/pdf. PDF is the lossy format:
// it carries no reliable heading/paragraph structure (unless tagged, which is
// rare), so the output is honestly plain — the page text in reading order,
// not richly-structured Markdown like HTML/DOCX.
//
// SUPPLY-CHAIN POSTURE (accept + isolate + monitor): ledongthuc/pdf is
// UNMAINTAINED (single-owner personal repo, long stale, no security contact,
// untagged pseudo-version) and it parses ATTACKER-CONTROLLED input — the
// weakest link in the tree. We keep it deliberately: it is the best pure-Go
// text extractor, and Lens ships a CGO-free static binary, which rules out the
// better-maintained MuPDF/PDFium bindings; the one maintained pure-Go fork
// (dslipak/pdf) regresses text quality (drops the Td positioning operator, so
// Td-laid-out lines run together). The risk is CONTROLLED, not ignored:
//  1. blast radius — every parse runs in a killable subprocess with a memory
//     ceiling + wall-clock kill (cmd/distill-worker + isolator.go); a zlib
//     bomb or cyclic-graph hang kills the worker, never the gateway;
//  2. version — pinned in go.mod (see the require-line note);
//  3. monitoring — the govulncheck CI gate fails the build on any future CVE
//     in this dep, so "unmaintained" cannot silently become "vulnerable".
//
// TEXT-LESS handling is the load-bearing behavior: a scanned/image-only PDF
// has no text operators, so extraction yields empty text. Both empty output
// AND any extraction error/panic are treated as "no text layer" → the result
// sets NeedsVision=true (the core's reserved signal becomes real here) with a
// clear warning, NOT empty-looking success and NOT garbage. This converter
// never OCRs: on a served request the orchestrator's vision fallback
// (vision.go, dispatched live by internal/proxy/vision_dispatch.go) reads the
// document with a vision model and books that call as its own spend; the
// preview runs no vision model and returns NeedsVision as-is.
//
// RESIDUAL RISK (must be handled at the integration layer, like html.go's
// stack-overflow note): the EXTRACTED-TEXT size is bounded here, but
// FlateDecode decompression and the PDF object/page-tree traversal happen
// INSIDE ledongthuc, which has no size/cycle/depth/ctx hooks. So a crafted PDF
// can still (a) inflate a zlib bomb to gigabytes → OOM (fatal, escapes
// recover), or (b) hang/stack-overflow on a cyclic object graph (GetPlainText
// runs to completion with no ctx). The 10 MiB input cap bounds the COMPRESSED
// input but not the amplification. The honest fix is to run conversion in a
// memory-limited, KILLABLE worker (GOMEMLIMIT / cgroup + hard wall-clock) at
// the request-path integration (stage 3) — not achievable in this leaf
// package without forking the library.
type pdfConverter struct{}

func (pdfConverter) Format() Format { return FormatPDF }

const pdfNeedsVisionWarning = "pdf: no extractable text layer (scanned/image-only, encrypted, or unparseable PDF) — on a served request a vision model reads it, and that call is charged as its own spend; a preview runs no vision model"

func (pdfConverter) Convert(ctx context.Context, input []byte) (res Result, err error) {
	// Local recover: ledongthuc/pdf panics liberally on malformed input
	// (unknown filters, interpreter asserts, short AES blocks, …). A malformed
	// PDF must neither crash nor hard-error here — treat any parser failure the
	// same as "no text layer" and route to vision.
	//
	// NOTE: this recover only protects because extractPDFText is called on THIS
	// goroutine. If extraction is ever moved to a goroutine (e.g. to add a
	// killable wall-clock bound), that goroutine MUST install its own recover —
	// recover does not cross goroutines.
	defer func() {
		if r := recover(); r != nil {
			res = needsVision()
			err = nil
		}
	}()

	text, truncated, xerr := extractPDFText(input)
	// GetPlainText is one-shot (no mid-parse cancellation), so we can only
	// honor ctx coarsely: if the caller cancelled while it ran, report that
	// rather than the result. Total latency is bounded by the input-size cap.
	if ctx.Err() != nil {
		return Result{Format: FormatPDF}, ctx.Err()
	}
	if xerr != nil || strings.TrimSpace(text) == "" {
		return needsVision(), nil
	}
	res = Result{Markdown: normalizeText(strings.TrimSpace(text)), Format: FormatPDF}
	if truncated {
		res.Warnings = append(res.Warnings, "pdf: extracted text truncated at the 10 MiB cap")
	}
	return res, nil
}

func needsVision() Result {
	return Result{Format: FormatPDF, NeedsVision: true, Warnings: []string{pdfNeedsVisionWarning}}
}

// extractPDFText pulls the plain text layer. ledongthuc's GetPlainText applies
// the library's spacing/row-ordering heuristics (grouping by row internally),
// so the result already carries line structure with correct intra-line
// spacing — better than naively concatenating Text fragments (which can split
// words). The output is size-bounded (read one byte past the cap to detect
// over-length and report truncation rather than silently dropping text).
func extractPDFText(input []byte) (text string, truncated bool, err error) {
	r, err := pdf.NewReader(bytes.NewReader(input), int64(len(input)))
	if err != nil {
		return "", false, err
	}
	plain, err := r.GetPlainText()
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	n, err := io.Copy(&b, io.LimitReader(plain, MaxInputBytes+1))
	if err != nil {
		return "", false, err
	}
	if n > MaxInputBytes {
		return b.String()[:MaxInputBytes], true, nil
	}
	return b.String(), false, nil
}
