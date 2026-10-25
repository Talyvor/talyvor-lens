package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/metrics"
)

// agent_allocator.go — F4-capstone step C.1: the live bounded-allocation glue. For an AGENT request (an
// API-key-authed request with LXCAgentAllocationEnabled on), the pre-serve seam debits the input-only LXC
// estimate against the per-scoped-key sub-budget via SpendLXCForAgent — BEFORE node selection and BEFORE the
// upstream call — so a blocked agent physically cannot reach the serve path or exceed its ceiling.
//
// CLOSED-LOOP + CENTRAL-COUNTERPARTY: the only value movement is the SpendLXCForAgent debit (workspace↔Talyvor
// pool); this file adds no Transfer/marketplace/refund path (asserted by agent_allocator_guard_test.go).
//
// SERVER-DERIVED DEBIT KEY: the request_id passed to SpendLXCForAgent is derived from a process-start random
// salt + the apiKeyID + a per-request crypto/rand nonce — NEVER the client X-Talyvor-Request-ID header. So a
// client replaying its header gets a fresh key and is charged again (cannot dodge); the claim table's
// exactly-once still protects a server-side retry that reuses the SAME derived key.
//
// B19.2 — THE ONE CLIENT-CHOSEN KEY: a request carrying an Idempotency-Key derives its key from it AND
// from the request itself (agentDebitKeyFor), so only a byte-identical retry — same credential, model and
// prompt — finds the first hold or claim and is charged once; anything else under the same
// Idempotency-Key derives a different key and is charged as new.

// agentSpender is the minimal debit surface (economy.DualTokenStore.SpendLXCForAgent satisfies it). An
// interface so the proxy doesn't hard-depend on economy internals.
type agentSpender interface {
	SpendLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, requestID string, lxcAmount int64, description string, meta economy.AgentDebitMeta) error
	// Reservation lifecycle (billing redesign) — satisfied by *economy.DualTokenStore. Settle returns the
	// µLXC ACTUALLY charged (clamped to the hold) so the caller can fund a royalty on the real payment.
	ReserveLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, reservationID string, heldLXC int64, meta economy.AgentDebitMeta) error
	// Returns the settled charge AND the CASH-BACKED portion of it. Two figures because they answer
	// different questions: the charge is what the customer paid, the cash-backed portion is what may
	// fund a royalty. Collapsing them would either under-bill the customer or over-mint the pool.
	SettleLXCReservation(ctx context.Context, reservationID string, finalLXC int64, meta economy.AgentDebitMeta) (settled, cashBacked int64, err error)
	ReleaseLXCReservation(ctx context.Context, reservationID, reason string) error
	// B23.13: with reservations off, settles the pre-serve debit to the delivered cost within the agent's limit.
	SettleAgentDebit(ctx context.Context, workspaceID, debitKey string, deliveredLXC int64, meta economy.AgentDebitMeta) (economy.AgentDebitSettlement, error)
}

// SetAgentSpender wires the agent-allocation debit + its enable flag, and mints the process-start salt used
// to derive debit keys. nil-safe: agentAllocationBlocks is inert until this is called.
func (p *Proxy) SetAgentSpender(spender agentSpender, enabled func() bool) {
	p.agentSpender = spender
	p.agentAllocEnabled = enabled
	if p.agentDebitSalt == nil {
		salt := make([]byte, 32)
		if _, err := rand.Read(salt); err != nil {
			// crypto/rand failure ⇒ leave salt nil so the gate fails CLOSED (deriveAgentDebitKey errors).
			slog.Error("economy: agent debit salt init failed (agent allocation fails closed)", slog.String("err", err.Error()))
			return
		}
		p.agentDebitSalt = salt
	}
}

