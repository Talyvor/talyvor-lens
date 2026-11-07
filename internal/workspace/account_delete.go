package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/tenantdata"
)

// B52.37 — A PERSON DELETES THEIR ACCOUNT.
//
// DeleteAccount removes the person's workspace and every row tenantdata.Manifest classifies Delete, in one
// transaction, and records the deletion in account_deletions (kept), which refuses the workspace's session
// tokens issued before it. Retain tables — the ledgers, receipts, compliance record — are not touched, and an agent
// one of them names is kept for it, anonymised. What others share is left to them: a room someone else is in passes
// to them or is closed, and their requests and mandates with the person are declined or revoked, not deleted.
//
// What else money has to keep is Nicolai's call. Until he records it, a workspace that still holds any balance
// or an open money item is refused, with what it holds, and nothing is deleted.

// ErrNoAccount: no workspace has that id.
var ErrNoAccount = errors.New("workspace: no such account")

// AccountHoldsMoneyError is the refusal: Held names, in words, what the workspace still holds.
type AccountHoldsMoneyError struct{ Held []string }

func (e *AccountHoldsMoneyError) Error() string {
	return "the account still holds " + strings.Join(e.Held, ", ")
}

// accountHoldings are the balances and open money items that stop a deletion, each a query on $1, the workspace.
var accountHoldings = []struct{ what, sql string }{
	{"credits", `SELECT EXISTS (SELECT 1 FROM lxc_balances WHERE workspace_id = $1 AND balance <> 0)`},
	{"LENS tokens", `SELECT EXISTS (SELECT 1 FROM lens_token_balances WHERE workspace_id = $1 AND balance <> 0)`},
	{"money in its agents' wallets", `SELECT EXISTS (SELECT 1 FROM agent_account_balances WHERE workspace_id = $1 AND balance_ulxc <> 0)`},
	{"money in its accounts", `SELECT EXISTS (SELECT 1 FROM money_account_balances b JOIN money_accounts a ON a.id = b.account_id
		WHERE a.workspace_id = $1 AND b.balance_minor <> 0)`},
	{"marketplace earnings not yet paid out", `SELECT EXISTS (SELECT 1 FROM market_journal_balances
		WHERE account LIKE 'seller:' || $1 || ':%' AND balance_usd_micros <> 0)`},
	{"a loan that is not finished", `SELECT EXISTS (SELECT 1 FROM agent_loans WHERE $1 IN (lender_workspace_id, borrower_workspace_id)
		AND status IN ('offered', 'active', 'late', 'defaulted'))`},
	{"money held in escrow", `SELECT EXISTS (SELECT 1 FROM agent_escrows WHERE $1 IN (payer_workspace_id, payee_workspace_id)
		AND status IN ('held', 'disputed'))`},
	{"a payment on its way", `SELECT EXISTS (SELECT 1 FROM money_payments WHERE workspace_id = $1 AND status IN ('pending', 'sent'))`},
	{"a cash-out on its way", `SELECT EXISTS (SELECT 1 FROM agent_cash_outs WHERE workspace_id = $1 AND status IN ('held', 'submitted'))`},
	{"an invoice waiting to be paid", `SELECT EXISTS (SELECT 1 FROM money_invoices WHERE workspace_id = $1 AND status = 'sent')`},
	{"credit-line spending not yet paid", `SELECT EXISTS (SELECT 1 FROM credit_line_draws WHERE workspace_id = $1 AND invoice_id = '')
		OR EXISTS (SELECT 1 FROM credit_line_invoices WHERE workspace_id = $1 AND paid_at IS NULL)`},
	{"earnings still being paid out", `SELECT EXISTS (SELECT 1 FROM money_earnings_payouts WHERE $1 IN (workspace_id, recipient_workspace_id)
		AND status IN ('held', 'pending'))`},
}

// holdings is what of accountHoldings workspace id holds, in words.
func holdings(ctx context.Context, tx pgx.Tx, id string) ([]string, error) {
	var held []string
	for _, h := range accountHoldings {
		var yes bool
		if err := tx.QueryRow(ctx, h.sql, id).Scan(&yes); err != nil {
			return nil, fmt.Errorf("workspace: does the account hold %s: %w", h.what, err)
		}
		if yes {
			held = append(held, h.what)
		}
	}
	return held, nil
}

