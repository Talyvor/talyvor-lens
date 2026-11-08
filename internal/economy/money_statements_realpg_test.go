package economy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// B56.14 — every payment reaches a statement.

// A company's GBP account over a period holding a payment in and out, an invoice payment and a mandate pull from another
// company, a payout, a card settlement and a return: every one is a line, opening plus in minus out is the closing
// balance and what the account holds, and the same closed period asked again after more money moved reads the same.
func TestAccountStatement_EveryKindIsALineAndThePeriodReconciles(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws, payer = "ws-b5614-statement", "ws-b5614-payer"
	company, partner := openMoney(t, s, ws, CurrencyGBP, MoneyCompany), openMoney(t, s, ws, CurrencyGBP, MoneyPartner)
	payerCo, payerPartner := openMoney(t, s, payer, CurrencyGBP, MoneyCompany), openMoney(t, s, payer, CurrencyGBP, MoneyPartner)

	n := 0
	post := func(e MoneyEntry, from, to string, amount int64) MoneyEntry {
		t.Helper()
		n++
		e.Capability, e.Funding, e.IdempotencyKey = CapabilityCurrencyAccounts, FundingTest, "b5614-"+string(rune('a'+n))
		e.Postings = []MoneyPosting{{AccountID: from, AmountMinor: -amount}, {AccountID: to, AmountMinor: amount}}
		out, err := s.PostMoney(ctx, e)
		if err != nil {
			t.Fatalf("%s: %v", e.Kind, err)
		}
		return out
	}
	outside := func(kind, who string) MoneyEntry {
		return MoneyEntry{WorkspaceID: ws, Kind: kind, Counterparty: who, PartnerRef: "ref-" + kind}
	}
	fromPayer := func(kind string) MoneyEntry {
		return MoneyEntry{WorkspaceID: payer, Kind: kind, Memo: kind + " from the payer", payeeWorkspaceID: ws}
	}

	post(outside("payment_in", "Acme Ltd"), partner.ID, company.ID, 100_00)
	post(MoneyEntry{WorkspaceID: payer, Kind: "payment_in", Counterparty: "Payer's bank"}, payerPartner.ID, payerCo.ID, 500_00)
	first := post(outside("payment_in", "Acme Ltd"), partner.ID, company.ID, 50_00)
	post(outside("payment_out", "Supplier Ltd"), company.ID, partner.ID, 20_00)
	post(fromPayer("invoice_payment"), payerCo.ID, company.ID, 30_00)
	post(fromPayer("mandate_pull"), payerCo.ID, company.ID, 15_00)
	post(outside("payout", "Owner"), company.ID, partner.ID, 10_00)
	post(outside("card_charge", "Corner Shop"), company.ID, partner.ID, 5_00)
	last := post(outside("return", "Supplier Ltd"), partner.ID, company.ID, 2_00)
	from, to := first.CreatedAt, last.CreatedAt.Add(time.Microsecond)

	st, err := s.AccountStatement(ctx, ws, "", company.ID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var sum int64
	for _, l := range st.Lines {
		kinds = append(kinds, l.Kind)
		sum += l.AmountMinor
	}
	if want := "payment_in payment_out invoice_payment mandate_pull payout card_charge return"; strings.Join(kinds, " ") != want {
		t.Fatalf("lines are %v; want %s", kinds, want)
	}
	if st.Currency != CurrencyGBP || st.OpeningMinor != 100_00 || st.InMinor != 97_00 || st.OutMinor != 35_00 || st.ClosingMinor != 162_00 {
		t.Fatalf("totals %+v; want GBP, opening 10000, in 9700, out 3500, closing 16200", st)
	}
	if sum != st.InMinor-st.OutMinor || st.Lines[len(st.Lines)-1].BalanceAfterMinor != st.ClosingMinor {
		t.Fatalf("lines sum to %d and end at %d; want %d and %d", sum, st.Lines[len(st.Lines)-1].BalanceAfterMinor, st.InMinor-st.OutMinor, st.ClosingMinor)
	}
	if b := moneyBalance(t, s, ws, company.ID); b.AmountMinor != st.ClosingMinor {
		t.Fatalf("the account holds %d; the statement closes at %d", b.AmountMinor, st.ClosingMinor)
	}
	if l := st.Lines[1]; l.Counterparty != "Supplier Ltd" || l.PartnerRef != "ref-payment_out" || l.Funding != FundingTest {
		t.Errorf("the payment out reads %+v; want Supplier Ltd, its partner reference, test", l)
	}
	if l := st.Lines[2]; l.Counterparty != "workspace:"+payer || l.Memo != "invoice_payment from the payer" {
		t.Errorf("the invoice payment reads %+v; want the paying company and its memo", l)
	}

	post(outside("payment_in", "Acme Ltd"), partner.ID, company.ID, 1_00)
	again, err := s.AccountStatement(ctx, ws, "", company.ID, from, to)
	if err != nil || !reflect.DeepEqual(again, st) {
		t.Fatalf("the closed period asked again = %+v (%v); want the same as %+v", again, err, st)
	}
	if _, err := s.AccountStatement(ctx, payer, "", company.ID, from, to); !errors.Is(err, ErrMoneyAccountNotFound) {
		t.Errorf("another company reads this account's statement: %v; want ErrMoneyAccountNotFound", err)
	}
}

// An agent's account is read for that agent only: another agent's statement of it is not found.
func TestAccountStatement_AnAgentReadsOnlyItsOwnAccount(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws = "ws-b5614-agents"
	a, err := s.CreateAgent(ctx, ws, "Buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateAgent(ctx, ws, "Seller", "owner")
	if err != nil {
		t.Fatal(err)
	}
	acct, err := s.OpenMoneyAccount(ctx, MoneyAccount{WorkspaceID: ws, AgentID: a.ID, Currency: CurrencyEUR, Purpose: MoneyAgent})
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	if st, err := s.AccountStatement(ctx, ws, a.ID, acct.ID, from, to); err != nil || st.AgentID != a.ID || st.Currency != CurrencyEUR {
		t.Fatalf("the agent's own statement = %+v (%v)", st, err)
	}
	if _, err := s.AccountStatement(ctx, ws, b.ID, acct.ID, from, to); !errors.Is(err, ErrMoneyAccountNotFound) {
		t.Fatalf("another agent reads it: %v; want ErrMoneyAccountNotFound", err)
	}
}

// An agent's marketplace uses and its payments to another company's agent go on the company's marketplace bill: its
// credit statement lists each "on the marketplace bill", and its balance does not move.
func TestAgentPeriodStatement_ListsWhatTheAgentPutOnTheMarketplaceBill(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws = "ws-b5614-market"
	agent, err := s.CreateAgent(ctx, ws, "Shopper", "owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateAgent(ctx, ws, "Other", "owner")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, u := range []struct {
		id, listing, agent, payee, charge string
		price                             int64
	}{
		{"use_b5614_listing", "lst_1", agent.ID, "", "billed", 2_500_000},
		{"use_b5614_pay", "", agent.ID, "agt_elsewhere", "billed", 7_000_000},
		{"use_b5614_free", "lst_2", agent.ID, "", "free", 0},
		{"use_b5614_other", "lst_1", other.ID, "", "billed", 2_500_000},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id,
			payee_agent_id, memo, price_ulxc, charge, used_at, ran_at) VALUES ($1, $2, 1, 'ws-b5614-seller', $3, $4, $5, $1, $6, $7, $8, $8)`,
			u.id, u.listing, ws, u.agent, u.payee, u.price, u.charge, at); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Minute)
	}
	st, err := s.AgentPeriodStatement(ctx, ws, agent.ID, at.Add(-time.Hour), at)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range st.MarketplaceBill {
		if l.Label != OnMarketplaceBill {
			t.Errorf("%s is labelled %q; want %q", l.UseID, l.Label, OnMarketplaceBill)
		}
		got = append(got, l.UseID+" "+l.Kind)
	}
	if want := "use_b5614_listing use; use_b5614_pay pay"; strings.Join(got, "; ") != want {
		t.Fatalf("marketplace bill lines %v; want %s", got, want)
	}
	if len(st.Lines) != 0 || len(st.Accounts) != 1 || st.Accounts[0].ClosingULXC != 0 {
		t.Fatalf("the agent's credits moved: %+v %+v; want no lines and a zero balance", st.Accounts, st.Lines)
	}
}
