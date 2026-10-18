package distill

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/metrics"
)

// mockVision is an in-memory VisionDispatcher for testing the fallback decision,
// shaping, and honest cost accounting WITHOUT a live model call. It records what
// it was asked to OCR so tests can prove the document was shaped correctly.
type mockVision struct {
	called   int
	gotBytes []byte
	gotMedia string
	gotFmt   Format
	md       string
	inTok    int
	outTok   int
	model    string // vision model that served it (carried into Savings.VisionModel)
	err      error
	panicMsg string // if set, DispatchVision panics (simulates a buggy dispatcher)
	mutate   bool   // if set, DispatchVision mutates req.Bytes (must not affect caller)
}

func (m *mockVision) DispatchVision(_ context.Context, req VisionRequest) (VisionResult, error) {
	m.called++
	m.gotBytes = req.Bytes
	m.gotMedia = req.MediaType
	m.gotFmt = req.Format
	if m.panicMsg != "" {
		panic(m.panicMsg)
	}
	if m.mutate {
		for i := range req.Bytes {
			req.Bytes[i] ^= 0xFF // scribble over whatever buffer we were handed
		}
	}
	if m.err != nil {
		return VisionResult{}, m.err
	}
	return VisionResult{Markdown: m.md, InputTokens: m.inTok, OutputTokens: m.outTok, Model: m.model}, nil
}

// visionFallback must carry the OCR call's token SPLIT (in/out) and model into
// Savings so the request path can book a durable, model-priced 'vision_ocr'
// token_events row (PR #4) — not just the blended VisionTokensCost total.
func TestVision_SavingsCarriesCostSplitAndModel(t *testing.T) {
	mv := &mockVision{md: "# OCR\n\nrecovered", inTok: 1000, outTok: 25, model: "claude-haiku-4-5"}
	_, sav, err := DistillWithCache(context.Background(), nil, buildPDF(), WithVision(mv))
	if err != nil {
		t.Fatal(err)
	}
	if sav.VisionInputTokens != 1000 || sav.VisionOutputTokens != 25 {
		t.Errorf("Savings must carry the OCR token split; in=%d out=%d want 1000/25", sav.VisionInputTokens, sav.VisionOutputTokens)
	}
	if sav.VisionModel != "claude-haiku-4-5" {
		t.Errorf("Savings must carry the vision model for pricing; got %q", sav.VisionModel)
	}
	if sav.VisionInputTokens+sav.VisionOutputTokens != sav.VisionTokensCost {
		t.Errorf("the split must reconcile with VisionTokensCost: %d+%d != %d", sav.VisionInputTokens, sav.VisionOutputTokens, sav.VisionTokensCost)
	}
}

// A NeedsVision document + a configured dispatcher routes to the seam, gets OCR'd
// text back, and is marked vision_ocr — the core of stage 5.
func TestVision_RoutesNeedsVisionToDispatcher(t *testing.T) {
	ctx := context.Background()
	in := buildPDF() // text-less PDF → NeedsVision
	mv := &mockVision{md: "# Scanned Title\n\nRecovered body text.", inTok: 1000, outTok: 40}

	res, _, err := DistillWithCache(ctx, nil, in, WithVision(mv))
	if err != nil {
		t.Fatal(err)
	}
	if mv.called != 1 {
		t.Fatalf("dispatcher should be called exactly once for a NeedsVision doc; called=%d", mv.called)
	}
	if res.NeedsVision {
		t.Error("after a successful OCR, NeedsVision must be resolved to false")
	}
	if !strings.Contains(res.Markdown, "Recovered body text.") {
		t.Errorf("result markdown should be the OCR text; got %q", res.Markdown)
	}
	if res.Method != MethodVisionOCR || res.DistillMethod() != "vision_ocr" {
		t.Errorf("result must be marked vision_ocr; Method=%q DistillMethod=%q", res.Method, res.DistillMethod())
	}
	// Shaping: the dispatcher received the original PDF bytes + the right media type.
	if !bytes.Equal(mv.gotBytes, in) {
		t.Error("dispatcher must receive the original document bytes")
	}
	if mv.gotMedia != "application/pdf" || mv.gotFmt != FormatPDF {
		t.Errorf("shaping: media=%q fmt=%q, want application/pdf / pdf", mv.gotMedia, mv.gotFmt)
	}
}

