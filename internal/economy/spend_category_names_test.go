package economy

import (
	"strings"
	"testing"
	"time"
)

// B55.46 — Lens's notices name a category as the suite does ("AI and software"), never "Ai software" or an id.
func TestNoticesNameCategoryInWords(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	alert := alertNotice(SpendAlert{Kind: "budget", Category: SpendAISoftware, Period: "month", Percent: 80,
		SpentMinor: 8000, LimitMinor: 10000, Currency: "GBP"})
	payment := paymentNotice(SpendPayment{Merchant: "Acme", AmountMinor: 500, Currency: "GBP", Category: SpendEatingOut, CreatedAt: now}, nil, nil)
	agentBudget := coachTemplate(budgetFact(SpendBudget{AgentID: "a1", Category: SpendAISoftware, Period: "month",
		AmountMinor: 10000, Currency: "GBP", Percent: 80, LeftMinor: 2000, PeriodEnd: now.AddDate(0, 0, 3)}, now))
	newMerchant := coachTemplate(findingFact(SpendFinding{Rule: FindingNewMerchant, Category: SpendAISoftware, AmountMinor: 500, Currency: "GBP"}))
	for _, c := range []struct{ got, want string }{
		{alert.Body, "AI and software is at 80% of this month's budget"},
		{payment.Body, "Eating out"},
		{agentBudget, "An agent's AI and software is at 80%"},
		{newMerchant, "paid an AI and software merchant for the first time"},
	} {
		if !strings.Contains(c.got, c.want) || strings.Contains(c.got, "Ai ") || strings.Contains(c.got, "_") {
			t.Errorf("notice reads %q, want it to name %q", c.got, c.want)
		}
	}
	if got := categoryWords(SpendTransfers); got != "transfers to people" {
		t.Errorf("categoryWords(transfers) = %q, want %q", got, "transfers to people")
	}
}
