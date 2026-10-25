package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
)

// lxc_gate.go — LXC GATING (Phase-2 Stage 2.4/2.5): the pre-serve check that
// BLOCKS a request (402) when the workspace's LXC balance can't cover the
// estimated cost. This is the first gate that can alter whether a request
// SUCCEEDS — it ships behind its own default-off flag (LXCGatingEnabled) and is
// inert until deliberately enabled.
//
// CHECK-before-serve (not debit-before-serve): a no-lock read of the LXC
// balance (the existing GetLXCBalance), decide block/allow, serve, and let the
// EXISTING post-serve shadow debit (shadowSpendLXC) book the real cost. No
// reservation, no refund primitive (refund is structurally forbidden by
// TestNoReverseConversionPath — untouched).
//
// COHERENCE — gating is INERT unless shadow is ALSO on. Gating means "block
// when unaffordable, then debit," but the only serving-path debit is
// shadowSpendLXC (gated on lxcShadowEnabled). Gating without shadow would block
// requests yet never move LXC — blocking with no accounting on a frozen
// balance. So the gate requires BOTH lxcGatingEnabled() AND lxcShadowEnabled():
// a two-flag staging (shadow=observe, shadow+gating=enforce) where a half-config
// fails safe toward serving.
//
// PRE-SERVE ESTIMATE is input-only (output unknown pre-call), so the gate
// UNDER-blocks by design — exactly like the budget gate's
// estCost = budgetEstimateUSD(model, prompt). ⚠ THAT COMPARISON USED TO BE FALSE IN
// ONE RESPECT AND IS NOW TRUE: the budget gate priced through alerts.CostUSD, which
// is exactly zero for any model the catalog does not hold, while this gate has always
// gone through the resolver. W6.19 (#487) moved it onto budgetEstimateUSD, which
// mirrors lxcEstimate — same resolver, same catalog.PurposeCharge. The true
// output-inclusive cost books post-serve via the shadow debit.
//
// FAIL-OPEN on a balance-read error: log and ALLOW (mirrors the workspace
// spend cap — "rather under-enforce than fail-closed on a transient DB error").
// A fail-open admit is still booked post-serve by the shadow debit — bounded
// slack, not a free call.

// lxcBalanceReader is the minimal read surface the gates need — no-lock balance
// reads. *economy.DualTokenStore satisfies it. Deliberately separate from
// lxcSpendSink so the shadow path stays untouched. GetUnallocatedLXC is the
// balance less what the workspace's agents hold (B19.13): what a request not
// made with an agent's key may spend.
type lxcBalanceReader interface {
	GetLXCBalance(ctx context.Context, workspaceID string) (int64, error)
	GetUnallocatedLXC(ctx context.Context, workspaceID string) (int64, error)
}

// creditLineReader is what a company's credit line can lend now (B22.4). *economy.DualTokenStore satisfies it.
type creditLineReader interface {
	CreditLineAvailableLXC(ctx context.Context, workspaceID string) (int64, error)
}

// SetLXCGate wires the LXC gating reader + its enable flag (read per-call). The
// proxy holds both as optional, nil-safe fields. The coherence rule also reads
// the existing lxcShadowEnabled (set by SetLXCSpendSink).
func (p *Proxy) SetLXCGate(reader lxcBalanceReader, enabled func() bool) {
	p.lxcGate = reader
	p.lxcGatingEnabled = enabled
}

