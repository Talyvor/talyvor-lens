package economy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
)

// workspace_money_kind.go — B59.6: A WORKSPACE IS BUSINESS OR PERSONAL, AND FIAT ONLY, STABLECOIN AND CRYPTO ONLY, OR
// HYBRID (launch pack 3.1 and 3.5).
//
// Both are set when the workspace is made (migration 0373; the provision route names them). The type never changes.
// The kind is the limit for its agents' wallet kinds (B48.1): a fiat workspace's agents are fiat, a stablecoin
// workspace's crypto, and a hybrid workspace's agents may be any, a new one holding both a fiat and a stablecoin
// balance. A single kind becomes hybrid at any time, and its agents then hold both; hybrid becomes a single kind only
// once the other side's balances are all zero, and is refused naming them otherwise. Every change is in the audit trail.

// The two account types.
const (
	AccountTypeBusiness = "business"
	AccountTypePersonal = "personal"
)

// The three kinds of money a workspace holds.
const (
	MoneyKindFiat       = "fiat"
	MoneyKindStablecoin = "stablecoin"
	MoneyKindHybrid     = "hybrid"
)

var (
	// ErrAccountType: a type that is not business or personal.
	ErrAccountType = errors.New("economy: an account is business or personal")
	// ErrMoneyKind: a kind that is not fiat, stablecoin or hybrid.
	ErrMoneyKind = errors.New("economy: an account's money is fiat, stablecoin or hybrid")
	// ErrWorkspaceNotFound: no workspace by that id.
	ErrWorkspaceNotFound = errors.New("economy: no such workspace")
)

// WorkspaceAccount is a workspace's type and kind of money.
type WorkspaceAccount struct {
	WorkspaceID string `json:"workspace_id"`
	AccountType string `json:"account_type"`
	MoneyKind   string `json:"money_kind"`
}

// MoneyKindRefusal refuses a change of kind, or an agent's wallet kind, that the workspace's kind does not allow: Field
// names what was refused, MoneyKind the workspace's kind, and Balances, on a change to a single kind, the other side's
// balances still to empty, in minor units by asset.
type MoneyKindRefusal struct {
	Field     string           `json:"field"`
	MoneyKind string           `json:"money_kind"`
	Balances  map[string]int64 `json:"balances,omitempty"`
}

func (r *MoneyKindRefusal) Error() string {
	if len(r.Balances) > 0 {
		assets := make([]string, 0, len(r.Balances))
		for a := range r.Balances {
			assets = append(assets, a)
		}
		sort.Strings(assets)
		held := make([]string, 0, len(assets))
		for _, a := range assets {
			held = append(held, minorString(r.Balances[a], a))
		}
		return "economy: the account still holds " + strings.Join(held, ", ") + ": move it out first"
	}
	return fmt.Sprintf("economy: a %s account cannot have that %s", r.MoneyKind, strings.ReplaceAll(r.Field, "_", " "))
}

// CheckAccountType refuses a type that is not business or personal.
func CheckAccountType(t string) error {
	if t != AccountTypeBusiness && t != AccountTypePersonal {
		return fmt.Errorf("%w, not %q", ErrAccountType, t)
	}
	return nil
}

// CheckMoneyKind refuses a kind that is not fiat, stablecoin or hybrid.
func CheckMoneyKind(k string) error {
	if k != MoneyKindFiat && k != MoneyKindStablecoin && k != MoneyKindHybrid {
		return fmt.Errorf("%w, not %q", ErrMoneyKind, k)
	}
	return nil
}

// agentKindOf is the wallet kind of a new agent in a workspace of kind.
func agentKindOf(kind string) string {
	switch kind {
	case MoneyKindFiat:
		return WalletKindFiat
	case MoneyKindStablecoin:
		return WalletKindCrypto
	}
	return WalletKindBoth
}

