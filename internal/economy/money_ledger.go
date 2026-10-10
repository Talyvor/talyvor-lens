package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// money_ledger.go — B30.2: MONEY IN CURRENCIES.
//
// A money account holds one currency — pounds, euros, dollars or USDC — for a workspace, or for one of its agents
// (migration 0221). Money moves as one entry of double-entry postings, in int64 minor units with the currency on
// every posting: pence and cents, and millionths of a USDC. A posting's amount is what it adds to its account —
// money in is positive, money out negative — so a company's and an agent's balances read as the money they hold,
// and the partner account through which money came in reads as the negative of what came in.
//
// The database refuses, when the transaction commits, an entry whose postings do not sum to zero per currency and
// funding, and any change to a posting; money_account_balances keeps each account's total in the same
// transaction. Every entry names the wallet capability (B30.1) it moves money for, and live money is asked of that
// capability before anything is written: an uncleared RED or AMBER one refuses it, and the entry is never written.
// Every entry carries an idempotency key, so a retried movement is the same entry and moves no money twice.
//
// Money in or out through a partner account is an outside payment, and names its Counterparty — the payer of money in,
// the payee of money out. Before any posting it is screened against the sanctions lists (B30.6, internal/screening):
// a match refuses it with a *screening.Refusal naming the compliance case, and nothing is written.
//
// Once it has moved, a payment in or out through a partner is monitored (B30.7, internal/monitoring): a pattern a
// rule finds opens or extends a compliance case, and never moves or stops money.
//
// Credits (LXC) are not money accounts: they stay on lxc_ledger and agent_postings, unchanged.

// The currencies a money account may hold.
const (
	CurrencyGBP  = "GBP"
	CurrencyEUR  = "EUR"
	CurrencyUSD  = "USD"
	CurrencyUSDC = "USDC"
)

// MoneyCurrencies is how many decimal places each currency's minor unit is: a penny or a cent is 2, and USDC
// counts millionths, as the token does.
var MoneyCurrencies = map[string]int{CurrencyGBP: 2, CurrencyEUR: 2, CurrencyUSD: 2, CurrencyUSDC: 6}

// The purposes of a money account.
const (
	MoneyCompany  = "company"  // the company's own money
	MoneyAgent    = "agent"    // an agent's own money
	MoneyPot      = "pot"      // money set aside, the company's or an agent's
	MoneyHold     = "hold"     // money held for a payment not yet made
	MoneySuspense = "suspense" // money in that is not yet matched to an account
	MoneyPartner  = "partner"  // what has come in and gone out through a partner
	MoneyRevenue  = "revenue"  // Talyvor's fees
)

var moneyPurposes = map[string]bool{MoneyCompany: true, MoneyAgent: true, MoneyPot: true, MoneyHold: true,
	MoneySuspense: true, MoneyPartner: true, MoneyRevenue: true}

// The states of a money account. Only an open one takes postings.
const (
	MoneyOpen   = "open"
	MoneyFrozen = "frozen"
	MoneyClosed = "closed"
)

var (
	// ErrMoneyAccountNotFound: no money account by that id in this workspace.
	ErrMoneyAccountNotFound = errors.New("economy: no such money account")
	// ErrMoneyAccountNotOpen: a posting to a frozen or closed account.
	ErrMoneyAccountNotOpen = errors.New("economy: the money account is not open")
	// ErrMoneyUnbalanced: an entry whose postings do not sum to zero in each currency, refused when it commits.
	ErrMoneyUnbalanced = errors.New("economy: a money entry's postings must sum to zero in each currency")
	// ErrIdempotencyKeyReused: an idempotency key already used for a different movement of money.
	ErrIdempotencyKeyReused = errors.New("economy: this idempotency key was already used for a different money movement")
)

// MoneyAccount is one account in one currency.
type MoneyAccount struct {
	ID                string    `json:"id"`
	WorkspaceID       string    `json:"workspace_id"`
	AgentID           string    `json:"agent_id,omitempty"`
	Currency          string    `json:"currency"`
	Purpose           string    `json:"purpose"`
	Status            string    `json:"status"`
	Name              string    `json:"name"`
	PartnerAccountRef string    `json:"partner_account_ref,omitempty"` // a partner account's account at the partner (B30.11)
	CreatedAt         time.Time `json:"created_at"`
}

