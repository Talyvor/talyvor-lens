package economy

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// briefing.go — B41.106: THE 30-SECOND DAILY BRIEFING.
//
// What a workspace's owner reads first each morning, decisions first (B41.6): the approvals waiting for them, anything
// blocked and why, the bills due this week, then what the agents spent yesterday — each payment, and each agent's other
// spend. Yesterday is the UTC day before at's. A day with none of these is quiet, and Line says so in one sentence.
//
// LXC amounts are in µLXC; GBP, EUR, USD and USDC are in their minor units.

// The kinds of a briefing item, in the order a briefing lists them.
const (
	BriefingApproval   = "approval"    // a payment or model call waiting for the owner's approval
	BriefingBlocked    = "blocked"     // a paused agent, a refused payment request item, a payment that failed or came back
	BriefingBill       = "bill"        // a bill due within the week, or overdue
	BriefingPayment    = "payment"     // a payment an agent or the company made yesterday
	BriefingAgentSpend = "agent_spend" // an agent's other spend yesterday: model calls, cards, fees
)

// BriefingItem is one line of a briefing.
type BriefingItem struct {
	Kind     string `json:"kind"`
	ID       string `json:"id,omitempty"`       // the approval, payment request, bill or payment
	AgentID  string `json:"agent_id,omitempty"` // the agent it is about; "" for the company's own
	Name     string `json:"name"`               // who is paid, the supplier, or the agent
	Amount   int64  `json:"amount"`             // 0 for a paused agent
	Currency string `json:"currency,omitempty"`
	Reason   string `json:"reason,omitempty"`   // why it waits or is blocked
	DueDate  string `json:"due_date,omitempty"` // a bill's, YYYY-MM-DD
	Text     string `json:"text"`               // the item as one sentence, for a push or a spoken briefing
}

// BriefingTotal is what was spent yesterday in one currency.
type BriefingTotal struct {
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
}

// Briefing is a workspace's daily briefing as of At.
type Briefing struct {
	At             time.Time       `json:"at"`
	DayStart       time.Time       `json:"day_start"` // yesterday, UTC
	DayEnd         time.Time       `json:"day_end"`
	Items          []BriefingItem  `json:"items"`
	SpentYesterday []BriefingTotal `json:"spent_yesterday"`
	Quiet          bool            `json:"quiet"`
	Line           string          `json:"line"` // the whole briefing in one line
}

// briefingAmount writes an amount for a person: µLXC as LXC, minor units as FormatMinor does.
func briefingAmount(amount int64, currency string) string {
	if currency != "LXC" {
		return FormatMinor(amount, currency)
	}
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	out := fmt.Sprintf("%d", amount/1_000_000)
	if frac := strings.TrimRight(fmt.Sprintf("%06d", amount%1_000_000), "0"); frac != "" {
		out += "." + frac
	}
	return sign + out + " LXC"
}

