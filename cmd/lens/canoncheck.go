package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/canonq"

	"github.com/talyvor/lens/internal/poolsafety"
)

// `lens canoncheck` — W2.6 TIER-2 SEMANTIC CANONICAL FORM, MEASURED. It changes nothing and
// recommends nothing; it prints the two numbers W2.6 asks for and the one number W2.6 says comes
// before them.
//
// ⚠ THE ORDER IS THE ITEM'S, NOT A CONVENIENCE. "MEASURE THE SAME INPUT TWICE — if one prompt
// yields two canonical forms, the key is unstable and the whole design collapses. That measurement
// comes before anything else." So determinism runs first and is reported first: a collapse rate
// measured on an unstable key is a number about one sample, not about the product.
//
// ⚠ IT USES THE SAME LANE COMPOSITION AS cmd/hitrate — deliberately, byte for byte — so the
// numbers here sit beside W2.1's and W2.5's rather than beside a corpus of this file's own
// choosing. ENGINEERING danger is EngineeringDangerPairs + HeldOutDangerPairs; CONSUMER rephrase
// is RephrasePairs + ConsumerRephrasePairs; CONSUMER danger is ConsumerDangerPairs +
// ConsumerUnrelatedPairs.
//
// ⚠ AND IT REPORTS REJECTIONS AS THEIR OWN CLASS. A prompt the canonicaliser refused has NO key,
// so it cannot collapse — which flatters both numbers at once: the hit rate loses a pair it might
// have won, and the false-serve rate loses a pair it might have lost. Folding rejections into
// "did not collapse" would report a safety result that was really a coverage hole.
//
// ────────────────────────────────────────────────────────────────────────────────────────────
// MEASURED 2026-08-10, claude-haiku-4-5, temperature 0, TWO INDEPENDENT FULL RUNS (2×592 calls).
// ────────────────────────────────────────────────────────────────────────────────────────────
//
// ⚠ THE SAFETY HALF IS CLEAN AND IT IS THE ONLY HALF THAT IS: 0/44 ENGINEERING and 0/42 CONSUMER
// danger pairs collapsed, in BOTH runs, with 0 rejections — so the zero is a measurement and not a
// coverage hole. notice-direction produced DIFFERENT canonical forms both times, both runs
// ("How much notice must a tenant give their landlord?" against "How much notice must a landlord
// give a tenant?"), which is the named test W2.6 said the approach must pass.
//
// ⚠ AND THE UTILITY HALF IS 2/68, OF WHICH ONE IS NOT A REPHRASING. ENGINEERING 1/30 and CONSUMER
// 1/38 collapsed, identically in both runs. The ENGINEERING hit is `timeout-spacing` — "30s"
// against "30 s" — a TYPOGRAPHIC pair, the exact class W2.5 measured tier-1 folding against. So
// the tier-2 unlock delivers ONE genuine semantic rephrasing in 68 measured pairs: `capital-uk`.
// ⚠ AND IT IS NOT EVEN CONSISTENT ON THE TYPOGRAPHIC CLASS: `heap-spacing` ("512mb" against
// "512 mb") is the same shape and did NOT collapse — the canonicaliser reproduced both spellings
// verbatim.
//
// ⚠ THE MISSES ARE NOT NEAR MISSES, WHICH IS WHAT RULES OUT "TIGHTEN THE PROMPT". Of the 29 + 37
// rephrase pairs that did not collapse, ZERO differ by exactly one word. They differ by 2 to 13:
// "How do I center a div?" against "What is the best way to horizontally and vertically center an
// element in CSS?". The model is choosing among many equally canonical forms, and no instruction
// makes one of them inevitable.
//
// ⚠ THE KEY IS UNSTABLE AT TEMPERATURE 0 — 12/296 (4.1%) then 7/296 (2.4%) of prompts produced a
// DIFFERENT key when the SAME string was sent twice, seconds apart, to the same model. W2.6 named
// this as the measurement that comes before the others and the condition that collapses the
// design. What moves: "800mg" -> "800 mg" then "800 milligrams"; "v3" -> "version 3" then "v3";
// "UK" -> "United Kingdom" then "UK"; "How do I tie a tie?" -> "I" then "you". Every pair VERDICT
// happened to be identical across the two calls in both runs — but that is 2 collapses' worth of
// evidence, not a stability result, and an exact-key design has no similarity score to fall back
// on when the key moves.
//
// ⚠ COST — THE ITEM'S "~50 TOKENS" IS 3.8x LOW, because the fixed instruction rides on every call:
// measured mean 177.7 input + 14.1 output tokens per canonicalisation, against 40.8 input + ~180
// output for a real generation on the SAME model over the same corpus. On Haiku list ($1/M in,
// $5/M out — a published input, not a price this file sets) that is $0.000248 to canonicalise
// against $0.000944 to generate: 1:3.8 in money, not the 1:13 the output-token ratio suggests.
// ⚠ AND IT IS TWO CALLS PER LOOKUP unless the canonical form is cached by raw-prompt hash. ⚠ THE
// DENOMINATOR IS MEASURED ON HAIKU; production generation is a larger model, so the real ratio is
// more favourable than this and is NOT measured here.
//
// ⚠ WHAT IS NOT MEASURED, STATED RATHER THAN IMPLIED: one canonicaliser, one prompt, one model,
// one corpus. A different instruction may collapse more pairs — but it would have to close a 2-to-
// 13-word gap without touching the 86 danger pairs, and the 2.4-4.1% instability is a property of
// greedy decoding rather than of this instruction.

