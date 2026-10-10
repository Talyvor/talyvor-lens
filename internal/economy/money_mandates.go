package economy

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// money_mandates.go — B30.19: MANDATES: A PAYEE MAY PULL MONEY WITHIN LIMITS THE OWNER SET.
//
// An agent — or its owner for it — grants a mandate on one of the agent's currency accounts to one payee: another
// Talyvor company, or an outside business by its id. It sets a maximum per pull, a maximum per calendar month (UTC)
// and an expiry. The payee pulls within it; the owner or the agent revokes it at any time, and a revoked or expired
// mandate pulls nothing (migration 0246).
//
// A pull is one money entry. To a Talyvor company it moves the money from the agent's account straight into the
// company's open account in the same currency, so the one entry is on both statements; it moves for
// currency_accounts. An outside business pulls with the mandate's key, shown once when the mandate is granted, and
// what it pulls moves for payments_out into the mandate's hold account, to be paid out to it. Both are RED: test money
// only until a clearance, asked by PostMoney. A pull never takes the agent's account below zero, and the payee's
// idempotency key makes a retried pull the same pull.

var (
	// ErrMandate: a mandate asked for with a payee, limits or an expiry it cannot have, or a pull of nothing.
	ErrMandate = errors.New("economy: invalid mandate")
	// ErrMandateNotFound: no such mandate, or not one this caller may use.
	ErrMandateNotFound = errors.New("economy: no such mandate")
	// ErrMandateRevoked: the mandate was revoked, so it pulls nothing.
	ErrMandateRevoked = errors.New("economy: the mandate was revoked")
	// ErrMandateExpired: the mandate is past its expiry, so it pulls nothing.
	ErrMandateExpired = errors.New("economy: the mandate has expired")
	// ErrMandateOverLimit: a pull over the mandate's maximum per pull or per month.
	ErrMandateOverLimit = errors.New("economy: the pull is over the mandate's limit")
	// ErrMandateShort: the agent's account does not hold the money pulled.
	ErrMandateShort = errors.New("economy: the account does not hold enough to pay this pull")
	// ErrMandatePayerPaused: the agent is paused or archived, so its mandates pull nothing.
	ErrMandatePayerPaused = errors.New("economy: the agent is paused or archived, so its mandates pull nothing")
	// ErrMandatePayeeAccount: the payee company has no open account in the mandate's currency to be paid into.
	ErrMandatePayeeAccount = errors.New("economy: the payee has no open account in this currency to be paid into")
)

// The states of a mandate.
const (
	MandateActive  = "active"
	MandateRevoked = "revoked"
	MandateExpired = "expired"
)

