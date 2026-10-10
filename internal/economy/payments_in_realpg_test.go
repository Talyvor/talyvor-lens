package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/partners"
)

// B30.15 — a test £120.00 quoting an agent's reference posts one balanced entry to that agent; one quoting a
// reference that names no account sits in suspense until the job pays it back after five business days; one the
// operator assigns leaves suspense for the account it was for and can no longer go back. Every line is on its
// account's statement with the payer's name and reference — all asserted on postings.
func TestPaymentsIn_MatchedMoneyPostsToTheAgentAndUnmatchedMoneyIsReturnedFromSuspense(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	s.SetAccountPartners(partners.NewRegistry(nil))
	const ws = "ws-b3015-payments-in"
	agent, err := s.CreateAgent(ctx, ws, "Collector", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := s.OpenCurrencyAccount(ctx, ws, "", CurrencyGBP, FundingTest)
	if err != nil {
		t.Fatal(err)
	}
	agentGBP, err := s.OpenCurrencyAccount(ctx, ws, agent.ID, CurrencyGBP, FundingTest)
	if err != nil {
		t.Fatal(err)
	}
	var partnerID, agentRef string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE purpose = 'partner' AND partner_account_ref = $1`,
		company.PartnerAccountRef).Scan(&partnerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT payment_reference FROM money_accounts WHERE id = $1`, agentGBP.ID).Scan(&agentRef); err != nil {
		t.Fatal(err)
	}
	postings := func(entryID string) string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT account_id, amount_minor, currency, funding FROM money_postings WHERE entry_id = $1 ORDER BY line`, entryID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var acct, cur, funding string
			var amount int64
			if err := rows.Scan(&acct, &amount, &cur, &funding); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%s %d %s %s", acct, amount, cur, funding))
		}
		return strings.Join(got, "; ")
	}
	receive := func(ref, reference string, minor int64) MoneyEntry {
		t.Helper()
		e, err := s.ReceivePayment(ctx, InboundPayment{PartnerAccountRef: company.PartnerAccountRef, PartnerRef: ref,
			Payer: "Acme Supplies Ltd", Reference: reference, AmountMinor: minor, Currency: CurrencyGBP})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	matched := receive("test_in_b3015_1", "INV-7 "+agentRef, 120_00)
	if got, want := postings(matched.ID), partnerID+" -12000 GBP test; "+agentGBP.ID+" 12000 GBP test"; got != want {
		t.Fatalf("the matched £120.00 posted %s; want %s", got, want)
	}

	unmatched := receive("test_in_b3015_2", "TLV000000000000", 75_00)
	var suspenseID string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'suspense' AND currency = 'GBP'`, ws).Scan(&suspenseID); err != nil {
		t.Fatalf("no GBP suspense account: %v", err)
	}
	if got, want := postings(unmatched.ID), partnerID+" -7500 GBP test; "+suspenseID+" 7500 GBP test"; got != want {
		t.Fatalf("the unmatched £75.00 posted %s; want %s", got, want)
	}
	held, err := s.SuspenseItems(ctx, 5)
	if err != nil || len(held) != 1 || held[0].EntryID != unmatched.ID || held[0].Payer != "Acme Supplies Ltd" {
		t.Fatalf("suspense = %+v, %v; want the £75.00 from Acme Supplies Ltd", held, err)
	}
	if n, err := s.ReturnDueSuspense(ctx, time.Now(), 5); err != nil || n != 0 {
		t.Fatalf("a return run on the day it arrived returned %d (%v); want none", n, err)
	}
	// Lens restarts: the Test partner has forgotten the account the money came to, and the job still pays it back.
	s.SetAccountPartners(partners.NewRegistry(nil))
	if n, err := s.ReturnDueSuspense(ctx, held[0].ReturnAfter, 5); err != nil || n != 1 {
		t.Fatalf("a return run once it was due returned %d (%v); want 1", n, err)
	}
	var returnID string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_entries WHERE workspace_id = $1 AND idempotency_key = $2 AND kind = 'payment_in_returned'`,
		ws, "suspense:"+unmatched.ID).Scan(&returnID); err != nil {
		t.Fatalf("no return entry: %v", err)
	}
	if got, want := postings(returnID), suspenseID+" -7500 GBP test; "+partnerID+" 7500 GBP test"; got != want {
		t.Fatalf("the return posted %s; want %s", got, want)
	}
	if n, err := s.ReturnDueSuspense(ctx, held[0].ReturnAfter.AddDate(0, 0, 7), 5); err != nil || n != 0 {
		t.Fatalf("a second run returned %d (%v); want none: it went back once", n, err)
	}

	// The operator assigns the next one to the company; then it is not in suspense to go back.
	assigned := receive("test_in_b3015_3", "for agent TLV00000000000F", 30_00)
	e, err := s.AssignSuspense(ctx, assigned.ID, company.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := postings(e.ID), suspenseID+" -3000 GBP test; "+company.ID+" 3000 GBP test"; got != want {
		t.Fatalf("the assignment posted %s; want %s", got, want)
	}
	if _, err := s.returnSuspense(ctx, assigned.ID); !errors.Is(err, ErrSuspenseNotHeld) {
		t.Fatalf("returning an assigned payment = %v; want ErrSuspenseNotHeld", err)
	}

	statement := func(account string) []MoneyStatementLine {
		t.Helper()
		lines, err := s.MoneyStatement(ctx, ws, "", account, 100)
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}
	if l := statement(agentGBP.ID); len(l) != 1 || l[0].Counterparty != "Acme Supplies Ltd" || l[0].Reference != "INV-7 "+agentRef ||
		l[0].AmountMinor != 120_00 || l[0].BalanceMinor != 120_00 {
		t.Errorf("the agent's statement = %+v; want the £120.00 from Acme Supplies Ltd with its reference", l)
	}
	if l := statement(suspenseID); len(l) != 4 || l[0].BalanceMinor != 0 || l[2].Kind != "payment_in_returned" || l[3].Counterparty != "Acme Supplies Ltd" {
		t.Errorf("the suspense statement = %+v; want in, returned, in, assigned, newest first, ending at zero", l)
	}
	if l := statement(company.ID); len(l) != 1 || l[0].Kind != "suspense_assigned" || l[0].Counterparty != "Acme Supplies Ltd" {
		t.Errorf("the company's statement = %+v; want the assigned £30.00 from Acme Supplies Ltd", l)
	}
}
