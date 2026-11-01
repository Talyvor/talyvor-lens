package economy

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/nativepush"
)

// gas_budgets.go — B48.5: AN AGENT NEVER NEEDS THE NETWORK'S COIN. A paymaster pays its operations' network fees,
// within its workspace's budget for them on each chain (in wei of the chain's coin): LENS_GAS_BUDGET_WEI, plus every
// top-up the owner makes. A sponsored operation holds its most possible fee when it is sent and is charged what the
// chain took (smartaccount.Chain.SendSponsored and Settle); one the budget cannot cover is refused before it is sent,
// and the owner is asked once to top the budget up — asked again only after they have.

// ErrGasTopUp wraps a top-up that is not a positive amount of wei on a chain.
var ErrGasTopUp = errors.New("economy: a top-up is a positive amount of wei on a chain")

// SetGasBudgetDefault is each workspace's budget for sponsored network fees on each chain before it tops it up.
func (s *DualTokenStore) SetGasBudgetDefault(wei int64) { s.gasBudgetWei = max(wei, 0) }

// GasBudget is a workspace's budget for sponsored network fees on one chain, in wei of its coin.
type GasBudget struct {
	ChainID    int64 `json:"chain_id"`
	BudgetWei  int64 `json:"budget_wei"`  // the default and every top-up
	HeldWei    int64 `json:"held_wei"`    // set aside for operations on their way
	ChargedWei int64 `json:"charged_wei"` // what the chain took for the operations it has run
	LeftWei    int64 `json:"left_wei"`
	// TopUpAsked: a send was refused for want of budget and the owner was asked to top it up.
	TopUpAsked bool `json:"top_up_asked"`
}

// gasBudgetTx is workspaceID's budget on chainID in tx, its row locked for the rest of tx.
func (s *DualTokenStore) gasBudgetTx(ctx context.Context, tx pgx.Tx, workspaceID string, chainID int64) (GasBudget, error) {
	if chainID <= 0 {
		return GasBudget{}, fmt.Errorf("%w: chain %d", ErrGasTopUp, chainID)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO gas_budgets (workspace_id, chain_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		workspaceID, chainID); err != nil {
		return GasBudget{}, fmt.Errorf("economy: gas budget: %w", err)
	}
	b := GasBudget{ChainID: chainID}
	var asked *time.Time
	err := tx.QueryRow(ctx, `SELECT top_up_asked_at,
		$3::bigint + (SELECT COALESCE(sum(wei), 0) FROM gas_budget_top_ups WHERE workspace_id = $1 AND chain_id = $2),
		(SELECT COALESCE(sum(held_wei), 0) FROM gas_sponsorships
			WHERE workspace_id = $1 AND chain_id = $2 AND charged_wei IS NULL AND released_at IS NULL),
		(SELECT COALESCE(sum(charged_wei), 0) FROM gas_sponsorships WHERE workspace_id = $1 AND chain_id = $2)
		FROM gas_budgets WHERE workspace_id = $1 AND chain_id = $2 FOR UPDATE`,
		workspaceID, chainID, s.gasBudgetWei).Scan(&asked, &b.BudgetWei, &b.HeldWei, &b.ChargedWei)
	if err != nil {
		return GasBudget{}, fmt.Errorf("economy: gas budget: %w", err)
	}
	b.LeftWei, b.TopUpAsked = b.BudgetWei-b.HeldWei-b.ChargedWei, asked != nil
	return b, nil
}

// inGasTx runs f in a transaction and commits it.
func (s *DualTokenStore) inGasTx(ctx context.Context, f func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WorkspaceGasBudget is workspaceID's budget on chainID.
func (s *DualTokenStore) WorkspaceGasBudget(ctx context.Context, workspaceID string, chainID int64) (b GasBudget, err error) {
	err = s.inGasTx(ctx, func(tx pgx.Tx) error { b, err = s.gasBudgetTx(ctx, tx, workspaceID, chainID); return err })
	return b, err
}

// TopUpGasBudget adds wei to workspaceID's budget on chainID, by userID, and clears its ask to top up. Talyvor pays
// what the budget sponsors, so only Talyvor's admins top up (the handler) until a top-up is paid for.
func (s *DualTokenStore) TopUpGasBudget(ctx context.Context, workspaceID string, chainID, wei int64, userID string) (b GasBudget, err error) {
	if wei <= 0 {
		return GasBudget{}, fmt.Errorf("%w: %d wei", ErrGasTopUp, wei)
	}
	err = s.inGasTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.gasBudgetTx(ctx, tx, workspaceID, chainID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO gas_budget_top_ups (workspace_id, chain_id, wei, user_id) VALUES ($1, $2, $3, $4)`,
			workspaceID, chainID, wei, userID); err != nil {
			return fmt.Errorf("economy: top up the gas budget: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE gas_budgets SET top_up_asked_at = NULL WHERE workspace_id = $1 AND chain_id = $2`,
			workspaceID, chainID); err != nil {
			return fmt.Errorf("economy: top up the gas budget: %w", err)
		}
		b, err = s.gasBudgetTx(ctx, tx, workspaceID, chainID)
		return err
	})
	return b, err
}