// Mandate is what an agent's account allows one payee to pull, and what it has pulled this month.
type Mandate struct {
	ID                   string     `json:"id"`
	WorkspaceID          string     `json:"workspace_id"`
	AgentID              string     `json:"agent_id"`
	AccountID            string     `json:"account_id"`
	Currency             string     `json:"currency"`
	PayeeWorkspaceID     string     `json:"payee_workspace_id,omitempty"`
	PayeeBusinessID      string     `json:"payee_business_id,omitempty"`
	PayeeName            string     `json:"payee_name"`
	HoldAccountID        string     `json:"hold_account_id,omitempty"` // an outside business's: where its pulls wait to be paid out
	MaxPerPullMinor      int64      `json:"max_per_pull_minor"`
	MaxPerMonthMinor     int64      `json:"max_per_month_minor"`
	PulledThisMonthMinor int64      `json:"pulled_this_month_minor"`
	ExpiresAt            time.Time  `json:"expires_at"`
	Status               string     `json:"status"`
	RevokedAt            *time.Time `json:"revoked_at,omitempty"`
	RevokedBy            string     `json:"revoked_by,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	PullKey              string     `json:"pull_key,omitempty"` // an outside business's key, answered once, when granted
}

// MandateGrant is a mandate asked for: on AccountID, to PayeeWorkspaceID or else PayeeBusinessID.
type MandateGrant struct {
	AccountID        string    `json:"account_id"`
	PayeeWorkspaceID string    `json:"payee_workspace_id"`
	PayeeBusinessID  string    `json:"payee_business_id"`
	PayeeName        string    `json:"payee_name"`
	MaxPerPullMinor  int64     `json:"max_per_pull_minor"`
	MaxPerMonthMinor int64     `json:"max_per_month_minor"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// MandatePayee is who pulls: a Talyvor company's workspace, or an outside business by its mandate's key.
type MandatePayee struct {
	WorkspaceID string
	Key         string
}

// MandatePull is one pull: the entry that moved the money, and what the mandate has pulled this month with it.
type MandatePull struct {
	MandateID            string     `json:"mandate_id"`
	AmountMinor          int64      `json:"amount_minor"`
	Currency             string     `json:"currency"`
	PulledThisMonthMinor int64      `json:"pulled_this_month_minor"`
	Entry                MoneyEntry `json:"entry"`
}

// mandateMonth is the start of this calendar month, UTC: the monthly maximum counts pulls since then.
const mandateMonth = `date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`

const mandateColumns = `m.id, m.workspace_id, m.agent_id, m.account_id, m.currency, COALESCE(m.payee_workspace_id, ''),
	COALESCE(m.payee_business_id, ''), m.payee_name, COALESCE(m.hold_account_id, ''), m.max_per_pull_minor, m.max_per_month_minor,
	m.expires_at, m.revoked_at, m.revoked_by, m.created_at,
	COALESCE((SELECT sum(p.amount_minor) FROM money_mandate_pulls p WHERE p.mandate_id = m.id AND p.pulled_at >= ` + mandateMonth + `), 0)::bigint`

func scanMandate(row pgx.Row) (Mandate, error) {
	var m Mandate
	err := row.Scan(&m.ID, &m.WorkspaceID, &m.AgentID, &m.AccountID, &m.Currency, &m.PayeeWorkspaceID, &m.PayeeBusinessID, &m.PayeeName,
		&m.HoldAccountID, &m.MaxPerPullMinor, &m.MaxPerMonthMinor, &m.ExpiresAt, &m.RevokedAt, &m.RevokedBy, &m.CreatedAt, &m.PulledThisMonthMinor)
	switch {
	case m.RevokedAt != nil:
		m.Status = MandateRevoked
	case !m.ExpiresAt.After(time.Now()):
		m.Status = MandateExpired
	default:
		m.Status = MandateActive
	}
	return m, err
}

// GrantMandate grants g on one of workspaceID's agent accounts — with agentID, only that agent's.
func (s *DualTokenStore) GrantMandate(ctx context.Context, workspaceID, agentID string, g MandateGrant) (Mandate, error) {
	g.PayeeWorkspaceID, g.PayeeBusinessID = strings.TrimSpace(g.PayeeWorkspaceID), strings.TrimSpace(g.PayeeBusinessID)
	g.PayeeName = strings.TrimSpace(g.PayeeName)
	switch {
	case (g.PayeeWorkspaceID == "") == (g.PayeeBusinessID == ""):
		return Mandate{}, fmt.Errorf("%w: name one payee: payee_workspace_id for a Talyvor company, or payee_business_id for an outside business", ErrMandate)
	case g.PayeeWorkspaceID == workspaceID:
		return Mandate{}, fmt.Errorf("%w: a mandate is for another company to pull, not this one", ErrMandate)
	case len(g.PayeeBusinessID) > 200 || len(g.PayeeName) > 200:
		return Mandate{}, fmt.Errorf("%w: a payee's id and name are at most 200 characters", ErrMandate)
	case g.MaxPerPullMinor <= 0 || g.MaxPerMonthMinor < g.MaxPerPullMinor:
		return Mandate{}, fmt.Errorf("%w: max_per_pull_minor is above zero and max_per_month_minor at least that", ErrMandate)
	case !g.ExpiresAt.After(time.Now()):
		return Mandate{}, fmt.Errorf("%w: expires_at is in the future", ErrMandate)
	}
	m := Mandate{ID: "mdt_" + uuid.NewString(), WorkspaceID: workspaceID, AccountID: g.AccountID, PayeeWorkspaceID: g.PayeeWorkspaceID,
		PayeeBusinessID: g.PayeeBusinessID, PayeeName: g.PayeeName, MaxPerPullMinor: g.MaxPerPullMinor, MaxPerMonthMinor: g.MaxPerMonthMinor,
		ExpiresAt: g.ExpiresAt, Status: MandateActive}
	var status string
	err := s.pool.QueryRow(ctx, `SELECT agent_id, currency, status FROM money_accounts
		WHERE id = $1 AND workspace_id = $2 AND purpose = 'agent' AND ($3 = '' OR agent_id = $3)`,
		g.AccountID, workspaceID, agentID).Scan(&m.AgentID, &m.Currency, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Mandate{}, fmt.Errorf("%w: no agent account %q", ErrMoneyAccountNotFound, g.AccountID)
	}
	if err != nil {
		return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
	}
	if status != MoneyOpen {
		return Mandate{}, fmt.Errorf("%w: %s is %s", ErrMoneyAccountNotOpen, g.AccountID, status)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var keySHA, hold any
	if m.PayeeWorkspaceID != "" {
		var name string
		var open bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), ''),
			EXISTS (SELECT 1 FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND purpose = 'company' AND status = 'open')`,
			m.PayeeWorkspaceID, m.Currency).Scan(&name, &open); err != nil {
			return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
		}
		if !open {
			return Mandate{}, fmt.Errorf("%w (%s)", ErrMandatePayeeAccount, m.Currency)
		}
		if m.PayeeName == "" {
			m.PayeeName = cmp.Or(name, m.PayeeWorkspaceID)
		}
	} else {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
		}
		m.PullKey = "mdk_" + hex.EncodeToString(raw)
		keySHA = mandateKeySHA(m.PullKey)
		if m.PayeeName == "" {
			m.PayeeName = m.PayeeBusinessID
		}
		m.HoldAccountID = "macc_" + uuid.NewString()
		hold = m.HoldAccountID
		if _, err := tx.Exec(ctx, `INSERT INTO money_accounts (id, workspace_id, agent_id, currency, purpose, name) VALUES ($1, $2, $3, $4, 'hold', $5)`,
			m.HoldAccountID, workspaceID, m.AgentID, m.Currency, "Pulled by "+m.PayeeName+" under mandate "+m.ID); err != nil {
			return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
		}
	}
	if err := tx.QueryRow(ctx, `INSERT INTO money_mandates (id, workspace_id, agent_id, account_id, currency, payee_workspace_id, payee_business_id,
		payee_name, pull_key_sha256, hold_account_id, max_per_pull_minor, max_per_month_minor, expires_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, COALESCE($9::text, ''), $10, $11, $12, $13) RETURNING created_at`,
		m.ID, workspaceID, m.AgentID, m.AccountID, m.Currency, m.PayeeWorkspaceID, m.PayeeBusinessID, m.PayeeName, keySHA, hold,
		m.MaxPerPullMinor, m.MaxPerMonthMinor, m.ExpiresAt).Scan(&m.CreatedAt); err != nil {
		return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
	}
	return m, nil
}

func mandateKeySHA(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// PullMandate pulls amountMinor under mandate mandateID for payee, as funding (test, or "" for test; or live), once
// per idempotencyKey: a retry answers the first pull, and moves no money twice.
func (s *DualTokenStore) PullMandate(ctx context.Context, mandateID string, payee MandatePayee, amountMinor int64, funding, idempotencyKey, memo string) (MandatePull, error) {
	if funding == "" {
		funding = FundingTest
	}
	switch {
	case amountMinor <= 0:
		return MandatePull{}, fmt.Errorf("%w: a pull takes some money, not %d", ErrMandate, amountMinor)
	case strings.TrimSpace(idempotencyKey) == "":
		return MandatePull{}, fmt.Errorf("%w: a pull needs an idempotency key", ErrMandate)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The mandate's row is held until the pull commits, so two pulls at once cannot both fit under one maximum. It is
	// read after the lock, in a statement of its own: one that waited for the lock counts the pulls committed meanwhile.
	var keySHA string
	var m Mandate
	err = tx.QueryRow(ctx, `SELECT pull_key_sha256 FROM money_mandates WHERE id = $1 FOR UPDATE`, mandateID).Scan(&keySHA)
	if err == nil {
		m, err = scanMandate(tx.QueryRow(ctx, `SELECT `+mandateColumns+` FROM money_mandates m WHERE m.id = $1`, mandateID))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return MandatePull{}, ErrMandateNotFound
	}
	if err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	// Who is not the payee finds no mandate.
	if payee.Key != "" {
		if m.PayeeBusinessID == "" || subtle.ConstantTimeCompare([]byte(mandateKeySHA(payee.Key)), []byte(keySHA)) != 1 {
			return MandatePull{}, ErrMandateNotFound
		}
	} else if payee.WorkspaceID == "" || payee.WorkspaceID != m.PayeeWorkspaceID {
		return MandatePull{}, ErrMandateNotFound
	}
	pull := MandatePull{MandateID: m.ID, AmountMinor: amountMinor, Currency: m.Currency}

	var entryID string
	var first int64
	err = tx.QueryRow(ctx, `SELECT entry_id, amount_minor FROM money_mandate_pulls WHERE mandate_id = $1 AND idempotency_key = $2`,
		m.ID, idempotencyKey).Scan(&entryID, &first)
	switch {
	case err == nil && first != amountMinor:
		return MandatePull{}, ErrIdempotencyKeyReused
	case err == nil:
		if pull.Entry, err = readMoneyEntry(ctx, tx, `e.id = $1`, entryID); err != nil {
			return MandatePull{}, err
		}
		pull.PulledThisMonthMinor = m.PulledThisMonthMinor
		return pull, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}

	switch {
	case m.Status == MandateRevoked:
		return MandatePull{}, ErrMandateRevoked
	case m.Status == MandateExpired:
		return MandatePull{}, ErrMandateExpired
	case amountMinor > m.MaxPerPullMinor:
		return MandatePull{}, fmt.Errorf("%w: %d is over the maximum per pull of %d %s", ErrMandateOverLimit, amountMinor, m.MaxPerPullMinor, m.Currency)
	case m.PulledThisMonthMinor+amountMinor > m.MaxPerMonthMinor:
		return MandatePull{}, fmt.Errorf("%w: %d more would take this month's pulls to %d, over the maximum per month of %d %s",
			ErrMandateOverLimit, amountMinor, m.PulledThisMonthMinor+amountMinor, m.MaxPerMonthMinor, m.Currency)
	}
	var paused bool
	if err := tx.QueryRow(ctx, `SELECT paused_at IS NOT NULL OR archived_at IS NOT NULL FROM agent_accounts WHERE id = $1`, m.AgentID).Scan(&paused); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if paused {
		return MandatePull{}, ErrMandatePayerPaused
	}
	// The account is held too, so no other movement spends the same money while this one is judged, and its balance is
	// read after the lock, as the mandate is.
	var held int64
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, m.AccountID); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT balance_minor FROM money_account_balances WHERE account_id = $1 AND funding = $2), 0)`,
		m.AccountID, funding).Scan(&held); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if held < amountMinor {
		return MandatePull{}, fmt.Errorf("%w: it holds %d %s %s", ErrMandateShort, held, funding, m.Currency)
	}

	e := MoneyEntry{WorkspaceID: m.WorkspaceID, Kind: "mandate_pull", IdempotencyKey: "mandate_pull:" + m.ID + ":" + idempotencyKey,
		Funding: funding, Memo: strings.TrimSpace(memo)}
	if e.Memo == "" {
		e.Memo = "Pulled by " + m.PayeeName + " under mandate " + m.ID
	}
	to := m.HoldAccountID
	if m.PayeeWorkspaceID != "" {
		e.Capability, e.payeeWorkspaceID = CapabilityCurrencyAccounts, m.PayeeWorkspaceID
		err := tx.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND purpose = 'company' AND status = 'open'`,
			m.PayeeWorkspaceID, m.Currency).Scan(&to)
		if errors.Is(err, pgx.ErrNoRows) {
			return MandatePull{}, fmt.Errorf("%w (%s)", ErrMandatePayeeAccount, m.Currency)
		}
		if err != nil {
			return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
		}
	} else {
		// shortcut: an outside business's pulls wait in the hold account; paying them out to it is payments out (B30.17).
		e.Capability = CapabilityPaymentsOut
	}
	e.Postings = []MoneyPosting{{AccountID: m.AccountID, AmountMinor: -amountMinor}, {AccountID: to, AmountMinor: amountMinor}}
	if pull.Entry, err = postMoneyTx(ctx, tx, e, s.screener); err != nil {
		return MandatePull{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO money_mandate_pulls (mandate_id, idempotency_key, entry_id, amount_minor) VALUES ($1, $2, $3, $4)`,
		m.ID, idempotencyKey, pull.Entry.ID, amountMinor); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return MandatePull{}, fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	pull.PulledThisMonthMinor = m.PulledThisMonthMinor + amountMinor
	return pull, nil
}

