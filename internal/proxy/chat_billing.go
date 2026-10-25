package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// chat_billing.go — B9.8: every browser-chat request is charged.
//
// A session key carries no APIKeyID, so it never reaches the agent reservation (the only thing that
// moved LXC by default — docs/model2-step4b-session-billing-measured.md), and until now a chat
// request billed nothing unless the workspace had a plan allowance in period.
//
// WHICH OF STEP 4b'S THREE MECHANISMS, AND WHY: (2), a direct debit of the workspace at settle,
// extending B1.6's allowance-then-prepaid charge to EVERY chat request, plus (3), a per-session bound.
// (1), a reservation keyed on the session key, would open an agent sub-budget for a credential that
// dies within the hour (migration 0122 refuses exactly that) and would hold against prepaid even when
// the plan allowance should pay first. (2) keeps Nicolai's order — allowance first, then prepaid —
// and B1.6 already runs it on both seams for subscribers.
//
// NEVER NEGATIVE: admission (below) refuses before the provider is called unless allowance + prepaid
// covers the CONSERVATIVE estimate (input + a bounded output allowance, the reservation's own hold
// estimate), and SpendLXC refuses rather than overdraw. Only concurrent requests racing the same
// balance can leave a served request unbilled; that is logged, and the balance still never goes
// below zero.

// sessionSpend is the per-session running total. *sessionkey.Store satisfies it.
type sessionSpend interface {
	Spent(ctx context.Context, id string) (int64, error)
	AddSpent(ctx context.Context, id string, ulxc int64) error
}

// SetSessionSpend wires the per-session bound: boundULXC is the most one chat session may be charged.
func (p *Proxy) SetSessionSpend(s sessionSpend, boundULXC int64) {
	p.sessionSpend = s
	p.sessionSpendBound = boundULXC
}

// chatSession reports whether this request is charged as the browser chat is, and its session key's ID.
// B17.132: the workspace's own token (JWT) too — like the session key it carries no APIKeyID, so it never
// reaches the agent reservation, and a question asked on it was served and charged nothing. It has no
// session row, so its ID is "" and the per-session bound does not apply.
func chatSession(ctx context.Context) (string, bool) {
	actx := auth.GetAuthContext(ctx)
	if actx == nil || actx.APIKeyID != "" || (actx.AuthMethod != auth.MethodSessionKey && actx.AuthMethod != auth.MethodJWT) {
		return "", false
	}
	return actx.SessionKeyID, true
}

// chatAdmission refuses a chat request BEFORE the provider is called when its conservative cost is
// not covered by the plan allowance plus prepaid credit, or would take the session past its bound.
// It returns the message to send with a 402. Read errors fail open, like the LXC gate: the
// post-serve charge still books what it can.
func (p *Proxy) chatAdmission(ctx context.Context, workspaceID, model, prompt string, maxOut int) (string, bool) {
	sessionID, ok := chatSession(ctx)
	if !ok || p == nil || workspaceID == "" {
		return "", false
	}
	est := reserveEstimateLXC(model, prompt, maxOut)
	if est <= 0 {
		return "", false
	}
	if p.sessionSpend != nil && sessionID != "" && p.sessionSpendBound > 0 {
		spent, err := p.sessionSpend.Spent(ctx, sessionID)
		if err != nil {
			slog.Warn("billing: chat session spend read failed (failing open)", slog.String("err", err.Error()))
		} else if spent+est > p.sessionSpendBound {
			return fmt.Sprintf("this chat session has reached its spending limit of %s LXC — start a new chat to continue",
				formatLXC(p.sessionSpendBound)), true
		}
	}
	var covered int64
	if p.allowance != nil {
		if rem, inPeriod, err := p.allowance.RemainingULXC(ctx, workspaceID, time.Now()); err == nil && inPeriod {
			covered = rem
		}
	}
	if covered >= est || p.lxcGate == nil {
		return "", false
	}
	balance, err := p.lxcGate.GetUnallocatedLXC(ctx, workspaceID) // B19.13: not what its agents hold
	if err != nil {
		slog.Warn("billing: chat balance read failed (failing open)", slog.String("workspace", workspaceID), slog.String("err", err.Error()))
		return "", false
	}
	if balance < p.withPlatformFee(ctx, workspaceID, est-covered) { // B32.11: past the allowance, with its fee
		return "not enough credit for this request — top up to continue", true
	}
	return "", false
}

// chargeChatUsage books a served chat request: the plan allowance first, then prepaid LXC for the
// rest (for a non-subscriber, all of it), and adds what was booked to the session's total. It
// reports whether the request was a chat request — the caller then books nothing else for it.
// VOID with respect to the response: post-serve, errors logged.
func (p *Proxy) chargeChatUsage(ctx context.Context, workspaceID string, costUSD float64) bool {
	_, chat := p.chargeChat(ctx, workspaceID, costUSD)
	return chat
}

