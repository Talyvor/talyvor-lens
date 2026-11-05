package economy

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	tokens "github.com/talyvor/lens/internal/money/holdings"
)

// wallet_statements.go — B48.14: HISTORY, STATEMENTS AND TAX EXPORTS FOR EVERY WALLET.
//
// A wallet's history is the money ledger's (B30.2): every posting on one of the owner's money accounts — an agent's,
// or, for the owner, every account the workspace and its agents hold — with what moved it: a payment in or out, a card,
// a fee, a move between accounts, and each movement on a chain (B48.6), with its network, transaction and the address
// on the other side. A posting is never changed or deleted, so a closed month's statement re-derives the same lines and
// the same closing balances whenever it is asked for, and each closing balance is the account's balance in the ledger.
//
// The accountant's export is worked from the movements on the chain: each non-stable token is a pool of lots, first in
// first out, across the owner's wallets and networks. A movement in acquires a lot at its value then; a movement out
// disposes of the oldest lots first, its value then the proceeds and those lots' cost its basis. A move between two of
// the owner's own wallets is neither. Values are in the owner's currency, as each movement was valued when it moved
// (0326); a movement without one is listed as unpriced, not valued at zero.

// WalletLine is one posting on one of a wallet's accounts.
type WalletLine struct {
	EntryID           string    `json:"entry_id"`
	Line              int       `json:"line"`
	At                time.Time `json:"at"`
	AccountID         string    `json:"account_id"`
	AccountName       string    `json:"account_name"` // chain:<network> for a wallet on a chain
	AgentID           string    `json:"agent_id,omitempty"`
	Currency          string    `json:"currency"`
	Kind              string    `json:"kind"` // the entry's: payment_in, card_charge, chain_deposit, ...
	AmountMinor       int64     `json:"amount_minor"`
	Funding           string    `json:"funding"`
	Counterparty      string    `json:"counterparty,omitempty"` // the payer or payee outside; on a chain, the other address
	Ref               string    `json:"ref,omitempty"`          // the partner's reference; on a chain, <transaction>:<log>
	Memo              string    `json:"memo,omitempty"`
	BalanceAfterMinor int64     `json:"balance_after_minor"`
}

// WalletAccount is one account's totals over a statement's period: Opening + In − Out = Closing.
type WalletAccount struct {
	AccountID    string `json:"account_id"`
	AccountName  string `json:"account_name"`
	AgentID      string `json:"agent_id,omitempty"`
	Purpose      string `json:"purpose"`
	Currency     string `json:"currency"`
	OpeningMinor int64  `json:"opening_minor"`
	InMinor      int64  `json:"in_minor"`
	OutMinor     int64  `json:"out_minor"`
	ClosingMinor int64  `json:"closing_minor"`
}

// WalletStatement is an agent's wallets, or all the owner's, over [From, To).
type WalletStatement struct {
	WorkspaceID string          `json:"workspace_id"`
	AgentID     string          `json:"agent_id,omitempty"`
	From        time.Time       `json:"from"`
	To          time.Time       `json:"to"`
	Accounts    []WalletAccount `json:"accounts"`
	Lines       []WalletLine    `json:"lines"`
}

// walletAccounts is the accounts that are the owner's money — not a partner's mirror, money not yet matched, or
// Talyvor's fees — of agentID, or of the whole workspace when agentID is "". $1 is the workspace, $2 the agent.
const walletAccounts = `SELECT id FROM money_accounts WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2)
	AND purpose NOT IN ('partner', 'suspense', 'revenue')`