// moneyLockSQL is economy.FreezeLockExclusiveSQL (economy imports this package): every movement of the workspace's
// money in the ledger holds it shared, so the deletion waits for those already moving and every later one waits for it.
const moneyLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('compliance_freeze:' || $1, 0))`

// keptAgentsSQL is the workspace's agents a kept money record names: its account, a mandate, an invoice.
const keptAgentsSQL = `SELECT COALESCE(array_agg(a.id), '{}') FROM agent_accounts a WHERE a.workspace_id = $1 AND (
	EXISTS (SELECT 1 FROM money_accounts WHERE agent_id = a.id) OR EXISTS (SELECT 1 FROM money_mandates WHERE agent_id = a.id)
	OR EXISTS (SELECT 1 FROM money_invoices WHERE agent_id = a.id))`

// cascadesSQL is each table that cascades from $1's rows, by its one foreign-key column.
const cascadesSQL = `SELECT c.conrelid::regclass::text, a.attname::text FROM pg_constraint c
JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
WHERE c.contype = 'f' AND c.confrelid = $1::regclass AND c.confdeltype = 'c' AND cardinality(c.conkey) = 1`

// accountTablesSQL lists each table's workspace columns, as the manifest's population does: a text column
// whose name contains "workspace".
const accountTablesSQL = `
SELECT c.relname, array_agg(a.attname::text ORDER BY a.attnum)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND NOT c.relispartition
  AND a.attname LIKE '%workspace%' AND a.atttypid IN ('text'::regtype, 'varchar'::regtype)