// MoneyPosting is one line of a money entry.
type MoneyPosting struct {
	AccountID   string `json:"account_id"`
	AmountMinor int64  `json:"amount_minor"` // what it adds to the account: money in positive, money out negative
	Currency    string `json:"currency"`     // the account's; "" when posting means the account's
	Funding     string `json:"funding"`      // the entry's: test or live
}

// MoneyEntry is one movement of money and its postings.
type MoneyEntry struct {
	ID             string         `json:"id"`
	WorkspaceID    string         `json:"workspace_id"`
	Capability     string         `json:"capability"` // the wallet capability the money moves for (B30.1)
	Kind           string         `json:"kind"`       // what moved it, in lower_snake_case
	IdempotencyKey string         `json:"idempotency_key"`
	Funding        string         `json:"funding"` // test or live
	Memo           string         `json:"memo,omitempty"`
	Counterparty   string         `json:"counterparty,omitempty"` // who money through a partner is from or to, screened first (B30.6)
	PartnerRef     string         `json:"partner_ref,omitempty"`  // the partner's reference for that payment, reconciled daily (B30.11)
	CreatedAt      time.Time      `json:"created_at"`
	Postings       []MoneyPosting `json:"postings"`

	payeeWorkspaceID string // a mandate pull's payee company (B30.19), or an invoice's issuer (B30.20): its account may be credited too
}

// PaymentScreener screens an outside payment before its money moves: *screening.Screener.
type PaymentScreener interface {
	ScreenPayment(ctx context.Context, p screening.Payment) error
}

// SetScreener is what screens money in and out through a partner (B30.6). Until it is set, no such money moves.
func (s *DualTokenStore) SetScreener(sc PaymentScreener) { s.screener = sc }

// MoneyMonitor runs transaction monitoring on a payment once it has moved (B30.7): *monitoring.Monitor. A hit opens or
// extends a compliance case; it never moves or stops money.
type MoneyMonitor interface {
	MoneyMoved(ctx context.Context, workspaceID, entryID string) error
}

// SetMonitor is what judges each payment in or out through a partner once it has moved (B30.7).
func (s *DualTokenStore) SetMonitor(m MoneyMonitor) { s.monitor = m }

