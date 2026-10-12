package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
)

// currency_accounts.go — B30.13: ACCOUNTS IN POUNDS, EUROS AND DOLLARS FOR A COMPANY AND EACH OF ITS AGENTS.
//
// A company opens an account in GBP, EUR or USD at the account partner — the Test partner until currency_accounts is
// cleared and a real one is configured. The ledger keeps it twice, under the partner's reference for it: as the
// company's money account, and as the partner account that mirrors it at the partner (what B30.11 reconciles). An
// agent's account is a sub-account of its company's in the same currency, kept in the ledger only, so the company's
// account opens first (migration 0240). Every account opens at zero.
//
// Opening is the first use of currency_accounts, class RED: a frozen workspace, or one that has not accepted the
// capability's terms, opens nothing, and an account opened for live money is refused, naming the class, until the
// capability is cleared.

// AccountPartners hands out the account partner for a capability, and the conversion partner that values a payment
// out for an agent's rules (B30.17): *partners.Registry.
type AccountPartners interface {
	Account(ctx context.Context, capability string) (partners.AccountPartner, error)
	FX(ctx context.Context, capability string) (partners.FXPartner, error)
}

// SetAccountPartners is where a company's currency accounts are opened (B30.13). Until it is set, none opens.
func (s *DualTokenStore) SetAccountPartners(p AccountPartners) { s.accountPartners = p }

var (
	// ErrCurrencyAccount: a currency account asked for in a currency or with a funding it cannot have.
	ErrCurrencyAccount = errors.New("economy: invalid currency account")
	// ErrCurrencyAccountExists: the company or agent already has an open account in that currency.
	ErrCurrencyAccountExists = errors.New("economy: an account in this currency is already open")
	// ErrCompanyAccountNeeded: an agent's account asked for before its company's in that currency.
	ErrCompanyAccountNeeded = errors.New("economy: an agent's account is a sub-account of its company's: open the company's account in this currency first")
	// ErrAccountNotOpened: the account partner did not open the account.
	ErrAccountNotOpened = errors.New("economy: the account partner did not open the account")
)

// CurrencyAccount is a company's or an agent's account in one currency, and what it holds.
type CurrencyAccount struct {
	MoneyAccount
	ParentAccountID string `json:"parent_account_id,omitempty"` // an agent's: its company's account in the same currency
	BalanceMinor    int64  `json:"balance_minor"`
	TestMinor       int64  `json:"test_minor"`
	LiveMinor       int64  `json:"live_minor"`
}

// OpenCurrencyAccount opens workspaceID's account in currency — the company's, or with agentID that agent's
// sub-account of the company's — for funding, test ("" too) or live. It opens at zero.
func (s *DualTokenStore) OpenCurrencyAccount(ctx context.Context, workspaceID, agentID, currency, funding string) (CurrencyAccount, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if funding == "" {
		funding = FundingTest
	}
	switch {
	case currency != CurrencyGBP && currency != CurrencyEUR && currency != CurrencyUSD:
		return CurrencyAccount{}, fmt.Errorf("%w: an account is in GBP, EUR or USD, not %q", ErrCurrencyAccount, currency)
	case funding != FundingTest && funding != FundingLive:
		return CurrencyAccount{}, fmt.Errorf("%w: funding is test or live, not %q", ErrCurrencyAccount, funding)
	}
	c, _ := CapabilityByKey(CapabilityCurrencyAccounts)
	if err := refuseFrozen(ctx, s.pool, workspaceID, c); err != nil {
		return CurrencyAccount{}, err
	}
	if err := refuseUnaccepted(ctx, s.pool, workspaceID, c); err != nil {
		return CurrencyAccount{}, err
	}
	if funding == FundingLive {
		refusal, err := capabilityLive(ctx, s.pool, workspaceID, c)
		if err != nil {
			return CurrencyAccount{}, err
		}
		if refusal != nil {
			return CurrencyAccount{}, refusal
		}
	}
	if agentID != "" {
		return s.openAgentCurrencyAccount(ctx, workspaceID, agentID, currency)
	}
	return s.openCompanyCurrencyAccount(ctx, workspaceID, currency)
}