GROUP BY c.relname`

// DeleteAccount deletes workspace id as described above. It returns ErrNoAccount for an unknown id and an
// *AccountHoldsMoneyError when it holds money; either way nothing changes. beforeCommit, if set, ends what the
// workspace has outside Postgres (its subscription in Stripe, its cached answers) once everything else is done and
// still under the lock: its error is returned and nothing is deleted.
func (m *Manager) DeleteAccount(ctx context.Context, id string, beforeCommit func(context.Context) error) error {
	txer, ok := m.pool.(interface {
		BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("workspace: deleting an account needs a database that begins transactions")
	}
	// Read committed under the workspace's money lock: each check below sees all the money that moved before it.
	tx, err := txer.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("workspace: begin account deletion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, moneyLockSQL, id); err != nil {
		return fmt.Errorf("workspace: lock the account's money: %w", err)
	}

	// The workspace row is locked too, so two deletions of one account run one after the other.
	var found string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id = $1 FOR UPDATE`, id).Scan(&found); errors.Is(err, pgx.ErrNoRows) {
		return ErrNoAccount
	} else if err != nil {
		return fmt.Errorf("workspace: lock account: %w", err)
	}
	if held, err := holdings(ctx, tx, id); err != nil {
		return err
	} else if len(held) > 0 {
		return &AccountHoldsMoneyError{Held: held}
	}

	// What another workspace holds open with it ends with it, rather than stopping it: a money request either
	// side sent is declined, and a mandate it granted or is paid by is revoked. No money moves on either.
	if _, err := tx.Exec(ctx, `UPDATE agent_money_requests SET status = 'declined', decided_at = now()
		WHERE $1 IN (from_workspace_id, to_workspace_id) AND status = 'pending'`, id); err != nil {
		return fmt.Errorf("workspace: decline money requests: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE money_mandates SET revoked_at = now(), revoked_by = 'account deleted'
		WHERE $1 IN (workspace_id, payee_workspace_id) AND revoked_at IS NULL`, id); err != nil {
		return fmt.Errorf("workspace: revoke mandates: %w", err)
	}
	// Its reusable cards are kept, for the money they explain, and frozen: the issuer asks Lens at every charge.
	if _, err := tx.Exec(ctx, `UPDATE money_reusable_cards SET status = 'frozen', updated_at = now()
		WHERE workspace_id = $1 AND status = 'active'`, id); err != nil {
		return fmt.Errorf("workspace: freeze cards: %w", err)
	}
	if err := leaveRooms(ctx, tx, id); err != nil {
		return err
	}
	keptAgents, err := keepAgents(ctx, tx, id)
	if err != nil {
		return err
	}
	// A listing market_lineage names (append-only: the record a royalty is owed along) is kept and taken down.
	var keptListings []string
	if err := tx.QueryRow(ctx, `WITH t AS (UPDATE market_listings m SET review_status = 'taken_down' WHERE m.workspace_id = $1
		AND EXISTS (SELECT 1 FROM market_lineage l WHERE m.id IN (l.parent_listing_id, l.child_listing_id)) RETURNING id)
		SELECT COALESCE(array_agg(id), '{}') FROM t`, id).Scan(&keptListings); err != nil {
		return fmt.Errorf("workspace: listings lineage keeps: %w", err)
	}
	kept := map[string][]string{"agent_accounts": keptAgents, "market_listings": keptListings}

	cols := map[string][]string{}
	rows, err := tx.Query(ctx, accountTablesSQL)
	if err != nil {
		return fmt.Errorf("workspace: workspace tables: %w", err)
	}
	for rows.Next() {
		var t string
		var c []string
		if err := rows.Scan(&t, &c); err != nil {
			rows.Close()
			return err
		}
		cols[t] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range tenantdata.MappingTables {
		cols[t] = append(cols[t], "id")
	}

	// One DELETE per Delete table, each under a savepoint; one a foreign key refuses is tried again after
	// the others. A table still refused at the end fails the whole deletion: never a partial one reported done.
	// Only the person's own rows go: a row whose other workspace column names someone else (a mandate paying
	// them, a message they removed from another's room) is that workspace's record too, and stays.
	// token_events is append-only (0055) and may hold the person's prompts: it goes the one sanctioned way.
	if err := audit.DeleteWorkspaceEvents(ctx, tx, id); err != nil {
		return err
	}
	pending := tenantdata.DeleteOrder()
	pending = pending[:len(pending)-1] // "workspaces" goes last, below
	for progress := true; progress && len(pending) > 0; {
		progress = false
		var refused []string
		for _, t := range pending {
			c, ok := cols[t]
			if !ok || t == "token_events" {
				continue // in the manifest, not in this schema; or gone above
			}
			mine, onlyMine := make([]string, len(c)), make([]string, len(c))
			for i, col := range c {
				q := pgx.Identifier{col}.Sanitize()
				mine[i], onlyMine[i] = q+` = $1`, `COALESCE(`+q+`, '') IN ($1, '')`
			}
			sp, err := tx.Begin(ctx)
			if err != nil {
				return fmt.Errorf("workspace: savepoint: %w", err)
			}
			q, args := `DELETE FROM `+pgx.Identifier{t}.Sanitize()+` WHERE (`+strings.Join(mine, " OR ")+`) AND `+strings.Join(onlyMine, " AND "), []any{id}
			if k := kept[t]; len(k) > 0 {
				q, args = q+` AND id <> ALL($2)`, append(args, k)
			}
			if _, err := sp.Exec(ctx, q, args...); err != nil {
				_ = sp.Rollback(ctx)
				refused = append(refused, t)
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return fmt.Errorf("workspace: release savepoint: %w", err)
			}
			progress = true
		}
		pending = refused
	}
	if len(pending) > 0 {
		return fmt.Errorf("workspace: account not deleted: these tables refused the delete: %s", strings.Join(pending, ", "))
	}
	// Money that landed in spite of the lock (a movement that does not take it) stops the deletion here.
	// shortcut: one written between this check and the commit is still missed; make every writer take the lock if so.
	if held, err := holdings(ctx, tx, id); err != nil {
		return err
	} else if len(held) > 0 {
		return &AccountHoldsMoneyError{Held: held}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, id); err != nil {
		return fmt.Errorf("workspace: delete account: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_deletions (workspace_id, deleted_at) VALUES ($1, now())
		ON CONFLICT (workspace_id) DO UPDATE SET deleted_at = now()`, id); err != nil {
		return fmt.Errorf("workspace: record account deletion: %w", err)
	}
	if beforeCommit != nil {
		if err := beforeCommit(ctx); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("workspace: commit account deletion: %w", err)
	}
	m.mu.Lock()
	delete(m.workspaces, id)
	m.mu.Unlock()
	return nil
}

