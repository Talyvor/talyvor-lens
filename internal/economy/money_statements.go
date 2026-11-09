package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// money_statements.go — B56.14: EVERY PAYMENT AN AGENT MAKES REACHES A STATEMENT.
//
// A currency account's statement covers the half-open period [From, To), as a credit statement does
// (agent_statements.go), and is built from money_postings alone: append-only, so a closed period asked again re-derives
// the same lines and totals. Every posting to the account is a line, whatever moved it — a payment in or out, an invoice
// payment, a mandate pull, a payout, a card settlement, a return — and a posting made by another company's entry (a
// mandate pull's payee, an invoice's issuer) is a line on this account's statement too.
//
// A posting belongs to the period its entry's created_at falls in, and lines are ordered by (created_at, entry, line),
// so a line's balance is the account's opening balance plus every line before it. Test and live money are one balance,
// as Balance reads it; each line says which it was.

// MoneyStatementLine is one posting to the account.
type MoneyStatementLine struct {
	EntryID           string    `json:"entry_id"`
	Line              int       `json:"line"`
	At                time.Time `json:"at"`
	Kind              string    `json:"kind"`         // the entry's: payment_in, invoice_payment, mandate_pull, card_charge, …
	AmountMinor       int64     `json:"amount_minor"` // money in positive, money out negative
	Funding           string    `json:"funding"`
	Counterparty      string    `json:"counterparty"` // who an outside payment was from or to; else the other account, or "workspace:<id>"
	Memo              string    `json:"memo,omitempty"`
	PartnerRef        string    `json:"partner_ref,omitempty"`
	BalanceAfterMinor int64     `json:"balance_after_minor"`
}

// MoneyStatement is one currency account over [From, To): Opening + In − Out = Closing, in its currency's minor units.
type MoneyStatement struct {
	WorkspaceID  string               `json:"workspace_id"`
	AccountID    string               `json:"account_id"`
	AgentID      string               `json:"agent_id,omitempty"`
	Currency     string               `json:"currency"`
	From         time.Time            `json:"from"`
	To           time.Time            `json:"to"`
	OpeningMinor int64                `json:"opening_minor"`
	InMinor      int64                `json:"in_minor"`
	OutMinor     int64                `json:"out_minor"`
	ClosingMinor int64                `json:"closing_minor"`
	Lines        []MoneyStatementLine `json:"lines"`
}

// AccountStatement is workspaceID's company, agent or suspense (B30.15) account accountID over [from, to). With agentID
// it is only that agent's account: another's, or the company's, is ErrMoneyAccountNotFound, as for its details.
func (s *DualTokenStore) AccountStatement(ctx context.Context, workspaceID, agentID, accountID string, from, to time.Time) (MoneyStatement, error) {
	st := MoneyStatement{WorkspaceID: workspaceID, AccountID: accountID, From: from, To: to, Lines: []MoneyStatementLine{}}
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(a.agent_id, ''), a.currency, t.opening, t.money_in, t.money_out
		FROM money_accounts a, LATERAL (
		  SELECT COALESCE(sum(p.amount_minor) FILTER (WHERE e.created_at < $4), 0)::bigint AS opening,
		         COALESCE(sum(p.amount_minor) FILTER (WHERE e.created_at >= $4 AND p.amount_minor > 0), 0)::bigint AS money_in,
		         COALESCE(-sum(p.amount_minor) FILTER (WHERE e.created_at >= $4 AND p.amount_minor < 0), 0)::bigint AS money_out
		    FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		   WHERE p.account_id = a.id AND e.created_at < $5) t
		WHERE a.id = $1 AND a.workspace_id = $2 AND a.purpose IN ('company', 'agent', 'suspense') AND ($3 = '' OR a.agent_id = $3)`,
		accountID, workspaceID, agentID, from, to).Scan(&st.AgentID, &st.Currency, &st.OpeningMinor, &st.InMinor, &st.OutMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyStatement{}, ErrMoneyAccountNotFound
	}
	if err != nil {
		return MoneyStatement{}, fmt.Errorf("economy: account statement: %w", err)
	}
	st.ClosingMinor = st.OpeningMinor + st.InMinor - st.OutMinor

	rows, err := s.pool.Query(ctx, `
		SELECT p.entry_id, p.line, e.created_at, e.kind, p.amount_minor, p.funding,
		       COALESCE(NULLIF(e.counterparty, ''),
		         (SELECT CASE WHEN o.workspace_id = $2 THEN o.id ELSE 'workspace:' || o.workspace_id END
		            FROM money_postings op JOIN money_accounts o ON o.id = op.account_id
		           WHERE op.entry_id = p.entry_id AND op.account_id <> p.account_id ORDER BY op.line LIMIT 1), ''),
		       e.memo, e.partner_ref,
		       sum(p.amount_minor) OVER (ORDER BY e.created_at, e.id, p.line)::bigint
		  FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		 WHERE p.account_id = $1 AND e.created_at >= $3 AND e.created_at < $4
		 ORDER BY e.created_at, e.id, p.line`, accountID, workspaceID, from, to)
	if err != nil {
		return MoneyStatement{}, fmt.Errorf("economy: account statement lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l MoneyStatementLine
		var running int64
		if err := rows.Scan(&l.EntryID, &l.Line, &l.At, &l.Kind, &l.AmountMinor, &l.Funding, &l.Counterparty, &l.Memo, &l.PartnerRef, &running); err != nil {
			return MoneyStatement{}, fmt.Errorf("economy: account statement lines: %w", err)
		}
		l.At = l.At.UTC()
		l.BalanceAfterMinor = st.OpeningMinor + running
		st.Lines = append(st.Lines, l)
	}
	if err := rows.Err(); err != nil {
		return MoneyStatement{}, fmt.Errorf("economy: account statement lines: %w", err)
	}
	return st, nil
}
