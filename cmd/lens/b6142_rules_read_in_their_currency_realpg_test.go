package main

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// b6142Payer is a euro account's agent whose rules are saved at the fixed test rate (the euro worth $1.10) and then
// read with the euro worth $1.155 — the test rate moved by 5% between saving the rules and reading them.
func b6142Payer(t *testing.T, ws, rules string) b468Payer {
	t.Helper()
	pool := agentRoutesDB(t)
	var reg *partners.Registry
	b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
			reg = r
			return sandboxMoneyLayer(t, pool, store, r)
		})
	b6112TopUp(t, pool, b, 400_000)
	if _, err := pool.Exec(context.Background(), `UPDATE money_payees SET created_at = now() - interval '2 days' WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	reg.UseFXRates(b6112Rates{big.NewRat(11, 10)})
	b6140Rules(t, b, rules)
	reg.UseFXRates(b6112Rates{big.NewRat(1155, 1000)})
	return b
}

// B61.42 — a euro agent's approval for €150.00 says it asks above €100.00 and leaves €1,850.00 this month, in euros,
// with the euro moved by 5% since the rules were saved. On main the sheet has only the µLXC the rules were written in
// for the parked gateway.
func TestB6142_TheApprovalSheetReadsTheRulesInTheirCurrency(t *testing.T) {
	b := b6142Payer(t, "ws-b6142-sheet", `{"currency":"EUR","approval_above_minor":10000,"monthly_limit_minor":200000}`)
	var need *economy.ApprovalNeededError
	if err := b.try("asks", 15_000); !errors.As(err, &need) {
		t.Fatalf("€150.00 above €100.00: %v, want it to ask", err)
	}
	a, err := b.store.GetAgentApproval(context.Background(), b.acct.WorkspaceID, need.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if got := b6140Field(t, a, "rules_currency"); got != "EUR" {
		t.Fatalf("the approval's rules are in %v, want EUR", got)
	}
	above, _ := b6140Field(t, a, "asks_above_minor").(float64)
	left, _ := b6140Field(t, a, "budget_left_after_minor").(float64)
	if got := economy.Money(int64(above), "EUR"); got != "€100.00" {
		t.Errorf("the sheet asks above %s, want €100.00", got)
	}
	if got := economy.Money(int64(left), "EUR"); got != "€1,850.00" {
		t.Errorf("the sheet leaves %s this month, want €1,850.00", got)
	}
}

// B61.42 — the euro agent's Know Your Agent credential states its limits in euros, as the owner typed them, with the
// euro moved by 5% since.
func TestB6142_TheCredentialStatesTheLimitsInTheirCurrency(t *testing.T) {
	b := b6142Payer(t, "ws-b6142-kya", `{"currency":"EUR","max_per_payment_minor":50000,"daily_limit_minor":30000,`+
		`"monthly_limit_minor":200000,"approval_above_minor":10000}`)
	f, err := b.store.AgentKYAFacts(context.Background(), b.acct.WorkspaceID, b.acct.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{"currency": "EUR", "max_per_payment_minor": 50000.0, "daily_limit_minor": 30000.0,
		"monthly_limit_minor": 200000.0, "approval_above_minor": 10000.0} {
		if got := b6140Field(t, f.Limits, field); got != want {
			t.Errorf("the credential's %s is %v, want %v", field, got, want)
		}
	}
}

// B61.42 — the euro agent's limit alert counts its €80.00 of a €100.00 daily limit in euros, with the euro moved by 5%
// since the rules were saved, and says so in euros, not LXC: what the payment was worth in euros as the rules let it
// through (B61.41). On main it is 924 LXC of 1,100 LXC, which is 84%.
func TestB6142_TheLimitAlertSaysTheRulesCurrency(t *testing.T) {
	b := b6142Payer(t, "ws-b6142-alert", `{"currency":"EUR","daily_limit_minor":10000}`)
	b.pay(t, "eighty", 8_000)
	raised, err := b.store.CheckSpendAlerts(context.Background(), b.acct.WorkspaceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var got []economy.SpendAlert
	for _, a := range raised {
		if a.Kind == economy.SpendAlertAgentLimit {
			got = append(got, a)
		}
	}
	if len(got) != 2 || got[1].Percent != 80 || got[1].Currency != "EUR" || got[1].SpentMinor != 8_000 || got[1].LimitMinor != 10_000 {
		t.Fatalf("the agent's limit alerts are %+v, want 50%% and 80%% of €100.00 with €80.00 spent", got)
	}
}