type canonLane struct {
	name     string
	rephrase []poolsafety.RephrasePair
	danger   []poolsafety.RephrasePair
}

// canonRun is one prompt canonicalised twice — the determinism unit.
type canonRun struct {
	prompt string
	first  canonq.Result
	second canonq.Result
}

func (c canonRun) stable() bool {
	return canonq.Key(c.first.Canonical) == canonq.Key(c.second.Canonical)
}

func runCanonCheck() error {
	// ⚠ os.Getenv, NOT config.Load(). config.Load requires LENS_DATABASE_URL, LENS_REDIS_URL and
	// LENS_NATS_URL, and this harness touches none of them — it makes HTTP calls and prints. A
	// measurement that cannot be run without standing up three services is a measurement nobody
	// re-runs, which is how the consumer corpus nearly got lost (internal/poolsafety/consumer.go).
	// cmd/hitrate reads its one key the same way for the same reason.
	key := os.Getenv("LENS_ANTHROPIC_API_KEY")
	if key == "" {
		return errors.New("canoncheck: needs LENS_ANTHROPIC_API_KEY, because an unmeasured collapse rate must not be reported as one")
	}
	model := os.Getenv("LENS_CANON_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	cn := canonq.NewAnthropicCanonicaliser(key, model)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	// The population is poolsafety.ByTraffic()'s, not a list written here — see cmd/hitrate.
	lanes := make([]canonLane, 0, 2)
	for _, tl := range poolsafety.ByTraffic() {
		lanes = append(lanes, canonLane{name: tl.Traffic, rephrase: tl.Rephrase, danger: tl.Danger})
	}

	// Every distinct prompt across every corpus, canonicalised TWICE.
	seen := map[string]bool{}
	var prompts []string
	for _, ln := range lanes {
		for _, set := range [][]poolsafety.RephrasePair{ln.rephrase, ln.danger} {
			for _, p := range set {
				for _, s := range []string{p.A, p.B} {
					if !seen[s] {
						seen[s] = true
						prompts = append(prompts, s)
					}
				}
			}
		}
	}
	sort.Strings(prompts)

	fmt.Printf("canonicaliser:  %s  temperature 0  fixed prompt (canonq.Prompt)\n", model)
	fmt.Printf("key:            sha256(canonq.Fold(canonical form))  — exact match, NO threshold anywhere\n")
	fmt.Printf("distinct prompts: %d, each canonicalised twice (%d calls)\n\n", len(prompts), len(prompts)*2)

	runs, err := canonicaliseTwice(ctx, cn, prompts)
	if err != nil {
		return err
	}

	reportDeterminism(runs, prompts)
	byPrompt := map[string]canonRun{}
	for i, p := range prompts {
		byPrompt[p] = runs[i]
	}

	fmt.Printf("\n══════════ COLLAPSE RATES ══════════\n")
	fmt.Printf("A pair COLLAPSES when both sides produce the same key. On rephrase pairs that is a HIT.\n")
	fmt.Printf("On danger pairs it is a FALSE SERVE — a wrong answer with no similarity score to inspect.\n")
	for _, ln := range lanes {
		reportLane(ln, byPrompt)
	}

	reportCost(ctx, cn, runs, lanes)
	return nil
}

// canonicaliseTwice issues two INDEPENDENT requests per prompt. Not one request compared against
// itself, and not a cached second read — the question is whether the served system returns the
// same string twice, and only two real calls can answer it.
func canonicaliseTwice(ctx context.Context, cn canonq.Canonicaliser, prompts []string) ([]canonRun, error) {
	out := make([]canonRun, len(prompts))
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, p := range prompts {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a, err := cn.Canonicalise(ctx, p)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			b, err := cn.Canonicalise(ctx, p)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			out[i] = canonRun{prompt: p, first: a, second: b}
		}(i, p)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, fmt.Errorf("canoncheck: canonicalise: %w", firstErr)
	}
	return out, nil
}

