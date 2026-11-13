package economy

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"
)

// money_overview.go — B30.28: THE CASH VIEW: EVERY CURRENCY, EVERY AGENT, AND THE RUNWAY.
//
// MoneyOverview reads, for a workspace — or with agentID for one agent — what its own accounts hold in each currency
// (company, agent and pot accounts, test and live apart), the money that came in and went out this calendar month
// (UTC), the next 30 days of standing orders and mandates, and the runway: the days the balance lasts at the rate the
// last 30 days spent. Money in and out is netted per entry across the accounts in view, so a move between them is
// neither: for the company a transfer to its agent nets to zero, for the agent it is money in.

// overviewDays is how far the overview looks back for the rate of spend, and forward for what is due.
const overviewDays = 30

// cashPurposes are the accounts whose money is the company's or an agent's to spend.
const cashPurposes = `('company', 'agent', 'pot')`

// CashTotal is what accounts hold in one currency, what moved this month, and how long it lasts.
type CashTotal struct {
	Currency          string `json:"currency"`
	BalanceMinor      int64  `json:"balance_minor"`
	TestMinor         int64  `json:"test_minor"`
	LiveMinor         int64  `json:"live_minor"`
	InThisMonthMinor  int64  `json:"in_this_month_minor"`
	OutThisMonthMinor int64  `json:"out_this_month_minor"`
	Out30DaysMinor    int64  `json:"out_last_30_days_minor"`
	// RunwayDays is the balance over the last 30 days' spend a day; nil when nothing went out in them.
	RunwayDays *int64     `json:"runway_days"`
	RunsOutOn  *time.Time `json:"runs_out_on,omitempty"`
}

// AgentCash is one agent's totals, a currency each.
type AgentCash struct {
	AgentID string      `json:"agent_id"`
	Name    string      `json:"name"`
	Totals  []CashTotal `json:"totals"`
}

// UpcomingPayment is a standing order or a mandate that may take money in the next 30 days.
type UpcomingPayment struct {
	Kind     string `json:"kind"` // standing_order | mandate
	ID       string `json:"id"`
	AgentID  string `json:"agent_id"`
	Payee    string `json:"payee"` // a standing order's saved payee id; a mandate's payee name
	Currency string `json:"currency"`
	// AmountMinor is a standing order's payment each time; a mandate's maximum per pull.
	AmountMinor int64      `json:"amount_minor"`
	Every       string     `json:"every,omitempty"`
	NextAt      *time.Time `json:"next_at,omitempty"`
	Times       int        `json:"times,omitempty"`
	// TotalMinor is the most it takes in the 30 days: every payment of a standing order, all a mandate may still pull.
	TotalMinor int64 `json:"total_minor"`
}

// MoneyOverview is the cash view of a workspace, or of one of its agents.
type MoneyOverview struct {
	WorkspaceID string            `json:"workspace_id"`
	AgentID     string            `json:"agent_id,omitempty"`
	AsOf        time.Time         `json:"as_of"`
	Totals      []CashTotal       `json:"totals"`
	Agents      []AgentCash       `json:"agents"`
	Upcoming    []UpcomingPayment `json:"upcoming"`
}

