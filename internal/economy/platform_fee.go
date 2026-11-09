package economy

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// platform_fee.go — B32.11: a platform fee on AI spend, by plan (Nicolai's decision of 5 Oct 2026).
//
// Every model call charged to a workspace's credits — at list price or at the pooled price, by a person or by
// an agent — carries a fee of its plan's basis points. The fee is its own lxc_ledger row (type platform_fee),
// written in the same transaction as the spend it is on, whose metadata names the request and the basis
// points; for an agent it is its own posting too. It rounds up to the µLXC. Every pre-call check counts it: a
// hold is the estimate plus its fee, and an agent's limits judge that whole amount. Prepaid credits pay it, as
// they pay the call.
//
// What the fee is NOT on: a request on the workspace's own provider keys (it is charged nothing at all), and
// usage drawn from a chat plan's included allowance (only what is past it reaches a spend here).
//
// The rate is the resolver's (billing.PlatformFeeBPS, wired in cmd/lens): unwired, every rate is 0 and every
// path writes exactly the rows it wrote before this file.

// LXCTypePlatformFee marks the platform fee's lxc_ledger row — Talyvor's own service, paid from credits.
const LXCTypePlatformFee = "platform_fee"

// feeDenominator is 100%, in basis points (fees.BPSDenominator; economy does not import fees).
const feeDenominator = 10_000

// FeeQuerier is a pool or a transaction.
type FeeQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PlatformFeeResolver answers the platform fee on workspaceID's AI spend, in basis points. Inside a spend it
// reads through the spend's own transaction.
type PlatformFeeResolver func(ctx context.Context, q FeeQuerier, workspaceID string) (int64, error)

// SetPlatformFee wires the fee's rate. nil, the fee is 0 everywhere.
func (s *DualTokenStore) SetPlatformFee(r PlatformFeeResolver) { s.platformFee = r }

func (s *DualTokenStore) platformFeeBPS(ctx context.Context, q FeeQuerier, workspaceID string) (int64, error) {
	if s == nil || s.platformFee == nil {
		return 0, nil
	}
	bps, err := s.platformFee(ctx, q, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("economy: the platform fee of %s: %w", workspaceID, err)
	}
	if bps < 0 || bps > feeDenominator {
		return 0, fmt.Errorf("economy: the platform fee of %s is %d basis points, outside 0 to %d", workspaceID, bps, feeDenominator)
	}
	return bps, nil
}

// PlatformFeeOn is the fee a spend of amount µLXC by workspaceID would carry now — what a pre-call check adds
// to its estimate.
func (s *DualTokenStore) PlatformFeeOn(ctx context.Context, workspaceID string, amount int64) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	bps, err := s.platformFeeBPS(ctx, s.pool, workspaceID)
	return PlatformFee(amount, bps), err
}

// PlatformFee is the fee on a spend of amount µLXC at bps, rounded up to the µLXC: amount × bps ÷ 10,000.
// Split by the denominator so no amount a call can cost overflows.
func PlatformFee(amount, bps int64) int64 {
	if amount <= 0 || bps <= 0 {
		return 0
	}
	q, r := amount/feeDenominator, amount%feeDenominator
	return q*bps + (r*bps+feeDenominator-1)/feeDenominator
}

// spendWithin is the largest spend whose spend plus fee at bps fits in gross: floor(gross × 10,000 ÷
// (10,000 + bps)). For it, spend + PlatformFee(spend) ≤ gross exactly — the fee's ceiling only rounds up to the
// whole µLXC that gross − spend already is — so a settle clamped to it never charges above its hold.
func spendWithin(gross, bps int64) int64 {
	if gross <= 0 {
		return 0
	}
	d := feeDenominator + bps
	return gross/d*feeDenominator + gross%d*feeDenominator/d
}

// FeeLabel is how a statement names the fee: "Platform fee 3%", "Platform fee 5.5%".
func FeeLabel(bps int64) string {
	return "Platform fee " + strconv.FormatFloat(float64(bps)/100, 'f', -1, 64) + "%"
}

type chargeRequestKey struct{}

// WithChargeRequest names the request a spend made with ctx is for, so its fee row names it (B32.11). An
// agent's hold and debit carry their own; this is for the workspace's own spending — the chat and the plan
// allowance's overflow — which carries none.
func WithChargeRequest(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, chargeRequestKey{}, requestID)
}

func chargeRequestFrom(ctx context.Context) string {
	id, _ := ctx.Value(chargeRequestKey{}).(string)
	return id
}

// insertPlatformFee writes the fee row: −fee (a refund of an over-estimated fee is +), balance after, in the
// caller's transaction. Its metadata names the request, the basis points and the spend it is on.
func insertPlatformFee(ctx context.Context, tx pgx.Tx, workspaceID string, fee, balanceAfter, bps, spend int64, requestID string) error {
	if fee == 0 {
		return nil
	}
	meta := map[string]interface{}{"platform_fee_bps": bps, "spend_ulxc": spend}
	if requestID != "" {
		meta["request_id"] = requestID
	}
	desc := FeeLabel(bps)
	if fee < 0 {
		desc += ", refunded with the estimate"
	}
	return insertLXCLedger(ctx, tx, workspaceID, -fee, balanceAfter, LXCTypePlatformFee, desc, meta)
}

// postAgentFee posts an agent key's platform fee as its own movement — agent → spend, kind platform_fee, naming
// the rate — inside the caller's transaction. It judges nothing: the hold or debit the fee belongs to already
// counted it against the agent's balance and limits. A key attached to no agent posts nothing.
func postAgentFee(ctx context.Context, tx pgx.Tx, scopedKeyID string, fee, bps int64, ref, model string) error {
	if fee == 0 {
		return nil
	}
	var agentID, workspaceID string
	err := tx.QueryRow(ctx,
		`SELECT a.id, a.workspace_id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		  WHERE k.scoped_key_id = $1`, scopedKeyID).Scan(&agentID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("economy: agent of key: %w", err)
	}
	return postFeeEntry(ctx, tx, workspaceID, agentID, fee, bps, ref, model)
}

// postFeeEntry is the platform_fee entry itself: fee from the agent to the spend side, naming the model the
// question asked for (its per-model caps count the fee too) and the rate. postModelEntry's insert, plus the
// rate: postings are append-only, so it is written with them.
func postFeeEntry(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, fee, bps int64, ref, model string) error {
	entry := uuid.New()
	for _, l := range []leg{{agentAccount(agentID), -fee}, {"spend", fee}} {
		if err := insertPosting(ctx, tx, `entry_id, workspace_id, account, amount_ulxc, kind, ref, model, fee_bps`,
			entry, workspaceID, l.account, l.amount, LXCTypePlatformFee, ref, nullIfEmpty(modelCapKey(model)), bps); err != nil {
			return fmt.Errorf("economy: post %s: %w", LXCTypePlatformFee, err)
		}
	}
	return nil
}