// B39.10: a served request's warnings say what happened — read by a vision
// model and charged — never that the vision converter "is a later PR".
func TestVision_ServedScannedPDFWarningsSayWhatHappens(t *testing.T) {
	mv := &mockVision{md: "recovered", inTok: 10, outTok: 2}
	res, _, err := DistillWithCache(context.Background(), nil, buildPDF(), WithVision(mv))
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(res.Warnings, "\n")
	if strings.Contains(all, "later PR") {
		t.Errorf("served warnings must not call vision OCR a later PR; got %q", all)
	}
	if !strings.Contains(all, "a vision model reads it") || !strings.Contains(all, "text recovered via vision-OCR") {
		t.Errorf("served warnings must say a vision model read the document; got %q", all)
	}
}

// The honesty assertion: vision OCR is SPEND. Its cost is recorded as a COST
// (Savings.VisionTokensCost + the vision cost counter), tokens_saved is NOT
// positive, and the distill_tokens_saved counter does NOT move. This is the
// anti-"silent saving" guard made executable.
func TestVision_CostRecordedNotSaving(t *testing.T) {
	ctx := context.Background()
	in := buildPDF()
	mv := &mockVision{md: "# OCR\n\nsome recovered text here", inTok: 1000, outTok: 25}

	savedBefore := testutil.ToFloat64(metrics.DistillTokensSavedTotal)
	costBefore := testutil.ToFloat64(metrics.DistillVisionTokensCostTotal)
	okBefore := testutil.ToFloat64(metrics.DistillVisionFallbackTotal.WithLabelValues("ok"))

	_, sav, err := DistillWithCache(ctx, nil, in, WithVision(mv))
	if err != nil {
		t.Fatal(err)
	}

	if sav.TokensSaved > 0 {
		t.Errorf("vision OCR must NEVER report a positive saving; TokensSaved=%d", sav.TokensSaved)
	}
	if sav.VisionTokensCost != 1025 {
		t.Errorf("vision cost must be recorded (in+out tokens); VisionTokensCost=%d want 1025", sav.VisionTokensCost)
	}

	savedAfter := testutil.ToFloat64(metrics.DistillTokensSavedTotal)
	costAfter := testutil.ToFloat64(metrics.DistillVisionTokensCostTotal)
	okAfter := testutil.ToFloat64(metrics.DistillVisionFallbackTotal.WithLabelValues("ok"))

	if savedAfter != savedBefore {
		t.Errorf("vision OCR must NOT increment distill_tokens_saved_total: before=%v after=%v", savedBefore, savedAfter)
	}
	if costAfter-costBefore != 1025 {
		t.Errorf("vision OCR must increment the vision cost counter by 1025: delta=%v", costAfter-costBefore)
	}
	if okAfter-okBefore != 1 {
		t.Errorf("a successful fallback must increment distill_vision_fallback_total{ok}: delta=%v", okAfter-okBefore)
	}
}

// Without a dispatcher, a NeedsVision doc is unaffected — exactly today's
// behavior (stays NeedsVision, no OCR, no cost). No regression.
func TestVision_NoDispatcherUnaffected(t *testing.T) {
	res, sav, err := DistillWithCache(context.Background(), nil, buildPDF())
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedsVision || res.Markdown != "" {
		t.Errorf("no dispatcher: NeedsVision doc must stay NeedsVision with no markdown; needsVision=%v md=%q", res.NeedsVision, res.Markdown)
	}
	if res.Method != "" {
		t.Errorf("no dispatcher: Method must be unset; got %q", res.Method)
	}
	if sav.TokensSaved != 0 || sav.VisionTokensCost != 0 {
		t.Errorf("no dispatcher: no saving, no cost; saved=%d cost=%d", sav.TokensSaved, sav.VisionTokensCost)
	}
}

// A document that does NOT need vision is never sent to the dispatcher, even when
// one is configured — the fallback is strictly for NeedsVision results.
func TestVision_NonVisionDocNotDispatched(t *testing.T) {
	mv := &mockVision{md: "should not be used"}
	in := []byte("<html><body><h1>Hi</h1><p>there</p></body></html>")

	res, sav, err := DistillWithCache(context.Background(), nil, in, WithVision(mv))
	if err != nil {
		t.Fatal(err)
	}
	if mv.called != 0 {
		t.Errorf("a non-NeedsVision doc must NOT hit the dispatcher; called=%d", mv.called)
	}
	if !strings.Contains(res.Markdown, "# Hi") || res.Method != "" {
		t.Errorf("normal conversion must be unchanged; md=%q method=%q", res.Markdown, res.Method)
	}
	if sav.VisionTokensCost != 0 {
		t.Errorf("a non-vision doc carries no vision cost; got %d", sav.VisionTokensCost)
	}
}

