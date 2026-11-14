package economy

import (
	"context"
	"errors"
	"fmt"
)

// money_live_key.go — B58.7: NO LIVE PAYMENT BY A PERSON UNTIL THE WORKSPACE HAS A KEY.
//
// A person's signature is asked for only once the workspace has a passkey or a phone key (B54.33), so a workspace with
// none paid on its web session alone. Live money now waits for a live key: a person's payment to a payee, a payout batch
// approved, a pull under a mandate and an agent's request approved, each of live money, is refused before anything is
// posted while the workspace has no passkey and no phone key that can sign (none removed or revoked). Test money is
// unchanged, so a workspace with no key keeps paying in the Sandbox.

// ErrKeyBeforeLivePayment: live money moves at a person's word and the workspace has no live passkey or phone key.
var ErrKeyBeforeLivePayment = errors.New("Add a passkey or your phone before your first payment.") //nolint:staticcheck // ST1005: shown to a person as this sentence (B58.7)

// liveKeyFirst refuses funding that is live while workspaceID has no live passkey and no live device key.
func liveKeyFirst(ctx context.Context, q pgxDB, workspaceID, funding string) error {
	if funding != FundingLive {
		return nil
	}
	var live bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_passkeys WHERE workspace_id = $1 AND removed_at IS NULL AND revoked_at IS NULL)
		OR EXISTS (SELECT 1 FROM workspace_device_keys WHERE workspace_id = $1 AND revoked_at IS NULL)`, workspaceID).Scan(&live); err != nil {
		return fmt.Errorf("economy: passkeys: %w", err)
	}
	if !live {
		return ErrKeyBeforeLivePayment
	}
	return nil
}

// approvalsLiveKeyFirst is liveKeyFirst for approving ids: refused when any of them is paid from live money.
func approvalsLiveKeyFirst(ctx context.Context, q pgxDB, workspaceID string, ids []string) error {
	var live bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_approvals WHERE workspace_id = $1 AND id = ANY($2) AND paid_from_funding = $3)`,
		workspaceID, ids, FundingLive).Scan(&live); err != nil {
		return fmt.Errorf("economy: decide approval: %w", err)
	}
	if !live {
		return nil
	}
	return liveKeyFirst(ctx, q, workspaceID, FundingLive)
}
