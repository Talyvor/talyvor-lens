package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
)

// payments_in.go — B30.15: RECEIVE MONEY FROM OUTSIDE.
//
// The account partner reports each payment that arrives at a company account's details on a signed webhook
// (cmd/lens/payments_in_handler.go), and ReceivePayment posts it to the account the details or the payer's reference
// name, with the payer's name and reference on the entry: the account's statement shows both. Money that names no
// open account sits in the workspace's suspense account until the operator assigns it to one of the workspace's
// accounts, or ReturnDueSuspense pays it back to the payer once it has sat there LENS_SUSPENSE_RETURN_DAYS business
// days. Either moves it out of suspense once: both post under the same idempotency key, under one lock.

// ErrSuspenseNotHeld: no payment in by that entry is in suspense — it was never there, or was returned or assigned.
var ErrSuspenseNotHeld = errors.New("economy: no payment in by that entry is held in suspense")

// SuspenseItem is a payment in that is held in suspense.
type SuspenseItem struct {
	EntryID           string    `json:"entry_id"` // the payment in
	WorkspaceID       string    `json:"workspace_id"`
	AccountID         string    `json:"account_id"` // the suspense account
	Payer             string    `json:"payer"`
	Reference         string    `json:"reference"`
	PartnerRef        string    `json:"partner_ref"`
	AmountMinor       int64     `json:"amount_minor"`
	Currency          string    `json:"currency"`
	Funding           string    `json:"funding"`
	ReceivedAt        time.Time `json:"received_at"`
	ReturnAfter       time.Time `json:"return_after"` // when the job pays it back, unless the operator assigns it first
	partnerAccountID  string
	partnerAccountRef string
}

// suspenseAccount is workspaceID's suspense account in currency, opened the first time it is needed.
func (s *DualTokenStore) suspenseAccount(ctx context.Context, workspaceID, currency string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `WITH opened AS (
			INSERT INTO money_accounts (id, workspace_id, currency, purpose, name) VALUES ($1, $2, $3, 'suspense', 'Unmatched money in')
			ON CONFLICT (workspace_id, currency) WHERE purpose = 'suspense' AND status <> 'closed' DO NOTHING RETURNING id)
		SELECT id FROM opened UNION ALL
		SELECT id FROM money_accounts WHERE workspace_id = $2 AND currency = $3 AND purpose = 'suspense' AND status <> 'closed'
		LIMIT 1`, "macc_"+uuid.NewString(), workspaceID, currency).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("economy: suspense account: %w", err)
	}
	return id, nil
}

