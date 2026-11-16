package economy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/workspace"
)

// agent_money_payments.go — B61.18: AN AGENT PAYS ANOTHER AGENT FROM ITS CURRENCY ACCOUNT (launch pack 1.5).
//
// An agent pays another agent — of its own workspace or another's — from its account in a currency into the other's
// account in the same currency: one money entry of two postings, the payer's account −amount and the payee's +amount,
// which both accounts' statements show. The payer's rules (B61.12) judge it as a payment from that account, in their own
// currency: outside them it is refused, and above the amount they ask the owner for it files an approval and moves
// nothing until the owner says yes and the agent pays again. It is made once under its idempotency key: the same key
// again answers the payment it made and judges nothing, and another payment under it is ErrIdempotencyKeyReused. Money of
// a kind either workspace does not hold is refused as wrong_money_kind (B59.42). Nothing is converted here: a payee with
// no account in the currency is refused in words, since Convert and Instant transfer move money between currencies.

var (
	// ErrAgentMoneyPayment: a payment to an agent without a payee, a positive amount in GBP, EUR, USD or USDC, or an
	// idempotency key.
	ErrAgentMoneyPayment = errors.New("economy: invalid payment to an agent")
	// ErrPayeeCurrency: the agent paid has no open account in the payment's currency.
	ErrPayeeCurrency = errors.New("economy: the agent paid has no account in this currency")
)

// AgentMoneyPayment is one payment from an agent's currency account to another agent's account in the same currency.
type AgentMoneyPayment struct {
	ID             string    `json:"id"` // its money entry
	WorkspaceID    string    `json:"workspace_id"`
	FromAgentID    string    `json:"from_agent_id"`
	FromAccountID  string    `json:"from_account_id"`
	ToWorkspaceID  string    `json:"to_workspace_id"`
	ToAgentID      string    `json:"to_agent_id"`
	ToAgentName    string    `json:"to_agent_name"`
	ToAccountID    string    `json:"to_account_id"`
	AmountMinor    int64     `json:"amount_minor"`
	Currency       string    `json:"currency"`
	Funding        string    `json:"funding"`
	Memo           string    `json:"memo,omitempty"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
}

// entryKey is the idempotency key of p's money entry: the paying agent's, so two agents' keys never meet.
func (p AgentMoneyPayment) entryKey() string {
	return "agent_money_pay:" + p.FromAgentID + ":" + p.IdempotencyKey
}

func (p *AgentMoneyPayment) normalise() error {
	p.ToAgentID = strings.TrimSpace(p.ToAgentID)
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	p.Memo = strings.Join(strings.Fields(p.Memo), " ")
	p.IdempotencyKey = strings.TrimSpace(p.IdempotencyKey)
	if p.Funding == "" {
		p.Funding = FundingTest
	}
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrAgentMoneyPayment, msg) }
	_, known := MoneyCurrencies[p.Currency]
	switch {
	case p.ToAgentID == "":
		return bad("a payment names the agent it pays (to_agent_id)")
	case !known:
		return bad(fmt.Sprintf("a payment is in GBP, EUR, USD or USDC, not %q", p.Currency))
	case p.AmountMinor <= 0:
		return bad("a payment's amount_minor is a positive number of pence, cents or millionths of a USDC")
	case p.Funding != FundingTest && p.Funding != FundingLive:
		return bad(fmt.Sprintf("funding is test or live, not %q", p.Funding))
	case p.IdempotencyKey == "" || len(p.IdempotencyKey) > 128:
		return bad("a payment carries an idempotency key (the Idempotency-Key header), up to 128 characters")
	case len(p.Memo) > 140:
		return bad("a payment's memo is up to 140 characters")
	}
	return nil
}

// openAgentAccount is agentID's open account in currency in workspaceID, or "" when it has none.
func openAgentAccount(ctx context.Context, q pgxDB, workspaceID, agentID, currency string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND agent_id = $2 AND purpose = 'agent'
		AND currency = $3 AND status = 'open' ORDER BY created_at DESC LIMIT 1`, workspaceID, agentID, currency).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("economy: agent's account: %w", err)
	}
	return id, nil
}

// paidAgentMoney is the payment p as the money entry e made it, or ErrIdempotencyKeyReused when e is another payment.
func paidAgentMoney(p AgentMoneyPayment, e MoneyEntry) (AgentMoneyPayment, error) {
	if e.Kind != "agent_payment" || len(e.Postings) != 2 || e.Funding != p.Funding || e.Memo != p.Memo ||
		e.Postings[0].AccountID != p.FromAccountID || e.Postings[0].AmountMinor != -p.AmountMinor || e.Postings[0].Currency != p.Currency ||
		e.Postings[1].AccountID != p.ToAccountID {
		return AgentMoneyPayment{}, ErrIdempotencyKeyReused
	}
	p.ID, p.CreatedAt = e.ID, e.CreatedAt
	return p, nil
}

