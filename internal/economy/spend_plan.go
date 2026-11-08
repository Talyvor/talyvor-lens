package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// spend_plan.go — B53.15: COACH'S MONTH PLAN — THE UPCOMING PAYMENTS, AND WHAT IS SAFE TO SPEND EACH DAY UNTIL MONEY
// COMES IN.
//
// The plan runs from today, in the owner's time zone (their notice settings, B53.7), to the day before their next
// income — the payday they set (or confirmed from Lens's proposal, read from three months of money in) — or to the
// month's last day while they have set none. Code works out every number:
//
//	money now      what the owner's own wallet and their agents' hold in the plan's currency (pots and holds are
//	               already set aside), and the balances read from their accounts at other banks (B53.13)
//	upcoming       every payment Talyvor knows of until then: bills due (B41.101), the subscriptions B53.5 found,
//	               and the agents' standing orders to payees (B30.18), each with its date and amount
//	budgets left   what the current budgets (B53.4) still mean to spend: an overall budget's, or else its categories'
//	safe per day   (money now − upcoming − budgets left) ÷ the days until the next income, never below zero
//
// No income falls inside the plan, since it ends at the next one; a late income is left out until it arrives.
// Off-plan findings, each with its numbers, are worded by the Coach's templates (B53.6) and become notices (B53.7): a
// payment the money now does not cover, and one the last seven days' pace of spending would leave short (both urgent,
// so they go at once); a bill larger than that supplier's before; and an income later than expected. It is the
// owner's own plan: it says what their money covers, never which product or debt to choose.

// Kinds of plan finding.
const (
	PlanUncovered  = "plan_uncovered"
	PlanPaceShort  = "plan_pace_short"
	PlanBillLarger = "plan_bill_larger"
	PlanIncomeLate = "plan_income_late"
)

// Where an upcoming payment comes from.
const (
	PlanFromBill          = "bill"
	PlanFromSubscription  = "subscription"
	PlanFromStandingOrder = "standing_order"
)

// planEarlyDays is how many days before payday an income may arrive and still count as that payday's.
const planEarlyDays = 3

// planMaxPayments bounds the payments one plan lists (an hourly standing order alone could make hundreds).
const planMaxPayments = 500

// SpendIncome is the owner's expected income: about AmountMinor of Currency on day Payday of each month.
type SpendIncome struct {
	Payday      int       `json:"payday"` // 1–31; a month with fewer days pays on its last day
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	Payer       string    `json:"payer,omitempty"` // whose money in is the income; empty: any money in
	Confirmed   bool      `json:"confirmed"`       // false: Lens's proposal, for the owner to confirm
	SetAt       time.Time `json:"set_at,omitzero"`
}