// heldSuspense is every payment in held in suspense where matches, oldest first, each due back days business days
// after it arrived. A payment in leaves suspense by the one entry keyed "suspense:<its id>".
func heldSuspense(ctx context.Context, q pgxDB, days int, where string, args ...any) ([]SuspenseItem, error) {
	rows, err := q.Query(ctx, `SELECT e.id, e.workspace_id, s.account_id, e.counterparty, e.memo, e.partner_ref, s.amount_minor, s.currency,
		s.funding, e.created_at, pp.account_id, pa.partner_account_ref
		FROM money_entries e
		JOIN money_postings s ON s.entry_id = e.id AND s.amount_minor > 0
		JOIN money_accounts sa ON sa.id = s.account_id AND sa.purpose = 'suspense'
		JOIN money_postings pp ON pp.entry_id = e.id AND pp.amount_minor < 0
		JOIN money_accounts pa ON pa.id = pp.account_id AND pa.purpose = 'partner'
		WHERE e.kind = 'payment_in' AND `+where+` AND NOT EXISTS (
			SELECT 1 FROM money_entries r WHERE r.workspace_id = e.workspace_id AND r.idempotency_key = 'suspense:' || e.id)
		ORDER BY e.created_at, e.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("economy: money in suspense: %w", err)
	}
	defer rows.Close()
	out := []SuspenseItem{}
	for rows.Next() {
		var it SuspenseItem
		if err := rows.Scan(&it.EntryID, &it.WorkspaceID, &it.AccountID, &it.Payer, &it.Reference, &it.PartnerRef, &it.AmountMinor,
			&it.Currency, &it.Funding, &it.ReceivedAt, &it.partnerAccountID, &it.partnerAccountRef); err != nil {
			return nil, fmt.Errorf("economy: money in suspense: %w", err)
		}
		it.ReturnAfter = afterBusinessDays(it.ReceivedAt, days)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: money in suspense: %w", err)
	}
	return out, nil
}

// afterBusinessDays is days business days, Monday to Friday, after t.
// shortcut: a bank holiday counts as a business day; upgrade when Lens keeps a holiday calendar.
func afterBusinessDays(t time.Time, days int) time.Time {
	for days > 0 {
		t = t.AddDate(0, 0, 1)
		if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday {
			days--
		}
	}
	return t
}

// SuspenseItems is every payment in held in suspense in any workspace, oldest first, each due back days business days
// after it arrived: what the operator assigns or lets return.
func (s *DualTokenStore) SuspenseItems(ctx context.Context, days int) ([]SuspenseItem, error) {
	return heldSuspense(ctx, s.pool, days, "true")
}

// leaveSuspense moves the payment in entryID out of suspense by the entry move builds from it, on a transaction that
// holds the payment's lock, so a return and an assignment cannot both happen. send, when set, runs once the entry
// has been screened and before it is written — the partner's payment — and may set its partner reference.
func (s *DualTokenStore) leaveSuspense(ctx context.Context, entryID string, move func(SuspenseItem) (MoneyEntry, error),
	send func(SuspenseItem, *MoneyEntry) error) (MoneyEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: leave suspense: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('suspense:' || $1, 0))`, entryID); err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: leave suspense: %w", err)
	}
	// Read after the lock, so a return or an assignment that committed while this waited is seen.
	held, err := heldSuspense(ctx, tx, 0, "e.id = $1", entryID)
	if err != nil {
		return MoneyEntry{}, err
	}
	if len(held) == 0 {
		return MoneyEntry{}, ErrSuspenseNotHeld
	}
	it := held[0]
	e, err := move(it)
	if err != nil {
		return MoneyEntry{}, err
	}
	e.WorkspaceID, e.Capability, e.IdempotencyKey, e.Funding = it.WorkspaceID, CapabilityPaymentsIn, "suspense:"+it.EntryID, it.Funding
	if send != nil {
		// Screened before the partner pays anything; postMoneyTx screens the same payment again and finds it so.
		if err := screenOutside(ctx, e, e.Postings, func(id string) bool { return id == it.partnerAccountID }, s.screener); err != nil {
			return MoneyEntry{}, err
		}
		if err := send(it, &e); err != nil {
			return MoneyEntry{}, err
		}
	}
	out, err := postMoneyTx(ctx, tx, e, s.screener)
	if err != nil {
		return MoneyEntry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return MoneyEntry{}, fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return MoneyEntry{}, fmt.Errorf("economy: leave suspense: %w", err)
	}
	return out, nil
}

