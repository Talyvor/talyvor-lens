package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

// allowance.go — B1.6 (W4.6.1 step 2b): the subscription allowance on the serving path.
//
// WHO IT COVERS: requests with NO agent key — the browser-chat session key, JWT. An agent
// (scoped workspace key) is billed by its own reservation / sub-budget ledger and never
// touches the allowance, so no request is paid for twice.
//
// WHY NOT shadowSpendLXC OR THE RESERVATION SETTLE: neither sees chat. Reservations are on
// by default and hold only for agent keys; the session key carries no APIKeyID, so its
// settle finds no reservation and charges nothing, and shadowSpendLXC is the `else` of
// reservationActive(). The draw therefore sits at both upstream serve seams (buffered and
// streamed) BESIDE that branch, not inside either arm.
//
// HARD CAP: the ledger clamps (billing.Service.Draw) — the allowance covers at most what
// is left, never more. Past it, a subscriber pays from prepaid LXC: the admission check
// refuses a request neither can cover, and the uncovered remainder is debited from
// prepaid for real (not the shadow debit — a subscriber's overflow is a charge whatever
// that flag says).
//
// INERT unless wired (LENS_SUBSCRIPTION_ALLOWANCE_ULXC > 0) AND the workspace has an
// allowance for the current period. A non-subscriber is served and booked exactly as before.

// subscriptionAllowance is the allowance surface. *billing.Service satisfies it.
type subscriptionAllowance interface {
	Draw(ctx context.Context, workspaceID string, costULXC int64, at time.Time) (covered int64, inPeriod bool, err error)
	RemainingULXC(ctx context.Context, workspaceID string, at time.Time) (remaining int64, inPeriod bool, err error)
}

// SetSubscriptionAllowance wires the allowance. Unwired (nil) = no allowance anywhere.
func (p *Proxy) SetSubscriptionAllowance(a subscriptionAllowance) {
	p.allowance = a
}

// allowanceGateBlocks refuses (402) a non-agent request from a workspace whose allowance
// plus prepaid LXC cannot cover the input-only estimate. False (admit) whenever the
// workspace has no allowance this period, and on any read error (fail-open, like the LXC
// gate: the post-serve charge still books what it can).
func (p *Proxy) allowanceGateBlocks(ctx context.Context, workspaceID, model, prompt string) bool {
	if p == nil || p.allowance == nil || workspaceID == "" || agentKeyIDFromContext(ctx) != "" {
		return false
	}
	remaining, inPeriod, err := p.allowance.RemainingULXC(ctx, workspaceID, time.Now())
	if err != nil || !inPeriod {
		return false
	}
	est := lxcEstimate(model, prompt)
	if est <= 0 || remaining >= est {
		return false
	}
	if p.lxcGate == nil {
		return false
	}
	balance, err := p.lxcGate.GetUnallocatedLXC(ctx, workspaceID) // B19.13: not what its agents hold
	if err != nil {
		slog.Warn("billing: allowance gate balance read failed (failing open; request allowed)",
			slog.String("workspace", workspaceID), slog.String("err", err.Error()))
		return false
	}
	return balance < p.withPlatformFee(ctx, workspaceID, est-remaining) // B32.11: past the allowance, with its fee
}

// chargeSubscriberUsage books a served non-agent request against the workspace's
// allowance and debits any uncovered remainder from prepaid LXC. It reports whether the
// workspace is in an allowance period — when true the caller must NOT also shadow-debit
// the same cost. VOID with respect to the response: post-serve, errors logged.
func (p *Proxy) chargeSubscriberUsage(ctx context.Context, workspaceID string, costUSD float64) bool {
	if p == nil || p.allowance == nil || workspaceID == "" || costUSD <= 0 || agentKeyIDFromContext(ctx) != "" {
		return false
	}
	costULXC := economy.ChargeULXC(costUSD) // a charge rounds UP, as shadowSpendLXC
	if costULXC <= 0 {
		return false
	}
	inPeriod, _ := p.subscriberCharge(ctx, workspaceID, costULXC)
	return inPeriod
}

// subscriberCharge draws costULXC from the allowance and debits the uncovered remainder from
// prepaid. inPeriod=false means the workspace has no allowance this period and nothing was booked;
// charged is what was booked (allowance drawn + prepaid debited).
func (p *Proxy) subscriberCharge(ctx context.Context, workspaceID string, costULXC int64) (inPeriod bool, charged int64) {
	if p.allowance == nil {
		return false, 0
	}
	covered, inPeriod, err := p.allowance.Draw(ctx, workspaceID, costULXC, time.Now())
	if err != nil {
		slog.Warn("billing: allowance draw failed (booked as a non-subscriber; serve unaffected)",
			slog.String("workspace", workspaceID), slog.Int64("cost_ulxc", costULXC), slog.String("err", err.Error()))
		return false, 0
	}
	if !inPeriod {
		return false, 0
	}
	charged = covered
	if rest := costULXC - covered; rest > 0 && p.lxcSink != nil {
		if err := p.lxcSink.SpendLXC(ctx, workspaceID, rest, "subscription: usage beyond the plan allowance"); err != nil {
			slog.Warn("billing: prepaid debit past the allowance failed (serve unaffected)",
				slog.String("workspace", workspaceID), slog.Int64("ulxc", rest), slog.String("err", err.Error()))
		} else {
			charged += rest
		}
	}
	return true, charged
}