// walletKindAllowed is whether an agent of walletKind may be in a workspace of kind.
func walletKindAllowed(kind, walletKind string) bool {
	return kind == MoneyKindHybrid || agentKindOf(kind) == walletKind
}

// workspaceMoneyKind is workspaceID's kind of money; a workspace with no row is hybrid, as every one made before B59.6.
func workspaceMoneyKind(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, workspaceID string) (string, error) {
	var kind string
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT money_kind FROM workspaces WHERE id = $1), 'hybrid')`, workspaceID).Scan(&kind); err != nil {
		return "", fmt.Errorf("economy: workspace's kind of money: %w", err)
	}
	return kind, nil
}

// checkAgentWalletKind refuses an agent of walletKind in workspaceID unless its kind of money allows it.
func checkAgentWalletKind(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, workspaceID, walletKind string) error {
	kind, err := workspaceMoneyKind(ctx, q, workspaceID)
	if err != nil {
		return err
	}
	if !walletKindAllowed(kind, walletKind) {
		return &MoneyKindRefusal{Field: "wallet_kind", MoneyKind: kind}
	}
	return nil
}

// WorkspaceAccount is workspaceID's type and kind of money.
func (s *DualTokenStore) WorkspaceAccount(ctx context.Context, workspaceID string) (WorkspaceAccount, error) {
	out := WorkspaceAccount{WorkspaceID: workspaceID}
	err := s.pool.QueryRow(ctx, `SELECT account_type, money_kind FROM workspaces WHERE id = $1`, workspaceID).Scan(&out.AccountType, &out.MoneyKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceAccount{}, ErrWorkspaceNotFound
	}
	if err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: workspace account: %w", err)
	}
	return out, nil
}

// SetWorkspaceMoneyKind changes workspaceID's kind of money to kind, by the person by. A single kind becomes hybrid, and
// every agent of it then holds both; hybrid becomes a single kind only while the other side holds nothing, its agents
// then holding that kind alone. Any other change is refused with a *MoneyKindRefusal, as is one with money left on the
// other side. Asking for the kind it has changes nothing.
func (s *DualTokenStore) SetWorkspaceMoneyKind(ctx context.Context, workspaceID, kind, by string) (WorkspaceAccount, error) {
	if err := CheckMoneyKind(kind); err != nil {
		return WorkspaceAccount{}, err
	}
	was, err := s.WorkspaceAccount(ctx, workspaceID)
	if err != nil {
		return WorkspaceAccount{}, err
	}
	switch {
	case was.MoneyKind == kind:
		return was, nil
	case kind == MoneyKindHybrid:
		return s.widenMoneyKind(ctx, was, by)
	case was.MoneyKind == MoneyKindHybrid:
		return s.narrowMoneyKind(ctx, was, kind, by)
	}
	return WorkspaceAccount{}, &MoneyKindRefusal{Field: "money_kind", MoneyKind: was.MoneyKind} // fiat to stablecoin goes through hybrid
}

// widenMoneyKind makes was's workspace hybrid, and each of its agents both: a wallet an agent gains opens when it is
// first read.
func (s *DualTokenStore) widenMoneyKind(ctx context.Context, was WorkspaceAccount, by string) (WorkspaceAccount, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockWorkspaceAgents(ctx, tx, was.WorkspaceID); err != nil {
		return WorkspaceAccount{}, err
	}
	rows, err := tx.Query(ctx, `UPDATE agent_accounts SET wallet_kind = 'both'
		WHERE workspace_id = $1 AND kind = 'agent' AND archived_at IS NULL AND wallet_kind <> 'both' RETURNING id`, was.WorkspaceID)
	if err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
	}
	agents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
	}
	if err := setMoneyKindTx(ctx, tx, was, MoneyKindHybrid, by, agents); err != nil {
		return WorkspaceAccount{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
	}
	was.MoneyKind = MoneyKindHybrid
	return was, nil
}

// narrowMoneyKind makes was's hybrid workspace kind, refused while the other side's accounts — the agents' wallets and
// currency accounts, the owner's, pots and holds — hold anything. Each agent becomes kind's wallet kind, its other
// wallet closed at its partner (B48.1), before the workspace changes, so a failure part way leaves it hybrid.
func (s *DualTokenStore) narrowMoneyKind(ctx context.Context, was WorkspaceAccount, kind, by string) (WorkspaceAccount, error) {
	held, err := s.otherSideBalances(ctx, was.WorkspaceID, kind)
	if err != nil {
		return WorkspaceAccount{}, err
	}
	if len(held) > 0 {
		return WorkspaceAccount{}, &MoneyKindRefusal{Field: "money_kind", MoneyKind: was.MoneyKind, Balances: held}
	}
	walletKind := agentKindOf(kind)
	rows, err := s.pool.Query(ctx, `SELECT id FROM agent_accounts WHERE workspace_id = $1 AND kind = 'agent' AND archived_at IS NULL
		AND wallet_kind <> $2 ORDER BY id`, was.WorkspaceID, walletKind)
	if err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
	}
	agents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
	}
	for _, id := range agents {
		if s.moneyLayer == nil { // no money layer, no wallets to close
			if _, err := s.pool.Exec(ctx, `UPDATE agent_accounts SET wallet_kind = $2 WHERE id = $1`, id, walletKind); err != nil {
				return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
			}
			continue
		}
		if _, err := s.SetAgentWalletKind(ctx, was.WorkspaceID, id, walletKind, ""); err != nil {
			return WorkspaceAccount{}, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setMoneyKindTx(ctx, tx, was, kind, by, agents); err != nil {
		return WorkspaceAccount{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspaceAccount{}, fmt.Errorf("economy: change the kind of money: %w", err)
	}
	was.MoneyKind = kind
	return was, nil
}

// setMoneyKindTx records was's workspace as kind in tx, and the change, with the agents it changed, in the audit trail.
func setMoneyKindTx(ctx context.Context, tx pgx.Tx, was WorkspaceAccount, kind, by string, agents []string) error {
	tag, err := tx.Exec(ctx, `UPDATE workspaces SET money_kind = $2, updated_at = now() WHERE id = $1 AND money_kind = $3`,
		was.WorkspaceID, kind, was.MoneyKind)
	if err != nil {
		return fmt.Errorf("economy: change the kind of money: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &MoneyKindRefusal{Field: "money_kind", MoneyKind: was.MoneyKind} // changed under us: ask again
	}
	if agents == nil {
		agents = []string{}
	}
	return appendAudit(ctx, tx, was.WorkspaceID, "workspace:"+was.WorkspaceID, "workspace.money_kind",
		map[string]any{"from": was.MoneyKind, "to": kind, "by": by, "agents": agents})
}

// otherSideBalances is what workspaceID's open accounts hold, by asset, on the side a workspace of kind gives up: the
// stablecoins for fiat, every other currency for stablecoin. Only assets holding something are named.
func (s *DualTokenStore) otherSideBalances(ctx context.Context, workspaceID, kind string) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.currency, sum(b.balance_minor)::bigint FROM money_accounts a
		JOIN money_account_balances b ON b.account_id = a.id
		WHERE a.workspace_id = $1 AND a.status = 'open' AND a.purpose IN ('company', 'agent', 'pot', 'hold')
		AND (a.currency = ANY($2)) = $3
		GROUP BY a.currency HAVING sum(b.balance_minor) <> 0`, workspaceID, []string{money.USDC}, kind == MoneyKindFiat)
	if err != nil {
		return nil, fmt.Errorf("economy: the other side's balances: %w", err)
	}
	defer rows.Close()
	held := map[string]int64{}
	for rows.Next() {
		var asset string
		var minor int64
		if err := rows.Scan(&asset, &minor); err != nil {
			return nil, err
		}
		held[asset] = minor
	}
	return held, rows.Err()
}