// DailyBriefing reads workspaceID's briefing for the morning of at.
func (s *DualTokenStore) DailyBriefing(ctx context.Context, workspaceID string, at time.Time) (Briefing, error) {
	at = at.UTC()
	today := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	b := Briefing{At: at, DayStart: today.AddDate(0, 0, -1), DayEnd: today, Items: []BriefingItem{}, SpentYesterday: []BriefingTotal{}}
	fail := func(err error) (Briefing, error) { return b, fmt.Errorf("economy: briefing: %w", err) }
	add := func(it BriefingItem) { b.Items = append(b.Items, it) }

	// Waiting for approval: an agent's LXC payments and model calls (B19.2), and payment requests' items (B40.3).
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, a.id, CASE WHEN p.payee_name <> '' THEN p.payee_name ELSE a.name END, p.amount_ulxc, 'LXC', p.payee_name, p.reason, p.created_at FROM agent_approvals p
		  JOIN agent_accounts a ON a.id = p.agent_id WHERE p.workspace_id = $1 AND p.status = 'pending'
		UNION ALL
		SELECT r.id, r.agent_id, COALESCE(min(i.payee_name), ''), sum(i.amount_minor)::bigint, i.currency,
		       CASE WHEN count(*) > 1 THEN count(*)::text || ' payments' ELSE COALESCE(min(i.payee_name), '') END, min(i.reason), r.created_at
		  FROM payment_requests r JOIN payment_request_items i ON i.request_id = r.id
		 WHERE r.workspace_id = $1 AND r.status = 'pending_approval' AND i.decision = 'needs_approval'
		 GROUP BY r.id, r.agent_id, i.currency, r.created_at
		ORDER BY 8, 1`, workspaceID)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var it BriefingItem
		var payee string
		var created time.Time
		if err := rows.Scan(&it.ID, &it.AgentID, &it.Name, &it.Amount, &it.Currency, &payee, &it.Reason, &created); err != nil {
			rows.Close()
			return fail(err)
		}
		it.Kind = BriefingApproval
		it.Text = briefingAmount(it.Amount, it.Currency) + " waits for your approval"
		if payee != "" {
			it.Text += ", to " + payee
		}
		add(it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}

	// Blocked: every agent paused, an agent paused, an item a payment request refused yesterday, a payment that failed.
	rows, err = s.pool.Query(ctx, `
		SELECT '', '', '', 0::bigint, '', reason, paused_at FROM agent_workspace_pauses WHERE workspace_id = $1
		UNION ALL
		SELECT '', id, name, 0::bigint, '', paused_reason, paused_at FROM agent_accounts WHERE workspace_id = $1 AND paused_at IS NOT NULL
		UNION ALL
		SELECT r.id, r.agent_id, i.payee_name, i.amount_minor, i.currency, i.reason, i.created_at
		  FROM payment_requests r JOIN payment_request_items i ON i.request_id = r.id
		 WHERE r.workspace_id = $1 AND i.decision = 'blocked' AND i.created_at >= $2 AND i.created_at < $3
		UNION ALL
		SELECT id, agent_id, payee_name, amount_minor, currency, CASE WHEN status = 'returned' THEN 'it came back' ELSE 'it failed' END
		       || CASE WHEN detail <> '' THEN ': ' || detail ELSE '' END, updated_at
		  FROM money_payments WHERE workspace_id = $1 AND status IN ('failed', 'returned') AND updated_at >= $2 AND updated_at < $3
		ORDER BY 7, 1`, workspaceID, b.DayStart, b.DayEnd)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var it BriefingItem
		var when time.Time
		if err := rows.Scan(&it.ID, &it.AgentID, &it.Name, &it.Amount, &it.Currency, &it.Reason, &when); err != nil {
			rows.Close()
			return fail(err)
		}
		it.Kind = BriefingBlocked
		switch {
		case it.Amount > 0:
			it.Text = briefingAmount(it.Amount, it.Currency) + " to " + it.Name + " was blocked"
		case it.AgentID == "":
			it.Name, it.Text = "Every agent", "Every agent is paused"
		default:
			it.Text = it.Name + " is paused"
		}
		if it.Reason != "" {
			it.Text += ": " + it.Reason
		}
		add(it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}

	// Bills due within the week, the overdue ones and those with no due date among them, soonest first.
	rows, err = s.pool.Query(ctx, `SELECT id, supplier, amount_minor, currency, COALESCE(to_char(due_date, 'YYYY-MM-DD'), '')
		FROM money_bills WHERE workspace_id = $1 AND status = 'due' AND (due_date IS NULL OR due_date < $2::date)
		ORDER BY due_date NULLS LAST, created_at, id`, workspaceID, today.AddDate(0, 0, 7))
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		it := BriefingItem{Kind: BriefingBill}
		if err := rows.Scan(&it.ID, &it.Name, &it.Amount, &it.Currency, &it.DueDate); err != nil {
			rows.Close()
			return fail(err)
		}
		it.Text = "A bill from " + it.Name + " for " + briefingAmount(it.Amount, it.Currency) + " is due"
		if it.DueDate != "" {
			it.Text += " on " + it.DueDate
		}
		add(it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}

	// Spent yesterday: each payment — an agent's LXC payment to another agent, a payment out of a currency account —
	// then each agent's other spend, netted, as its spending rules count it (agentSpendPostings).
	rows, err = s.pool.Query(ctx, `
		SELECT 'payment', p.entry_id::text, a.id, COALESCE((SELECT b.name FROM agent_postings q JOIN agent_accounts b
		         ON b.workspace_id = q.workspace_id AND q.account = 'agent:' || b.id WHERE q.entry_id = p.entry_id AND q.amount_ulxc > 0 LIMIT 1), ''),
		       -p.amount_ulxc, 'LXC', p.ref, p.created_at -- a payment's ref is its memo (agent_payments.go)
		  FROM agent_postings p JOIN agent_accounts a ON a.workspace_id = p.workspace_id AND p.account = 'agent:' || a.id
		 WHERE p.workspace_id = $1 AND p.kind = 'pay' AND p.amount_ulxc < 0 AND p.created_at >= $2 AND p.created_at < $3
		UNION ALL
		SELECT 'payment', id, agent_id, payee_name, amount_minor, currency, reference, created_at FROM money_payments
		 WHERE workspace_id = $1 AND status IN ('pending', 'sent', 'completed') AND created_at >= $2 AND created_at < $3
		UNION ALL
		SELECT 'agent_spend', '', a.id, a.name, -sum(p.amount_ulxc)::bigint, 'LXC', '', max(p.created_at)
		  FROM agent_postings p JOIN agent_accounts a ON a.workspace_id = p.workspace_id AND p.account = 'agent:' || a.id
		 WHERE p.workspace_id = $1 AND p.kind IN ('spend', 'hold', 'settle', 'release', 'card', 'platform_fee')
		   AND p.created_at >= $2 AND p.created_at < $3
		 GROUP BY a.id, a.name HAVING sum(p.amount_ulxc) < 0
		ORDER BY 1 DESC, 8, 2`, workspaceID, b.DayStart, b.DayEnd)
	if err != nil {
		return fail(err)
	}
	totals := map[string]int64{}
	var order []string
	for rows.Next() {
		var it BriefingItem
		var memo string
		var when time.Time
		if err := rows.Scan(&it.Kind, &it.ID, &it.AgentID, &it.Name, &it.Amount, &it.Currency, &memo, &when); err != nil {
			rows.Close()
			return fail(err)
		}
		if it.Kind == BriefingPayment {
			it.Text = briefingAmount(it.Amount, it.Currency) + " was paid to " + it.Name
			if memo != "" {
				it.Text += " for " + memo
			}
		} else {
			it.Text = it.Name + " spent " + briefingAmount(it.Amount, it.Currency)
		}
		if _, ok := totals[it.Currency]; !ok {
			order = append(order, it.Currency)
		}
		totals[it.Currency] += it.Amount
		add(it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	for _, c := range order {
		b.SpentYesterday = append(b.SpentYesterday, BriefingTotal{Currency: c, Amount: totals[c]})
	}

	b.Quiet = len(b.Items) == 0
	b.Line = briefingLine(b)
	return b, nil
}

// briefingLine is the briefing in one line: how many of each kind, and what was spent.
func briefingLine(b Briefing) string {
	if b.Quiet {
		return "A quiet day: nothing was spent yesterday, and nothing is waiting for you."
	}
	n := map[string]int{}
	for _, it := range b.Items {
		n[it.Kind]++
	}
	count := func(k, one, many string) string {
		switch n[k] {
		case 0:
			return ""
		case 1:
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n[k], many)
	}
	var parts []string
	for _, p := range []string{
		count(BriefingApproval, "approval waiting", "approvals waiting"),
		count(BriefingBlocked, "thing blocked", "things blocked"),
		count(BriefingBill, "bill due this week", "bills due this week"),
		count(BriefingPayment, "payment yesterday", "payments yesterday"),
	} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(b.SpentYesterday) > 0 {
		var spent []string
		for _, t := range b.SpentYesterday {
			spent = append(spent, briefingAmount(t.Amount, t.Currency))
		}
		parts = append(parts, "spent yesterday "+strings.Join(spent, " and "))
	}
	return strings.Join(parts, ", ") + "."
}