// AssignSuspense moves the payment in entryID out of suspense into accountID, an open company or agent account of the
// same workspace in the same currency: the operator has found whose it is. It keeps the payer and reference.
func (s *DualTokenStore) AssignSuspense(ctx context.Context, entryID, accountID string) (MoneyEntry, error) {
	return s.leaveSuspense(ctx, entryID, func(it SuspenseItem) (MoneyEntry, error) {
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_accounts WHERE id = $1 AND workspace_id = $2 AND currency = $3
			AND purpose IN ('company', 'agent') AND status = 'open')`, accountID, it.WorkspaceID, it.Currency).Scan(&ok); err != nil {
			return MoneyEntry{}, fmt.Errorf("economy: assign from suspense: %w", err)
		}
		if !ok {
			return MoneyEntry{}, fmt.Errorf("%w: %s is not an open %s company or agent account of the workspace the money came to",
				ErrMoneyAccountNotFound, accountID, it.Currency)
		}
		return MoneyEntry{Kind: "suspense_assigned", Memo: it.Reference, Counterparty: it.Payer, Postings: []MoneyPosting{
			{AccountID: it.AccountID, AmountMinor: -it.AmountMinor, Currency: it.Currency},
			{AccountID: accountID, AmountMinor: it.AmountMinor, Currency: it.Currency},
		}}, nil
	}, nil)
}

// ReturnDueSuspense pays back to its payer, through the account partner, every payment in that has sat in suspense
// days business days by now, and answers how many went back. One the partner does not complete, or screening holds,
// stays in suspense for the next run.
func (s *DualTokenStore) ReturnDueSuspense(ctx context.Context, now time.Time, days int) (int, error) {
	if s.accountPartners == nil {
		return 0, errors.New("economy: no account partner is set, so nothing in suspense goes back")
	}
	held, err := s.SuspenseItems(ctx, days)
	if err != nil {
		return 0, err
	}
	returned := 0
	for _, due := range held {
		if due.ReturnAfter.After(now) {
			continue
		}
		if _, err := s.returnSuspense(ctx, due.EntryID); err != nil {
			slog.Warn("economy: a payment in suspense did not go back; the next run tries again",
				"workspace_id", due.WorkspaceID, "entry_id", due.EntryID, "err", err)
			continue
		}
		returned++
	}
	return returned, nil
}

// returnSuspense pays the payment in entryID back to its payer from the account it arrived in, and posts it out of
// suspense through the partner account. The partner's payment is keyed by the entry, so a retry never pays twice.
func (s *DualTokenStore) returnSuspense(ctx context.Context, entryID string) (MoneyEntry, error) {
	return s.leaveSuspense(ctx, entryID, func(it SuspenseItem) (MoneyEntry, error) {
		return MoneyEntry{Kind: "payment_in_returned", Memo: it.Reference, Counterparty: it.Payer, Postings: []MoneyPosting{
			{AccountID: it.AccountID, AmountMinor: -it.AmountMinor, Currency: it.Currency},
			{AccountID: it.partnerAccountID, AmountMinor: it.AmountMinor, Currency: it.Currency},
		}}, nil
	}, func(it SuspenseItem, e *MoneyEntry) error {
		p, err := s.accountPartners.Account(ctx, CapabilityPaymentsIn)
		if err != nil {
			return fmt.Errorf("economy: return from suspense: %w", err)
		}
		// shortcut: the return is a payment to the payer by name; upgrade to returning the original payment by its
		// reference when a real account partner's adapter is written.
		req := partners.PaymentRequest{ID: "suspense_return:" + it.EntryID, AccountRef: it.partnerAccountRef,
			Amount: partners.Money{Minor: it.AmountMinor, Currency: it.Currency}, Payee: partners.Payee{Name: it.Payer}, Reference: "Returned " + it.PartnerRef}
		res, err := p.SendPayment(ctx, req)
		if errors.Is(err, partners.ErrNotFound) && p.Name() == "test" {
			// The Test partner forgets its accounts when Lens restarts: open the company's again (idempotent on its id).
			var id, holder string
			if qerr := s.pool.QueryRow(ctx, `SELECT id, name FROM money_accounts WHERE partner_account_ref = $1 AND purpose = 'company'`,
				it.partnerAccountRef).Scan(&id, &holder); qerr == nil {
				if _, oerr := p.OpenAccount(ctx, partners.AccountRequest{ID: id, Holder: holder, Currency: it.Currency}); oerr == nil {
					res, err = p.SendPayment(ctx, req)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("economy: return from suspense: %w", err)
		}
		if res.Status != partners.StatusCompleted {
			return fmt.Errorf("economy: return from suspense: the partner answered %s: %s", res.Status, res.Detail)
		}
		e.PartnerRef = res.Ref
		return nil
	})
}

// MoneyStatementLine is one line on a money account's statement.
type MoneyStatementLine struct {
	EntryID      string    `json:"entry_id"`
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"`
	Counterparty string    `json:"counterparty,omitempty"` // the payer of money in, the payee of money out
	Reference    string    `json:"reference,omitempty"`
	AmountMinor  int64     `json:"amount_minor"` // what it added to the account: money in positive, money out negative
	Currency     string    `json:"currency"`
	Funding      string    `json:"funding"`
	BalanceMinor int64     `json:"balance_minor"` // the account's test or live balance, as the line's funding, after it
}

// MoneyStatement is the latest limit lines on workspaceID's company, agent or suspense account accountID, newest
// first — with agentID, only if it is that agent's.
func (s *DualTokenStore) MoneyStatement(ctx context.Context, workspaceID, agentID, accountID string, limit int) ([]MoneyStatementLine, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_accounts WHERE id = $1 AND workspace_id = $2
		AND purpose IN ('company', 'agent', 'suspense') AND ($3 = '' OR agent_id = $3))`, accountID, workspaceID, agentID).Scan(&ok); err != nil {
		return nil, fmt.Errorf("economy: money statement: %w", err)
	}
	if !ok {
		return nil, ErrMoneyAccountNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT id, created_at, kind, counterparty, memo, amount_minor, currency, funding, balance FROM (
			SELECT e.id, e.created_at, e.kind, e.counterparty, e.memo, p.amount_minor, p.currency, p.funding, p.line,
				sum(p.amount_minor) OVER (PARTITION BY p.funding ORDER BY e.created_at, e.id, p.line)::bigint AS balance
			FROM money_postings p JOIN money_entries e ON e.id = p.entry_id WHERE p.account_id = $1) l
		ORDER BY created_at DESC, id DESC, line DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("economy: money statement: %w", err)
	}
	defer rows.Close()
	out := []MoneyStatementLine{}
	for rows.Next() {
		var l MoneyStatementLine
		if err := rows.Scan(&l.EntryID, &l.At, &l.Kind, &l.Counterparty, &l.Reference, &l.AmountMinor, &l.Currency, &l.Funding, &l.BalanceMinor); err != nil {
			return nil, fmt.Errorf("economy: money statement: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: money statement: %w", err)
	}
	return out, nil
}