func (s *DualTokenStore) openCompanyCurrencyAccount(ctx context.Context, workspaceID, currency string) (CurrencyAccount, error) {
	if s.accountPartners == nil {
		return CurrencyAccount{}, fmt.Errorf("%w: no account partner is set", ErrAccountNotOpened)
	}
	var holder string
	var open bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), ''),
		EXISTS (SELECT 1 FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND purpose = 'company' AND status <> 'closed')`,
		workspaceID, currency).Scan(&holder, &open); err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: open currency account: %w", err)
	}
	if open {
		return CurrencyAccount{}, fmt.Errorf("%w: the company's %s account", ErrCurrencyAccountExists, currency)
	}
	if holder == "" {
		holder = workspaceID
	}
	p, err := s.accountPartners.Account(ctx, CapabilityCurrencyAccounts)
	if err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: open currency account: %w", err)
	}
	a := CurrencyAccount{MoneyAccount: MoneyAccount{ID: "macc_" + uuid.NewString(), WorkspaceID: workspaceID, Currency: currency,
		Purpose: MoneyCompany, Status: MoneyOpen, Name: holder}}
	// shortcut: an account the partner leaves pending is refused, not kept; upgrade when a real partner opens accounts later.
	opened, err := p.OpenAccount(ctx, partners.AccountRequest{ID: a.ID, Holder: holder, Currency: currency})
	if err != nil {
		return CurrencyAccount{}, fmt.Errorf("%w: %v", ErrAccountNotOpened, err)
	}
	if opened.Status != partners.StatusCompleted {
		return CurrencyAccount{}, fmt.Errorf("%w: it answered %s: %s", ErrAccountNotOpened, opened.Status, opened.Detail)
	}
	a.PartnerAccountRef = opened.Ref

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: open currency account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	insert := `INSERT INTO money_accounts (id, workspace_id, currency, purpose, name, partner_account_ref) VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`
	if err := tx.QueryRow(ctx, insert, a.ID, workspaceID, currency, MoneyCompany, holder, opened.Ref).Scan(&a.CreatedAt); err != nil {
		return CurrencyAccount{}, currencyAccountErr(err, "the company's "+currency+" account")
	}
	var at time.Time
	if err := tx.QueryRow(ctx, insert, "macc_"+uuid.NewString(), workspaceID, currency, MoneyPartner, p.Name(), opened.Ref).Scan(&at); err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: open currency account: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: open currency account: %w", err)
	}
	return a, nil
}

func (s *DualTokenStore) openAgentCurrencyAccount(ctx context.Context, workspaceID, agentID, currency string) (CurrencyAccount, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&ok); err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: open currency account: %w", err)
	}
	if !ok {
		return CurrencyAccount{}, ErrAgentNotFound
	}
	a := CurrencyAccount{MoneyAccount: MoneyAccount{ID: "macc_" + uuid.NewString(), WorkspaceID: workspaceID, AgentID: agentID,
		Currency: currency, Purpose: MoneyAgent, Status: MoneyOpen}}
	err := s.pool.QueryRow(ctx, `INSERT INTO money_accounts (id, workspace_id, agent_id, currency, purpose, parent_account_id)
		SELECT $1, $2, $3, $4, 'agent', c.id FROM money_accounts c
		WHERE c.workspace_id = $2 AND c.currency = $4 AND c.purpose = 'company' AND c.status = 'open'
		RETURNING parent_account_id, created_at`, a.ID, workspaceID, agentID, currency).Scan(&a.ParentAccountID, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CurrencyAccount{}, fmt.Errorf("%w (%s)", ErrCompanyAccountNeeded, currency)
	}
	if err != nil {
		return CurrencyAccount{}, currencyAccountErr(err, "this agent's "+currency+" account")
	}
	return a, nil
}

// currencyAccountErr is ErrCurrencyAccountExists for an insert a unique index refused: one opened at the same time.
func currencyAccountErr(err error, which string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrCurrencyAccountExists, which)
	}
	return fmt.Errorf("economy: open currency account: %w", err)
}

// CurrencyAccounts is workspaceID's company and agent accounts, or with agentID that agent's only, and what each
// holds: the company's first, then by currency.
func (s *DualTokenStore) CurrencyAccounts(ctx context.Context, workspaceID, agentID string) ([]CurrencyAccount, error) {
	return currencyAccounts(ctx, s.pool, workspaceID, agentID, "")
}

// currencyAccount is workspaceID's company or agent account accountID, and what it holds.
func currencyAccount(ctx context.Context, q pgxDB, workspaceID, accountID string) (CurrencyAccount, error) {
	out, err := currencyAccounts(ctx, q, workspaceID, "", accountID)
	if err != nil {
		return CurrencyAccount{}, err
	}
	if len(out) == 0 {
		return CurrencyAccount{}, fmt.Errorf("%w: %s", ErrMoneyAccountNotFound, accountID)
	}
	return out[0], nil
}

func currencyAccounts(ctx context.Context, q pgxDB, workspaceID, agentID, accountID string) ([]CurrencyAccount, error) {
	rows, err := q.Query(ctx, `SELECT a.id, a.workspace_id, COALESCE(a.agent_id, ''), a.currency, a.purpose, a.status, a.name,
		a.partner_account_ref, COALESCE(a.parent_account_id, ''), a.created_at,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'test'), 0)::bigint,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'live'), 0)::bigint
		FROM money_accounts a LEFT JOIN money_account_balances b ON b.account_id = a.id
		WHERE a.workspace_id = $1 AND a.purpose IN ('company', 'agent') AND ($2 = '' OR a.agent_id = $2) AND ($3 = '' OR a.id = $3)
		GROUP BY a.id ORDER BY a.agent_id NULLS FIRST, a.currency, a.created_at`, workspaceID, agentID, accountID)
	if err != nil {
		return nil, fmt.Errorf("economy: currency accounts: %w", err)
	}
	defer rows.Close()
	out := []CurrencyAccount{}
	for rows.Next() {
		var a CurrencyAccount
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.AgentID, &a.Currency, &a.Purpose, &a.Status, &a.Name, &a.PartnerAccountRef,
			&a.ParentAccountID, &a.CreatedAt, &a.TestMinor, &a.LiveMinor); err != nil {
			return nil, fmt.Errorf("economy: currency accounts: %w", err)
		}
		a.BalanceMinor = a.TestMinor + a.LiveMinor
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: currency accounts: %w", err)
	}
	return out, nil
}