// deriveAgentDebitKey = hex(SHA256(salt ‖ apiKeyID ‖ nonce)), nonce = 32 crypto/rand bytes. Server-derived +
// per-request-unique: the client header never participates, so a header replay cannot reuse a key.
func deriveAgentDebitKey(salt []byte, apiKeyID string) (string, error) {
	if len(salt) == 0 {
		return "", errors.New("proxy: agent debit salt not initialized")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(apiKeyID))
	h.Write(nonce)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// agentKeyIDFromContext returns the scoped API-key ID for an API-key-authed request, else "" (JWT/admin/anon
// carry no APIKeyID — C.0 guarantees this, so they structurally cannot enter the agent path).
func agentKeyIDFromContext(ctx context.Context) string {
	if actx := auth.GetAuthContext(ctx); actx != nil {
		return actx.APIKeyID
	}
	// B17.121: a key from POST /v1/api/keys is stamped as an APIKey only, never an AuthContext; it is the
	// workspace's own key all the same, so it is held and charged like one ("global" is the synthetic stand-in).
	if k := auth.GetAPIKey(ctx); k != nil && k.ID != "" && k.ID != "global" {
		return k.ID
	}
	return ""
}

// agentStrategy picks the routing strategy: price-aware for an active agent request (B's clamp bounds Lens's
// node cost), else the default least-loaded — non-agent traffic is unchanged.
func (p *Proxy) agentStrategy(apiKeyID string) localrouter.RoutingStrategy {
	if apiKeyID != "" && p.agentAllocEnabled != nil && p.agentAllocEnabled() && p.agentSpender != nil {
		return localrouter.StrategyPriceAware
	}
	return localrouter.StrategyLeastLoaded
}

// agentAllocationBlocks performs the PRE-SERVE agent debit and reports whether the request must be BLOCKED
// (402). Inert (returns false, no debit) for a non-agent request, flag-off, unwired spender, or a
// zero/unknown estimate. Otherwise it debits the input-only LXC estimate against the sub-budget via
// SpendLXCForAgent, keyed on a server-derived request id, and BLOCKS unless the debit succeeded — so the
// serve path is entered IFF a debit was booked (airtight ceiling). Fail-CLOSED: any debit error blocks (a
// bounded agent must not serve on an unverifiable budget).
// requestID is the token_events request id (the handler's per-request id), stamped onto the debit row's
// metadata so the money row joins to its usage row. `model` is the REQUESTED model — this debit precedes
// routing, so it is what the charge was estimated on, not necessarily the model that served.
func (p *Proxy) agentAllocationBlocks(ctx context.Context, apiKeyID, wsID, model, prompt, requestID string) bool {
	_, err := p.agentAllocate(ctx, apiKeyID, wsID, model, prompt, requestID)
	return err != nil
}

type agentDebitedCtxKey struct{}

// agentDebit is the pre-serve debit a request's charge was booked under, for the post-serve settle (B23.13).
type agentDebit struct {
	debitKey       string
	requestedModel string
	requestID      string
}

// agentDebited reports whether this request's charge was already booked by the pre-serve agent debit, so
// the post-serve shadow debit does not charge it a second time (B23.2).
func agentDebited(ctx context.Context) bool {
	_, debited := ctx.Value(agentDebitedCtxKey{}).(agentDebit)
	return debited
}

// agentAllocate is agentAllocationBlocks returning WHY it blocks (nil: serve), so the handler can tell an
// agent's rules (B19.2) from its balance. On a booked debit the returned context says so (agentDebited).
func (p *Proxy) agentAllocate(ctx context.Context, apiKeyID, wsID, model, prompt, requestID string) (context.Context, error) {
	if apiKeyID == "" || p.agentSpender == nil || p.agentAllocEnabled == nil || !p.agentAllocEnabled() {
		return ctx, nil // non-agent / inert → no debit, no block (today's behavior)
	}
	estLXC := lxcEstimate(model, prompt)
	if estLXC <= 0 {
		// An UNKNOWN MODEL no longer lands here — lxcEstimate prices it on the derived floor — which is
		// the whole point: this branch used to serve unpriced traffic with no debit at all, and it is
		// the path taken whenever reservations are off.
		//
		// ⚠ IT IS NOT "EMPTY PROMPT ONLY", WHICH IS WHAT THIS SAID. lxcEstimate converts bytes to
		// tokens with len(prompt)/4, which FLOORS, so ANY prompt under 4 bytes is zero tokens, prices
		// at zero on every model, and takes this branch. MEASURED against a FULLY EXHAUSTED sub-budget
		// on real Postgres (TestRealPG_AnExhaustedAgentCeilingStillServesASubFourBytePrompt): a
		// 77-byte prompt is BLOCKED and "ok?" / "go" / "y" are SERVED, booking no lxc_spend_claims row
		// at all. So the "airtight ceiling" above — "the serve path is entered IFF a debit was booked"
		// — holds for every prompt except the short ones, and an agent at its ceiling keeps drawing
		// completions whose OUTPUT this input-only estimate never bounded.
		//
		// ⚠ NOT REPAIRED HERE BECAUSE EVERY REPAIR MOVES A NUMBER: ceil-ing the token count re-prices
		// every prompt whose length is not a multiple of 4; a 1 µLXC floor invents a charge;
		// blocking short prompts refuses traffic that is served today. That is a pricing/product
		// decision. The boundary is pinned in lxc_estimate_short_prompt_test.go so it cannot widen
		// silently, and the same free path exists in lxc_gate.go#lxcGateBlocks.
		return ctx, nil
	}
	debitKey, err := p.agentDebitKeyFor(ctx, apiKeyID, model, prompt)
	if err != nil {
		slog.Error("economy: agent debit key derivation failed (failing closed)", slog.String("err", err.Error()))
		return ctx, err // fail closed — cannot mint a safe key ⇒ do not serve
	}
	// The debit row carries the REQUESTED model + token_events request_id (non-content — AgentDebitMeta),
	// so the ledger is readable and joins to token_events. NEVER prompt text/hash/embedding (0055 immutable).
	hctx, cancel := context.WithTimeout(ctx, preServeHoldTimeout)
	err = p.agentSpender.SpendLXCForAgent(hctx, apiKeyID, wsID, debitKey, estLXC, "proof-of-agent-allocation: pre-serve estimate debit",
		economy.AgentDebitMeta{RequestedModel: model, RequestID: requestID})
	cancel()
	if err == nil {
		// debited ⇒ allow (serve)
		return context.WithValue(ctx, agentDebitedCtxKey{}, agentDebit{debitKey: debitKey, requestedModel: model, requestID: requestID}), nil
	}
	if !expectedAgentRefusal(err) {
		// Unexpected (e.g. transient DB) error — fail CLOSED to keep the ceiling airtight.
		slog.Warn("economy: agent debit failed (failing closed)", slog.String("agent", apiKeyID), slog.String("err", err.Error()))
	}
	return ctx, err // ErrSubBudgetExceeded / ErrInsufficientLXC / a rule / any error ⇒ block
}

// expectedAgentRefusal: a refusal the agent's balance or rules make, as opposed to a failure.
func expectedAgentRefusal(err error) bool {
	return errors.Is(err, economy.ErrSubBudgetExceeded) || errors.Is(err, economy.ErrInsufficientLXC) ||
		errors.Is(err, economy.ErrAgentRule) || errors.Is(err, economy.ErrApprovalRequired)
}

type idempotencyCtxKey struct{}

// withAgentCall carries what B19.2 needs about an agent's request to its hold or debit: what the rules
// judge besides the amount (economy.AgentRequest), and the Idempotency-Key a retry repeats.
func withAgentCall(ctx context.Context, apiKeyID, model, provider, prompt, idempotencyKey string) context.Context {
	fp := sha256.Sum256([]byte("approval\x00" + apiKeyID + "\x00" + model + "\x00" + prompt))
	ctx = economy.WithAgentRequest(ctx, economy.AgentRequest{Model: model, Provider: provider, Fingerprint: hex.EncodeToString(fp[:])})
	return context.WithValue(ctx, idempotencyCtxKey{}, idempotencyKey)
}

// agentDebitKeyFor is the reservation or claim id of an agent request: deriveAgentDebitKey's fresh key,
// or — for a request carrying an Idempotency-Key — one derived from it, the credential, the model and the
// prompt, with no process salt, so a retry that reaches another process after a restart still finds it.
func (p *Proxy) agentDebitKeyFor(ctx context.Context, apiKeyID, model, prompt string) (string, error) {
	idem, _ := ctx.Value(idempotencyCtxKey{}).(string)
	if idem == "" {
		return deriveAgentDebitKey(p.agentDebitSalt, apiKeyID)
	}
	h := sha256.Sum256([]byte("idempotency\x00" + apiKeyID + "\x00" + idem + "\x00" + model + "\x00" + prompt))
	return hex.EncodeToString(h[:]), nil
}

// writeAgentRefusal answers a blocked agent request: its rules' refusal says which rule (403) — the requests-per-
// minute rule's is 429 (B28.302), since the same request goes through once the minute has room; a hold Lens could
// not take for its locks is 503 and says so, never that the balance is short (B35.2); a balance or ceiling
// refusal is the 402 it always was, and any other failure a 503 (B39.5). Returns the metrics reason.
func writeAgentRefusal(w http.ResponseWriter, err error) string {
	if errors.Is(err, economy.ErrLockContention) {
		writeError(w, http.StatusServiceUnavailable, "Lens could not take the hold for this request; nothing was charged, and it can be sent again")
		return "agent_hold_unavailable"
	}
	if errors.Is(err, economy.ErrAgentRequestRate) {
		writeError(w, http.StatusTooManyRequests, strings.TrimPrefix(err.Error(), "economy: "))
		return "agent_rate"
	}
	if errors.Is(err, economy.ErrAgentRule) || errors.Is(err, economy.ErrApprovalRequired) {
		writeError(w, http.StatusForbidden, strings.TrimPrefix(err.Error(), "economy: "))
		return "agent_rule"
	}
	if !expectedAgentRefusal(err) {
		// B39.5: a hold or debit that failed rather than refused (Postgres down, preServeHoldTimeout spent) is
		// not a short balance.
		writeError(w, http.StatusServiceUnavailable, "Lens could not check this agent's budget just now; send the request again shortly")
		return "agent_hold_failed"
	}
	writeError(w, http.StatusPaymentRequired, "agent LXC sub-budget exceeded or insufficient balance")
	return "agent_blocked"
}

// preServeHoldTimeout bounds an agent's pre-serve hold or debit (B39.5). With Postgres frozen mid-query the call
// otherwise waits out the 60 s request timeout (#174); an unfinished hold is refunded by the reservation sweeper.
const preServeHoldTimeout = 5 * time.Second

// ─── Reservation seam (billing redesign) ────────────────────────────────────
//
// When LXCReservationEnabled, the pre-serve seam RESERVES a conservative (output-aware) hold instead of
// permanently debiting an estimate; the post-serve seam SETTLES the DELIVERED cost or RELEASES a cache hit
// (free). The reservation id is the same server-derived debit key; it rides the request context from the
// hold to the settle/release so no serve-path plumbing threads it by hand.

type reservationCtxKey struct{}

// reservationHandle is what the pre-serve hold parks on the context for the post-serve seam to resolve.
type reservationHandle struct {
	reservationID string
	requestID     string // token_events join, stamped on the settle's delivered-charge row
}

func withReservation(ctx context.Context, h reservationHandle) context.Context {
	return context.WithValue(ctx, reservationCtxKey{}, h)
}

func reservationFrom(ctx context.Context) (reservationHandle, bool) {
	h, ok := ctx.Value(reservationCtxKey{}).(reservationHandle)
	return h, ok
}

// reservationActive reports whether the reservation path is on AND wired.
func (p *Proxy) reservationActive() bool {
	return p.reservationEnabled != nil && p.reservationEnabled() && p.agentSpender != nil && p.agentAllocEnabled != nil && p.agentAllocEnabled()
}

// agentReserveBlocks performs the PRE-SERVE HOLD and reports whether the request must be BLOCKED (402). On a
// successful hold it returns a context carrying the reservation handle (for settle/release) and false. Inert
// (returns ctx, false — no hold) for a non-agent request or a zero estimate. Fail-CLOSED: any hold error
// blocks (a bounded agent must not serve on an unverifiable budget). maxOut is the caller-BOUNDED output
// allowance (explicit max_tokens else the configured cap) so the hold is a conservative upper bound.
//
// ⚠ THE ZERO-ESTIMATE CASE IS NOT WHAT THIS FILE USED TO SAY IT WAS ("unknown model / empty"). MEASURED, in
// lxc_estimate_embeddings_test.go: the unknown model was closed (it holds a large positive amount), and what
// is left is not an empty prompt but an ENTIRE ENDPOINT AT ANY SIZE. extractPrompt reads `messages[]`; an
// OpenAI embeddings body carries `input`, so a 40 KB embeddings request derives the EMPTY prompt, and for
// the three seeded embedding models the output rate is 0 — so the maxOut term that is supposed to make this
// a true upper bound contributes exactly nothing and the hold collapses to 0. The request then carries NO
// reservation: the settle has nothing to bill against and the sub-budget ceiling is never consulted.
//
// maxOut itself is NEVER 0 — every branch of boundedMaxOut is positive, and that is pinned. The zero comes
// from the rate and from the extractor, not from the allowance. NOT REPAIRED HERE: teaching extractPrompt to
// read `input` is blocked on the cross-tenant pooling decision recorded in embeddings_route_test.go, and
// pricing embeddings at this seam starts refusing traffic that is served today. Both are product decisions.
func (p *Proxy) agentReserveBlocks(ctx context.Context, apiKeyID, wsID, model, prompt, requestID string, maxOut int) (context.Context, bool) {
	rctx, err := p.agentReserve(ctx, apiKeyID, wsID, model, prompt, requestID, maxOut)
	return rctx, err != nil
}

// agentReserve is agentReserveBlocks returning WHY it blocks (nil: serve), so the handler can tell an
// agent's rules (B19.2) from its balance.
func (p *Proxy) agentReserve(ctx context.Context, apiKeyID, wsID, model, prompt, requestID string, maxOut int) (context.Context, error) {
	if apiKeyID == "" || p.agentSpender == nil {
		return ctx, nil
	}
	heldLXC := agentHoldLXC(model, prompt, maxOut)
	if heldLXC <= 0 {
		return ctx, nil // ⚠ every embeddings request reaches here, at any size — see the docstring
	}
	reservationID, err := p.agentDebitKeyFor(ctx, apiKeyID, model, prompt)
	if err != nil {
		slog.Error("economy: reservation key derivation failed (failing closed)", slog.String("err", err.Error()))
		return ctx, err
	}
	hctx, cancel := context.WithTimeout(ctx, preServeHoldTimeout)
	err = p.agentSpender.ReserveLXCForAgent(hctx, apiKeyID, wsID, reservationID, heldLXC,
		economy.AgentDebitMeta{RequestedModel: model, RequestID: requestID})
	cancel()
	if err != nil {
		if !expectedAgentRefusal(err) {
			slog.Warn("economy: reservation hold failed (failing closed)", slog.String("agent", apiKeyID), slog.String("err", err.Error()))
		}
		return ctx, err // ceiling / insufficient / a rule / any error ⇒ block
	}
	return withReservation(ctx, reservationHandle{reservationID: reservationID, requestID: requestID}), nil
}

// postAnswerWriteTimeout bounds each write Lens makes after the answer is sent (B39.4). The pool sets no statement
// or lock timeout, so without it a write stuck on a locked row or a full pool waits forever and holds its connection.
const postAnswerWriteTimeout = 10 * time.Second

// detachBounded detaches ctx from the request's cancellation, keeping its values, and gives it its own
// postAnswerWriteTimeout: a write after the answer survives a client hanging up but cannot hang forever.
// A write that times out fails as any failed write does — logged and, where it is counted, counted.
func detachBounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), postAnswerWriteTimeout)
}

