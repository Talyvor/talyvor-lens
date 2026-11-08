package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
)

// agent_trading_accounts.go — B30.48: AGENT TRADING ACCOUNTS AT A BROKER PARTNER.
//
// An agent opens a trading account in US dollars at the broker partner — the Test broker, in-process, until
// trade_equities is cleared and a real one is configured — and funds it from its own USD account. The ledger keeps the
// account's cash in a money account of its own (purpose broker, migration 0365) under the broker's reference for it:
// funding is one entry for trade_equities, the agent's USD account to the trading account, so the agent's money stays
// the agent's and is counted once. Opening and funding are uses of trade_equities, class RED: a frozen workspace, or
// one that has not accepted its terms, does neither, and live money is refused, naming the class, until it is cleared.
//
// shortcut: the cash is the ledger's, and no transfer is sent to the broker, since BrokerPartner has no deposit and the
// Test broker keeps no cash; upgrade when a real broker adapter needs its account funded.

// BrokerPartners hands out the broker partner for a capability: *partners.Registry.
type BrokerPartners interface {
	Broker(ctx context.Context, capability string) (partners.BrokerPartner, error)
}

// SetBrokerPartners is where agents' trading accounts are opened (B30.48). Until it is set, none opens.
func (s *DualTokenStore) SetBrokerPartners(p BrokerPartners) { s.brokerPartners = p }

// MoneyBroker is the purpose of a trading account's cash in the ledger.
const MoneyBroker = "broker"

// simulatedPrices is what the Test broker's prices are called wherever they are shown.
const simulatedPrices = "simulated prices"

var (
	// ErrTradingAccountNotFound: the agent has no trading account in this workspace.
	ErrTradingAccountNotFound = errors.New("economy: this agent has no trading account")
	// ErrTradingAccount: a funding that is not one — an amount that is not positive, or no idempotency key.
	ErrTradingAccount = errors.New("economy: invalid trading account funding")
	// ErrAgentUSDAccountNeeded: the agent has no open USD account to fund its trading account from.
	ErrAgentUSDAccountNeeded = errors.New("economy: a trading account is funded from the agent's USD account: open it first")
)

// TradingAccount is an agent's trading account at the broker partner, and the cash it holds.
type TradingAccount struct {
	ID                string    `json:"id"`
	AgentID           string    `json:"agent_id"`
	Currency          string    `json:"currency"`
	Partner           string    `json:"partner"`
	PartnerAccountRef string    `json:"partner_account_ref"`
	AccountID         string    `json:"account_id"` // the ledger's money account for its cash
	CashMinor         int64     `json:"cash_minor"`
	TestMinor         int64     `json:"test_minor"`
	LiveMinor         int64     `json:"live_minor"`
	Prices            string    `json:"prices,omitempty"` // "simulated prices" at the Test broker
	CreatedAt         time.Time `json:"created_at"`
}

// OpenTradingAccount opens agentID's USD trading account at the broker partner, or answers the one it has.
func (s *DualTokenStore) OpenTradingAccount(ctx context.Context, workspaceID, agentID string) (TradingAccount, error) {
	if a, err := s.TradingAccount(ctx, workspaceID, agentID); !errors.Is(err, ErrTradingAccountNotFound) {
		return a, err
	}
	c, _ := CapabilityByKey(CapabilityTradeEquities)
	if err := refuseFrozen(ctx, s.pool, workspaceID, c); err != nil {
		return TradingAccount{}, err
	}
	if err := refuseUnaccepted(ctx, s.pool, workspaceID, c); err != nil {
		return TradingAccount{}, err
	}
	var holder string
	err := s.pool.QueryRow(ctx, `SELECT name FROM agent_accounts WHERE id = $1 AND workspace_id = $2`, agentID, workspaceID).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return TradingAccount{}, ErrAgentNotFound
	}
	if err != nil {
		return TradingAccount{}, fmt.Errorf("economy: open trading account: %w", err)
	}
	if s.brokerPartners == nil {
		return TradingAccount{}, fmt.Errorf("%w: no broker partner is set", ErrAccountNotOpened)
	}
	p, err := s.brokerPartners.Broker(ctx, CapabilityTradeEquities)
	if err != nil {
		return TradingAccount{}, fmt.Errorf("economy: open trading account: %w", err)
	}
	a := TradingAccount{ID: "tacc_" + uuid.NewString(), AgentID: agentID, Currency: CurrencyUSD, Partner: p.Name(),
		AccountID: "macc_" + uuid.NewString()}
	opened, err := p.OpenAccount(ctx, partners.AccountRequest{ID: a.ID, Holder: holder, Currency: CurrencyUSD})
	if err != nil {
		return TradingAccount{}, fmt.Errorf("%w: %v", ErrAccountNotOpened, err)
	}
	if opened.Status != partners.StatusCompleted {
		return TradingAccount{}, fmt.Errorf("%w: it answered %s: %s", ErrAccountNotOpened, opened.Status, opened.Detail)
	}
	a.PartnerAccountRef = opened.Ref

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TradingAccount{}, fmt.Errorf("economy: open trading account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO money_accounts (id, workspace_id, agent_id, currency, purpose, name, partner_account_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, a.AccountID, workspaceID, agentID, CurrencyUSD, MoneyBroker, a.Partner, a.PartnerAccountRef); err != nil {
		return TradingAccount{}, fmt.Errorf("economy: open trading account: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO agent_trading_accounts (id, workspace_id, agent_id, currency, partner, partner_account_ref, money_account_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (workspace_id, agent_id, currency) DO NOTHING`,
		a.ID, workspaceID, agentID, CurrencyUSD, a.Partner, a.PartnerAccountRef, a.AccountID)
	if err != nil {
		return TradingAccount{}, fmt.Errorf("economy: open trading account: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if err := tx.Commit(ctx); err != nil {
			return TradingAccount{}, fmt.Errorf("economy: open trading account: %w", err)
		}
	}
	// One opened at the same time is the agent's account; this one is rolled back.
	return s.TradingAccount(ctx, workspaceID, agentID)
}