// If the dispatcher errors (vision model down, etc.), the fallback degrades
// gracefully: the result stays NeedsVision, no fake cost, no Go error, and a
// warning records why. Best-effort, never crash the conversion.
func TestVision_DispatchErrorStaysNeedsVision(t *testing.T) {
	mv := &mockVision{err: errors.New("vision backend unavailable")}

	beforeErr := testutil.ToFloat64(metrics.DistillVisionFallbackTotal.WithLabelValues("error"))
	res, sav, err := DistillWithCache(context.Background(), nil, buildPDF(), WithVision(mv))
	if err != nil {
		t.Fatalf("a dispatch failure must not fail the conversion; err=%v", err)
	}
	if mv.called != 1 {
		t.Errorf("dispatcher should have been attempted once; called=%d", mv.called)
	}
	if !res.NeedsVision || res.Method == MethodVisionOCR {
		t.Errorf("on dispatch error the result must remain NeedsVision and NOT be marked vision_ocr; needsVision=%v method=%q", res.NeedsVision, res.Method)
	}
	if sav.VisionTokensCost != 0 {
		t.Errorf("a failed OCR must record zero cost (no fake spend); got %d", sav.VisionTokensCost)
	}
	if !hasWarningContaining(res.Warnings, "vision") {
		t.Errorf("a failed fallback should leave a warning explaining it; warnings=%v", res.Warnings)
	}
	afterErr := testutil.ToFloat64(metrics.DistillVisionFallbackTotal.WithLabelValues("error"))
	if afterErr-beforeErr != 1 {
		t.Errorf("a failed fallback must increment distill_vision_fallback_total{error}: delta=%v", afterErr-beforeErr)
	}
}

// A dispatcher that returns empty text is treated as "still no text layer":
// the result stays NeedsVision (not a fake empty success), zero cost.
func TestVision_EmptyOCRStaysNeedsVision(t *testing.T) {
	mv := &mockVision{md: "   \n  ", inTok: 500, outTok: 0}
	res, sav, err := DistillWithCache(context.Background(), nil, buildPDF(), WithVision(mv))
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedsVision || res.Method == MethodVisionOCR {
		t.Errorf("empty OCR must stay NeedsVision, not a vision_ocr success; needsVision=%v method=%q", res.NeedsVision, res.Method)
	}
	if sav.VisionTokensCost != 0 {
		t.Errorf("empty OCR records no usable output → zero cost; got %d", sav.VisionTokensCost)
	}
}

// A panicking dispatcher (a buggy stage-3 implementation) must NOT crash the
// caller — distill is fail-safe by construction. The panic degrades to the same
// graceful outcome as a dispatch error: NeedsVision stays, no cost, a warning.
func TestVision_DispatcherPanicStaysNeedsVision(t *testing.T) {
	mv := &mockVision{panicMsg: "boom: dispatcher blew up"}

	beforeErr := testutil.ToFloat64(metrics.DistillVisionFallbackTotal.WithLabelValues("error"))
	var (
		res Result
		sav Savings
		err error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a panicking dispatcher must not escape DistillWithCache; got panic %v", r)
			}
		}()
		res, sav, err = DistillWithCache(context.Background(), nil, buildPDF(), WithVision(mv))
	}()
	if err != nil {
		t.Fatalf("a dispatcher panic must not surface as a conversion error; err=%v", err)
	}
	if !res.NeedsVision || res.Method == MethodVisionOCR {
		t.Errorf("after a dispatcher panic the result must remain NeedsVision; needsVision=%v method=%q", res.NeedsVision, res.Method)
	}
	if sav.VisionTokensCost != 0 {
		t.Errorf("a panicked OCR records zero cost; got %d", sav.VisionTokensCost)
	}
	if !hasWarningContaining(res.Warnings, "vision") {
		t.Errorf("a panicked fallback should leave a warning; warnings=%v", res.Warnings)
	}
	afterErr := testutil.ToFloat64(metrics.DistillVisionFallbackTotal.WithLabelValues("error"))
	if afterErr-beforeErr != 1 {
		t.Errorf("a panicked fallback must increment distill_vision_fallback_total{error}: delta=%v", afterErr-beforeErr)
	}
}

// The dispatcher receives the document bytes to OCR, but must never be able to
// mutate the caller's input (which is also what gets cached/returned). The seam
// hands the dispatcher its own copy.
func TestVision_DispatcherCannotMutateCallerInput(t *testing.T) {
	in := buildPDF()
	orig := append([]byte(nil), in...) // snapshot of the caller's bytes
	mv := &mockVision{md: "# ok\n\nrecovered", inTok: 10, outTok: 5, mutate: true}

	_, _, err := DistillWithCache(context.Background(), nil, in, WithVision(mv))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, orig) {
		t.Error("a dispatcher must not be able to mutate the caller's input bytes (seam must clone)")
	}
}

func hasWarningContaining(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