// MoneyOverview reads workspaceID's cash view at now — with agentID, that agent's alone.
func (s *DualTokenStore) MoneyOverview(ctx context.Context, workspaceID, agentID string, now time.Time) (MoneyOverview, error) {
	now = now.UTC()
	o := MoneyOverview{WorkspaceID: workspaceID, AgentID: agentID, AsOf: now, Totals: []CashTotal{}, Agents: []AgentCash{}, Upcoming: []UpcomingPayment{}}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	since := now.AddDate(0, 0, -overviewDays)

	// whole[currency] is everything in view; agents[id][currency] each agent's.
	whole := map[string]*CashTotal{}
	agents := map[string]map[string]*CashTotal{}
	names := map[string]string{}
	at := func(agent, currency string) *CashTotal {
		m := whole
		if agent != "" {
			if agents[agent] == nil {
				agents[agent] = map[string]*CashTotal{}
				o.Agents = append(o.Agents, AgentCash{AgentID: agent})
			}
			m = agents[agent]
		}
		if m[currency] == nil {
			m[currency] = &CashTotal{Currency: currency}
		}
		return m[currency]
	}

	rows, err := s.pool.Query(ctx, `SELECT COALESCE(a.agent_id, ''), COALESCE(g.name, ''), a.currency,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'test'), 0)::bigint,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'live'), 0)::bigint
		FROM money_accounts a LEFT JOIN money_account_balances b ON b.account_id = a.id LEFT JOIN agent_accounts g ON g.id = a.agent_id
		WHERE a.workspace_id = $1 AND a.purpose IN `+cashPurposes+` AND ($2 = '' OR a.agent_id = $2)
		GROUP BY 1, 2, 3 ORDER BY 1, 3`, workspaceID, agentID)
	if err != nil {
		return MoneyOverview{}, fmt.Errorf("economy: money overview: %w", err)
	}
	for rows.Next() {
		var agent, name, currency string
		var test, live int64
		if err := rows.Scan(&agent, &name, &currency, &test, &live); err != nil {
			rows.Close()
			return MoneyOverview{}, fmt.Errorf("economy: money overview: %w", err)
		}
		add := func(t *CashTotal) {
			t.TestMinor, t.LiveMinor, t.BalanceMinor = t.TestMinor+test, t.LiveMinor+live, t.BalanceMinor+test+live
		}
		add(at("", currency))
		if agent != "" {
			add(at(agent, currency))
			names[agent] = name
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MoneyOverview{}, fmt.Errorf("economy: money overview: %w", err)
	}

	// Each entry's net over the accounts in view — once for all of them (whole) and once per agent — is money in when
	// positive and out when negative.
	rows, err = s.pool.Query(ctx, `SELECT x.whole, COALESCE(x.agent_id, ''), x.currency,
		COALESCE(sum(x.net) FILTER (WHERE x.net > 0 AND x.at >= $3), 0)::bigint,
		COALESCE(-sum(x.net) FILTER (WHERE x.net < 0 AND x.at >= $3), 0)::bigint,
		COALESCE(-sum(x.net) FILTER (WHERE x.net < 0 AND x.at >= $4), 0)::bigint
		FROM (
		  SELECT GROUPING(a.agent_id) = 1 AS whole, a.agent_id, a.currency, max(e.created_at) AS at, sum(p.amount_minor) AS net
		    FROM money_postings p JOIN money_accounts a ON a.id = p.account_id JOIN money_entries e ON e.id = p.entry_id
		   WHERE a.workspace_id = $1 AND a.purpose IN `+cashPurposes+` AND ($2 = '' OR a.agent_id = $2)
		     AND e.created_at >= LEAST($3::timestamptz, $4::timestamptz)
		   GROUP BY GROUPING SETS ((e.id, a.currency, a.agent_id), (e.id, a.currency))
		) x
		WHERE x.whole OR x.agent_id IS NOT NULL
		GROUP BY 1, 2, 3`, workspaceID, agentID, month, since)
	if err != nil {
		return MoneyOverview{}, fmt.Errorf("economy: money overview flows: %w", err)
	}
	for rows.Next() {
		var isWhole bool
		var agent, currency string
		var in, out, out30 int64
		if err := rows.Scan(&isWhole, &agent, &currency, &in, &out, &out30); err != nil {
			rows.Close()
			return MoneyOverview{}, fmt.Errorf("economy: money overview flows: %w", err)
		}
		if isWhole {
			agent = ""
		}
		t := at(agent, currency)
		t.InThisMonthMinor, t.OutThisMonthMinor, t.Out30DaysMinor = in, out, out30
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MoneyOverview{}, fmt.Errorf("economy: money overview flows: %w", err)
	}

	runway := func(m map[string]*CashTotal) []CashTotal {
		out := []CashTotal{}
		for _, cur := range slices.Sorted(maps.Keys(m)) {
			t := *m[cur]
			if t.Out30DaysMinor > 0 {
				days := runwayDays(t.BalanceMinor, t.Out30DaysMinor)
				on := time.Date(now.Year(), now.Month(), now.Day()+int(days), 0, 0, 0, 0, time.UTC)
				t.RunwayDays, t.RunsOutOn = &days, &on
			}
			out = append(out, t)
		}
		return out
	}
	o.Totals = runway(whole)
	for i := range o.Agents {
		o.Agents[i].Name = names[o.Agents[i].AgentID]
		o.Agents[i].Totals = runway(agents[o.Agents[i].AgentID])
	}

	if o.Upcoming, err = s.upcomingPayments(ctx, workspaceID, agentID, now); err != nil {
		return MoneyOverview{}, err
	}
	return o, nil
}

// runwayDays is how many whole days balance lasts at out30 over 30 days, without overflowing a large balance.
func runwayDays(balance, out30 int64) int64 {
	if balance <= 0 {
		return 0
	}
	return balance/out30*overviewDays + balance%out30*overviewDays/out30
}

// upcomingPayments is what the active standing orders and mandates of workspaceID — with agentID, that agent's — may
// take before now plus 30 days.
func (s *DualTokenStore) upcomingPayments(ctx context.Context, workspaceID, agentID string, now time.Time) ([]UpcomingPayment, error) {
	end := now.AddDate(0, 0, overviewDays)
	out := []UpcomingPayment{}
	schedules, err := s.ListAgentSchedules(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	for _, sc := range schedules {
		if !sc.Active || sc.AwaitingSignature || sc.ToPayeeID == "" || sc.FromAgentID == "" || (agentID != "" && sc.FromAgentID != agentID) {
			continue
		}
		u := UpcomingPayment{Kind: "standing_order", ID: sc.ID, AgentID: sc.FromAgentID, Payee: sc.ToPayeeID, Currency: sc.Currency,
			AmountMinor: sc.AmountMinor, Every: sc.Every}
		// A tick missed while Lens was down still runs once, so one due before now counts too.
		for t := sc.NextRunAt; t.Before(end) && (sc.EndAt == nil || !t.After(*sc.EndAt)); t = nextTick(t, sc.Every) {
			if u.Times == 0 {
				next := t.UTC()
				u.NextAt = &next
			}
			u.Times++
		}
		if u.Times > 0 {
			u.TotalMinor = sc.AmountMinor * int64(u.Times)
			out = append(out, u)
		}
	}
	granted, _, err := s.Mandates(ctx, workspaceID, agentID)
	if err != nil {
		return nil, err
	}
	for _, m := range granted {
		if m.Status != MandateActive {
			continue
		}
		// What is left of this month's maximum, and a whole month's for each month that starts before the window ends.
		total := max(m.MaxPerMonthMinor-m.PulledThisMonthMinor, 0)
		stop := end
		if m.ExpiresAt.Before(stop) {
			stop = m.ExpiresAt
		}
		for t := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC); t.Before(stop); t = t.AddDate(0, 1, 0) {
			total += m.MaxPerMonthMinor
		}
		out = append(out, UpcomingPayment{Kind: "mandate", ID: m.ID, AgentID: m.AgentID, Payee: m.PayeeName, Currency: m.Currency,
			AmountMinor: m.MaxPerPullMinor, TotalMinor: total})
	}
	return out, nil
}