// FundTradingAccount moves amountMinor cents of funding money — test or live — from agentID's USD account into its
// trading account, as one entry under key. The same key again answers the entry it first wrote.
func (s *DualTokenStore) FundTradingAccount(ctx context.Context, workspaceID, agentID string, amountMinor int64, funding, key string) (MoneyEntry, error) {
	if funding == "" {
		funding = FundingTest
	}
	if amountMinor <= 0 || strings.TrimSpace(key) == "" {
		return MoneyEntry{}, fmt.Errorf("%w: it needs a positive amount_minor and an idempotency_key", ErrTradingAccount)
	}
	ta, err := s.TradingAccount(ctx, workspaceID, agentID)
	if err != nil {
		return MoneyEntry{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: fund trading account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return MoneyEntry{}, err
	}
	// The money stays the agent's, so no spending rule counts it; the owner's mandate is asked before each order instead.
	if err := refuseIfPaused(ctx, tx, agentID); err != nil {
		return MoneyEntry{}, err
	}
	var from string
	err = tx.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND agent_id = $2 AND currency = 'USD'
		AND purpose = 'agent' AND status = 'open' ORDER BY created_at LIMIT 1`, workspaceID, agentID).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyEntry{}, ErrAgentUSDAccountNeeded
	}
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: fund trading account: %w", err)
	}
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityTradeEquities, Kind: "trading_account_funding",
		IdempotencyKey: "trading_account_funding:" + agentID + ":" + key, Funding: funding, Memo: "To the trading account at " + ta.Partner,
		Postings: []MoneyPosting{{AccountID: from, AmountMinor: -amountMinor}, {AccountID: ta.AccountID, AmountMinor: amountMinor}}}, s.screener)
	if err != nil {
		return MoneyEntry{}, err
	}
	// Read after posting, under the agent's lock: a replay posted nothing, and a new entry that overdraws is rolled back.
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		from, funding).Scan(&held); err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: fund trading account: %w", err)
	}
	if held < 0 {
		return MoneyEntry{}, fmt.Errorf("%w: the agent's USD account holds %s of %s money, and this is %s", ErrAgentFunds,
			minorString(held+amountMinor, CurrencyUSD), funding, minorString(amountMinor, CurrencyUSD))
	}
	if err := tx.Commit(ctx); err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: fund trading account: %w", err)
	}
	return e, nil
}

// TradingAccount is agentID's trading account in workspaceID and the cash it holds, test and live apart.
func (s *DualTokenStore) TradingAccount(ctx context.Context, workspaceID, agentID string) (TradingAccount, error) {
	var a TradingAccount
	err := s.pool.QueryRow(ctx, `SELECT t.id, t.agent_id, t.currency, t.partner, t.partner_account_ref, t.money_account_id, t.created_at,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'test'), 0)::bigint,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'live'), 0)::bigint
		FROM agent_trading_accounts t LEFT JOIN money_account_balances b ON b.account_id = t.money_account_id
		WHERE t.workspace_id = $1 AND t.agent_id = $2 AND t.currency = 'USD' GROUP BY t.id`, workspaceID, agentID).Scan(
		&a.ID, &a.AgentID, &a.Currency, &a.Partner, &a.PartnerAccountRef, &a.AccountID, &a.CreatedAt, &a.TestMinor, &a.LiveMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return TradingAccount{}, ErrTradingAccountNotFound
	}
	if err != nil {
		return TradingAccount{}, fmt.Errorf("economy: trading account: %w", err)
	}
	a.CashMinor = a.TestMinor + a.LiveMinor
	if a.Partner == "test" {
		a.Prices = simulatedPrices
	}
	return a, nil
}
