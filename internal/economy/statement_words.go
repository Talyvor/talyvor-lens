package economy

import (
	"context"
	"fmt"
	"strings"
)

// statement_words.go — B61.131: what a statement document (the branded PDF) writes for a person in place of the
// server's codes (DESIGN.md 16.1). The JSON and the CSV keep the codes; a person never reads one.

// kindWords is each kind of line in words: the web's (AccountStatement.tsx KIND_WORDS, Withdrawal for payout), then
// an agent wallet's postings in the words the web's ledger uses for them (format.ts).
var kindWords = map[string]string{
	"card_charge": "Card payment", "card_refund": "Refund", "card_dispute_won": "Dispute won", "payment_out": "Paid out",
	"payment_in": "Paid in", "invoice_payment": "Invoice paid", "mandate_pull": "Collected under a mandate",
	"payout": "Withdrawal", "return": "Returned", "test_money": "Paid in", "chain_deposit": "Paid in",
	"fund": "Funded", "withdraw": "Withdrawal", "spend": "Spent", "settle": "Spent", "hold": "Held for a request",
	"release": "Released", "card": "Card payment", "platform_fee": "Talyvor fee", "pay": "Transfer", "transfer": "Transfer",
	"transfer_in": "Transfer in", "transfer_out": "Transfer out", "escrow": "Escrow", "cash_out": "Cash-out",
	"agent_funding": "Funded", "agent_funding_back": "Transfer", "person_payment": "Paid out", "standing_order": "Paid out",
	"payment_in_returned": "Returned", "invoice_card": "Invoice paid", "invoice_card_returned": "Returned", "test_top_up": "Paid in",
	"instant_transfer_move": "Instant transfer", "instant_transfer_payout": "Instant transfer",
}

// KindWords is kind in words. A kind with none is "Money in" or "Money out" by the way amountMinor moved, never the code.
func KindWords(kind string, amountMinor int64) string {
	if w, ok := kindWords[kind]; ok {
		return w
	}
	if amountMinor < 0 {
		return "Money out"
	}
	return "Money in"
}

// StateWords is where a line's money stands, in words, only when it is not complete: "" for completed.
func StateWords(state string) string {
	switch state {
	case "pending", "sent":
		return "Pending"
	case "failed":
		return "Failed"
	case "returned":
		return "Returned"
	}
	return ""
}

var statementCurrencyWords = map[string][2]string{
	CurrencyUSD: {"US dollar balance", "US dollars"}, CurrencyEUR: {"Euro balance", "euros"},
	CurrencyGBP: {"British pound balance", "British pounds"}, CurrencyUSDC: {"USDC balance", "USDC"}, "EURC": {"EURC balance", "EURC"},
}

// BalanceName is a currency's balance as a statement names it: "US dollar balance".
func BalanceName(currency string) string {
	if w, ok := statementCurrencyWords[currency]; ok {
		return w[0]
	}
	return currency + " balance"
}

// CurrencyWords is a currency as a sentence names it: "US dollars".
func CurrencyWords(currency string) string {
	if w, ok := statementCurrencyWords[currency]; ok {
		return w[1]
	}
	return currency
}

// MoneyFigure is a non-negative amount as Money writes it, without its sign or currency, for a column of figures:
// "13,315.97".
func MoneyFigure(minor int64, currency string) string {
	if minor < 0 {
		minor = -minor
	}
	if currency == "LXC" {
		return grouped(minor*usdTenMillionthsPerULXC, 7, 2)
	}
	if places, ok := MoneyCurrencies[currency]; ok {
		return grouped(minor, places, 2)
	}
	return strings.TrimSuffix(strings.TrimPrefix(FormatMinor(minor, currency), "-"), " "+currency)
}

// StatementWho is who a statement document is for, as its first page names them.
type StatementWho struct {
	Holder      string            // the workspace's name
	AccountType string            // "Business account" or "Personal account"
	Agents      map[string]string // each of the workspace's agents' names, by id
	Accounts    map[string]string // each of its money accounts as a line names it: "The company", or its agent's name
	Sandbox     bool              // the money it states is not live: it says so on every page
}

// StatementWho is workspaceID's name, its kind of account and its agents' names, and whether capability's money is
// Sandbox for it. A label that cannot be read is Sandbox.
func (s *DualTokenStore) StatementWho(ctx context.Context, workspaceID, capability string) (StatementWho, error) {
	who := StatementWho{Agents: map[string]string{}, Accounts: map[string]string{}, Sandbox: true}
	var kind string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), ''),
		COALESCE((SELECT account_type FROM workspaces WHERE id = $1), '')`, workspaceID).Scan(&who.Holder, &kind); err != nil {
		return who, fmt.Errorf("economy: statement holder: %w", err)
	}
	who.AccountType = "Business account"
	if kind == "personal" {
		who.AccountType = "Personal account"
	}
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM agent_accounts WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return who, fmt.Errorf("economy: statement agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return who, fmt.Errorf("economy: statement agents: %w", err)
		}
		who.Agents[id] = name
	}
	if err := rows.Err(); err != nil {
		return who, fmt.Errorf("economy: statement agents: %w", err)
	}
	accounts, err := s.pool.Query(ctx, `SELECT a.id, CASE WHEN a.purpose = 'company' THEN 'The company' ELSE COALESCE(g.name, '') END
		FROM money_accounts a LEFT JOIN agent_accounts g ON g.id = a.agent_id
		WHERE a.workspace_id = $1 AND a.purpose IN ('company', 'agent')`, workspaceID)
	if err != nil {
		return who, fmt.Errorf("economy: statement accounts: %w", err)
	}
	defer accounts.Close()
	for accounts.Next() {
		var id, name string
		if err := accounts.Scan(&id, &name); err != nil {
			return who, fmt.Errorf("economy: statement accounts: %w", err)
		}
		who.Accounts[id] = name
	}
	if err := accounts.Err(); err != nil {
		return who, fmt.Errorf("economy: statement accounts: %w", err)
	}
	if labels, err := s.WorkspaceLabels(ctx, workspaceID); err == nil {
		who.Sandbox = labels[capability] != LabelLive
	}
	return who, nil
}