// settleReservation SETTLES the held reservation (if any) to the DELIVERED cost — called at the post-serve
// seam so the hold is released PROMPTLY, not on a timer (the sweeper is for crashes only). deliveredUSD is
// converted to µLXC (ceil); the primitive clamps to [0, held] so a mis-estimate cannot over-bill.
//
// RETURNS the USD the consumer was ACTUALLY charged for this request (0 when there is NO reservation — a
// non-agent/plain-key request, or the reservation path off — and 0 on a settle error). The cross-tenant
// royalty seam reads this: a royalty is funded ONLY by what the consumer really paid, so a 0 here mints 0.
// servedModel is the model that ACTUALLY served this request (post-route) — stamped on the delivered-charge
// spend row so the money row reads "requested X, served Y, charged Z". requested_model + request_id are
// sourced from the reservation row inside the primitive (the hold wrote them), so only the served model,
// known here and nowhere in the row, is passed through.
func (p *Proxy) settleReservation(ctx context.Context, deliveredUSD float64, servedModel string) float64 {
	return p.settleReservationBasis(ctx, deliveredUSD, servedModel, "")
}

// settleReservationBasis is settleReservation plus the PRICE BASIS of deliveredUSD. priceBasis is
// "fallback" when the served model was absent from the catalog and the charge is therefore a derived
// guess; empty when the price was exact. It rides onto the spend row so the guess is auditable later.
func (p *Proxy) settleReservationBasis(ctx context.Context, deliveredUSD float64, servedModel, priceBasis string) float64 {
	h, ok := reservationFrom(ctx)
	if !ok || p.agentSpender == nil {
		return 0 // no reservation on this request ⇒ the consumer was charged nothing (plain key / path off)
	}
	// B27.4: every settle runs after the answer is written, so a client that hangs up right then cancels
	// r's context. Detach it (values kept), as the streamed seam's storeCtx does, or the settle aborts, the
	// hold is swept and refunded, and the question is free. The same holds for every settle and release below.
	ctx, cancel := detachBounded(ctx)
	defer cancel()
	finalLXC := int64(0)
	if deliveredUSD > 0 {
		finalLXC = economy.ChargeULXC(deliveredUSD)
	}
	settledLXC, _, err := p.agentSpender.SettleLXCReservation(ctx, h.reservationID, finalLXC,
		economy.AgentDebitMeta{ServedModel: servedModel, PriceBasis: priceBasis})
	if err != nil {
		// The response is already served. A failed settle leaves the hold, which the stranded sweeper later
		// REFUNDS (never over-charges): the customer is protected on the error path, and the answer goes unbilled,
		// so it is an ERROR and counted (B35.2) — never silent. Return 0 charge ⇒ the royalty seam treats it as
		// unfunded (deflationary; never mint on a failed bill).
		slog.Error("economy: reservation settle failed — the answer is unbilled (hold will be swept/refunded; royalty treated as unfunded)",
			slog.String("reservation", h.reservationID), slog.Int64("final_ulxc", finalLXC), slog.String("err", err.Error()))
		metrics.AgentSettleFailed("reservation")
		return 0
	}
	return float64(settledLXC) * economy.LXCUSDValue / 1e6 // the USD the consumer ACTUALLY paid
}

