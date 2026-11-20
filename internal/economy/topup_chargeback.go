package economy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LXCTypeTopUpChargeback marks the credits a charged-back top-up takes back (B30.92).
const LXCTypeTopUpChargeback = "top_up_chargeback"

// ReverseTopUpTx takes back, on the caller's tx, the credits of a top-up whose payment was charged back:
// lxcAmount µLXC from what the workspace holds itself and has not spent, never what its agents hold and never
// below zero. A test top-up (test) takes test-funded credits, a live one the rest. It also removes backing µLXC
// of the workspace's cash backing, so the returned money funds no royalty. It writes one lxc_ledger row of
// LXCTypeTopUpChargeback for what it took, with what it could not (already spent) as unrecovered_ulxc, and
// answers what it took. Billing calls it and never writes lxc_ledger or lxc_balances itself.
func ReverseTopUpTx(ctx context.Context, tx pgx.Tx, workspaceID string, lxcAmount, backing int64, test bool, reason string, metadata map[string]interface{}) (int64, error) {
	if lxcAmount <= 0 {
		return 0, errors.New("economy: a chargeback amount must be positive")
	}
	bal, minted, spent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	var allocated, testFunded int64
	if err := tx.QueryRow(ctx, allocatedSQL, workspaceID).Scan(&allocated); err != nil {
		return 0, fmt.Errorf("economy: allocated LXC: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT test_funded_ulxc FROM lxc_balances WHERE workspace_id = $1`, workspaceID).Scan(&testFunded); err != nil {
		return 0, fmt.Errorf("economy: test-funded credits: %w", err)
	}
	own := max(bal-allocated, 0)
	unspent := max(own-testFunded, 0) // a live top-up's credits are the ones not test-funded
	if test {
		unspent = min(own, testFunded)
	}
	taken := min(lxcAmount, unspent)
	if taken > 0 {
		// The test-funded part comes off before the balance falls, which would otherwise clamp it a second time.
		if test {
			if _, err := tx.Exec(ctx, `UPDATE lxc_balances SET test_funded_ulxc = GREATEST(test_funded_ulxc - $2, 0) WHERE workspace_id = $1`,
				workspaceID, taken); err != nil {
				return 0, fmt.Errorf("economy: take back test-funded credits: %w", err)
			}
		}
		meta := make(map[string]interface{}, len(metadata)+1)
		for k, v := range metadata {
			meta[k] = v
		}
		meta["unrecovered_ulxc"] = lxcAmount - taken
		if err := insertLXCLedger(ctx, tx, workspaceID, -taken, bal-taken, LXCTypeTopUpChargeback, reason, meta); err != nil {
			return 0, err
		}
		if err := writeLXCBalance(ctx, tx, workspaceID, bal-taken, minted, spent); err != nil {
			return 0, err
		}
	}
	if err := ReduceCashBackedForRefund(ctx, tx, workspaceID, backing); err != nil {
		return 0, err
	}
	return taken, nil
}
