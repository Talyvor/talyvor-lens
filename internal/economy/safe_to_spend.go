package economy

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// safe_to_spend.go — B41.105: SAFE TO SPEND, THE ONE NUMBER A PERSON ACTUALLY NEEDS.
//
// For each agent and for the company, in each currency it holds: the balance, less what is already promised out of it
// this period — the scheduled payments still to run before the month ends, the bills due by then (the company's, B41.101)
// and the payments waiting for approval. A hold is already out of the balance (a 'hold' posting debits it), so it is
// not taken off twice. The period is the calendar month (UTC) of at, as the month-end forecast's (B19.6).
//
// LXC is in µLXC; GBP, EUR and USD are in minor units. A figure may be negative: more is promised than is there.

// SafeToSpendFigure is one holder's safe-to-spend figure in one currency, and what it is made of.
type SafeToSpendFigure struct {
	Currency         string `json:"currency"` // LXC (µLXC) or GBP, EUR, USD (minor units)
	Balance          int64  `json:"balance"`
	Scheduled        int64  `json:"scheduled"`
	BillsDue         int64  `json:"bills_due"`
	PendingApprovals int64  `json:"pending_approvals"`
	SafeToSpend      int64  `json:"safe_to_spend"`
}

// AgentSafeToSpend is one agent's figures.
type AgentSafeToSpend struct {
	AgentID string              `json:"agent_id"`
	Name    string              `json:"name"`
	Figures []SafeToSpendFigure `json:"figures"`
}

// SafeToSpend is a workspace's safe-to-spend figures as of At, for the company and for each agent.
type SafeToSpend struct {
	At          time.Time           `json:"at"`
	PeriodStart time.Time           `json:"period_start"`
	PeriodEnd   time.Time           `json:"period_end"`
	Company     []SafeToSpendFigure `json:"company"`
	Agents      []AgentSafeToSpend  `json:"agents"`
}

// safeBook adds up figures by holder ("" is the company) and currency.
type safeBook map[string]map[string]*SafeToSpendFigure

func (b safeBook) at(holder, currency string) *SafeToSpendFigure {
	if b[holder] == nil {
		b[holder] = map[string]*SafeToSpendFigure{}
	}
	if b[holder][currency] == nil {
		b[holder][currency] = &SafeToSpendFigure{Currency: currency}
	}
	return b[holder][currency]
}

// figures lists a holder's figures, LXC first and then by currency, each netted.
func (b safeBook) figures(holder string) []SafeToSpendFigure {
	out := []SafeToSpendFigure{}
	for _, f := range b[holder] {
		f.SafeToSpend = f.Balance - f.Scheduled - f.BillsDue - f.PendingApprovals
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Currency == "LXC") != (out[j].Currency == "LXC") {
			return out[i].Currency == "LXC"
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// ticksBefore counts a schedule's ticks from its next run up to, not including, end, and none after its own end.
func ticksBefore(sc AgentSchedule, end time.Time) int64 {
	var n int64
	for t := sc.NextRunAt; t.Before(end) && (sc.EndAt == nil || !t.After(*sc.EndAt)); t = nextTick(t, sc.Every) {
		n++
	}
	return n
}

// SafeToSpend works out workspaceID's safe-to-spend figures for the month of at.
func (s *DualTokenStore) SafeToSpend(ctx context.Context, workspaceID string, at time.Time) (SafeToSpend, error) {
	at = at.UTC()
	start := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := SafeToSpend{At: at, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0), Agents: []AgentSafeToSpend{}}
	book := safeBook{}
	fail := func(err error) (SafeToSpend, error) { return out, fmt.Errorf("economy: safe to spend: %w", err) }

	// The company's LXC is the workspace's whole balance; each agent's is its own share of it.
	var wsBalance int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM lxc_balances WHERE workspace_id = $1), 0)::bigint`,
		workspaceID).Scan(&wsBalance); err != nil {
		return fail(err)
	}
	book.at("", "LXC").Balance = wsBalance
	rows, err := s.pool.Query(ctx, `SELECT a.id, a.name, COALESCE(b.balance_ulxc, 0)::bigint FROM agent_accounts a
		LEFT JOIN agent_account_balances b ON b.workspace_id = a.workspace_id AND b.account = 'agent:' || a.id
		WHERE a.workspace_id = $1 ORDER BY a.created_at, a.id`, workspaceID)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var a AgentSafeToSpend
		var balance int64
		if err := rows.Scan(&a.AgentID, &a.Name, &balance); err != nil {
			rows.Close()
			return fail(err)
		}
		book.at(a.AgentID, "LXC").Balance = balance
		out.Agents = append(out.Agents, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}

	accounts, err := s.CurrencyAccounts(ctx, workspaceID, "")
	if err != nil {
		return out, err
	}
	for _, a := range accounts {
		if a.AgentID != "" {
			book.at(a.AgentID, a.Currency).Balance += a.BalanceMinor
		}
		// An agent's account is a sub-account of the company's, kept apart in the ledger: the company holds both.
		book.at("", a.Currency).Balance += a.BalanceMinor
	}

	schedules, err := s.ListAgentSchedules(ctx, workspaceID)
	if err != nil {
		return out, err
	}
	for _, sc := range schedules {
		if !sc.Active {
			continue
		}
		n := ticksBefore(sc, out.PeriodEnd)
		if sc.ToPayeeID != "" { // a standing order pays out of the agent's account in its currency (B30.18)
			book.at(sc.FromAgentID, sc.Currency).Scheduled += n * sc.AmountMinor
			book.at("", sc.Currency).Scheduled += n * sc.AmountMinor
			continue
		}
		book.at(sc.FromAgentID, "LXC").Scheduled += n * sc.AmountULXC
		if sc.ToAgentID == "" { // paying another of the company's agents leaves the company's LXC where it was
			book.at("", "LXC").Scheduled += n * sc.AmountULXC
		}
	}

	rows, err = s.pool.Query(ctx, `SELECT currency, sum(amount_minor)::bigint FROM money_bills
		WHERE workspace_id = $1 AND status = 'due' AND currency <> '' AND (due_date IS NULL OR due_date < $2)
		GROUP BY currency`, workspaceID, out.PeriodEnd)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var currency string
		var due int64
		if err := rows.Scan(&currency, &due); err != nil {
			rows.Close()
			return fail(err)
		}
		book.at("", currency).BillsDue += due
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}

	// Waiting for approval: an agent's own LXC payments and model calls (B19.2), and payment requests' items (B40.3).
	rows, err = s.pool.Query(ctx, `
		SELECT agent_id, 'LXC', sum(amount_ulxc)::bigint FROM agent_approvals WHERE workspace_id = $1 AND status = 'pending'
		 GROUP BY agent_id
		UNION ALL
		SELECT r.agent_id, i.currency, sum(i.amount_minor)::bigint FROM payment_requests r
		  JOIN payment_request_items i ON i.request_id = r.id
		 WHERE r.workspace_id = $1 AND r.status = 'pending_approval' AND i.decision = 'needs_approval'
		 GROUP BY r.agent_id, i.currency`, workspaceID)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var agentID, currency string
		var pending int64
		if err := rows.Scan(&agentID, &currency, &pending); err != nil {
			rows.Close()
			return fail(err)
		}
		if agentID != "" {
			book.at(agentID, currency).PendingApprovals += pending
		}
		book.at("", currency).PendingApprovals += pending
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}

	out.Company = book.figures("")
	for i := range out.Agents {
		out.Agents[i].Figures = book.figures(out.Agents[i].AgentID)
	}
	return out, nil
}