// SponsoredGas is workspaceID's budget on chainID as a sponsored operation holds and is charged from it.
func (s *DualTokenStore) SponsoredGas(workspaceID string, chainID int64) smartaccount.GasBudget {
	return workspaceGas{s, workspaceID, chainID}
}

type workspaceGas struct {
	s           *DualTokenStore
	workspaceID string
	chainID     int64
}

func (g workspaceGas) Hold(ctx context.Context, hash string, fee *big.Int) error {
	var asked bool
	var refused error // committed with the ask, not rolled back by it
	err := g.s.inGasTx(ctx, func(tx pgx.Tx) error {
		b, err := g.s.gasBudgetTx(ctx, tx, g.workspaceID, g.chainID)
		if err != nil {
			return err
		}
		var released bool
		switch err := tx.QueryRow(ctx, `SELECT released_at IS NOT NULL FROM gas_sponsorships WHERE chain_id = $1 AND user_op_hash = $2 AND workspace_id = $3`,
			g.chainID, hash, g.workspaceID).Scan(&released); {
		case err == nil && !released:
			return nil // held, or charged, already
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("economy: gas hold: %w", err)
		}
		if !fee.IsInt64() || fee.Sign() <= 0 || fee.Int64() > b.LeftWei {
			if !b.TopUpAsked {
				if _, err := tx.Exec(ctx, `UPDATE gas_budgets SET top_up_asked_at = now() WHERE workspace_id = $1 AND chain_id = $2`,
					g.workspaceID, g.chainID); err != nil {
					return fmt.Errorf("economy: ask to top up: %w", err)
				}
				asked = true
			}
			refused = fmt.Errorf("%w: %d wei left on chain %d, and this send's network fee may be up to %s wei; the owner has been asked to top it up",
				smartaccount.ErrGasBudgetSpent, max(b.LeftWei, 0), g.chainID, fee)
			return nil
		}
		// A released operation sent again is held again, against what is left now.
		_, err = tx.Exec(ctx, `INSERT INTO gas_sponsorships (chain_id, user_op_hash, workspace_id, held_wei) VALUES ($1, $2, $3, $4)
			ON CONFLICT (chain_id, user_op_hash) DO UPDATE SET held_wei = EXCLUDED.held_wei, released_at = NULL
			WHERE gas_sponsorships.workspace_id = EXCLUDED.workspace_id AND gas_sponsorships.released_at IS NOT NULL`,
			g.chainID, hash, g.workspaceID, fee.Int64())
		return err
	})
	if err != nil {
		return err
	}
	if asked {
		g.s.notifyGasBudgetSpent(g.workspaceID, g.chainID)
	}
	return refused
}

func (g workspaceGas) Release(ctx context.Context, hash string) error {
	_, err := g.s.pool.Exec(ctx, `UPDATE gas_sponsorships SET released_at = now()
		WHERE chain_id = $1 AND user_op_hash = $2 AND workspace_id = $3 AND charged_wei IS NULL AND released_at IS NULL`,
		g.chainID, hash, g.workspaceID)
	return err
}

func (g workspaceGas) Charge(ctx context.Context, hash string, charged *big.Int) error {
	if !charged.IsInt64() || charged.Sign() < 0 {
		return fmt.Errorf("economy: the chain charged %s wei for %s", charged, hash)
	}
	// A released operation is charged too: a bundler that answered with an error may still have put it on the chain.
	tag, err := g.s.pool.Exec(ctx, `UPDATE gas_sponsorships SET charged_wei = $4, settled_at = now(), released_at = NULL
		WHERE chain_id = $1 AND user_op_hash = $2 AND workspace_id = $3 AND charged_wei IS NULL`,
		g.chainID, hash, g.workspaceID, charged.Int64())
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var done bool // charged before: nothing changes
	if err := g.s.pool.QueryRow(ctx, `SELECT charged_wei IS NOT NULL FROM gas_sponsorships WHERE chain_id = $1 AND user_op_hash = $2 AND workspace_id = $3`,
		g.chainID, hash, g.workspaceID).Scan(&done); err != nil || !done {
		return fmt.Errorf("economy: no hold for %s on chain %d to charge", hash, g.chainID)
	}
	return nil
}

// notifyGasBudgetSpent asks the workspace's phones, in the background, to top up its budget for network fees.
func (s *DualTokenStore) notifyGasBudgetSpent(workspaceID string, chainID int64) {
	if s.nativePusher == nil {
		return
	}
	m := nativepush.Message{Title: "Network fees budget used up",
		Body: "Your agents' budget for network fees is used up, so their next payments can't be sent. Top it up to carry on.",
		Data: map[string]string{"type": "gas_budget_spent", "workspace_id": workspaceID, "chain_id": strconv.FormatInt(chainID, 10)}}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.pushDevices(ctx, workspaceID, m)
	}()
}