// settleAgentDebit is the reservation-off settle (B23.13): the pre-serve agent debit charged the input-only
// estimate, so the post-serve seam settles the difference to the DELIVERED cost — refunded when the answer cost
// less, charged when it cost more, up to what the agent's limit still allows; the rest is written off, with
// the question it belongs to. No-op for a request no agent debit was booked for.
func (p *Proxy) settleAgentDebit(ctx context.Context, wsID string, deliveredUSD float64, servedModel, priceBasis string) {
	deliveredLXC := int64(0)
	if deliveredUSD > 0 {
		deliveredLXC = economy.ChargeULXC(deliveredUSD)
	}
	p.settleAgentDebitULXC(ctx, wsID, deliveredLXC, economy.AgentDebitMeta{ServedModel: servedModel, PriceBasis: priceBasis})
}

// settleAgentDebitULXC is settleAgentDebit for a cost already in µLXC — a cache serve's price (B26.9). meta
// carries what the settling row says about the serve; the requested model and the question come from the debit.
// It returns the cash-backed part of the question's charge (0 on a settle error) and whether an agent debit
// was booked for this request at all (B27.6).
func (p *Proxy) settleAgentDebitULXC(ctx context.Context, wsID string, deliveredLXC int64, meta economy.AgentDebitMeta) (cashBackedULXC int64, debited bool) {
	d, ok := ctx.Value(agentDebitedCtxKey{}).(agentDebit)
	if !ok || p.agentSpender == nil {
		return 0, false
	}
	ctx, cancel := detachBounded(ctx) // B27.4, B39.4: see settleReservationBasis
	defer cancel()
	meta.RequestedModel, meta.RequestID = d.requestedModel, d.requestID
	s, err := p.agentSpender.SettleAgentDebit(ctx, wsID, d.debitKey, deliveredLXC, meta)
	if err != nil {
		// The response is already served; the question stays charged its estimate, not what it cost (B35.2).
		slog.Error("economy: agent debit settle failed (charged the pre-serve estimate)",
			slog.String("request_id", d.requestID), slog.String("debit_key", d.debitKey),
			slog.Int64("delivered_ulxc", deliveredLXC), slog.String("err", err.Error()))
		metrics.AgentSettleFailed("debit")
		return 0, true
	}
	if s.WrittenOffULXC > 0 {
		slog.Info("economy: agent's limit cut a question's charge short; the rest is written off",
			slog.String("request_id", d.requestID), slog.Int64("written_off_ulxc", s.WrittenOffULXC))
	}
	return s.CashBackedULXC, true
}