// leaveRooms hands each room the person owns that someone else is in to its earliest member that is not a viewer, as
// an owner hands a room over (rooms.Store.handOver; the heir's plan is not asked), or, with none, closes it and keeps
// it, ownerless, so that deleting the person's rows never cascades away another member's. A room only they are in goes
// with them.
func leaveRooms(ctx context.Context, tx pgx.Tx, id string) error {
	rows, err := tx.Query(ctx, `SELECT r.id, COALESCE((SELECT m.workspace_id FROM room_members m WHERE m.room_id = r.id
		AND m.workspace_id <> $1 AND m.role <> 'viewer' AND m.removed_at IS NULL AND m.banned_at IS NULL
		ORDER BY m.joined_at, m.workspace_id LIMIT 1), '')
		FROM rooms r WHERE r.owner_workspace_id = $1 AND EXISTS (SELECT 1 FROM room_members m WHERE m.room_id = r.id AND m.workspace_id <> $1)
		FOR UPDATE OF r`, id)
	if err != nil {
		return fmt.Errorf("workspace: rooms: %w", err)
	}
	type heir struct{ room, to string }
	heirs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (heir, error) {
		var h heir
		return h, r.Scan(&h.room, &h.to)
	})
	if err != nil {
		return fmt.Errorf("workspace: rooms: %w", err)
	}
	for _, h := range heirs {
		if h.to == "" {
			if _, err := tx.Exec(ctx, `UPDATE rooms SET owner_workspace_id = '', status = 'closed', wallet_agent_id = NULL WHERE id = $1`, h.room); err != nil {
				return fmt.Errorf("workspace: close room: %w", err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE room_members SET may_spend = false, role = CASE role WHEN 'owner' THEN 'editor' ELSE role END
			WHERE room_id = $1`, h.room); err != nil {
			return fmt.Errorf("workspace: hand room over: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE room_members SET role = 'owner', may_spend = true WHERE room_id = $1 AND workspace_id = $2`, h.room, h.to); err != nil {
			return fmt.Errorf("workspace: hand room over: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE rooms SET owner_workspace_id = $2, owner_since = now(), wallet_agent_id = NULL WHERE id = $1`, h.room, h.to); err != nil {
			return fmt.Errorf("workspace: hand room over: %w", err)
		}
	}
	return nil
}

// keepAgents keeps each agent a kept money record names, for that record, without the person's words on it: its name,
// handle and description go, it is paused and archived, and everything that cascades from it — its keys, rules, cards
// — is deleted as deleting it would. It answers their ids.
func keepAgents(ctx context.Context, tx pgx.Tx, id string) ([]string, error) {
	var kept []string
	if err := tx.QueryRow(ctx, keptAgentsSQL, id).Scan(&kept); err != nil {
		return nil, fmt.Errorf("workspace: agents money keeps: %w", err)
	}
	if len(kept) == 0 {
		return kept, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_accounts SET name = 'Deleted agent', handle = NULL, description = '', owner_user_id = '',
		paused_at = COALESCE(paused_at, now()), paused_reason = 'account deleted', archived_at = COALESCE(archived_at, now())
		WHERE id = ANY($1)`, kept); err != nil {
		return nil, fmt.Errorf("workspace: anonymise agents: %w", err)
	}
	rows, err := tx.Query(ctx, cascadesSQL, "agent_accounts")
	if err != nil {
		return nil, fmt.Errorf("workspace: agents' dependants: %w", err)
	}
	type fk struct{ table, col string }
	fks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fk, error) {
		var f fk
		return f, r.Scan(&f.table, &f.col)
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: agents' dependants: %w", err)
	}
	for _, f := range fks {
		if _, err := tx.Exec(ctx, `DELETE FROM `+pgx.Identifier{f.table}.Sanitize()+` WHERE `+pgx.Identifier{f.col}.Sanitize()+` = ANY($1)`, kept); err != nil {
			return nil, fmt.Errorf("workspace: delete %s of kept agents: %w", f.table, err)
		}
	}
	return kept, nil
}

// AccountDeletedAt reports when the account of workspace id was deleted, if it was. The auth manager refuses
// the workspace's session tokens issued before then.
func (m *Manager) AccountDeletedAt(ctx context.Context, id string) (time.Time, bool, error) {
	if m.pool == nil {
		return time.Time{}, false, nil
	}
	var at time.Time
	err := m.pool.QueryRow(ctx, `SELECT deleted_at FROM account_deletions WHERE workspace_id = $1`, id).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("workspace: account deletion of %s: %w", id, err)
	}
	return at, true, nil
}
