package economy

import (
	"testing"
	"time"
)

// B53.15 — three monthly payments in from the same payer propose that income, on the last one's day at its amount;
// a payer seen twice, or one who stopped, proposes nothing.
func TestProposeIncome(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	in := []planMoneyIn{
		{"Acme Ltd", "GBP", 250000, time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)},
		{"Test payer", "GBP", 5000, time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)},
		{"Acme Ltd", "GBP", 250000, time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)},
		{"Test payer", "GBP", 5000, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)},
		{"ACME LTD ", "GBP", 251000, time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)},
	}
	got := proposeIncome(in, now)
	if got == nil || got.Payday != 28 || got.AmountMinor != 251000 || got.Currency != "GBP" || got.Confirmed {
		t.Fatalf("proposal = %+v, want 28th, 2510.00 GBP, unconfirmed", got)
	}
	if got := proposeIncome(in, now.AddDate(0, 2, 0)); got != nil {
		t.Fatalf("proposal two months after the last = %+v, want none", got)
	}
}