// PlanPayment is one upcoming payment and whether the money now covers it.
type PlanPayment struct {
	Source      string `json:"source"` // bill, subscription or standing_order
	ID          string `json:"id"`     // the bill's, the subscription's merchant, the standing order's
	Payee       string `json:"payee"`
	AgentID     string `json:"agent_id,omitempty"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Due         string `json:"due"` // YYYY-MM-DD in the owner's time zone; an overdue one is due today
	Covered     bool   `json:"covered"`
	usual       int64  // a bill's: its supplier's largest bill before
}

// PlanFinding is one way the month is off plan, with its numbers and the Coach's sentence.
type PlanFinding struct {
	Kind            string       `json:"kind"`
	Urgent          bool         `json:"urgent"`
	Payment         *PlanPayment `json:"payment,omitempty"`
	ShortMinor      int64        `json:"short_minor,omitempty"`
	PacePerDayMinor int64        `json:"pace_per_day_minor,omitempty"`
	KeepToMinor     int64        `json:"keep_to_per_day_minor,omitempty"` // pace_short: the pace that still covers it
	UsualMinor      int64        `json:"usual_minor,omitempty"`           // bill_larger: the supplier's largest before
	ExpectedOn      string       `json:"expected_on,omitempty"`           // income_late
	Sentence        string       `json:"sentence"`
}

// SpendPlan is the owner's month plan.
type SpendPlan struct {
	Currency         string        `json:"currency"`
	Timezone         string        `json:"timezone"`
	Today            string        `json:"today"`
	Until            string        `json:"until"` // the plan's last day: the day before the next income, or the month's last
	NextIncome       string        `json:"next_income,omitempty"`
	Days             int           `json:"days"` // today to Until, both counted
	Income           *SpendIncome  `json:"income"`
	ProposedIncome   *SpendIncome  `json:"proposed_income,omitempty"`
	MoneyNowMinor    int64         `json:"money_now_minor"`
	UpcomingMinor    int64         `json:"upcoming_minor"`
	BudgetsLeftMinor int64         `json:"budgets_left_minor"`
	AvailableMinor   int64         `json:"available_minor"` // below zero when the payments are not covered
	SafePerDayMinor  int64         `json:"safe_per_day_minor"`
	PacePerDayMinor  int64         `json:"pace_per_day_minor"` // the last seven days' spending, bills and subscriptions aside
	Payments         []PlanPayment `json:"payments"`
	Findings         []PlanFinding `json:"findings"`
}

// civil is t's date in loc, as midnight UTC, so dates subtract to whole days.
func civil(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}

// paydayIn is day of month m of year y, or the month's last day when it has fewer.
func paydayIn(y int, m time.Month, day int) time.Time {
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return time.Date(y, m, min(day, last), 0, 0, 0, 0, time.UTC)
}

func daysBetween(a, b time.Time) int { return int(b.Sub(a).Hours()+12) / 24 }

func planDate(t time.Time) string { return t.Format(time.DateOnly) }

// planDue is a payment's date in a plan from today: an overdue one is due today.
func planDue(d, today time.Time) string {
	if d.Before(today) {
		d = today
	}
	return planDate(d)
}

// planDay is a date as a sentence says it: "28 November".
func planDay(d time.Time) string { return d.Format("2 January") }

// SetSpendIncome saves workspaceID's expected income, which confirms it.
func (s *DualTokenStore) SetSpendIncome(ctx context.Context, workspaceID string, in SpendIncome) error {
	in.Payer = strings.TrimSpace(in.Payer)
	switch {
	case in.Payday < 1 || in.Payday > 31:
		return fmt.Errorf("%w: payday must be a day of the month, 1 to 31", ErrSpending)
	case in.AmountMinor <= 0 || in.AmountMinor > 1<<40:
		return fmt.Errorf("%w: amount_minor must be above zero", ErrSpending)
	case !budgetCurrency.MatchString(in.Currency):
		return fmt.Errorf("%w: currency must be a currency code such as GBP", ErrSpending)
	case len(in.Payer) > 200:
		return fmt.Errorf("%w: payer is at most 200 characters", ErrSpending)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO spend_plan_income (workspace_id, payday, amount_minor, currency, payer) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id) DO UPDATE SET payday = EXCLUDED.payday, amount_minor = EXCLUDED.amount_minor, currency = EXCLUDED.currency,
		payer = EXCLUDED.payer, set_at = now()`, workspaceID, in.Payday, in.AmountMinor, in.Currency, in.Payer); err != nil {
		return fmt.Errorf("economy: income: %w", err)
	}
	return nil
}

// planMoneyIn is one payment into the owner's wallets from outside.
type planMoneyIn struct {
	payer       string
	currency    string
	amountMinor int64
	at          time.Time
}

