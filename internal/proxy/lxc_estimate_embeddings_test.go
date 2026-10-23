package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
)

// ⚠ THE FIFTH FREE PATH — AND IT IS NOT MEASURED IN PROMPT BYTES.
//
// lxc_estimate_short_prompt_test.go pinned the FOURTH: `len(prompt)/4` floors, so a 1–3 byte prompt
// is zero tokens and takes both callers' `<= 0` free branch. That file's own framing is "shorter than
// four bytes", and it left one question open in the queue, verbatim:
//
//	"AND THE HOLD PATH WAS NOT MEASURED HERE — SOMEBODY SHOULD. reserveEstimateLXC shares the same
//	 len(prompt)/4 floor but adds maxOutTokens, so heldLXC is probably > 0 for a short prompt whenever
//	 the caller-bounded output allowance is non-zero; agentReserveBlocks' own `heldLXC <= 0 → no hold`
//	 branch is therefore reachable only if maxOut can be 0, which I did NOT verify through the wire."
//
// ⚠ THIS FILE ANSWERS IT, AND THE ANSWER IS NOT THE ONE THE QUESTION EXPECTED. `maxOut` can NOT be 0:
// boundedMaxOut returns the explicit max_tokens, else the configured cap, else a hardcoded 4096, and
// every one of those branches is > 0 (pinned below). The `heldLXC <= 0` branch is reachable anyway,
// through TWO doors neither the fourth free path nor its question looked at:
//
//	DOOR 1 — THE ESTIMATOR IS BLIND, NOT LENIENT. extractPrompt reads `messages[]`. An OpenAI
//	         EMBEDDINGS body carries `input`. Every embeddings request therefore yields the EMPTY
//	         prompt — at 12 bytes and at 40 KB alike — so the free path is not "under four bytes",
//	         it is AN ENTIRE ENDPOINT, at any size. /v1/proxy/openai/* is a wildcard route onto
//	         HandleOpenAI (registered in cmd/lens/main.go — no line cited, it would decay), and
//	         embeddings_route_test.go proves that route really forwards and is really served.
//
//	DOOR 2 — THE HOLD'S OUTPUT ALLOWANCE PRICES AT ZERO. The conservative hold exists precisely to
//	         close the input-only leak by adding maxOut × the output rate. For the three SEEDED
//	         embedding models that rate is 0 — correctly, embeddings emit no output tokens — so the
//	         term that was supposed to make the hold a true upper bound contributes exactly nothing,
//	         and the hold collapses onto the same blind zero.
//
// The two zeros are INDEPENDENT and they stack: door 1 alone frees the two input-only seams
// (agentAllocationBlocks, lxcGateBlocks) on ANY model; door 2 is what additionally frees the HOLD,
// which is the seam documented as the airtight one.
//
// ⚠ THE EMPTY PROMPT WAS ALREADY KNOWN — TO THE CACHE, AND ONLY TO THE CACHE.
// embeddings_route_test.go's third test pins the very same fact and says of it: "EVERY EXTRACTOR
// STILL SEES AN EMPTY PROMPT — that is not fixed here, and THE CACHE GUARD is what makes it safe."
// The cache guard does make the CACHE safe. Nothing was ever asked about the three money seams that
// read the same extractor's output, and that is the whole defect: a fact was measured, pinned, and
// its blast radius scoped to one consumer.
//
// ⚠ NOT FIXED, DELIBERATELY, AND FOR A REASON THAT IS WRITTEN DOWN NEXT DOOR. Teaching extractPrompt
// to read `input` is the obvious repair and it is FORBIDDEN here: embeddings_route_test.go states
// that if extractPrompt ever derives a prompt from an embeddings body, "the CROSS-TENANT POOLING
// question must be decided first: these are source-code embeddings". Pricing embeddings at the money
// seams instead would start REFUSING requests that are served today, on a ceiling nobody set with
// embeddings in mind. Both are product decisions. This file pins the measured boundary from both
// sides so the path cannot widen silently and whoever decides has to edit a test that says what they
// are changing.

// embeddingsModels are the seeded models whose OUTPUT rate is legitimately zero — door 2.
var embeddingsModels = []string{"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002"}

// realEmbeddingsBody is the body talyvor-code v0.1.0 actually sends when it indexes a codebase
// (embeddings_route_test.go documents that client), scaled to a size nobody would call negligible.
func realEmbeddingsBody(t *testing.T, model string, inputBytes int) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model,
		"input": strings.Repeat("func main() { fmt.Println(\"hello\") }\n", inputBytes/37+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < inputBytes {
		t.Fatalf("fixture too small: %d bytes, want >= %d", len(body), inputBytes)
	}
	return body
}

