package economy

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"
)

// spend_findings.go — B53.5: WHAT IS UNUSUAL, AND THE SUBSCRIPTIONS YOU PAY (also B41.120's lens side).
//
// Both are worked out on read from the payments Spending lists (B53.3), wallet by wallet — the owner's own and each
// agent's — so nothing new is stored.
//
// UNUSUAL. Each payment in the period is compared with what the same wallet paid before it, over the year before the
// period, and each rule that it trips is one finding carrying its numbers:
//   - new_merchant: a merchant the wallet has never paid, for at least SpendNewMerchantMinor;
//   - above_usual: above the wallet's own 95th percentile for that category and currency, once it has
//     spendPercentileHistory payments there to have a percentile at all;
//   - new_country: a card payment in a country the wallet has never paid in;
//   - duplicate: the same merchant and amount as a payment SpendDuplicateWindow or less before it;
//   - gambling: a card payment under a gambling merchant category code;
//   - agent_unusual_spend: the agent rule of B19.6 (5× its 7-day hourly rate), as agent_spend_alerts recorded it.
// A wallet with no earlier payment has no pattern yet: its first payment is never a new merchant or a new country.
//
// SUBSCRIPTIONS. A wallet's payments to one merchant are a subscription when its last three at the same amount and
// currency are a week (6 to 8 days) or a month (27 to 33 days) apart, and the next is not overdue by more than that
// slack. The next date is a week after the last, or the next month on the day of the month the three began (the
// month's last day when it is shorter); the reminder is RemindDaysBeforeRenewal days before it (B41.120).
// shortcut: weekly and monthly only — a yearly one needs three years of charges; add it when a year of data is not enough.

// SpendNewMerchantMinor is the least a payment to a new merchant must be to be a finding (minor units, any currency).
// A setting Nicolai fills (LENS_SPEND_NEW_MERCHANT_MINOR); until he does, every new merchant is one.
var SpendNewMerchantMinor int64

// SpendDuplicateWindow is how close two payments to the same merchant for the same amount must be to read as a likely
// duplicate. A setting Nicolai fills (LENS_SPEND_DUPLICATE_MINUTES).
var SpendDuplicateWindow = 10 * time.Minute

// RemindDaysBeforeRenewal is how many days before a subscription renews its reminder falls (B41.120).
const RemindDaysBeforeRenewal = 3

// Unusual-spending rules.
const (
	FindingNewMerchant       = "new_merchant"
	FindingAboveUsual        = "above_usual"
	FindingNewCountry        = "new_country"
	FindingDuplicate         = "duplicate"
	FindingGambling          = "gambling"
	FindingAgentUnusualSpend = "agent_unusual_spend"
)

// spendPercentileHistory is how many earlier payments in a category a 95th percentile needs to mean anything.
const spendPercentileHistory = 20

// gamblingMCCs are the merchant category codes of betting, lotteries and casinos.
var gamblingMCCs = []string{"7800", "7801", "7802", "7995", "9406"}

// SpendFinding is one payment, or one agent's hour, a rule found unusual, with the numbers it compared.
type SpendFinding struct {
	Rule        string    `json:"rule"`
	EntryID     string    `json:"entry_id,omitempty"`
	AgentID     string    `json:"agent_id,omitempty"` // empty: the owner's own wallet
	Merchant    string    `json:"merchant,omitempty"`
	Category    string    `json:"category,omitempty"`
	Country     string    `json:"country,omitempty"`
	AmountMinor int64     `json:"amount_minor,omitempty"`
	Currency    string    `json:"currency,omitempty"`
	At          time.Time `json:"at"`
	// new_merchant: the least a new merchant's payment must be.
	ThresholdMinor *int64 `json:"threshold_minor,omitempty"`
	// above_usual: the wallet's 95th percentile in the category, over how many earlier payments.
	UsualMinor *int64 `json:"usual_minor,omitempty"`
	History    int    `json:"history,omitempty"`
	// duplicate: the earlier payment, and how many minutes before.
	DuplicateOf  string `json:"duplicate_of,omitempty"`
	MinutesApart *int   `json:"minutes_apart,omitempty"`
	// agent_unusual_spend: the hour's spend and the agent's usual hourly rate, in µLXC (B19.6).
	LastHourULXC     int64 `json:"last_hour_ulxc,omitempty"`
	UsualPerHourULXC int64 `json:"usual_per_hour_ulxc,omitempty"`
}