// RevokeMandate revokes workspaceID's mandate mandateID — with agentID, only that agent's — for good. Revoking it again
// answers it as it was revoked.
func (s *DualTokenStore) RevokeMandate(ctx context.Context, workspaceID, agentID, mandateID, by string) (Mandate, error) {
	m, err := scanMandate(s.pool.QueryRow(ctx, `WITH r AS (UPDATE money_mandates SET revoked_at = COALESCE(revoked_at, now()),
		revoked_by = CASE WHEN revoked_at IS NULL THEN $4 ELSE revoked_by END
		WHERE id = $1 AND workspace_id = $2 AND ($3 = '' OR agent_id = $3) RETURNING *)
		SELECT `+mandateColumns+` FROM r m`, mandateID, workspaceID, agentID, by))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mandate{}, ErrMandateNotFound
	}
	if err != nil {
		return Mandate{}, fmt.Errorf("economy: revoke mandate: %w", err)
	}
	return m, nil
}

// Mandates is what workspaceID has granted — with agentID, only that agent's — and, without, what it has been granted
// by other companies, each with what it has pulled this month: the newest first.
func (s *DualTokenStore) Mandates(ctx context.Context, workspaceID, agentID string) (granted, received []Mandate, err error) {
	read := func(where string, args ...any) ([]Mandate, error) {
		rows, err := s.pool.Query(ctx, `SELECT `+mandateColumns+` FROM money_mandates m WHERE `+where+` ORDER BY m.created_at DESC, m.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("economy: mandates: %w", err)
		}
		defer rows.Close()
		out := []Mandate{}
		for rows.Next() {
			m, err := scanMandate(rows)
			if err != nil {
				return nil, fmt.Errorf("economy: mandates: %w", err)
			}
			out = append(out, m)
		}
		return out, rows.Err()
	}
	if granted, err = read(`m.workspace_id = $1 AND ($2 = '' OR m.agent_id = $2)`, workspaceID, agentID); err != nil {
		return nil, nil, err
	}
	received = []Mandate{}
	if agentID == "" {
		if received, err = read(`m.payee_workspace_id = $1`, workspaceID); err != nil {
			return nil, nil, err
		}
	}
	return granted, received, nil
}
