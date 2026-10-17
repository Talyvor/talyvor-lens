package proxy

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/workspace"
)

// stream_unpriced_cost_test.go — the streamed settle and the buffered settle
// pricing the same request differently, and what that costs the budget.
//
// ⚠ THE CLAIM UNDER TEST IS THE CODE'S OWN. recordStreamSpend says of its cost
// basis: "Mirrors the buffered path exactly." The buffered path prices through
// alerts.CostUSDResolved on BOTH branches, and says why in as many words —
// "Priced through the resolver so an unknown model still cannot come out at
// zero" (proxy.go, the costEstimated block). The streamed path used
// alerts.CostUSDDetailed / alerts.CostUSD, and those return EXACTLY ZERO for a
// model the catalog does not price.
//
// ⚠ AND THE POLICY FOR AN UNPRICED MODEL IS ALREADY WRITTEN DOWN, in
// alerts.WarnUnpricedModel's own message: a charge falls back "to its cheapest (a
// floor, never an over-bill)" and "until then this workspace is under-billed".
// So zero was never the intended answer; the resolver's floor was. Aligning the
// streamed path is implementing that, not choosing it.
//
// The catalog holds 45 models. `gpt-4`, `gpt-4-turbo` and `claude-3-opus` are not
// among them — measured, not assumed, by the first test below, which refuses to
// run if the model it uses has since been priced.

// unpricedProbeModel is a model this test needs to be ABSENT from the catalog.
// Using a real, expensive, widely-used name rather than an invented one, because
// the finding is that real names are missing.
const unpricedProbeModel = "gpt-4"

// pricedProbeModel is present, and is the control: whatever is asserted about an
// unpriced model must NOT hold for a priced one, or the test is measuring the
// plumbing rather than the pricing.
const pricedProbeModel = "gpt-4o"

type recordingBudget struct {
	spent    float64
	calls    int
	decision budgets.Decision
}

func (r *recordingBudget) CheckBudget(_ context.Context, _, _, _ string, _ float64) budgets.Decision {
	return r.decision
}

func (r *recordingBudget) RecordSpend(_ context.Context, _, _, _ string, cost float64) {
	r.spent += cost
	r.calls++
}

type nopAlertSink struct{}

func (nopAlertSink) IsCircuitOpen(string, string, string) bool { return false }
func (nopAlertSink) GetDowngradeModel(string, string) string   { return "" }
func (nopAlertSink) RecordSpend(context.Context, string, string, string, string, string, int, int, string, string, string, string, bool) error {
	return nil
}
func (nopAlertSink) RecordSpendWithDistill(context.Context, string, string, string, string, string, int, int, string, string, string, string, bool, string) error {
	return nil
}
func (nopAlertSink) RecordSpendWithTare(context.Context, string, string, string, string, string, int, int, string, string, string, string, bool, string, alerts.TareMeter) error {
	return nil
}
func (nopAlertSink) RecordCacheServe(context.Context, string, string, string, string, string, int, int, string, string, string, string) error {
	return nil
}
func (nopAlertSink) RecordNodeServe(context.Context, string, string, string, string, string, int, int, string, string, string, float64) error {
	return nil
}

func streamSpendProxy(b *recordingBudget) *Proxy {
	return &Proxy{alertManager: nopAlertSink{}, budgetService: b}
}

// TestCatalogStillDoesNotPriceTheProbeModel — the premise, asserted rather than
// assumed. If somebody adds gpt-4 to the catalog, everything below stops
// measuring what it claims to, and this says so instead of going quietly green.
func TestCatalogStillDoesNotPriceTheProbeModel(t *testing.T) {
	if _, _, ok := catalog.Price(unpricedProbeModel); ok {
		t.Fatalf("%s is now IN the catalog. Good — but every test in this file was written "+
			"around it being absent. Pick another absent model or delete the file with the "+
			"reason.", unpricedProbeModel)
	}
	if _, _, ok := catalog.Price(pricedProbeModel); !ok {
		t.Fatalf("%s is NOT in the catalog, so the priced control below is not a control",
			pricedProbeModel)
	}
	// And the zero is a property of the plain helpers, not of small token counts.
	if c := alerts.CostUSD(unpricedProbeModel, 100_000, 100_000); c != 0 {
		t.Fatalf("alerts.CostUSD(%s, 100k, 100k) = %v, want 0 — the premise has changed",
			unpricedProbeModel, c)
	}
}

// ── the finding ──

func TestStreamSettle_UnpricedModelStillReachesTheBudget(t *testing.T) {
	b := &recordingBudget{}
	p := streamSpendProxy(b)

	p.recordStreamSpend(context.Background(), streamSpend{
		wsID: "ws-1", model: unpricedProbeModel, logging: workspace.LoggingFull,
		estInputTokens: 50_000,
	}, streamUsage{}, string(make([]byte, 200_000)))

	// NON-VACUITY: the settle ran and fed the budget at all.
	if b.calls != 1 {
		t.Fatalf("budget was fed %d times, want 1 — the settle did not run, so the amount "+
			"below is not what this test measures", b.calls)
	}
	if b.spent <= 0 {
		t.Errorf("a streamed request on %q — 50k input tokens, 50k output — recorded %v into the "+
			"budget.\n"+
			"    recordStreamSpend says of this number: \"Mirrors the buffered path exactly.\" It "+
			"does not: the buffered path prices through alerts.CostUSDResolved, whose own comment "+
			"is \"so an unknown model still cannot come out at zero\", while this path used "+
			"alerts.CostUSD, which returns exactly zero for a model the catalog does not hold.\n"+
			"    A hard_block budget cannot be reached by streamed traffic it books at zero.",
			unpricedProbeModel, b.spent)
	}
}