// moneyIn is the money that came from outside into workspaceID's own and agents' wallets, and into its accounts at
// other banks, since from; oldest first.
func (s *DualTokenStore) moneyIn(ctx context.Context, workspaceID string, from time.Time) ([]planMoneyIn, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.counterparty, po.currency, po.amount_minor, e.created_at FROM money_entries e
			JOIN money_postings po ON po.entry_id = e.id JOIN money_accounts a ON a.id = po.account_id
		WHERE e.workspace_id = $1 AND e.created_at >= $2 AND a.purpose IN ('company', 'agent') AND po.amount_minor > 0
			AND EXISTS (SELECT 1 FROM money_postings q JOIN money_accounts b ON b.id = q.account_id
				WHERE q.entry_id = e.id AND b.purpose = 'partner' AND q.amount_minor < 0)
		UNION ALL
		SELECT merchant, currency, amount_minor, booked_at FROM bank_transactions WHERE workspace_id = $1 AND booked_at >= $2 AND amount_minor > 0
		ORDER BY 4`, workspaceID, from)
	if err != nil {
		return nil, fmt.Errorf("economy: money in: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (planMoneyIn, error) {
		var m planMoneyIn
		err := row.Scan(&m.payer, &m.currency, &m.amountMinor, &m.at)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: money in: %w", err)
	}
	return out, nil
}

// proposeIncome is the income that money in (oldest first) shows: a payer whose last three payments in came 27 to 33
// days apart, the last within five weeks of now — on the last one's day, at its amount; the largest such. Nil if none.
func proposeIncome(in []planMoneyIn, now time.Time) *SpendIncome {
	groups := map[[2]string][]planMoneyIn{}
	for _, m := range in {
		if k := [2]string{spendMerchantKey(m.payer), m.currency}; k[0] != "" {
			groups[k] = append(groups[k], m)
		}
	}
	var best *SpendIncome
	for _, g := range groups {
		if len(g) < 3 {
			continue
		}
		run := g[len(g)-3:]
		ok := now.Sub(run[2].at) <= 35*24*time.Hour
		for i := 1; i < 3; i++ {
			gap := run[i].at.Sub(run[i-1].at)
			ok = ok && gap >= 27*24*time.Hour && gap <= 33*24*time.Hour
		}
		if ok && (best == nil || run[2].amountMinor > best.AmountMinor) {
			best = &SpendIncome{Payday: run[2].at.UTC().Day(), AmountMinor: run[2].amountMinor, Currency: run[2].currency,
				Payer: strings.TrimSpace(run[2].payer)}
		}
	}
	return best
}

// SpendPlan is workspaceID's month plan at now.
func (s *DualTokenStore) SpendPlan(ctx context.Context, workspaceID string, now time.Time) (SpendPlan, error) {
	set, _, err := s.noticeSettings(ctx, workspaceID)
	if err != nil {
		return SpendPlan{}, err
	}
	loc, err := time.LoadLocation(set.Timezone)
	if err != nil {
		loc = time.UTC
	}
	today := civil(now, loc)
	plan := SpendPlan{Timezone: loc.String(), Today: planDate(today), Payments: []PlanPayment{}, Findings: []PlanFinding{}}

	var inc SpendIncome
	err = s.pool.QueryRow(ctx, `SELECT payday, amount_minor, currency, payer, set_at FROM spend_plan_income WHERE workspace_id = $1`, workspaceID).
		Scan(&inc.Payday, &inc.AmountMinor, &inc.Currency, &inc.Payer, &inc.SetAt)
	switch {
	case err == nil:
		inc.Confirmed = true
		plan.Income = &inc
	case !errors.Is(err, pgx.ErrNoRows):
		return SpendPlan{}, fmt.Errorf("economy: income: %w", err)
	}
	moneyIn, err := s.moneyIn(ctx, workspaceID, now.AddDate(0, -3, -7))
	if err != nil {
		return SpendPlan{}, err
	}
	if plan.Income == nil {
		plan.ProposedIncome = proposeIncome(moneyIn, now)
	}

	// The plan's currency: its income's, else the one the owner holds most of — by value, each currency's minor units
	// scaled by its own decimals, so 2 USDC (2,000,000 millionths) never outweighs $5,000.00 (500,000 cents).
	switch {
	case plan.Income != nil:
		plan.Currency = plan.Income.Currency
	case plan.ProposedIncome != nil:
		plan.Currency = plan.ProposedIncome.Currency
	default:
		var currencies []string
		var places []int32
		for c, d := range MoneyCurrencies {
			currencies, places = append(currencies, c), append(places, int32(d))
		}
		err := s.pool.QueryRow(ctx, `SELECT b.currency FROM money_account_balances b JOIN money_accounts a ON a.id = b.account_id
			JOIN unnest($2::text[], $3::int[]) AS d(currency, places) ON d.currency = b.currency
			WHERE a.workspace_id = $1 AND a.purpose IN ('company', 'agent') GROUP BY b.currency, d.places
			ORDER BY sum(b.balance_minor) / 10::numeric ^ d.places DESC, b.currency LIMIT 1`,
			workspaceID, currencies, places).Scan(&plan.Currency)
		if errors.Is(err, pgx.ErrNoRows) {
			plan.Currency, err = CurrencyGBP, nil
		}
		if err != nil {
			return SpendPlan{}, fmt.Errorf("economy: plan: %w", err)
		}
	}
	cur := plan.Currency

	until := time.Date(today.Year(), today.Month()+1, 0, 0, 0, 0, 0, time.UTC) // the month's last day
	if plan.Income != nil {
		next := paydayIn(today.Year(), today.Month(), inc.Payday)
		if !next.After(today) {
			next = paydayIn(today.Year(), today.Month()+1, inc.Payday)
		}
		last := paydayIn(next.Year(), next.Month()-1, inc.Payday)
		plan.NextIncome, until = planDate(next), next.AddDate(0, 0, -1)
		// Late: the last payday is past and was set before it, and its money has not come in since three days before.
		if last.Before(today) && !last.Before(civil(inc.SetAt, loc)) {
			var got int64
			for _, m := range moneyIn {
				if m.currency == cur && !civil(m.at, loc).Before(last.AddDate(0, 0, -planEarlyDays)) &&
					(inc.Payer == "" || spendMerchantKey(m.payer) == spendMerchantKey(inc.Payer)) {
					got += m.amountMinor
				}
			}
			if inc.Payer != "" && got == 0 || inc.Payer == "" && got < inc.AmountMinor {
				plan.Findings = append(plan.Findings, PlanFinding{Kind: PlanIncomeLate, ExpectedOn: planDate(last),
					Sentence: coachTemplate(CoachFact{Kind: PlanIncomeLate, Amount: FormatMinor(inc.AmountMinor, cur), Due: planDay(last)})})
			}
		}
	}
	plan.Until, plan.Days = planDate(until), daysBetween(today, until)+1

	if err := s.pool.QueryRow(ctx, `SELECT
			COALESCE((SELECT sum(b.balance_minor) FROM money_account_balances b JOIN money_accounts a ON a.id = b.account_id
				WHERE a.workspace_id = $1 AND a.purpose IN ('company', 'agent') AND b.currency = $2), 0)::bigint +
			COALESCE((SELECT sum(balance_minor) FROM bank_connections WHERE workspace_id = $1 AND currency = $2 AND balance_minor IS NOT NULL), 0)::bigint`,
		workspaceID, cur).Scan(&plan.MoneyNowMinor); err != nil {
		return SpendPlan{}, fmt.Errorf("economy: plan: %w", err)
	}
	if plan.Payments, err = s.planPayments(ctx, workspaceID, cur, now, today, until, loc); err != nil {
		return SpendPlan{}, err
	}

	// Budgets left: the overall budget on every wallet, else the wallets' overall ones, else the category ones —
	// never two budgets on the same money. shortcut: a weekly budget counts only this week's; add its later weeks
	// when the plan should hold them back too.
	budgets, err := s.SpendBudgets(ctx, workspaceID, now)
	if err != nil {
		return SpendPlan{}, err
	}
	var all, overall, categories int64
	for _, b := range budgets {
		if b.Currency != cur {
			continue
		}
		switch left := max(b.LeftMinor, 0); {
		case b.Category == "" && b.AgentID == BudgetAllWallets:
			all = max(all, left)
		case b.Category == "":
			overall += left
		default:
			categories += left
		}
	}
	switch {
	case all > 0:
		plan.BudgetsLeftMinor = all
	case overall > 0:
		plan.BudgetsLeftMinor = overall
	default:
		plan.BudgetsLeftMinor = categories
	}

	// The pace: the last seven days' spending, bills and subscriptions aside (the payments hold them already).
	_, off := time.Date(today.Year(), today.Month(), today.Day(), 12, 0, 0, 0, loc).Zone()
	week, err := s.Spending(ctx, workspaceID, today.AddDate(0, 0, -6).Add(-time.Duration(off)*time.Second), now.Add(time.Second))
	if err != nil {
		return SpendPlan{}, err
	}
	var spent int64
	for _, p := range week.Payments {
		if p.Currency == cur && p.Category != SpendBills && p.Category != SpendSubscriptions {
			spent += p.AmountMinor
		}
	}
	plan.PacePerDayMinor = spent / 7

	// What the money now covers, and the first payment it, or the pace, leaves short.
	var uncovered, paceShort *PlanFinding
	for i := range plan.Payments {
		p := &plan.Payments[i]
		plan.UpcomingMinor += p.AmountMinor
		dayTotal := plan.UpcomingMinor // with the rest of the payments due the same day
		for _, q := range plan.Payments[i+1:] {
			if q.Due == p.Due {
				dayTotal += q.AmountMinor
			}
		}
		left := plan.MoneyNowMinor - plan.UpcomingMinor
		p.Covered = left >= 0
		due, _ := time.Parse(time.DateOnly, p.Due)
		days := int64(daysBetween(today, due))
		pp := *p
		if uncovered == nil && !p.Covered {
			uncovered = &PlanFinding{Kind: PlanUncovered, Urgent: true, Payment: &pp, ShortMinor: min(-left, p.AmountMinor)}
		}
		if after := plan.MoneyNowMinor - dayTotal - plan.PacePerDayMinor*days; uncovered == nil && paceShort == nil && after < 0 {
			paceShort = &PlanFinding{Kind: PlanPaceShort, Urgent: true, Payment: &pp, ShortMinor: min(-after, p.AmountMinor),
				PacePerDayMinor: plan.PacePerDayMinor, KeepToMinor: max(plan.MoneyNowMinor-dayTotal, 0) / max(days, 1)}
		}
		if p.usual > 0 && p.AmountMinor > p.usual {
			plan.Findings = append(plan.Findings, PlanFinding{Kind: PlanBillLarger, Payment: &pp, UsualMinor: p.usual,
				Sentence: coachTemplate(CoachFact{Kind: PlanBillLarger, Merchant: p.Payee, Amount: FormatMinor(p.AmountMinor, cur),
					Usual: FormatMinor(p.usual, cur), Due: planDay(due)})})
		}
	}
	for _, f := range []*PlanFinding{uncovered, paceShort} {
		if f == nil {
			continue
		}
		due, _ := time.Parse(time.DateOnly, f.Payment.Due)
		fact := CoachFact{Kind: f.Kind, Merchant: f.Payment.Payee, Amount: FormatMinor(f.Payment.AmountMinor, cur), Due: planDay(due),
			Short: FormatMinor(f.ShortMinor, cur)}
		if f.Kind == PlanPaceShort {
			fact.Pace, fact.KeepTo = FormatMinor(f.PacePerDayMinor, cur), FormatMinor(f.KeepToMinor, cur)
		}
		f.Sentence = coachTemplate(fact)
		plan.Findings = append(plan.Findings, *f)
	}
	plan.AvailableMinor = plan.MoneyNowMinor - plan.UpcomingMinor - plan.BudgetsLeftMinor
	plan.SafePerDayMinor = max(plan.AvailableMinor, 0) / int64(plan.Days)
	return plan, nil
}

// planPayments is workspaceID's payments in cur due from today to until, by date: bills due, subscriptions, and the
// agents' standing orders to payees.
func (s *DualTokenStore) planPayments(ctx context.Context, workspaceID, cur string, now, today, until time.Time, loc *time.Location) ([]PlanPayment, error) {
	rows, err := s.pool.Query(ctx, `SELECT b.id, b.supplier, b.amount_minor, b.due_date,
			COALESCE((SELECT max(o.amount_minor) FROM money_bills o WHERE o.workspace_id = b.workspace_id AND o.id <> b.id
				AND lower(btrim(o.supplier)) = lower(btrim(b.supplier)) AND o.currency = b.currency AND o.created_at < b.created_at), 0)::bigint
		FROM money_bills b WHERE b.workspace_id = $1 AND b.status = 'due' AND b.currency = $2 AND b.due_date <= $3
		ORDER BY b.due_date, b.id LIMIT $4`, workspaceID, cur, until, planMaxPayments)
	if err != nil {
		return nil, fmt.Errorf("economy: plan bills: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlanPayment, error) {
		p := PlanPayment{Source: PlanFromBill, Currency: cur}
		var due time.Time
		err := row.Scan(&p.ID, &p.Payee, &p.AmountMinor, &due, &p.usual)
		p.Due = planDue(due, today)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: plan bills: %w", err)
	}
	subs, err := s.SpendSubscriptions(ctx, workspaceID, now)
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		for at := sub.NextAt; sub.Currency == cur && !civil(at, loc).After(until) && len(out) < planMaxPayments; {
			out = append(out, PlanPayment{Source: PlanFromSubscription, ID: spendMerchantKey(sub.Merchant), Payee: sub.Merchant,
				AgentID: sub.AgentID, AmountMinor: sub.AmountMinor, Currency: cur, Due: planDue(civil(at, loc), today)})
			if sub.Interval == "weekly" {
				at = at.AddDate(0, 0, 7)
			} else {
				at = sameDayNextMonth(at, sub.NextAt.UTC().Day())
			}
		}
	}
	rows, err = s.pool.Query(ctx, `SELECT s.id, p.name, s.from_agent_id, s.amount_minor, s.every, s.next_run_at, s.end_at
		FROM agent_payment_schedules s JOIN money_payees p ON p.id = s.to_payee_id AND p.workspace_id = s.workspace_id
		WHERE s.workspace_id = $1 AND s.active AND s.currency = $2 AND s.amount_minor > 0 ORDER BY s.next_run_at, s.id`, workspaceID, cur)
	if err != nil {
		return nil, fmt.Errorf("economy: plan standing orders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p := PlanPayment{Source: PlanFromStandingOrder, Currency: cur}
		var every string
		var at time.Time
		var end *time.Time
		if err := rows.Scan(&p.ID, &p.Payee, &p.AgentID, &p.AmountMinor, &every, &at, &end); err != nil {
			return nil, fmt.Errorf("economy: plan standing orders: %w", err)
		}
		for ; !civil(at, loc).After(until) && (end == nil || !at.After(*end)) && len(out) < planMaxPayments; at = nextTick(at, every) {
			p.Due = planDue(civil(at, loc), today)
			out = append(out, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: plan standing orders: %w", err)
	}
	slices.SortStableFunc(out, func(a, b PlanPayment) int { return strings.Compare(a.Due, b.Due) })
	return out, nil
}

// planSubject is what makes a finding one notice: its kind, and its payment and that payment's date, or its payday.
func (f PlanFinding) planSubject() string {
	if f.Payment == nil {
		return f.Kind + "/" + f.ExpectedOn
	}
	return f.Kind + "/" + f.Payment.Source + ":" + f.Payment.ID + "/" + f.Payment.Due
}