// SpendFindings is the unusual spending of workspaceID's wallets in [from, to); a zero from or to is as Spending's.
// shortcut: reads every payment of the year before the period; keep running totals when a year outgrows one read.
func (s *DualTokenStore) SpendFindings(ctx context.Context, workspaceID string, from, to time.Time) ([]SpendFinding, error) {
	period, err := s.Spending(ctx, workspaceID, from, to)
	if err != nil {
		return nil, err
	}
	all, err := s.Spending(ctx, workspaceID, period.From.AddDate(-1, 0, 0), period.To)
	if err != nil {
		return nil, err
	}
	out := spendFindings(all.Payments, period.From, SpendNewMerchantMinor, SpendDuplicateWindow)
	rows, err := s.pool.Query(ctx, `SELECT agent_id, last_hour_ulxc, usual_per_hour_ulxc, created_at FROM agent_spend_alerts
		WHERE workspace_id = $1 AND created_at >= $2 AND created_at < $3 ORDER BY created_at`, workspaceID, period.From, period.To)
	if err != nil {
		return nil, fmt.Errorf("economy: spend findings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		f := SpendFinding{Rule: FindingAgentUnusualSpend}
		if err := rows.Scan(&f.AgentID, &f.LastHourULXC, &f.UsualPerHourULXC, &f.At); err != nil {
			return nil, fmt.Errorf("economy: spend findings: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: spend findings: %w", err)
	}
	slices.SortStableFunc(out, func(a, b SpendFinding) int { return b.At.Compare(a.At) })
	return out, nil
}

// spendFindings applies the payment rules to each of payments (newest first, as Spending lists them) made at or
// after from, against the same wallet's payments before it.
func spendFindings(payments []SpendPayment, from time.Time, newMerchantMinor int64, dupWindow time.Duration) []SpendFinding {
	type wallet struct {
		paid      int
		merchants map[string]bool
		countries map[string]bool
		amounts   map[[2]string][]int64      // category, currency → amounts, ascending
		last      map[[3]string]SpendPayment // merchant, currency, amount → its latest payment
	}
	wallets := map[string]*wallet{}
	out := []SpendFinding{}
	for i := len(payments) - 1; i >= 0; i-- {
		p := payments[i]
		w := wallets[p.AgentID]
		if w == nil {
			w = &wallet{merchants: map[string]bool{}, countries: map[string]bool{}, amounts: map[[2]string][]int64{}, last: map[[3]string]SpendPayment{}}
			wallets[p.AgentID] = w
		}
		merchant := spendMerchantKey(p.Merchant)
		catKey := [2]string{p.Category, p.Currency}
		dupKey := [3]string{merchant, p.Currency, fmt.Sprint(p.AmountMinor)}
		if !p.CreatedAt.Before(from) {
			finding := func(rule string) *SpendFinding {
				out = append(out, SpendFinding{Rule: rule, EntryID: p.EntryID, AgentID: p.AgentID, Merchant: p.Merchant, Category: p.Category,
					Country: p.Country, AmountMinor: p.AmountMinor, Currency: p.Currency, At: p.CreatedAt})
				return &out[len(out)-1]
			}
			if w.paid > 0 && !w.merchants[merchant] && p.AmountMinor >= newMerchantMinor {
				finding(FindingNewMerchant).ThresholdMinor = &newMerchantMinor
			}
			if h := w.amounts[catKey]; len(h) >= spendPercentileHistory {
				p95 := h[int(math.Ceil(0.95*float64(len(h))))-1] // nearest rank
				if p.AmountMinor > p95 {
					f := finding(FindingAboveUsual)
					f.UsualMinor, f.History = &p95, len(h)
				}
			}
			if w.paid > 0 && p.Country != "" && !w.countries[p.Country] {
				finding(FindingNewCountry)
			}
			if prev, ok := w.last[dupKey]; ok && p.CreatedAt.Sub(prev.CreatedAt) <= dupWindow {
				f := finding(FindingDuplicate)
				minutes := int(p.CreatedAt.Sub(prev.CreatedAt).Minutes())
				f.DuplicateOf, f.MinutesApart = prev.EntryID, &minutes
			}
			if slices.Contains(gamblingMCCs, p.MCC) {
				finding(FindingGambling)
			}
		}
		w.paid++
		w.merchants[merchant] = true
		if p.Country != "" {
			w.countries[p.Country] = true
		}
		h := w.amounts[catKey]
		at, _ := slices.BinarySearch(h, p.AmountMinor)
		w.amounts[catKey] = slices.Insert(h, at, p.AmountMinor)
		w.last[dupKey] = p
	}
	slices.Reverse(out) // newest first, like the payments
	return out
}

// SpendSubscription is a payment that recurs: to one merchant, for one amount, weekly or monthly.
type SpendSubscription struct {
	AgentID      string    `json:"agent_id,omitempty"` // empty: the owner's own wallet
	Merchant     string    `json:"merchant"`
	Category     string    `json:"category"`
	AmountMinor  int64     `json:"amount_minor"`
	Currency     string    `json:"currency"`
	Interval     string    `json:"interval"` // weekly or monthly
	Charges      int       `json:"charges"`  // at this amount in the last four months
	LastAt       time.Time `json:"last_at"`
	NextAt       time.Time `json:"next_at"`
	RemindOn     time.Time `json:"remind_on"` // RemindDaysBeforeRenewal days before NextAt
	MonthlyMinor int64     `json:"monthly_minor"`
	YearlyMinor  int64     `json:"yearly_minor"`
}

// SpendSubscriptions is the subscriptions workspaceID's wallets are paying at now, soonest to renew first.
func (s *DualTokenStore) SpendSubscriptions(ctx context.Context, workspaceID string, now time.Time) ([]SpendSubscription, error) {
	recent, err := s.Spending(ctx, workspaceID, now.AddDate(0, -4, 0), now.Add(time.Second))
	if err != nil {
		return nil, err
	}
	return spendSubscriptions(recent.Payments, now), nil
}

// spendSubscriptions finds the subscriptions among payments (newest first) that are still running at now.
func spendSubscriptions(payments []SpendPayment, now time.Time) []SpendSubscription {
	groups := map[[3]string][]SpendPayment{} // wallet, merchant, currency → its payments, newest first
	var order [][3]string
	for _, p := range payments {
		k := [3]string{p.AgentID, spendMerchantKey(p.Merchant), p.Currency}
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	out := []SpendSubscription{}
	for _, k := range order {
		g := groups[k]
		run := []SpendPayment{} // the latest amount's payments, newest first
		for _, p := range g {
			if p.AmountMinor == g[0].AmountMinor {
				run = append(run, p)
			}
		}
		if len(run) < 3 {
			continue
		}
		last, mid, first := run[0].CreatedAt.UTC(), run[1].CreatedAt.UTC(), run[2].CreatedAt.UTC() // dates in UTC, never the server's zone
		within := func(lo, hi int) bool {
			for _, gap := range []time.Duration{last.Sub(mid), mid.Sub(first)} {
				if gap < time.Duration(lo)*24*time.Hour || gap > time.Duration(hi)*24*time.Hour {
					return false
				}
			}
			return true
		}
		sub := SpendSubscription{AgentID: k[0], Merchant: run[0].Merchant, Category: run[0].Category, AmountMinor: run[0].AmountMinor,
			Currency: k[2], LastAt: last}
		var slack time.Duration
		switch {
		case within(6, 8):
			sub.Interval, slack = "weekly", 24*time.Hour
			sub.NextAt = last.AddDate(0, 0, 7)
			sub.YearlyMinor, sub.MonthlyMinor = sub.AmountMinor*52, sub.AmountMinor*52/12
		case within(27, 33):
			sub.Interval, slack = "monthly", 3*24*time.Hour
			sub.NextAt = sameDayNextMonth(last, first.Day())
			sub.YearlyMinor, sub.MonthlyMinor = sub.AmountMinor*12, sub.AmountMinor
		default:
			continue
		}
		if now.After(sub.NextAt.Add(slack)) { // stopped
			continue
		}
		sub.Charges = len(run)
		sub.RemindOn = sub.NextAt.AddDate(0, 0, -RemindDaysBeforeRenewal)
		out = append(out, sub)
	}
	slices.SortStableFunc(out, func(a, b SpendSubscription) int { return a.NextAt.Compare(b.NextAt) })
	return out
}

// sameDayNextMonth is day of the month after t's, at t's time of day — the month's last day when it has fewer days.
func sameDayNextMonth(t time.Time, day int) time.Time {
	firstOfNext := time.Date(t.Year(), t.Month()+1, 1, t.Hour(), t.Minute(), t.Second(), 0, t.Location())
	if days := firstOfNext.AddDate(0, 1, -1).Day(); day > days {
		day = days
	}
	return firstOfNext.AddDate(0, 0, day-1)
}