// PayAgentMoney pays in.AmountMinor of in.Currency from workspaceID's agent fromAgentID's account in that currency to
// the agent in.ToAgentID (its id or handle) — this workspace's or another's — into its account in the same currency,
// under in.IdempotencyKey, judged by the payer's rules.
func (s *DualTokenStore) PayAgentMoney(ctx context.Context, workspaceID, fromAgentID string, in AgentMoneyPayment) (AgentMoneyPayment, error) {
	if err := in.normalise(); err != nil {
		return AgentMoneyPayment{}, err
	}
	p := in
	p.WorkspaceID, p.FromAgentID = workspaceID, fromAgentID
	to, err := s.ResolveWallet(ctx, p.ToAgentID)
	if err != nil {
		return AgentMoneyPayment{}, err
	}
	p.ToWorkspaceID, p.ToAgentID, p.ToAgentName = to.WorkspaceID, to.WalletID, to.Name
	if p.ToAgentID == fromAgentID {
		return AgentMoneyPayment{}, ErrSameAgent
	}
	// B25.1: test money moves only between test workspaces, real money only between real ones.
	if err := workspace.CheckMoneyWall(ctx, s.pool, workspaceID, p.ToWorkspaceID); err != nil {
		return AgentMoneyPayment{}, err
	}
	if err := checkMoneySide(ctx, s.pool, workspaceID, "currency", moneyKindOf(p.Currency)); err != nil {
		return AgentMoneyPayment{}, err
	}
	if err := checkMoneySide(ctx, s.pool, p.ToWorkspaceID, "to_agent_id", moneyKindOf(p.Currency)); err != nil {
		return AgentMoneyPayment{}, err
	}
	if p.FromAccountID, err = openAgentAccount(ctx, s.pool, workspaceID, fromAgentID, p.Currency); err != nil {
		return AgentMoneyPayment{}, err
	}
	if p.FromAccountID == "" {
		return AgentMoneyPayment{}, fmt.Errorf("%w: the agent has no open %s account to pay from", ErrMoneyAccountNotFound, p.Currency)
	}
	if p.ToAccountID, err = openAgentAccount(ctx, s.pool, p.ToWorkspaceID, p.ToAgentID, p.Currency); err != nil {
		return AgentMoneyPayment{}, err
	}
	if p.ToAccountID == "" {
		return AgentMoneyPayment{}, fmt.Errorf("%w: %s has no %s account, so it cannot be paid in %s. An agent pays another in a currency both hold; Convert or Instant transfer move money between currencies",
			ErrPayeeCurrency, cmp.Or(p.ToAgentName, p.ToAgentID), p.Currency, p.Currency)
	}
	// The same key again answers the payment it made, and judges nothing again.
	if e, err := readMoneyEntry(ctx, s.pool, `e.workspace_id = $1 AND e.idempotency_key = $2`, workspaceID, p.entryKey()); err == nil {
		return paidAgentMoney(p, e)
	}
	if s.accountPartners == nil && !dollarLike(p.Currency) {
		return AgentMoneyPayment{}, fmt.Errorf("%w: no account partner is set to value the payment", ErrAccountNotOpened)
	}
	value, err := s.paymentValue(ctx, OutsidePayment{WorkspaceID: workspaceID, IdempotencyKey: p.entryKey(), AmountMinor: p.AmountMinor, Currency: p.Currency})
	if err != nil {
		return AgentMoneyPayment{}, err
	}
	fp := sha256.Sum256([]byte("agent_money_pay\x00" + fromAgentID + "\x00" + p.FromAccountID + "\x00" + p.ToAgentID + "\x00" +
		strconv.FormatInt(p.AmountMinor, 10) + "\x00" + p.Currency + "\x00" + p.Funding + "\x00" + p.Memo))
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]),
		Payee:    Payee{Kind: "agent", ID: p.ToAgentID, Name: p.ToAgentName},
		Memo:     strings.TrimSuffix(Money(p.AmountMinor, p.Currency)+" to "+cmp.Or(p.ToAgentName, p.ToAgentID)+" — "+p.Memo, " — "),
		PaidFrom: PaidFrom{Kind: "account", AccountID: p.FromAccountID, Currency: p.Currency, AmountMinor: p.AmountMinor, funding: p.Funding}})

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentMoneyPayment{}, fmt.Errorf("economy: pay an agent: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, fromAgentID); err != nil {
		return AgentMoneyPayment{}, err
	}
	// Read again under the agent's lock: the same key paid twice at once pays once, and the second answers the first.
	if e, err := readMoneyEntry(ctx, tx, `e.workspace_id = $1 AND e.idempotency_key = $2`, workspaceID, p.entryKey()); err == nil {
		return paidAgentMoney(p, e)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, p.FromAccountID); err != nil {
		return AgentMoneyPayment{}, fmt.Errorf("economy: pay an agent: %w", err)
	}
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		p.FromAccountID, p.Funding).Scan(&held); err != nil {
		return AgentMoneyPayment{}, fmt.Errorf("economy: pay an agent: %w", err)
	}
	if held < p.AmountMinor {
		return AgentMoneyPayment{}, fmt.Errorf("%w: the account holds %s of %s money, and this payment is %s", ErrAgentFunds,
			Money(held, p.Currency), p.Funding, Money(p.AmountMinor, p.Currency))
	}
	if err := s.enforceAgentRules(ctx, tx, workspaceID, fromAgentID, value, p.entryKey()); err != nil {
		return AgentMoneyPayment{}, s.refusedMovement(ctx, tx, err)
	}
	e := MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityCurrencyAccounts, Kind: "agent_payment", IdempotencyKey: p.entryKey(),
		Funding: p.Funding, Memo: p.Memo, Postings: []MoneyPosting{
			{AccountID: p.FromAccountID, AmountMinor: -p.AmountMinor, Currency: p.Currency},
			{AccountID: p.ToAccountID, AmountMinor: p.AmountMinor, Currency: p.Currency}}}
	if p.ToWorkspaceID != workspaceID {
		e.payeeWorkspaceID = p.ToWorkspaceID // the payee's agent account, credited from this workspace's entry
	}
	if e, err = postMoneyTx(ctx, tx, e, s.screener); err != nil {
		return AgentMoneyPayment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return AgentMoneyPayment{}, fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return AgentMoneyPayment{}, fmt.Errorf("economy: pay an agent: %w", err)
	}
	p.ID, p.CreatedAt = e.ID, e.CreatedAt
	return p, nil
}