func TestStreamSettle_UnpricedModelWithProviderUsageStillReachesTheBudget(t *testing.T) {
	// The OTHER branch. u.present takes CostUSDDetailed, which is zero for an
	// unpriced model too — so fixing only the estimate branch would leave the
	// provider-usage branch booking zero.
	b := &recordingBudget{}
	p := streamSpendProxy(b)

	p.recordStreamSpend(context.Background(), streamSpend{
		wsID: "ws-1", model: unpricedProbeModel, logging: workspace.LoggingFull,
	}, streamUsage{
		present: true, inputTokens: 50_000, outputTokens: 50_000,
		uncachedInputTokens: 50_000,
	}, "")

	if b.calls != 1 {
		t.Fatalf("budget was fed %d times, want 1", b.calls)
	}
	if b.spent <= 0 {
		t.Errorf("the provider-usage branch recorded %v for %q — CostUSDDetailed returns zero for "+
			"a model the catalog does not price, exactly like the estimate branch",
			b.spent, unpricedProbeModel)
	}
}

// ── the control: a PRICED model must be unaffected ──

func TestStreamSettle_PricedModelIsUnchanged(t *testing.T) {
	b := &recordingBudget{}
	p := streamSpendProxy(b)

	p.recordStreamSpend(context.Background(), streamSpend{
		wsID: "ws-1", model: pricedProbeModel, logging: workspace.LoggingFull,
		estInputTokens: 1000,
	}, streamUsage{}, string(make([]byte, 4000)))

	want := alerts.CostUSD(pricedProbeModel, 1000, 1000)
	if want <= 0 {
		t.Fatalf("the priced control model prices at %v — it is not a control", want)
	}
	if b.spent != want {
		t.Errorf("a PRICED model settled at %v, want %v (its exact catalog price). Aligning the "+
			"unpriced case must not move a model the catalog actually holds.", b.spent, want)
	}
}

// ── the two paths must agree, which is the claim the comment makes ──

func TestStreamedAndBufferedPriceTheSameRequestTheSameWay(t *testing.T) {
	const inT, outT = 50_000, 50_000
	for _, model := range []string{unpricedProbeModel, pricedProbeModel} {
		buffered, _ := alerts.CostUSDResolved(model, catalog.PurposeCharge, inT, 0, 0, outT)

		b := &recordingBudget{}
		p := streamSpendProxy(b)
		p.recordStreamSpend(context.Background(), streamSpend{
			wsID: "ws-1", model: model, logging: workspace.LoggingFull,
			estInputTokens: inT,
		}, streamUsage{}, string(make([]byte, outT*4)))

		if b.calls != 1 {
			t.Fatalf("%s: budget fed %d times, want 1", model, b.calls)
		}
		if b.spent != buffered {
			t.Errorf("%s: streamed settle = %v, buffered settle = %v. recordStreamSpend claims to "+
				"mirror the buffered path exactly; the same request must not cost two different "+
				"amounts depending on whether the client asked for a stream.",
				model, b.spent, buffered)
		}
	}
}

// ⚠ THE "DID I MOVE ANYTHING I SHOULD NOT" GUARD, on a money path. Swapping
// CostUSDDetailed for CostUSDResolved on the provider-usage branch must be
// byte-identical for a PRICED model WITH a cache breakdown — the case that branch
// exists for. If it is not, the change stopped being surgical and started being a
// repricing.
func TestStreamSettle_PricedModelWithCacheBreakdownIsByteIdentical(t *testing.T) {
	u := streamUsage{
		present:               true,
		inputTokens:           10_000,
		outputTokens:          2_000,
		uncachedInputTokens:   6_000,
		cachedInputTokens:     3_000,
		cacheWriteInputTokens: 1_000,
	}
	want := alerts.CostUSDDetailed(pricedProbeModel,
		u.uncachedInputTokens, u.cachedInputTokens, u.cacheWriteInputTokens, u.outputTokens)
	if want <= 0 {
		t.Fatalf("the cache-aware control priced at %v — it is not a control", want)
	}

	b := &recordingBudget{}
	p := streamSpendProxy(b)
	p.recordStreamSpend(context.Background(), streamSpend{
		wsID: "ws-1", model: pricedProbeModel, logging: workspace.LoggingFull,
	}, u, "")

	if b.spent != want {
		t.Errorf("cache-aware settle for a PRICED model = %v, want %v (exactly what "+
			"CostUSDDetailed produced before). The swap must not move a model the catalog holds.",
			b.spent, want)
	}
}