// lxcEstimate is the input-only pre-serve LXC cost estimate (output=0),
// converted at the fixed peg — in µLXC (SEC-2). It gates a CHARGE, so it rounds
// UP (ceil): a conservative estimate never under-reserves a sub-µLXC.
//
// ⚠ IT PRICES VIA CostUSDResolved, NOT CostUSD, for the same reason reserveEstimateLXC does — and this
// one is easier to miss, because both of its callers treat a zero as PERMISSION:
//
//	agentAllocationBlocks — the immediate-DEBIT path taken whenever p.reservationActive() is false
//	                        (proxy.go:799). `estLXC <= 0` returned false = serve, WITHOUT DEBITING.
//	                        LENS_LXC_AGENT_ALLOCATION_ENABLED defaults TRUE, so this was live.
//	lxcGateBlocks         — the balance ADMISSION gate. A zero can never exceed a balance, so an
//	                        unknown model was never gated, even on a workspace with no credit.
//
// PurposeCharge (the FLOOR) is right for both. This debit is immediate and never refunded, so
// over-charging on a rate we admit is a guess is the indefensible direction; and for the gate a floor
// starts blocking a zero-balance workspace without over-blocking a paying one.
//
// An EMPTY prompt still yields 0, correctly — zero tokens really is zero cost.
//
// ⚠ BUT IT IS NOT THE ONLY CASE, WHICH IS WHAT THIS SAID ("for that case and only that case"). The
// ceil above is applied to the µLXC conversion; the TOKEN conversion one line below is len(prompt)/4,
// which FLOORS. Every prompt of 1, 2 or 3 bytes is therefore zero tokens and zero µLXC on ANY model,
// known or unknown, and takes both callers' `<= 0` branch: agentAllocationBlocks serves it without
// debiting (measured against an exhausted sub-budget on real Postgres — see
// TestRealPG_AnExhaustedAgentCeilingStillServesASubFourBytePrompt) and lxcGateBlocks admits it on a
// workspace with no credit, since a zero can never exceed a balance. The boundary is pinned from both
// sides in lxc_estimate_short_prompt_test.go; closing it re-prices or refuses live traffic and is a
// decision rather than a repair.
//
// ⚠ AND "UNDER FOUR BYTES" IS STILL TOO SMALL A DESCRIPTION OF THE FREE PATH. The estimate is not floored
// on the bytes the CLIENT SENT, it is floored on the bytes extractPrompt CAN SEE — and extractPrompt reads
// `messages[]` only. An OpenAI EMBEDDINGS body carries `input`, so every embeddings request yields the empty
// prompt and prices at 0 no matter how large it is: 40 KB of source code, measured, on ANY model. That route
// is live (`/v1/proxy/openai/*` is a wildcard to HandleOpenAI) and a released client uses it. The empty
// prompt was already pinned by embeddings_route_test.go — but only as a CACHE fact, with the cache guard
// named as what makes it safe; nothing was ever asked about the three money seams reading the same
// extractor. Pinned from both sides in lxc_estimate_embeddings_test.go.
func lxcEstimate(model, prompt string) int64 {
	estUSD, prov := alerts.CostUSDResolved(model, catalog.PurposeCharge, len(prompt)/4, 0, 0, 0)
	if prov == catalog.ProvenanceFallback && len(prompt) > 0 {
		alerts.WarnUnpricedModel(model, catalog.PurposeCharge, estUSD)
	}
	return economy.ChargeULXC(estUSD) // µLXC
}

// reserveEstimateLXC is the CONSERVATIVE (output-aware) pre-serve HOLD, in µLXC: input tokens from the
// prompt PLUS a BOUNDED output allowance (maxOutTokens), priced on the requested model, ceil. Unlike the
// input-only lxcEstimate (which under-holds against an output-inclusive charge and would leak the ceiling
// by exactly the output cost), this is a true upper bound on the delivered cost, so the settle only ever
// refunds. maxOutTokens is BOUNDED by the caller (explicit max_tokens, else a sane cap — never catalog max).
//
// ⚠ IT PRICES VIA CostUSDResolved, NOT CostUSD. An unknown model used to price at 0 here, which made
// heldLXC 0, which made agentReserveBlocks return "no hold" — so the request carried no reservation, the
// settle had nothing to charge against, and the sub-budget ceiling was never consulted. A hold falls back
// to the provider's most expensive known model: over-holding is refunded by the settle, under-holding
// leaks the ceiling, so HIGH is the conservative direction here.
//
// ⚠ "A TRUE UPPER BOUND ON THE DELIVERED COST" IS FALSE FOR AN EMBEDDING MODEL, AND MEASURED SO
// (lxc_estimate_embeddings_test.go). The bound is input + maxOutTokens × the OUTPUT rate, and for the three
// seeded embedding models that rate is 0 — correctly, embeddings emit no output tokens. So the output term
// vanishes, the whole hold reduces to the input term, and the input term is itself 0 because extractPrompt
// reads `messages[]` while an embeddings body carries `input`. A 40 KB embeddings request therefore holds
// NOTHING on every allowance a caller can supply (1, 512, 4096, 128000 all measured), and takes
// agentReserveBlocks' `heldLXC <= 0 → no hold` branch. maxOut is never the way in — it cannot be 0.
func reserveEstimateLXC(model, prompt string, maxOutTokens int) int64 {
	if maxOutTokens < 0 {
		maxOutTokens = 0
	}
	estUSD, _ := alerts.CostUSDResolved(model, catalog.PurposeHold, len(prompt)/4, 0, 0, maxOutTokens)
	return economy.ChargeULXC(estUSD) // µLXC
}

// agentHoldLXC is the hold an agent's request takes (B17.106): reserveEstimateLXC with its input counted by
// holdInputTokens rather than len(prompt)/4, so an answer that uses all of its max_tokens still settles inside
// its hold. The settle cuts a charge to the hold (B35.3) and writes the rest off, so a hold below what the
// provider counts is an answer the ledger under-charges. Chat admission keeps reserveEstimateLXC.
func agentHoldLXC(model, prompt string, maxOutTokens int) int64 {
	if maxOutTokens < 0 {
		maxOutTokens = 0
	}
	estUSD, _ := alerts.CostUSDResolved(model, catalog.PurposeHold, holdInputTokens(prompt), 0, 0, maxOutTokens)
	return economy.ChargeULXC(estUSD) // µLXC
}

// holdFramingTokens is what a provider adds to a request's text before counting it: the roles and turn markers
// around each message.
const holdFramingTokens = 16