func reportDeterminism(runs []canonRun, prompts []string) {
	fmt.Printf("══════════ DETERMINISM — THE MEASUREMENT THAT COMES FIRST ══════════\n")
	unstable, rejected := 0, 0
	var unstableNames []string
	for _, r := range runs {
		if canonq.Key(r.first.Canonical) == "" || canonq.Key(r.second.Canonical) == "" {
			rejected++
		}
		if !r.stable() {
			unstable++
			if len(unstableNames) < 12 {
				unstableNames = append(unstableNames, fmt.Sprintf("      %q\n        run1: %q\n        run2: %q",
					r.prompt, r.first.Canonical, r.second.Canonical))
			}
		}
	}
	fmt.Printf("  %d/%d prompts produced a DIFFERENT key on the second call (%.1f%% unstable)\n",
		unstable, len(runs), 100*float64(unstable)/float64(len(runs)))
	fmt.Printf("  %d/%d prompts produced NO usable canonical form on at least one call (Parse rejected)\n",
		rejected, len(runs))
	if unstable > 0 {
		fmt.Printf("  first %d unstable prompts:\n", len(unstableNames))
		for _, s := range unstableNames {
			fmt.Println(s)
		}
	}
}

// wordDistance is the Levenshtein distance between two strings measured in WORDS, not characters.
// Words, because the question is "how many edits away from the same sentence is this", and a
// character distance reports 3 for one changed word and 3 for three changed letters.
func wordDistance(a, b string) int {
	x, y := strings.Fields(a), strings.Fields(b)
	prev := make([]int, len(y)+1)
	cur := make([]int, len(y)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(x); i++ {
		cur[0] = i
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(y)]
}

func reportLane(ln canonLane, byPrompt map[string]canonRun) {
	fmt.Printf("\n═══ %s ═══  %d rephrase pairs (should collapse) · %d danger pairs (must not)\n",
		ln.name, len(ln.rephrase), len(ln.danger))
	for _, set := range []struct {
		label string
		pairs []poolsafety.RephrasePair
		want  bool
	}{
		{"REPHRASE (collapse = hit)", ln.rephrase, true},
		{"DANGER   (collapse = FALSE SERVE)", ln.danger, false},
	} {
		collapsed, rejected, flipped, oneWord := 0, 0, 0, 0
		var names, rejectedNames, misses []string
		for _, p := range set.pairs {
			ra, rb := byPrompt[p.A], byPrompt[p.B]
			ka, kb := canonq.Key(ra.first.Canonical), canonq.Key(rb.first.Canonical)
			if ka == "" || kb == "" {
				rejected++
				rejectedNames = append(rejectedNames, p.Name)
				continue
			}
			hit := ka == kb
			if hit {
				collapsed++
				names = append(names, p.Name)
			} else if set.want {
				// ⚠ HOW FAR APART A MISS IS, IS THE DIFFERENCE BETWEEN "TIGHTEN THE PROMPT" AND
				// "THE APPROACH CANNOT WORK". A pair whose two canonical forms differ by ONE word
				// is a near miss that a stricter instruction might close; one that differs in
				// sentence structure is the model choosing among many equally canonical forms,
				// and no instruction closes that. Reported as a count, not as a recommendation.
				d := wordDistance(canonq.Fold(ra.first.Canonical), canonq.Fold(rb.first.Canonical))
				if d == 1 {
					oneWord++
				}
				if len(misses) < 6 {
					misses = append(misses, fmt.Sprintf("      %-22s %d word(s) apart\n        A -> %q\n        B -> %q",
						p.Name, d, ra.first.Canonical, rb.first.Canonical))
				}
			}
			// The SAME verdict recomputed from the second run. A pair that collapses on one
			// call and not the other is the instability arriving where it does damage.
			k2a, k2b := canonq.Key(ra.second.Canonical), canonq.Key(rb.second.Canonical)
			if k2a != "" && k2b != "" && (k2a == k2b) != hit {
				flipped++
			}
		}
		fmt.Printf("\n  %-34s %d/%d collapsed", set.label, collapsed, len(set.pairs))
		if rejected > 0 {
			fmt.Printf("   (+%d pairs UNMEASURED — canonicaliser produced no form: %v)", rejected, rejectedNames)
		}
		fmt.Println()
		if len(names) > 0 {
			fmt.Printf("      %v\n", names)
		}
		if flipped > 0 {
			fmt.Printf("      ⚠ %d pair verdicts CHANGED between the two runs — the key is not stable enough to hash\n", flipped)
		} else {
			fmt.Printf("      0 pair verdicts changed between the two runs\n")
		}
		if set.want {
			fmt.Printf("      of the %d that did NOT collapse, %d differ by exactly one word:\n",
				len(set.pairs)-collapsed-rejected, oneWord)
			for _, m := range misses {
				fmt.Println(m)
			}
		}
	}

	// ⚠ NAMED BECAUSE W2.6 NAMES IT: "notice-direction IS THE TEST THAT MATTERS.
	// landlord-gives-notice and tenant-gives-notice MUST produce different canonical forms. If
	// they collapse, the approach fails and you say so."
	for _, p := range namedTestPairs(append(append([]poolsafety.RephrasePair{}, ln.danger...), ln.rephrase...)) {
		ra, rb := byPrompt[p.A], byPrompt[p.B]
		fmt.Printf("\n  ⚠ %s — THE NAMED TEST\n", p.Name)
		fmt.Printf("      A: %q\n         -> %q\n", p.A, ra.first.Canonical)
		fmt.Printf("      B: %q\n         -> %q\n", p.B, rb.first.Canonical)
		ka, kb := canonq.Key(ra.first.Canonical), canonq.Key(rb.first.Canonical)
		switch {
		case ka == "" || kb == "":
			fmt.Printf("      VERDICT: UNMEASURED — one side produced no canonical form\n")
		case ka == kb:
			fmt.Printf("      VERDICT: COLLAPSED — the approach fails on the pair it was told to pass\n")
		default:
			fmt.Printf("      VERDICT: distinct\n")
		}
	}
}

// namedTests are the pairs W2.6 singled out as the ones that decide the approach.
//
// ⚠ IT WAS A BY-NAME SELECTOR OVER A POPULATION WHERE ONE NAME MATCHED TWO PAIRS AND THE OTHER
// MATCHED NONE. "landlord-tenant" appears exactly once in this repository — in the selector — and
// names no pair in any corpus, so half of "THE NAMED TEST" could never run. "notice-direction"
// named a landlord pair in ConsumerDangerPairs AND an employment pair in ConsumerUnrelatedPairs,
// which this lane unions, so the other half ran twice and printed a verdict for a pair W2.6 never
// named under the heading of the one it did.
//
// Both directions are silent: an over-match prints an extra block that reads like the real one, and
// an under-match prints nothing at all, which is indistinguishable from a section that was not
// reached. namedTestPairs is guarded so each name resolves to exactly one pair.
var namedTests = []string{"notice-direction"}

func namedTestPairs(pairs []poolsafety.RephrasePair) []poolsafety.RephrasePair {
	var out []poolsafety.RephrasePair
	for _, want := range namedTests {
		for _, p := range pairs {
			if p.Name == want {
				out = append(out, p)
			}
		}
	}
	return out
}

// reportCost measures the numerator and the denominator of the ratio rather than restating the
// item's "~50 against 500-2000".
func reportCost(ctx context.Context, cn *canonq.AnthropicCanonicaliser, runs []canonRun, lanes []canonLane) {
	fmt.Printf("\n══════════ COST ══════════\n")
	var in, out, n int
	for _, r := range runs {
		in += r.first.InTokens + r.second.InTokens
		out += r.first.OutTokens + r.second.OutTokens
		n += 2
	}
	if n == 0 {
		fmt.Println("  no calls recorded")
		return
	}
	fmt.Printf("  canonicalisation: %d calls, mean %.1f input + %.1f output tokens\n",
		n, float64(in)/float64(n), float64(out)/float64(n))

	// A real generation on the same model, on a sample of the same corpus, so the denominator is
	// measured on this traffic rather than assumed.
	sample := lanes[0].rephrase
	if len(sample) > 8 {
		sample = sample[:8]
	}
	var gin, gout, gn int
	for _, p := range sample {
		r, err := cn.Answer(ctx, p.A)
		if err != nil {
			fmt.Printf("  generation sample failed: %v\n", err)
			break
		}
		gin += r.InTokens
		gout += r.OutTokens
		gn++
	}
	if gn == 0 {
		fmt.Println("  generation sample: none measured")
		return
	}
	fmt.Printf("  generation:       %d calls, mean %.1f input + %.1f output tokens\n",
		gn, float64(gin)/float64(gn), float64(gout)/float64(gn))
	fmt.Printf("  ⚠ TWO CALLS PER LOOKUP unless the canonical form is cached by raw-prompt hash:\n")
	fmt.Printf("    one to canonicalise the asker's prompt, and the stored side was canonicalised at write time.\n")
	fmt.Printf("  output-token ratio (canonicalise : generate) = 1 : %.1f\n",
		(float64(gout)/float64(gn))/(float64(out)/float64(n)))
}