// WalletStatement is agentID's wallets — every wallet the owner has, when agentID is "" — over [from, to).
func (s *DualTokenStore) WalletStatement(ctx context.Context, workspaceID, agentID string, from, to time.Time) (WalletStatement, error) {
	st := WalletStatement{WorkspaceID: workspaceID, AgentID: agentID, From: from, To: to, Accounts: []WalletAccount{}, Lines: []WalletLine{}}
	if err := s.walletAgent(ctx, workspaceID, agentID); err != nil {
		return st, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, COALESCE(a.agent_id, ''), a.purpose, a.currency,
		       COALESCE(sum(p.amount_minor) FILTER (WHERE e.created_at < $3), 0)::bigint,
		       COALESCE(sum(p.amount_minor) FILTER (WHERE e.created_at >= $3 AND e.created_at < $4 AND p.amount_minor > 0), 0)::bigint,
		       COALESCE(-sum(p.amount_minor) FILTER (WHERE e.created_at >= $3 AND e.created_at < $4 AND p.amount_minor < 0), 0)::bigint
		  FROM money_accounts a
		  LEFT JOIN money_postings p ON p.account_id = a.id
		  LEFT JOIN money_entries e ON e.id = p.entry_id
		 WHERE a.id IN (`+walletAccounts+`) AND a.created_at < $4
		 GROUP BY a.id ORDER BY COALESCE(a.agent_id, ''), a.currency, a.name, a.id`, workspaceID, agentID, from, to)
	if err != nil {
		return st, fmt.Errorf("economy: wallet statement totals: %w", err)
	}
	opening := map[string]int64{}
	for rows.Next() {
		var a WalletAccount
		if err := rows.Scan(&a.AccountID, &a.AccountName, &a.AgentID, &a.Purpose, &a.Currency, &a.OpeningMinor, &a.InMinor, &a.OutMinor); err != nil {
			rows.Close()
			return st, err
		}
		a.ClosingMinor = a.OpeningMinor + a.InMinor - a.OutMinor
		opening[a.AccountID] = a.OpeningMinor
		st.Accounts = append(st.Accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("economy: wallet statement totals: %w", err)
	}
	st.Lines, err = walletLines(s.pool.Query(ctx, walletPeriodLinesSQL, workspaceID, agentID, from, to))
	for i := range st.Lines {
		st.Lines[i].BalanceAfterMinor += opening[st.Lines[i].AccountID]
	}
	return st, err
}

// WalletActivity is agentID's wallets' — every wallet the owner has, when agentID is "" — limit latest postings,
// newest first, each with its account's balance after it.
func (s *DualTokenStore) WalletActivity(ctx context.Context, workspaceID, agentID string, limit int) ([]WalletLine, error) {
	if err := s.walletAgent(ctx, workspaceID, agentID); err != nil {
		return nil, err
	}
	// shortcut: the newest limit postings, no cursor to page further back; the monthly statements cover the rest.
	lines, err := walletLines(s.pool.Query(ctx, walletLatestLinesSQL, workspaceID, agentID, limit))
	if err != nil {
		return nil, err
	}
	// Each balance is the account's running sum from its first posting, which a window over the newest rows cannot see.
	for i := range lines {
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0)::bigint FROM money_postings p
			JOIN money_entries e ON e.id = p.entry_id
			WHERE p.account_id = $1 AND (e.created_at, e.id, p.line) <= ($2, $3, $4)`,
			lines[i].AccountID, lines[i].At, lines[i].EntryID, lines[i].Line).Scan(&lines[i].BalanceAfterMinor); err != nil {
			return nil, fmt.Errorf("economy: wallet activity balance: %w", err)
		}
	}
	return lines, nil
}

// walletAgent is ErrAgentNotFound unless agentID is "" or one of workspaceID's agents.
func (s *DualTokenStore) walletAgent(ctx context.Context, workspaceID, agentID string) error {
	if agentID == "" {
		return nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return fmt.Errorf("economy: wallet: %w", err)
	}
	if !exists {
		return ErrAgentNotFound
	}
	return nil
}

// walletLinesSQL reads the postings on the wallet's accounts; each query below adds which, and in what order. Each
// balance it reads is the running sum of the lines read, in time order; the caller adds what came before them.
const walletLinesSQL = `
		SELECT e.id, p.line, e.created_at, a.id, a.name, COALESCE(a.agent_id, ''), p.currency, e.kind, p.amount_minor, p.funding,
		       e.counterparty, e.partner_ref, e.memo,
		       sum(p.amount_minor) OVER (PARTITION BY a.id ORDER BY e.created_at, e.id, p.line)::bigint
		  FROM money_postings p
		  JOIN money_entries e ON e.id = p.entry_id
		  JOIN money_accounts a ON a.id = p.account_id
		 WHERE a.id IN (` + walletAccounts + `) AND `