// holdInputTokens counts a prompt's input as densely as a provider's tokenizer does: a token for each digit,
// which Claude counts one by one, a token for each ASCII punctuation mark, a token for every three other bytes,
// and the request's framing. On 9 Oct the testers' 48-character sum ("What is 4219 + 5977? Reply with the
// number only.") was 25 input tokens to Claude Sonnet 5; len(prompt)/4 held 12, and the answer that ran to its
// max_tokens of 16 cost 2100 µLXC against a hold of 1840. On 10 Oct forty JSON rows (3571 bytes, 1281 of them
// quotes, colons, commas and braces) were 1808 input tokens; counting punctuation with the letters held 1560,
// and the settle cut the charge to the hold (B17.128). Never less than len(prompt)/4, so no hold is smaller
// than it was. An empty prompt (an embeddings body, which carries `input`, not `messages`) counts nothing.
func holdInputTokens(prompt string) int {
	if prompt == "" {
		return 0
	}
	dense := 0
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c > ' ' && c < 0x7f && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			dense++ // a digit or a punctuation mark
		}
	}
	return dense + (len(prompt)-dense+2)/3 + holdFramingTokens
}

// platformFeeReader is the platform fee a charge of an amount would carry (B32.11). *economy.DualTokenStore
// satisfies it.
type platformFeeReader interface {
	PlatformFeeOn(ctx context.Context, workspaceID string, amount int64) (int64, error)
}

// withPlatformFee is est plus the platform fee workspaceID's charge of it would carry — the price every pre-call
// check of a charge to credits compares with what the workspace has (B32.11). A read error adds nothing: the
// gates fail open, and the charge itself still takes the fee or is refused.
func (p *Proxy) withPlatformFee(ctx context.Context, workspaceID string, est int64) int64 {
	r, ok := p.lxcGate.(platformFeeReader)
	if !ok || est <= 0 {
		return est
	}
	fee, err := r.PlatformFeeOn(ctx, workspaceID, est)
	if err != nil {
		slog.Warn("billing: platform fee read failed (pre-call check without it)",
			slog.String("workspace", workspaceID), slog.String("err", err.Error()))
		return est
	}
	return est + fee
}

// lxcGateBlocks reports whether the request should be BLOCKED (true) for
// insufficient LXC. The caller (after the budget gate, before the upstream
// call) does writeError(402)+return on true, so "upstream never called" is
// structural by placement. Returns false (allow) whenever the gate is inert,
// the estimate is zero, or the balance read errors (fail-open).
//
// B26.2: the same for every logging policy. The debit fires for LoggingNone on both seams since B17.17,
// so exempting it here (as this gate once did) let such a workspace be served past a zero balance.
func (p *Proxy) lxcGateBlocks(ctx context.Context, workspaceID, model, prompt string) bool {
	if p == nil || p.lxcGate == nil || p.lxcGatingEnabled == nil || !p.lxcGatingEnabled() {
		return false
	}
	// COHERENCE: inert unless shadow is also on (no block without accounting).
	if p.lxcShadowEnabled == nil || !p.lxcShadowEnabled() {
		return false
	}
	estLXC := lxcEstimate(model, prompt)
	if estLXC <= 0 {
		// ⚠ "nothing to charge against" is the ESTIMATOR's opinion, not the request's size: every
		// embeddings request lands here at any size, and so does any prompt under four bytes. The
		// return is ABOVE the balance read, so no balance can ever refuse them. Both pinned.
		return false
	}
	estLXC = p.withPlatformFee(ctx, workspaceID, estLXC)
	read := p.lxcGate.GetUnallocatedLXC // B19.13: the workspace's own request cannot use its agents' LXC
	if agentKeyIDFromContext(ctx) != "" {
		read = p.lxcGate.GetLXCBalance
	}
	balance, err := read(ctx, workspaceID)
	if err != nil {
		// FAIL-OPEN — allow and log, mirroring the spend cap. The post-serve
		// shadow debit still books the real cost (bounded slack, not free).
		slog.Warn("economy: LXC gate balance read failed (failing open; request allowed)",
			slog.String("workspace", workspaceID),
			slog.String("err", err.Error()),
		)
		return false
	}
	// B22.4: an agent of a company with a credit line may spend past the balance, up to what the line can lend.
	if cl, ok := p.lxcGate.(creditLineReader); ok && agentKeyIDFromContext(ctx) != "" {
		if avail, err := cl.CreditLineAvailableLXC(ctx, workspaceID); err == nil {
			balance += avail
		}
	}
	// B1.6: allowance left counts before prepaid, so a subscriber is not refused
	// for having no top-up. A read error counts no allowance (prepaid alone decides).
	if p.allowance != nil && agentKeyIDFromContext(ctx) == "" {
		if rem, _, err := p.allowance.RemainingULXC(ctx, workspaceID, time.Now()); err == nil {
			balance += rem
		}
	}
	return balance < estLXC
}
