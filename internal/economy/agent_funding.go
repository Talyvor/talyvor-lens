package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// agent_funding.go — B61.66: THE OWNER GIVES AN AGENT MONEY FROM THE COMPANY'S ACCOUNT, AND TAKES IT BACK, NOW.
//
// One movement between the company's account in a currency and one of its agents' accounts in the same currency,
// either way, as the monthly allowance moves it (money_allowances.go): one entry under currency_accounts, kind
// agent_funding or agent_funding_back, its two postings summing to zero, under a key that holds both accounts, the
// direction and the caller's key, so the same request again is the same entry and moves nothing twice. Live money
// is refused, as the allowance's is, until currency_accounts is cleared. It is the owner's move: the agent's rules do
// not judge it and no passkey is asked. Neither account may give more than it holds in the funding moved — an agent's
// account holds nothing that is held for a payment, which sits in a hold account — and each move is an entry on the
// agent's audit chain in the same transaction.

// The directions of an agent funding.
const (
	ToAgent   = "to_agent"   // from the company's account into the agent's
	ToCompany = "to_company" // from the agent's account back into the company's
)

var (
	// ErrAgentFunding: a funding asked for with a direction, amount, key or funding it cannot have.
	ErrAgentFunding = errors.New("economy: invalid move between the company and an agent")
	// ErrAgentFundingShort: the account it comes from holds less than the amount.
	ErrAgentFundingShort = errors.New("economy: not enough in the account to move")
)

// AgentFunding is one move between the company's account and an agent's, and what each holds after it.
type AgentFunding struct {
	Entry               MoneyEntry `json:"entry"`
	Direction           string     `json:"direction"`
	Currency            string     `json:"currency"`
	AmountMinor         int64      `json:"amount_minor"`
	CompanyAccountID    string     `json:"company_account_id"`
	AgentAccountID      string     `json:"agent_account_id"`
	AgentID             string     `json:"agent_id"`
	CompanyBalanceMinor int64      `json:"company_balance_minor"` // in the funding moved
	AgentBalanceMinor   int64      `json:"agent_balance_minor"`
}

// MoveAgentMoney moves amountMinor between workspaceID's agent account agentAccountID and the company's open account in
// its currency: into it for ToAgent, back out of it for ToCompany. by is who asked, for the audit chain.
func (s *DualTokenStore) MoveAgentMoney(ctx context.Context, workspaceID, agentAccountID, direction string, amountMinor int64,
	funding, key, by string) (AgentFunding, error) {
	if funding == "" {
		funding = FundingTest
	}
	key = strings.TrimSpace(key)
	switch {
	case direction != ToAgent && direction != ToCompany:
		return AgentFunding{}, fmt.Errorf("%w: direction is %s or %s, not %q", ErrAgentFunding, ToAgent, ToCompany, direction)
	case amountMinor <= 0:
		return AgentFunding{}, fmt.Errorf("%w: it needs a positive amount_minor", ErrAgentFunding)
	case key == "":
		return AgentFunding{}, fmt.Errorf("%w: it needs an idempotency_key", ErrAgentFunding)
	case funding != FundingTest && funding != FundingLive:
		return AgentFunding{}, fmt.Errorf("%w: funding is test or live, not %q", ErrAgentFunding, funding)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentFunding{}, fmt.Errorf("economy: fund agent: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	f := AgentFunding{Direction: direction, AmountMinor: amountMinor, AgentAccountID: agentAccountID}
	err = tx.QueryRow(ctx, `SELECT a.agent_id, a.currency, c.id FROM money_accounts a
		JOIN money_accounts c ON c.workspace_id = a.workspace_id AND c.currency = a.currency AND c.purpose = 'company' AND c.status = 'open'
		WHERE a.id = $1 AND a.workspace_id = $2 AND a.purpose = 'agent' AND a.status = 'open'`,
		agentAccountID, workspaceID).Scan(&f.AgentID, &f.Currency, &f.CompanyAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentFunding{}, fmt.Errorf("%w: no open agent account %s with an open company account in its currency in this workspace",
			ErrMoneyAccountNotFound, agentAccountID)
	}
	if err != nil {
		return AgentFunding{}, fmt.Errorf("economy: fund agent: %w", err)
	}
	// As the allowance does: the balances read below are the ones posted against, with no payment between.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`,
		[]string{f.CompanyAccountID, agentAccountID}); err != nil {
		return AgentFunding{}, fmt.Errorf("economy: fund agent: %w", err)
	}
	held := func(account string) (int64, error) {
		var n int64
		err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
			account, funding).Scan(&n)
		return n, err
	}
	e := MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityCurrencyAccounts, Kind: "agent_funding",
		IdempotencyKey: "agent_funding:" + direction + ":" + f.CompanyAccountID + ":" + agentAccountID + ":" + key,
		Funding:        funding,
		Memo:           "From the company",
		Postings:       []MoneyPosting{{AccountID: f.CompanyAccountID, AmountMinor: -amountMinor}, {AccountID: agentAccountID, AmountMinor: amountMinor}}}
	from, whose := f.CompanyAccountID, "the company's"
	if direction == ToCompany {
		e.Kind, e.Memo = "agent_funding_back", "Back to the company"
		e.Postings = []MoneyPosting{{AccountID: agentAccountID, AmountMinor: -amountMinor}, {AccountID: f.CompanyAccountID, AmountMinor: amountMinor}}
		from, whose = agentAccountID, "the agent's"
	}
	var replay bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_entries WHERE workspace_id = $1 AND idempotency_key = $2)`,
		workspaceID, e.IdempotencyKey).Scan(&replay); err != nil {
		return AgentFunding{}, fmt.Errorf("economy: fund agent: %w", err)
	}
	if !replay && funding == FundingLive {
		// Live money is refused in the capability's words, as the allowance's is, before what the account holds is read.
		c, _ := CapabilityByKey(CapabilityCurrencyAccounts)
		refusal, err := capabilityLive(ctx, tx, workspaceID, c)
		if err != nil {
			return AgentFunding{}, err
		}
		if refusal != nil {
			return AgentFunding{}, refusal
		}
	}
	if !replay {
		has, err := held(from)
		if err != nil {
			return AgentFunding{}, fmt.Errorf("economy: fund agent: %w", err)
		}
		if has < amountMinor {
			return AgentFunding{}, fmt.Errorf("%w: %s %s account holds %s, less than %s", ErrAgentFundingShort, whose, f.Currency,
				minorString(has, f.Currency), minorString(amountMinor, f.Currency))
		}
	}
	if f.Entry, err = postMoneyTx(ctx, tx, e, noPartnerPostings); err != nil {
		return AgentFunding{}, err
	}
	if !replay {
		if err := appendAudit(ctx, tx, workspaceID, "agent:"+f.AgentID, "agent."+e.Kind, map[string]any{"by": by, "entry_id": f.Entry.ID,
			"direction": direction, "amount_minor": amountMinor, "currency": f.Currency, "funding": funding,
			"company_account_id": f.CompanyAccountID, "agent_account_id": agentAccountID}); err != nil {
			return AgentFunding{}, err
		}
	}
	if f.CompanyBalanceMinor, err = held(f.CompanyAccountID); err != nil {
		return AgentFunding{}, fmt.Errorf("economy: fund agent: %w", err)
	}
	if f.AgentBalanceMinor, err = held(agentAccountID); err != nil {
		return AgentFunding{}, fmt.Errorf("economy: fund agent: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return AgentFunding{}, fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return AgentFunding{}, fmt.Errorf("economy: fund agent: %w", err)
	}
	return f, nil
}