// chargeChat is chargeChatUsage that also returns what it booked, in µLXC: the figure a stream's charge
// frame reports under the answer (B17.129).
func (p *Proxy) chargeChat(ctx context.Context, workspaceID string, costUSD float64) (charged int64, chat bool) {
	sessionID, ok := chatSession(ctx)
	if !ok || p == nil || workspaceID == "" {
		return 0, false
	}
	costULXC := economy.ChargeULXC(costUSD) // a charge rounds UP
	if costULXC <= 0 {
		tallyCharge(ctx, 0)
		return 0, true
	}
	inPeriod, charged := p.subscriberCharge(ctx, workspaceID, costULXC)
	if !inPeriod {
		if p.lxcSink == nil {
			slog.Error("billing: chat request served UNBILLED — no LXC sink wired",
				slog.String("workspace", workspaceID), slog.Int64("ulxc", costULXC))
		} else if err := p.lxcSink.SpendLXC(ctx, workspaceID, costULXC, "chat: metered usage"); err != nil {
			slog.Error("billing: chat request served UNBILLED — prepaid debit failed",
				slog.String("workspace", workspaceID), slog.Int64("ulxc", costULXC), slog.String("err", err.Error()))
		} else {
			charged = costULXC
		}
	}
	if charged > 0 && p.sessionSpend != nil && sessionID != "" {
		if err := p.sessionSpend.AddSpent(ctx, sessionID, charged); err != nil {
			slog.Warn("billing: chat session spend not recorded", slog.String("err", err.Error()))
		}
	}
	tallyCharge(ctx, charged)
	return charged, true
}

// formatLXC renders µLXC as whole LXC for a message.
func formatLXC(ulxc int64) string {
	return fmt.Sprintf("%d", ulxc/1_000_000)
}

// lxcMetaSpender is the prepaid debit that records the pool metadata and reports its cash-backed part.
// *economy.DualTokenStore satisfies it.
type lxcMetaSpender interface {
	SpendLXCMeta(ctx context.Context, workspaceID string, lxcAmount int64, description string, metadata map[string]interface{}) (int64, error)
}

// chargeChatPooled charges a browser-chat POOLED serve exactly like an agent's (B9.3): the discounted
// price (list × (1 − r)) from the plan allowance first, then prepaid LXC, with the pool figures on the
// prepaid row. It returns the royalty basis in USD — the allowance-covered part (paid for by the plan
// fee) plus the CASH-BACKED part of the prepaid debit, as the agent settle does — and whether this
// was a chat request at all. The funding invariant is unchanged: nothing charged, nothing minted. A test
// workspace's whole prepaid debit is test-backed (B26.6), as economy's settle counts it.
func (p *Proxy) chargeChatPooled(ctx context.Context, price pooledPrice) (float64, bool) {
	sessionID, ok := chatSession(ctx)
	if !ok || p == nil {
		return 0, false
	}
	workspaceID := auth.GetAuthContext(ctx).WorkspaceID
	amount := price.ChargedULXC
	if amount <= 0 || workspaceID == "" {
		return 0, true
	}
	var covered int64
	if p.allowance != nil {
		c, inPeriod, err := p.allowance.Draw(ctx, workspaceID, amount, time.Now())
		if err != nil {
			slog.Warn("billing: allowance draw for a chat pooled serve failed (charged to prepaid)", slog.String("err", err.Error()))
		} else if inPeriod {
			covered = c
		}
	}
	funded, charged := covered, covered
	if rest := amount - covered; rest > 0 {
		ms, ok := p.lxcSink.(lxcMetaSpender)
		if !ok {
			slog.Error("billing: chat pooled serve UNBILLED — no LXC sink wired", slog.String("workspace", workspaceID))
		} else if cash, err := ms.SpendLXCMeta(ctx, workspaceID, rest, "chat: pooled answer", map[string]interface{}{
			"served_model": price.modelForRow, "price_basis": price.PriceBasis,
			"pool_list_ulxc": price.ListULXC, "pool_saved_ulxc": price.SavedULXC, "pool_discount_rate": price.Rate,
		}); err != nil {
			slog.Error("billing: chat pooled serve UNBILLED — prepaid debit failed",
				slog.String("workspace", workspaceID), slog.Int64("ulxc", rest), slog.String("err", err.Error()))
		} else {
			if p.isSynthetic(workspaceID) {
				cash = rest
			}
			funded += cash
			charged += rest
		}
	}
	if charged > 0 && p.sessionSpend != nil && sessionID != "" {
		if err := p.sessionSpend.AddSpent(ctx, sessionID, charged); err != nil {
			slog.Warn("billing: chat session spend not recorded", slog.String("err", err.Error()))
		}
	}
	return float64(funded) * economy.LXCUSDValue / 1e6, true
}
