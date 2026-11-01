package catalog

import (
	"testing"
	"time"
)

// B23.7 — a model its provider retired leaves the chat picker (the suite offers
// `!deprecated && output_per_1m > 0`) and keeps its price, so a request already recorded under it
// still bills the same.
func TestCatalog_RetiredMistralModelsLeaveThePickerAndKeepTheirPrice(t *testing.T) {
	for id, want := range map[string][2]float64{"open-mistral-7b": {0.025, 0.025}, "mistral-nemo": {0.015, 0.045}} {
		m, ok := Get(id)
		if !ok {
			t.Fatalf("%s is gone from the catalog — a past request under it could no longer price", id)
		}
		if !m.Deprecated {
			t.Errorf("%s is not deprecated — the chat picker still offers a model Mistral retired", id)
		}
		if m.InputPer1M != want[0] || m.OutputPer1M != want[1] {
			t.Errorf("%s prices %v/%v, want %v/%v — no price changes here", id, m.InputPer1M, m.OutputPer1M, want[0], want[1])
		}
	}
}

// B38.8 — Anthropic retired Claude Opus 4.1 on 2026-08-05: out of the picker, its price kept for history, and
// it names the successor Anthropic documents, which must itself be an offered model.
func TestCatalog_RetiredOpus41LeavesThePickerKeepsItsPriceAndNamesOpus48(t *testing.T) {
	m, ok := Get("claude-opus-4-1-20250805")
	if !ok || m.ID != "claude-opus-4-1" {
		t.Fatalf("claude-opus-4-1-20250805 resolves to %q (ok=%v) — a past request under it could no longer price", m.ID, ok)
	}
	if !m.Deprecated || m.RetiredSuccessor != "claude-opus-4-8" {
		t.Errorf("claude-opus-4-1 deprecated=%v successor=%q, want retired in favour of claude-opus-4-8", m.Deprecated, m.RetiredSuccessor)
	}
	if m.InputPer1M != 15 || m.OutputPer1M != 75 || m.CachedInputPer1M != 1.5 || m.CacheWritePer1M != 18.75 {
		t.Errorf("claude-opus-4-1 prices changed: %+v", m)
	}
	if s, ok := Get(m.RetiredSuccessor); !ok || s.Deprecated || s.RetiredSuccessor != "" {
		t.Errorf("successor %s is not an offered model (ok=%v, %+v)", m.RetiredSuccessor, ok, s)
	}
}

// B17.277 — OpenAI retired GPT-4.1 nano (404 model_not_found): out of the picker, its price kept for history.
func TestCatalog_RetiredGPT41NanoLeavesThePickerAndKeepsItsPrice(t *testing.T) {
	m, ok := Get("gpt-4.1-nano")
	if !ok {
		t.Fatal("gpt-4.1-nano is gone from the catalog — a past request under it could no longer price")
	}
	if !m.Deprecated {
		t.Error("gpt-4.1-nano is not deprecated — the chat picker still offers a model OpenAI retired")
	}
	if m.InputPer1M != 0.10 || m.OutputPer1M != 0.40 || m.CachedInputPer1M != 0.025 {
		t.Errorf("gpt-4.1-nano prices changed: %+v", m)
	}
}

// B38.11 — Anthropic retires Claude Sonnet 4.5 at 2026-11-30T17:00Z: until then the listing offers it with
// "retiring on 30 Nov 2026" beside its name; from then it is deprecated (out of the picker) and retired in
// favour of claude-sonnet-5-5, an offered model. Its stored row never changes, so model watch reads it raw.
func TestCatalog_Sonnet45IsListedAsRetiringUntil30NovThenRetiredForSonnet55(t *testing.T) {
	listed := func(at time.Time) Model {
		t.Helper()
		defer SetClock(func() time.Time { return at })()
		for _, m := range Listing() {
			if m.ID == "claude-sonnet-4-5" {
				return m
			}
		}
		t.Fatalf("claude-sonnet-4-5 is not in the listing at %s", at)
		return Model{}
	}
	before := listed(time.Date(2026, 11, 30, 16, 59, 59, 0, time.UTC))
	if before.Deprecated || before.DisplayName != "Claude Sonnet 4.5 · retiring on 30 Nov 2026" {
		t.Errorf("before retirement: deprecated=%v name=%q, want offered as \"Claude Sonnet 4.5 · retiring on 30 Nov 2026\"", before.Deprecated, before.DisplayName)
	}
	after := listed(time.Date(2026, 11, 30, 17, 0, 0, 0, time.UTC))
	if !after.Deprecated || after.DisplayName != "Claude Sonnet 4.5" || after.RetiredSuccessor != "claude-sonnet-5-5" {
		t.Errorf("from retirement: deprecated=%v name=%q successor=%q, want out of the picker naming claude-sonnet-5-5", after.Deprecated, after.DisplayName, after.RetiredSuccessor)
	}
	if raw, _ := Get("claude-sonnet-4-5-20250929"); raw.Deprecated || raw.DisplayName != "Claude Sonnet 4.5" {
		t.Errorf("the stored row changed: %+v", raw)
	}
	if s, ok := Get("claude-sonnet-5-5"); !ok || s.Deprecated || s.RetiredSuccessor != "" {
		t.Errorf("successor claude-sonnet-5-5 is not an offered model (ok=%v, %+v)", ok, s)
	}
}