// settleServedOffSeam settles an agent's question answered OFF the upstream seams — by a registered node or
// by local routing (B26.5). Before it, those paths neither settled nor released, so the stranded sweeper
// refunded the hold in full and the question was free. Priced as the buffered seam prices a serve without
// provider usage: the served model's catalog charge rate on len/4 tokens, through the resolver so an
// unpriced model still cannot come out at zero. No-op for a request no hold or agent debit was booked for,
// so a non-agent serve never trips the unpriced-model alert.
func (p *Proxy) settleServedOffSeam(ctx context.Context, wsID, servedModel string, inT, outT int) {
	_, held := reservationFrom(ctx)
	_, debited := ctx.Value(agentDebitedCtxKey{}).(agentDebit)
	if !held && !debited {
		return
	}
	servedCostUSD, prov := alerts.CostUSDResolved(servedModel, catalog.PurposeCharge, inT, 0, 0, outT)
	priceBasis := ""
	if prov == catalog.ProvenanceFallback {
		priceBasis = prov.String()
	}
	if p.reservationActive() {
		p.settleReservationBasis(ctx, servedCostUSD, servedModel, priceBasis)
	} else {
		p.settleAgentDebit(ctx, wsID, servedCostUSD, servedModel, priceBasis)
	}
}

// settleReservationPooled is settleReservationBasis for a CROSS-TENANT POOLED hit: it settles the
// charge and puts the pooled-discount disclosure on the money row, so the customer's evidence lives
// in the ledger and not only in a response header they may never have looked at.
//
// It passes the LIST price and the RATE, and deliberately NOT the saving: the settle clamps the
// charge to the hold, so the saving is derived inside the insert from what was actually debited
// (economy.AgentDebitMeta.toSpendMap). Passing it here would create a second source of truth that
// disagrees with the amount on the very same row.
//
// The rate is stamped per-row because it is tunable at boot: reading it back from config to explain
// an old charge would silently re-price history.
func (p *Proxy) settleReservationPooled(ctx context.Context, chargedUSD float64, price pooledPrice) float64 {
	h, ok := reservationFrom(ctx)
	if !ok || p.agentSpender == nil {
		return 0 // no reservation ⇒ the consumer was charged nothing (plain key / path off)
	}
	ctx, cancel := detachBounded(ctx) // B27.4, B39.4: see settleReservationBasis
	defer cancel()
	settledLXC, cashBackedLXC, err := p.agentSpender.SettleLXCReservation(ctx, h.reservationID, price.ChargedULXC,
		economy.AgentDebitMeta{ServedModel: price.modelForRow, PriceBasis: price.PriceBasis,
			PoolListULXC: price.ListULXC, PoolDiscountRate: price.Rate})
	if err != nil {
		slog.Error("economy: pooled reservation settle failed — the answer is unbilled (hold will be swept/refunded; royalty treated as unfunded)",
			slog.String("reservation", h.reservationID), slog.Int64("final_ulxc", price.ChargedULXC), slog.String("err", err.Error()))
		metrics.AgentSettleFailed("pooled")
		return 0
	}
	// ⚠ THIS RETURNS THE CASH-BACKED PORTION, NOT THE CHARGE. The customer is still billed the full
	// settledLXC — that is untouched and lives on the ledger row. What changes is the ROYALTY BASIS:
	// only the part of this spend that real money paid for may mint LENS. Credit that arrived by
	// admin grant or by converting earnings funds no royalty, because no cash entered the system.
	//
	// The two figures are returned separately rather than collapsed because they answer different
	// questions, and settleReservationBasis's charge return is read by recordDistillServes —
	// repurposing it there would have silently corrupted distill attribution.
	if settledLXC > 0 && cashBackedLXC < settledLXC {
		slog.Info("poolroyalty: royalty basis reduced to the cash-backed portion of this spend",
			slog.Int64("settled_ulxc", settledLXC), slog.Int64("cash_backed_ulxc", cashBackedLXC),
			slog.String("reservation", h.reservationID))
	}
	return float64(cashBackedLXC) * economy.LXCUSDValue / 1e6
}

// releaseReservation REFUNDS the held reservation in full (if any) — an own-cache hit (no upstream call, no
// contributor ⇒ free) or a serve that delivered nothing. No-op without a reservation on the context.
func (p *Proxy) releaseReservation(ctx context.Context, reason string) {
	h, ok := reservationFrom(ctx)
	if !ok || p.agentSpender == nil {
		return
	}
	ctx, cancel := detachBounded(ctx) // B27.4, B39.4: see settleReservationBasis
	defer cancel()
	if err := p.agentSpender.ReleaseLXCReservation(ctx, h.reservationID, reason); err != nil {
		slog.Warn("economy: reservation release failed (hold will be swept/refunded)",
			slog.String("reservation", h.reservationID), slog.String("err", err.Error()))
	}
}