// MoneyBalance is what a money account holds: the sum of its postings, and how much of it is test money.
type MoneyBalance struct {
	AccountID   string `json:"account_id"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	TestMinor   int64  `json:"test_minor"`
	LiveMinor   int64  `json:"live_minor"`
}

// OpenMoneyAccount opens an account in a for a.WorkspaceID: in a.Currency, for a.Purpose, and for the agent a.AgentID
// when it is set — an agent's own account needs one, and a company, suspense, partner or revenue account takes none.
// It opens with nothing in it.
func (s *DualTokenStore) OpenMoneyAccount(ctx context.Context, a MoneyAccount) (MoneyAccount, error) {
	a.Currency = strings.ToUpper(strings.TrimSpace(a.Currency))
	switch {
	case a.WorkspaceID == "":
		return MoneyAccount{}, errors.New("economy: a money account needs a workspace")
	case MoneyCurrencies[a.Currency] == 0:
		return MoneyAccount{}, fmt.Errorf("economy: a money account holds GBP, EUR, USD or USDC, not %q", a.Currency)
	case !moneyPurposes[a.Purpose]:
		return MoneyAccount{}, fmt.Errorf("economy: %q is not a money account's purpose", a.Purpose)
	case a.Purpose == MoneyAgent && a.AgentID == "":
		return MoneyAccount{}, errors.New("economy: an agent's money account needs the agent")
	case a.AgentID != "" && a.Purpose != MoneyAgent && a.Purpose != MoneyPot && a.Purpose != MoneyHold:
		return MoneyAccount{}, fmt.Errorf("economy: a %s account belongs to the company, not to an agent", a.Purpose)
	case a.PartnerAccountRef != "" && a.Purpose != MoneyPartner:
		return MoneyAccount{}, fmt.Errorf("economy: only a partner account names an account at the partner, not a %s account", a.Purpose)
	}
	if a.AgentID != "" {
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
			a.AgentID, a.WorkspaceID).Scan(&ok); err != nil {
			return MoneyAccount{}, fmt.Errorf("economy: open money account: %w", err)
		}
		if !ok {
			return MoneyAccount{}, ErrAgentNotFound
		}
	}
	a.ID = "macc_" + uuid.NewString()
	a.Status = MoneyOpen
	if err := s.pool.QueryRow(ctx, `INSERT INTO money_accounts (id, workspace_id, agent_id, currency, purpose, name, partner_account_ref)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7) RETURNING created_at`,
		a.ID, a.WorkspaceID, a.AgentID, a.Currency, a.Purpose, a.Name, a.PartnerAccountRef).Scan(&a.CreatedAt); err != nil {
		return MoneyAccount{}, fmt.Errorf("economy: open money account: %w", err)
	}
	return a, nil
}

// PostMoney moves money between e.WorkspaceID's accounts: it writes e and its postings in one transaction and answers
// the entry as recorded. A posting of zero is left out — a fee that is 0 moves nothing. Live money is first asked of e.Capability, which
// refuses it with a *CapabilityRefusal while it is uncleared, and then nothing is written. The same idempotency key
// again answers the entry it first wrote, or ErrIdempotencyKeyReused when the movement differs. An entry that does
// not sum to zero in each currency is refused when it commits, with ErrMoneyUnbalanced. Money in or out through a
// partner account is screened first, and a match refuses it with a *screening.Refusal before any posting. A workspace
// an operator has frozen (B30.8) moves nothing: a *CapabilityRefusal that is ErrWorkspaceFrozen; nor one that has not
// accepted the capability's latest terms (B30.9): a *CapabilityRefusal that is ErrTermsNotAccepted.
func (s *DualTokenStore) PostMoney(ctx context.Context, e MoneyEntry) (MoneyEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: post money: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := postMoneyTx(ctx, tx, e, s.screener)
	if err != nil {
		return MoneyEntry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return MoneyEntry{}, fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return MoneyEntry{}, fmt.Errorf("economy: post money: %w", err)
	}
	s.monitorMoved(ctx, out)
	return out, nil
}

// monitorMoved runs transaction monitoring on e once it has moved, when it is a payment through a partner (B30.7).
// Monitoring never undoes it, so a monitor that fails is logged and the nightly run judges the payment again.
func (s *DualTokenStore) monitorMoved(ctx context.Context, e MoneyEntry) {
	if s.monitor == nil || e.Counterparty == "" {
		return
	}
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.monitor.MoneyMoved(mctx, e.WorkspaceID, e.ID); err != nil {
		slog.Warn("economy: transaction monitoring did not judge a payment; the nightly run will",
			"workspace_id", e.WorkspaceID, "entry_id", e.ID, "err", err)
	}
}

// postMoneyTx is PostMoney on tx, the caller's transaction, so the movement commits or rolls back with the rows
// that explain it. Whether it balances is judged when tx commits. screener screens money in or out through a partner.
func postMoneyTx(ctx context.Context, tx pgx.Tx, e MoneyEntry, screener PaymentScreener) (MoneyEntry, error) {
	c, ok := CapabilityByKey(e.Capability)
	switch {
	case !ok || !b30[c.Key]:
		return MoneyEntry{}, fmt.Errorf("economy: money in currencies moves only for a money capability (B30), not %q", e.Capability)
	case e.WorkspaceID == "":
		return MoneyEntry{}, errors.New("economy: a money entry needs a workspace")
	case e.IdempotencyKey == "":
		return MoneyEntry{}, errors.New("economy: a money entry needs an idempotency key")
	case e.Funding != FundingTest && e.Funding != FundingLive:
		return MoneyEntry{}, fmt.Errorf("economy: money is test or live, not %q", e.Funding)
	}
	postings := make([]MoneyPosting, 0, len(e.Postings))
	ids := make([]string, 0, len(e.Postings))
	for _, p := range e.Postings {
		if p.AmountMinor == 0 {
			continue
		}
		p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
		p.Funding = e.Funding
		postings = append(postings, p)
		ids = append(ids, p.AccountID)
	}
	if len(postings) == 0 {
		return MoneyEntry{}, errors.New("economy: a money entry must move some money")
	}

	e.Counterparty = strings.TrimSpace(e.Counterparty)
	e.PartnerRef = strings.TrimSpace(e.PartnerRef)
	e.ID = "mle_" + uuid.NewString()
	err := tx.QueryRow(ctx, `INSERT INTO money_entries (id, workspace_id, capability, kind, idempotency_key, memo, counterparty, partner_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (workspace_id, idempotency_key) DO NOTHING RETURNING created_at`,
		e.ID, e.WorkspaceID, e.Capability, e.Kind, e.IdempotencyKey, e.Memo, e.Counterparty, e.PartnerRef).Scan(&e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return replayedMoneyEntry(ctx, tx, e, postings)
	}
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: post money: %w", err)
	}

	// B30.8: a frozen workspace moves no money in currencies, test or live; a retry of what moved before the freeze
	// answered above.
	if err := refuseFrozen(ctx, tx, e.WorkspaceID, c); err != nil {
		return MoneyEntry{}, err
	}
	// B30.9: nor, test or live, until the workspace has accepted the capability's latest terms.
	if err := refuseUnaccepted(ctx, tx, e.WorkspaceID, c); err != nil {
		return MoneyEntry{}, err
	}
	if e.payeeWorkspaceID != "" {
		if err := refuseFrozen(ctx, tx, e.payeeWorkspaceID, c); err != nil {
			return MoneyEntry{}, err
		}
	}
	if e.Funding == FundingLive {
		refusal, err := capabilityLive(ctx, tx, e.WorkspaceID, c)
		if err != nil {
			return MoneyEntry{}, err
		}
		if refusal != nil {
			return MoneyEntry{}, refusal
		}
	}

	// Each account is the entry's workspace's — another's is not found, but for a mandate pull's payee's company account
	// or an invoice issuer's agent account — and is held open until tx ends: a freeze or a close waits for the money
	// already moving.
	rows, err := tx.Query(ctx, `SELECT id, currency, status, purpose FROM money_accounts
		WHERE id = ANY($1) AND (workspace_id = $2 OR (workspace_id = $3 AND purpose IN ('company', 'agent'))) FOR SHARE`,
		ids, e.WorkspaceID, e.payeeWorkspaceID)
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: post money: %w", err)
	}
	type held struct{ currency, status, purpose string }
	accounts := map[string]held{}
	for rows.Next() {
		var id string
		var h held
		if err := rows.Scan(&id, &h.currency, &h.status, &h.purpose); err != nil {
			rows.Close()
			return MoneyEntry{}, fmt.Errorf("economy: post money: %w", err)
		}
		accounts[id] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: post money: %w", err)
	}

	var sql strings.Builder
	sql.WriteString(`INSERT INTO money_postings (entry_id, line, account_id, amount_minor, currency, funding) VALUES `)
	args := []any{e.ID, e.Funding}
	for i, p := range postings {
		h, ok := accounts[p.AccountID]
		switch {
		case !ok:
			return MoneyEntry{}, fmt.Errorf("%w: %s", ErrMoneyAccountNotFound, p.AccountID)
		case h.status != MoneyOpen:
			return MoneyEntry{}, fmt.Errorf("%w: %s is %s", ErrMoneyAccountNotOpen, p.AccountID, h.status)
		case p.Currency == "":
			postings[i].Currency = h.currency
		case p.Currency != h.currency:
			return MoneyEntry{}, fmt.Errorf("economy: account %s holds %s, not %s", p.AccountID, h.currency, p.Currency)
		}
		if i > 0 {
			sql.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&sql, "($1, %d, $%d, $%d, $%d, $2)", i+1, n+1, n+2, n+3)
		args = append(args, p.AccountID, p.AmountMinor, postings[i].Currency)
	}
	// B30.6: money in or out through a partner is screened before any posting.
	if err := screenOutside(ctx, e, postings, func(id string) bool { return accounts[id].purpose == MoneyPartner }, screener); err != nil {
		return MoneyEntry{}, err
	}
	// B30.4: live money moves within the limit for one movement at the workspace's verification level.
	if e.Funding == FundingLive {
		over, err := overLevelLimit(ctx, tx, e.WorkspaceID, c, postings)
		if err != nil {
			return MoneyEntry{}, err
		}
		if over != nil {
			return MoneyEntry{}, &CapabilityRefusal{Capability: c, Level: over}
		}
	}
	if _, err := tx.Exec(ctx, sql.String(), args...); err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: post money: %w", err)
	}
	e.Postings = postings
	return e, nil
}

// screenOutside screens e when it moves money through a partner account: money out to its counterparty for what the
// partner postings put into partner accounts, money in from it for what they take out. Each currency and direction
// is screened as its own payment, under the entry's idempotency key — an entry whose partner postings net to zero is
// screened both ways.
func screenOutside(ctx context.Context, e MoneyEntry, postings []MoneyPosting, partner func(string) bool, screener PaymentScreener) error {
	type leg struct {
		currency, direction string
	}
	through := map[leg]int64{}
	var legs []leg
	for _, p := range postings {
		if !partner(p.AccountID) {
			continue
		}
		l, amount := leg{p.Currency, "out"}, p.AmountMinor
		if amount < 0 {
			l, amount = leg{p.Currency, "in"}, -amount
		}
		if _, ok := through[l]; !ok {
			legs = append(legs, l)
		}
		through[l] += amount
	}
	if len(legs) == 0 {
		return nil
	}
	switch {
	case e.Counterparty == "":
		return errors.New("economy: money in or out through a partner names who it is from or to (counterparty), to be screened first")
	case screener == nil:
		return fmt.Errorf("%w: no screener is set, so no money moves in or out through a partner", partners.ErrScreeningUnavailable)
	}
	for _, l := range legs {
		if err := screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: e.WorkspaceID, ID: e.IdempotencyKey, Capability: e.Capability,
			Direction: l.direction, Counterparty: e.Counterparty, AmountMinor: through[l], Currency: l.currency, Funding: e.Funding}); err != nil {
			return err
		}
	}
	return nil
}

// replayedMoneyEntry answers the entry already written under e's idempotency key, when it is the same movement as
// e: the same capability, kind, funding, counterparty and partner reference, and the same postings in the same order.
func replayedMoneyEntry(ctx context.Context, tx pgx.Tx, e MoneyEntry, postings []MoneyPosting) (MoneyEntry, error) {
	first, err := readMoneyEntry(ctx, tx, `e.workspace_id = $1 AND e.idempotency_key = $2`, e.WorkspaceID, e.IdempotencyKey)
	if err != nil {
		return MoneyEntry{}, err
	}
	same := first.Capability == e.Capability && first.Kind == e.Kind && first.Funding == e.Funding &&
		first.Counterparty == e.Counterparty && first.PartnerRef == e.PartnerRef && len(first.Postings) == len(postings)
	for i := 0; same && i < len(postings); i++ {
		p, q := postings[i], first.Postings[i]
		same = p.AccountID == q.AccountID && p.AmountMinor == q.AmountMinor && (p.Currency == "" || p.Currency == q.Currency)
	}
	if !same {
		return MoneyEntry{}, ErrIdempotencyKeyReused
	}
	return first, nil
}

// readMoneyEntry reads the one entry where matches, and its postings in order.
func readMoneyEntry(ctx context.Context, q pgxDB, where string, args ...any) (MoneyEntry, error) {
	rows, err := q.Query(ctx, `SELECT e.id, e.workspace_id, e.capability, e.kind, e.idempotency_key, e.memo, e.counterparty, e.partner_ref, e.created_at,
		p.account_id, p.amount_minor, p.currency, p.funding
		FROM money_entries e JOIN money_postings p ON p.entry_id = e.id WHERE `+where+` ORDER BY p.line`, args...)
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: read money entry: %w", err)
	}
	defer rows.Close()
	var e MoneyEntry
	for rows.Next() {
		var p MoneyPosting
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.Capability, &e.Kind, &e.IdempotencyKey, &e.Memo, &e.Counterparty, &e.PartnerRef, &e.CreatedAt,
			&p.AccountID, &p.AmountMinor, &p.Currency, &p.Funding); err != nil {
			return MoneyEntry{}, fmt.Errorf("economy: read money entry: %w", err)
		}
		e.Funding = p.Funding
		e.Postings = append(e.Postings, p)
	}
	if err := rows.Err(); err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: read money entry: %w", err)
	}
	if e.ID == "" {
		return MoneyEntry{}, errors.New("economy: read money entry: none")
	}
	return e, nil
}

// Balance reads what workspaceID's money account accountID holds: the sum of its postings, kept by
// money_account_balances as each is written, test and live money together and each apart.
func (s *DualTokenStore) Balance(ctx context.Context, workspaceID, accountID string) (MoneyBalance, error) {
	b := MoneyBalance{AccountID: accountID}
	err := s.pool.QueryRow(ctx, `SELECT a.currency,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'test'), 0)::bigint,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'live'), 0)::bigint
		FROM money_accounts a LEFT JOIN money_account_balances b ON b.account_id = a.id
		WHERE a.id = $1 AND a.workspace_id = $2 GROUP BY a.currency`, accountID, workspaceID).Scan(&b.Currency, &b.TestMinor, &b.LiveMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyBalance{}, ErrMoneyAccountNotFound
	}
	if err != nil {
		return MoneyBalance{}, fmt.Errorf("economy: money balance: %w", err)
	}
	b.AmountMinor = b.TestMinor + b.LiveMinor
	return b, nil
}