// promptAsTheMoneySeamsSeeIt runs the REAL extractor over the REAL body. It deliberately does not
// hardcode "" — if extractPrompt is ever taught to read `input`, every assertion below must red.
func promptAsTheMoneySeamsSeeIt(t *testing.T, body []byte) (model, prompt string) {
	t.Helper()
	model, prompt, err := extractPrompt(body)
	if err != nil {
		t.Fatalf("extractPrompt: %v", err)
	}
	return model, prompt
}

// DOOR 1, and the size that makes it a class rather than an edge: 40 KB of source code is worth
// zero input tokens to all three estimators, because none of them can see it.
func TestLXCEstimate_AnEmbeddingsRequestIsFreeAtAnySize(t *testing.T) {
	const inputBytes = 40 * 1024

	for _, model := range append([]string{"gpt-4o"}, embeddingsModels...) {
		t.Run(model, func(t *testing.T) {
			body := realEmbeddingsBody(t, model, inputBytes)
			gotModel, prompt := promptAsTheMoneySeamsSeeIt(t, body)
			if gotModel != model {
				t.Fatalf("extractPrompt read model %q, want %q — the fixture is not the shape under test", gotModel, model)
			}
			if prompt != "" {
				t.Fatalf("MEASURED BEHAVIOUR CHANGED: extractPrompt now derives %d bytes from a %d-byte "+
					"embeddings body. That is very likely the intended repair — but embeddings_route_test.go "+
					"requires the CROSS-TENANT POOLING decision first, so update both files deliberately.",
					len(prompt), len(body))
			}
			if got := lxcEstimate(model, prompt); got != 0 {
				t.Errorf("lxcEstimate(%q, <embeddings prompt>) = %d, want 0 — this is the measured free "+
					"path; a non-zero here means the estimate stopped depending on extractPrompt", model, got)
			}

			// ⚠ POSITIVE CONTROL, AND IT IS THE POINT OF THE WHOLE FILE. The SAME bytes, handed to the
			// SAME estimator as a prompt, price at a clean positive number. So the zero above is
			// BLINDNESS, not a free rate — an assertion that only showed "it is 0" would pass equally
			// against a model priced at zero, an unwired catalog, or an estimator that always returns 0.
			var req struct {
				Input string `json:"input"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatal(err)
			}
			if got := lxcEstimate(model, req.Input); got <= 0 {
				t.Fatalf("CONTROL FAILED: the same %d bytes priced as a prompt = %d µLXC, want > 0. "+
					"The zero above would then be a pricing fact, not the extraction hole this file "+
					"documents, and every conclusion here is void.", len(req.Input), got)
			}
		})
	}
}

// DOOR 2 — THE HANDED-ON QUESTION, ANSWERED. maxOut is never 0; the hold collapses anyway.
func TestReserveEstimateLXC_TheHoldCollapsesForAnEmbeddingModelWhateverMaxOutIs(t *testing.T) {
	body := realEmbeddingsBody(t, embeddingsModels[0], 40*1024)
	_, prompt := promptAsTheMoneySeamsSeeIt(t, body)

	// The question asked whether maxOut can be 0. It cannot — every branch of boundedMaxOut is > 0.
	for _, c := range []struct {
		name     string
		explicit int
		cap      func() int
	}{
		{"explicit max_tokens", 512, func() int { return 4096 }},
		{"configured cap", 0, func() int { return 4096 }},
		{"cap unwired ⇒ hardcoded 4096", 0, nil},
		{"cap returns 0 ⇒ hardcoded 4096", 0, func() int { return 0 }},
	} {
		if got := boundedMaxOut(c.explicit, c.cap); got <= 0 {
			t.Fatalf("boundedMaxOut(%s) = %d — if maxOut CAN now be 0, the hold's free path has a "+
				"second door and agentReserveBlocks' comment must say so", c.name, got)
		}
	}

	// ...and yet the hold is zero, for every allowance a caller could possibly supply.
	for _, model := range embeddingsModels {
		for _, maxOut := range []int{1, 512, 4096, 128000} {
			if got := reserveEstimateLXC(model, prompt, maxOut); got != 0 {
				t.Errorf("MEASURED BEHAVIOUR CHANGED: reserveEstimateLXC(%q, <embeddings>, maxOut=%d) "+
					"= %d, want 0. The hold now binds for an embedding model — update this file and "+
					"agent_allocator.go's `heldLXC <= 0` comment deliberately.", model, maxOut, got)
			}
		}
	}

	// ⚠ POSITIVE CONTROL: the SAME empty prompt on a CHAT model holds a large positive amount. That is
	// what proves the zero is the embedding model's zero OUTPUT rate and not the empty prompt — the
	// fourth free path's `len(prompt)/4` alone does NOT free the hold, exactly as the question guessed.
	if got := reserveEstimateLXC("gpt-4o", prompt, 4096); got <= 0 {
		t.Fatalf("CONTROL FAILED: reserveEstimateLXC(gpt-4o, \"\", 4096) = %d, want > 0. The hold is "+
			"supposed to bind on the output allowance alone; if it does not, the zeros above prove "+
			"nothing about embedding models specifically.", got)
	}
}

// The population door 2 opens through, pinned. A CHAT model acquiring a zero output rate would put
// every one of its requests on the same free hold, so the set is asserted exactly, not by count.
func TestCatalog_TheZeroOutputRateModelsAreExactlyTheEmbeddingOnes(t *testing.T) {
	zeroOut := map[string]bool{}
	for _, m := range catalog.All() {
		if m.OutputPer1M <= 0 && m.Tier != catalog.TierSpeech { // speech models are refused on the chat seams (B40.10)
			zeroOut[m.ID] = true
		}
	}
	for _, want := range embeddingsModels {
		if !zeroOut[want] {
			t.Errorf("%s no longer has a zero output rate — door 2 may be closed for it; re-measure "+
				"reserveEstimateLXC before assuming this file still describes the system", want)
		}
		delete(zeroOut, want)
	}
	for extra := range zeroOut {
		rates, prov := catalog.ResolveRates(extra, catalog.PurposeHold)
		t.Errorf("⚠ A NEW ZERO-OUTPUT-RATE MODEL: %q (hold rates %+v, provenance %v). If it serves "+
			"CHAT traffic, the reservation hold is now zero for EVERY request to it, not just for "+
			"embeddings — measure agentReserveBlocks against it before adding it here.", extra, rates, prov)
	}
}

// THE SECOND SEAM, on a zero balance. lxcGateBlocks short-circuits ABOVE the balance read, so an
// embeddings request is admitted by a gate that never even asks what the workspace can afford.
func TestLXCGateBlocks_AnEmbeddingsRequestIsAdmittedOnAZeroBalance(t *testing.T) {
	body := realEmbeddingsBody(t, embeddingsModels[0], 40*1024)
	model, prompt := promptAsTheMoneySeamsSeeIt(t, body)

	// CONTROL: the same zero-balance workspace, the same gate, an ordinary prompt ⇒ BLOCKED. Without
	// this, "it was admitted" is equally consistent with a gate that is simply off.
	const ordinary = "a prompt long enough that the input-token estimate is a clean positive number"
	if !gateProxy(&fakeLXCReader{balance: 0}, true, true).
		lxcGateBlocks(context.Background(), "wsEmb", model, ordinary) {
		t.Fatalf("CONTROL FAILED: a zero balance must BLOCK a %d-byte prompt on %s (est %d µLXC)",
			len(ordinary), model, lxcEstimate(model, ordinary))
	}

	r := &fakeLXCReader{balance: 0}
	if gateProxy(r, true, true).lxcGateBlocks(context.Background(), "wsEmb", model, prompt) {
		t.Fatalf("MEASURED BEHAVIOUR CHANGED: a %d-byte embeddings request is now gated on a zero "+
			"balance — update this test and lxc_gate.go's comment deliberately", len(body))
	}
	if r.calls != 0 {
		t.Errorf("the zero-estimate branch read the balance %d time(s) — it is documented as returning "+
			"before the read, and that is why no balance can ever refuse an embeddings request", r.calls)
	}
}

// THE COST, THROUGH REAL POSTGRES AND THE REAL CEILING — the immediate-debit seam.
//
// ⚠ ASSERTS ON lxc_spend_claims, NOT ON A RETURN VALUE. `blocked == false` says the request was
// allowed; the ABSENCE of the claim row (written inside SpendLXCForAgent's single transaction) is the
// durable proof that the debit path was never entered at all — not "debited zero", never billed.
func TestRealPG_AnExhaustedAgentCeilingServesA40KBEmbeddingsRequest(t *testing.T) {
	p, _, pool := agentAllocHarness(t)
	ctx := context.Background()
	const key = "agent-embeddings-freepath"
	const ws = "ws-embeddings-freepath"

	if _, err := pool.Exec(ctx,
		`INSERT INTO lxc_balances (workspace_id, balance) VALUES ($1, 100000000)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, 1000, 1000)`, key, ws); err != nil {
		t.Fatal(err)
	}
	var ceiling, spent int64
	if err := pool.QueryRow(ctx,
		`SELECT ceiling_lxc, spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1`,
		key).Scan(&ceiling, &spent); err != nil {
		t.Fatal(err)
	}
	if ceiling-spent != 0 {
		t.Fatalf("fixture: sub-budget must be exhausted, remaining = %d", ceiling-spent)
	}

	claims := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM lxc_spend_claims WHERE scoped_key_id = $1`, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// POSITIVE CONTROL: the ceiling is live and refuses an ordinary prompt on this very row.
	const ordinary = "a prompt long enough that the input-token estimate is a clean positive number"
	if !p.agentAllocationBlocks(ctx, key, ws, agentTestModel, ordinary, "req-emb-control") {
		t.Fatalf("CONTROL FAILED: an exhausted sub-budget (ceiling=%d spent=%d) must BLOCK a %d-byte "+
			"prompt (est %d µLXC). Everything below would otherwise mean the ceiling is inert rather "+
			"than bypassed.", ceiling, spent, len(ordinary), lxcEstimate(agentTestModel, ordinary))
	}
	if n := claims(); n != 0 {
		t.Fatalf("a blocked request must leave no claim row (the tx rolls back); got %d", n)
	}

	// THE FREE PATH: 40 KB of source code, same exhausted agent, same flag — served, no debit
	// attempted, on the embedding models AND on the chat model (door 1 needs neither).
	for _, model := range append([]string{agentTestModel}, embeddingsModels...) {
		body := realEmbeddingsBody(t, model, 40*1024)
		_, prompt := promptAsTheMoneySeamsSeeIt(t, body)
		if p.agentAllocationBlocks(ctx, key, ws, model, prompt, "req-emb-"+model) {
			t.Fatalf("MEASURED BEHAVIOUR CHANGED: a %d-byte embeddings request on %s is now BLOCKED "+
				"at an exhausted ceiling. That is very likely the intended repair — but it refuses "+
				"traffic that is served today, so update this test and both callers' comments "+
				"deliberately rather than deleting the assertion.", len(body), model)
		}
		if n := claims(); n != 0 {
			t.Fatalf("a %d-byte embeddings request on %s booked a claim row (%d) — the debit path was "+
				"entered after all; the free path this file documents no longer exists", len(body), model, n)
		}
	}

	var spentAfter int64
	if err := pool.QueryRow(ctx,
		`SELECT spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1`, key).Scan(&spentAfter); err != nil {
		t.Fatal(err)
	}
	if spentAfter != spent {
		t.Errorf("spent_lxc moved %d -> %d across requests that booked no claim", spent, spentAfter)
	}
}

// THE THIRD CALLER OF THE FLOOR, MEASURED THROUGH REAL POSTGRES — the one the queue recorded as
// "the only one still unmeasured". The hold is the seam the ceiling is supposed to be airtight
// against: it debits the workspace, writes an lxc_reservations row, and blocks when the sub-budget
// cannot cover it. For an embeddings request it does none of those things.
func TestRealPG_TheReservationHoldIsNeverTakenForAnEmbeddingsRequest(t *testing.T) {
	p, _, pool := seamProxy(t)
	ctx := context.Background()
	const key = "agent-embeddings-hold"
	const ws = "ws-embeddings-hold"

	seamFund(t, pool, ws, 100000000)
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, 1000, 1000)`, key, ws); err != nil {
		t.Fatal(err)
	}

	holds := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM lxc_reservations WHERE scoped_key_id = $1`, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// POSITIVE CONTROL: an ordinary chat prompt on the SAME exhausted row is BLOCKED by the hold.
	const ordinary = "a prompt long enough that the input-token estimate is a clean positive number"
	if _, blocked := p.agentReserveBlocks(ctx, key, ws, agentTestModel, ordinary, "req-hold-control", 4096); !blocked {
		t.Fatalf("CONTROL FAILED: an exhausted sub-budget must BLOCK the hold for a %d-byte prompt "+
			"(hold %d µLXC). Without this the passes below could just mean the reservation path is off.",
			len(ordinary), agentHoldLXC(agentTestModel, ordinary, 4096))
	}
	if n := holds(); n != 0 {
		t.Fatalf("a blocked hold must leave no lxc_reservations row (the tx rolls back); got %d", n)
	}

	// THE FREE PATH ON THE HOLD: 40 KB of source code to an embedding model. Not blocked, and the
	// returned context carries NO reservation — so the post-serve settle has nothing to bill against
	// and the delivered cost is never reconciled to this agent's ceiling at all.
	for _, model := range embeddingsModels {
		body := realEmbeddingsBody(t, model, 40*1024)
		_, prompt := promptAsTheMoneySeamsSeeIt(t, body)
		rctx, blocked := p.agentReserveBlocks(ctx, key, ws, model, prompt, "req-hold-"+model, 4096)
		if blocked {
			t.Fatalf("MEASURED BEHAVIOUR CHANGED: the hold now BLOCKS a %d-byte embeddings request on "+
				"%s at an exhausted ceiling — update this test and agent_allocator.go deliberately", len(body), model)
		}
		if _, ok := reservationFrom(rctx); ok {
			t.Fatalf("a hold WAS taken for %s — the `heldLXC <= 0` branch was not entered and this "+
				"file's door 2 no longer exists", model)
		}
		if n := holds(); n != 0 {
			t.Fatalf("%s booked %d lxc_reservations row(s) — the hold path was entered after all", model, n)
		}
	}
}
