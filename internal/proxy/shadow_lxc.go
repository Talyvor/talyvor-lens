package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/economy"
)

// shadow_lxc.go — the SHADOW LXC spend path (Phase-2 Stage 2.4/2.5), the FIRST
// live-serving-path change. It records a shadow LXC debit AFTER the response is
// served, alongside (never replacing) the cost_usd write — OBSERVATIONAL ONLY.
//
// THE SAFETY IS STRUCTURAL: shadowSpendLXC returns NOTHING. A void, post-serve,
// error-swallowed call cannot block, delay, fail, or alter any request — there
// is no return value a serve path could branch on. Insufficient LXC, a sink
// error, a nil sink, or the flag being off all just no-op/log. Same risk
// profile as the adjacent cost_usd RecordSpend (which also logs-only-on-error).
//
// Inert by default: gated on LXCShadowSpendEnabled (default false) AND a wired
// sink AND costUSD > 0. Off ⇒ no SpendLXC call at all.
//
// PRE-FLIP OPERATIONAL CAVEAT (does not affect serving, but matters before the
// flag is turned on in prod): SpendLXC opens a tx that takes a per-workspace
// `SELECT ... FOR UPDATE` row lock on lxc_balances — a serialization point the
// append-only cost_usd INSERT never had. With the flag ON, concurrent
// same-workspace requests serialize on that lock POST-SERVE (the response is
// already flushed, so the client is unaffected), holding the serve goroutine +
// a pooled connection slightly longer. Negligible at normal concurrency;
// validate connection-pool headroom under high same-workspace load before
// enabling. This is the deliberate synchronous-same-ctx posture (mirrors the
// adjacent cost_usd write); it is NOT made async precisely to keep that posture.

// lxcSpendSink is the minimal LXC-debit surface the proxy depends on — exactly
// one method, mirroring the royaltySink precedent (the proxy injects
// capabilities as narrow interfaces, never concrete economy stores).
// *economy.DualTokenStore.SpendLXC satisfies it.
type lxcSpendSink interface {
	SpendLXC(ctx context.Context, workspaceID string, lxcAmount int64, desc string) error
}

// SetLXCSpendSink wires the shadow LXC spend capability. enabled is read
// PER-CALL so the flag stays live (not captured). The proxy holds both as
// optional, nil-safe fields — a nil sink or nil/false enabled keeps the path
// inert, like the other nil-tolerant proxy seams.
func (p *Proxy) SetLXCSpendSink(sink lxcSpendSink, enabled func() bool) {
	p.lxcSink = sink
	p.lxcShadowEnabled = enabled
}

// shadowSpendLXC records a shadow LXC debit for a just-served AI call. VOID by
// design: it cannot affect the response. Called at the post-serve RecordSpend
// seam with the SAME ctx as the adjacent cost_usd write (so it inherits the
// identical lifecycle, including the streaming path's detached context).
func (p *Proxy) shadowSpendLXC(ctx context.Context, workspaceID string, costUSD float64) {
	if p == nil || p.lxcSink == nil || p.lxcShadowEnabled == nil || !p.lxcShadowEnabled() {
		return
	}
	if costUSD <= 0 {
		return
	}
	// B23.2: an agent's request already paid its pre-serve debit (reservations off) — that is its charge.
	if agentDebited(ctx) {
		return
	}
	// cost_usd → µLXC at the fixed peg ($0.10/LXC). SEC-2: µLXC is an integer
	// smallest-unit (1e-6 LXC, the old roundTo(_,6) precision). This is a CHARGE
	// (billing debit) for the served call, so it rounds UP (ceil) — the shadow
	// debit never under-bills a sub-µLXC.
	lxcAmount := economy.ChargeULXC(costUSD) // µLXC
	if lxcAmount <= 0 {
		// A sub-threshold positive cost rounds to 0 — nothing to debit, and no
		// spurious SpendLXC(0) / "debit failed" warning.
		return
	}
	if err := p.lxcSink.SpendLXC(ctx, workspaceID, lxcAmount, "shadow: AI call billing"); err != nil {
		// Logged-and-swallowed, exactly like the adjacent cost_usd RecordSpend.
		// Insufficient LXC, DB errors — none of it touches the served response.
		slog.Warn("economy: shadow LXC debit failed (observational; serve unaffected)",
			slog.String("workspace", workspaceID),
			slog.Int64("lxc_ulxc", lxcAmount),
			slog.Float64("cost_usd", costUSD),
			slog.String("err", err.Error()),
		)
	}
}