const (
	walletPeriodLinesSQL = walletLinesSQL + `e.created_at >= $3 AND e.created_at < $4 ORDER BY e.created_at, e.id, p.line`
	walletLatestLinesSQL = walletLinesSQL + `true ORDER BY e.created_at DESC, e.id DESC, p.line DESC LIMIT $3`
)

// walletLines reads the lines one of the two queries above found.
func walletLines(rows pgx.Rows, err error) ([]WalletLine, error) {
	if err != nil {
		return nil, fmt.Errorf("economy: wallet lines: %w", err)
	}
	defer rows.Close()
	out := []WalletLine{}
	for rows.Next() {
		var l WalletLine
		if err := rows.Scan(&l.EntryID, &l.Line, &l.At, &l.AccountID, &l.AccountName, &l.AgentID, &l.Currency, &l.Kind, &l.AmountMinor,
			&l.Funding, &l.Counterparty, &l.Ref, &l.Memo, &l.BalanceAfterMinor); err != nil {
			return nil, err
		}
		l.At = l.At.UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// stableAssets are the tokens that hold their value against a currency: disposing of one is not a gain or a loss.
var stableAssets = map[string]bool{"USDC": true, "EURC": true, "USDT": true}

// TaxMovement is one movement of a non-stable token on the chain, as the tax export reads it.
type TaxMovement struct {
	ID            string    `json:"id"`
	At            time.Time `json:"at"`
	AgentID       string    `json:"agent_id"`
	Network       string    `json:"network"`
	TxHash        string    `json:"tx_hash"`
	Asset         string    `json:"asset"`
	Direction     string    `json:"direction"` // in: an acquisition; out: a disposal
	QuantityMinor int64     `json:"quantity_minor"`
	ValueMinor    *int64    `json:"value_minor"` // in ValueCurrency's minor units, when it moved; nil = not priced
	ValueCurrency string    `json:"value_currency,omitempty"`
}

// TaxDisposal is one disposal of a non-stable token and its gain or loss, first in, first out.
type TaxDisposal struct {
	TaxMovement
	ProceedsMinor  int64 `json:"proceeds_minor"`
	CostBasisMinor int64 `json:"cost_basis_minor"`
	GainMinor      int64 `json:"gain_minor"` // proceeds − cost basis; a loss is negative
	// NoBasisQuantityMinor is how much of it came from no acquisition the export saw, so has no cost basis.
	NoBasisQuantityMinor int64 `json:"no_basis_quantity_minor,omitempty"`
}

// TaxExport is the accountant's export of the owner's disposals in [From, To), in Currency.
type TaxExport struct {
	WorkspaceID    string        `json:"workspace_id"`
	Currency       string        `json:"currency"`
	Method         string        `json:"method"` // FIFO
	From           time.Time     `json:"from"`
	To             time.Time     `json:"to"`
	Disposals      []TaxDisposal `json:"disposals"`
	ProceedsMinor  int64         `json:"proceeds_minor"`
	CostBasisMinor int64         `json:"cost_basis_minor"`
	GainMinor      int64         `json:"gain_minor"`
	// Unpriced is each movement up to To with no value in Currency: the export leaves it out, and the accountant
	// values it.
	Unpriced []TaxMovement `json:"unpriced"`
}

// WalletTaxExport is the owner's accountant export for [from, to): every disposal of a non-stable token from any of
// their agents' wallets, with its cost basis first in, first out, and its gain or loss, in their currency.
func (s *DualTokenStore) WalletTaxExport(ctx context.Context, workspaceID string, from, to time.Time) (TaxExport, error) {
	country, err := ownerCountry(ctx, s.pool, workspaceID)
	if err != nil {
		return TaxExport{}, err
	}
	// Posted and not reversed (B48.6), up to the period's end: a lot bought before the period is the basis of a sale in it.
	// A move between two of the owner's own wallets is neither an acquisition nor a disposal.
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.seen_at, m.agent_id, m.network, m.tx_hash, m.asset, m.direction, m.amount_minor, m.value_minor, m.value_currency
		  FROM chain_movements m
		 WHERE m.workspace_id = $1 AND m.status IN ('confirmed', 'final') AND m.seen_at < $2 AND NOT (m.asset = ANY($3))
		   AND NOT EXISTS (SELECT 1 FROM agent_smart_accounts o WHERE o.workspace_id = $1 AND lower(o.address) = lower(m.counterparty))
		 ORDER BY m.seen_at, m.id`, workspaceID, to, stableList())
	if err != nil {
		return TaxExport{}, fmt.Errorf("economy: tax export: %w", err)
	}
	defer rows.Close()
	var moves []TaxMovement
	for rows.Next() {
		var m TaxMovement
		if err := rows.Scan(&m.ID, &m.At, &m.AgentID, &m.Network, &m.TxHash, &m.Asset, &m.Direction, &m.QuantityMinor, &m.ValueMinor, &m.ValueCurrency); err != nil {
			return TaxExport{}, err
		}
		m.At = m.At.UTC()
		moves = append(moves, m)
	}
	if err := rows.Err(); err != nil {
		return TaxExport{}, fmt.Errorf("economy: tax export: %w", err)
	}
	x := FIFOGains(moves, tokens.CurrencyFor(country), from, to)
	x.WorkspaceID = workspaceID
	return x, nil
}

func stableList() []string {
	out := make([]string, 0, len(stableAssets))
	for a := range stableAssets {
		out = append(out, a)
	}
	return out
}

// FIFOGains works out, from moves in time order, each disposal in [from, to) and its gain in currency: every
// asset a pool of lots, a disposal taking the oldest first. A movement not valued in currency is left out and listed.
func FIFOGains(moves []TaxMovement, currency string, from, to time.Time) TaxExport {
	type lot struct{ qty, cost int64 }
	pools := map[string][]lot{}
	x := TaxExport{Currency: currency, Method: "FIFO", From: from, To: to, Disposals: []TaxDisposal{}, Unpriced: []TaxMovement{}}
	sort.SliceStable(moves, func(i, j int) bool { return moves[i].At.Before(moves[j].At) })
	for _, m := range moves {
		if !m.At.Before(to) {
			break
		}
		if m.ValueMinor == nil || !strings.EqualFold(m.ValueCurrency, currency) {
			x.Unpriced = append(x.Unpriced, m)
			continue
		}
		if m.Direction == "in" {
			pools[m.Asset] = append(pools[m.Asset], lot{m.QuantityMinor, *m.ValueMinor})
			continue
		}
		d := TaxDisposal{TaxMovement: m, ProceedsMinor: *m.ValueMinor}
		left, pool := m.QuantityMinor, pools[m.Asset]
		for left > 0 && len(pool) > 0 {
			take := min(left, pool[0].qty)
			// The lot's cost in proportion to what is taken; the whole of what is left when it is used up, so no part of
			// a lot's cost is lost to rounding.
			cost := pool[0].cost
			if take < pool[0].qty {
				c := new(big.Int).Mul(big.NewInt(pool[0].cost), big.NewInt(take))
				cost = c.Quo(c, big.NewInt(pool[0].qty)).Int64()
			}
			d.CostBasisMinor += cost
			pool[0].qty, pool[0].cost, left = pool[0].qty-take, pool[0].cost-cost, left-take
			if pool[0].qty == 0 {
				pool = pool[1:]
			}
		}
		pools[m.Asset] = pool
		d.NoBasisQuantityMinor = left
		d.GainMinor = d.ProceedsMinor - d.CostBasisMinor
		if m.At.Before(from) { // a disposal before the period still used up its lots
			continue
		}
		x.Disposals = append(x.Disposals, d)
		x.ProceedsMinor += d.ProceedsMinor
		x.CostBasisMinor += d.CostBasisMinor
		x.GainMinor += d.GainMinor
	}
	return x
}
