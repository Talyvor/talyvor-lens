package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// B22.5 — on the migrated schema, with test money: company A's agent lends company B's agent 100 LXC at 10%
// in three daily instalments; B accepts and the principal is paid out; two instalments are taken with their
// interest; the third cannot be, so the loan goes late, and missed again (with the late fee) it defaults. Both
// statements carry the loan. A private user on either side is refused as class RED, and an offer of live money
// with no clearance is refused as class AMBER.
func TestCompanyLoans_LendRepayLateDefaultAndWhoMayBorrow(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	const lxc = int64(1_000_000)
	for ws, company := range map[string]bool{"co-a": true, "co-b": true, "co-live": true, "person": false} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, company) VALUES ($1, $1, $1, true, $2)`, ws, company); err != nil {
			t.Fatal(err)
		}
	}
	agent := func(ws, handle, funding string, credits int64) Agent {
		t.Helper()
		a, err := s.CreateAgent(ctx, ws, handle, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetAgentHandle(ctx, ws, a.ID, handle); err != nil {
			t.Fatal(err)
		}
		if credits > 0 {
			if _, err := s.CreditLXC(ctx, ws, credits, "stripe top-up", map[string]interface{}{"funding": funding}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FundAgent(ctx, ws, a.ID, credits); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	lender := agent("co-a", "lender", FundingTest, 500*lxc)
	borrower := agent("co-b", "borrower", "", 0)
	person := agent("person", "person", FundingTest, 500*lxc)
	liveLender := agent("co-live", "live-lender", FundingLive, 500*lxc)
	terms := LoanTerms{PrincipalULXC: 100 * lxc, InterestBPS: 1000, Instalments: 3, Every: "day", LateFeeULXC: 2 * lxc, Memo: "working capital"}

	// Who may lend and borrow.
	for _, try := range []struct{ ws, agent, to string }{{"co-a", lender.ID, "@person"}, {"person", person.ID, "@borrower"}} {
		if _, err := s.OfferLoan(ctx, try.ws, try.agent, try.to, terms); !errors.Is(err, ErrLoanCompaniesOnly) || !strings.Contains(err.Error(), "class RED") {
			t.Fatalf("%s offering to %s = %v, want refused as class RED", try.ws, try.to, err)
		}
	}
	if _, err := s.OfferLoan(ctx, "co-live", liveLender.ID, "@borrower", terms); !errors.Is(err, ErrCapabilityNotCleared) || !strings.Contains(err.Error(), "class AMBER") {
		t.Fatalf("an offer of live money with no clearance = %v, want refused as class AMBER", err)
	}

	// A offers, B accepts: the principal is paid out.
	offer, err := s.OfferLoan(ctx, "co-a", lender.ID, "@borrower", terms)
	if err != nil {
		t.Fatal(err)
	}
	loan, err := s.AnswerLoan(ctx, "co-b", offer.ID, true)
	if err != nil || loan.Status != "active" || loan.NextDueAt == nil {
		t.Fatalf("accept = %+v, %v", loan, err)
	}
	holds := func(ws string, a Agent) int64 {
		t.Helper()
		var bal int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE workspace_id = $1 AND account = $2`,
			ws, agentAccount(a.ID)).Scan(&bal); err != nil {
			t.Fatal(err)
		}
		return bal
	}
	if holds("co-b", borrower) != 100*lxc {
		t.Fatalf("the borrower holds %d, want the 100 LXC principal", holds("co-b", borrower))
	}

	// Four daily ticks: instalments 1 and 2 are taken; 3 (36.67 LXC) is more than the 26.67 LXC left: late,
	// then default.
	for i := 0; i < 4; i++ {
		if loan, err = s.GetLoan(ctx, "co-a", offer.ID); err != nil || loan.NextDueAt == nil {
			t.Fatalf("tick %d: loan %+v, %v", i, loan, err)
		}
		if _, err := s.RunLoanRepayments(ctx, loan.NextDueAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if loan, err = s.GetLoan(ctx, "co-b", offer.ID); err != nil || loan.Status != "defaulted" || loan.PaidInstalments != 2 || loan.NextDueAt != nil {
		t.Fatalf("after four ticks = %+v, %v; want defaulted with 2 instalments paid", loan, err)
	}
	var kinds []string
	var interest int64
	for _, e := range loan.Events {
		kinds = append(kinds, e.Kind)
		if e.Kind == "instalment" {
			interest += e.InterestULXC
		}
	}
	if got := strings.Join(kinds, " "); got != "payout instalment instalment missed late missed defaulted" || interest != 6_666_666 {
		t.Fatalf("events %q with %d µLXC interest paid; want payout, two instalments with 6.666666 LXC interest, late, default", got, interest)
	}
	if loan.Events[5].LateFeeULXC != 2*lxc {
		t.Errorf("the retried instalment asked %d µLXC late fee, want 2 LXC", loan.Events[5].LateFeeULXC)
	}
	paid := 2 * (33_333_333 + 3_333_333)
	if holds("co-a", lender) != 400*lxc+int64(paid) || holds("co-b", borrower) != 100*lxc-int64(paid) {
		t.Errorf("lender holds %d, borrower %d; want %d and %d", holds("co-a", lender), holds("co-b", borrower), 400*lxc+int64(paid), 100*lxc-int64(paid))
	}
	// A loan's money cannot come back as a plain refund.
	if _, err := s.RefundTransfer(ctx, "co-b", loan.Events[0].TransferID); !errors.Is(err, ErrTransferNotFound) {
		t.Errorf("refunding the payout = %v, want refused", err)
	}

	// Both statements carry the loan and its events.
	for _, ws := range []string{"co-a", "co-b"} {
		st, err := s.WorkspaceAgentStatement(ctx, ws, time.Now().Add(-time.Hour), time.Now().AddDate(0, 0, 10))
		if err != nil || len(st.Loans) != 1 || st.Loans[0].ID != offer.ID || len(st.Loans[0].Events) != 7 {
			t.Errorf("%s's statement loans = %+v, %v; want the loan with its 7 events", ws, st.Loans, err)
		}
	}
}

// B26.8 — three loans fall due in one tick and the first of them cannot be paid: the run records the miss,
// takes the other two in the same tick and reports Missed=1. Missed again a period on, the run reports the default.
func TestCompanyLoans_OneRunTakesEveryDueLoanAndCountsTheMiss(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	const lxc = int64(1_000_000)
	for _, ws := range []string{"co-a", "co-b"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	agent := func(ws, handle string, credits int64) Agent {
		t.Helper()
		a, err := s.CreateAgent(ctx, ws, handle, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetAgentHandle(ctx, ws, a.ID, handle); err != nil {
			t.Fatal(err)
		}
		if credits > 0 {
			if _, err := s.CreditLXC(ctx, ws, credits, "stripe top-up", map[string]interface{}{"funding": FundingTest}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FundAgent(ctx, ws, a.ID, credits); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	lender := agent("co-a", "lender", 500*lxc)
	// One instalment of 110 LXC on a 100 LXC loan: "short" holds only the principal, the other two 20 LXC more.
	terms := LoanTerms{PrincipalULXC: 100 * lxc, InterestBPS: 1000, Instalments: 1, Every: "day", LateFeeULXC: 2 * lxc}
	var ids []string
	var due time.Time
	for _, b := range []struct {
		handle string
		extra  int64
	}{{"short", 0}, {"payer-one", 20 * lxc}, {"payer-two", 20 * lxc}} {
		agent("co-b", b.handle, b.extra)
		offer, err := s.OfferLoan(ctx, "co-a", lender.ID, "@"+b.handle, terms)
		if err != nil {
			t.Fatal(err)
		}
		loan, err := s.AnswerLoan(ctx, "co-b", offer.ID, true)
		if err != nil || loan.NextDueAt == nil {
			t.Fatalf("accept %s = %+v, %v", b.handle, loan, err)
		}
		ids = append(ids, loan.ID)
		due = *loan.NextDueAt
	}

	res, err := s.RunLoanRepayments(ctx, due.Add(time.Second))
	if err != nil || res != (LoanRunResult{Paid: 2, Missed: 1}) {
		t.Fatalf("the tick = %+v, %v; want Paid=2 Missed=1", res, err)
	}
	loans, err := s.ListLoans(ctx, "co-b")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range loans {
		var kinds []string
		for _, e := range l.Events {
			kinds = append(kinds, e.Kind)
		}
		got[l.ID] = l.Status + ": " + strings.Join(kinds, " ")
	}
	for i, want := range []string{"late: payout missed late", "repaid: payout instalment", "repaid: payout instalment"} {
		if got[ids[i]] != want {
			t.Errorf("loan %d after the tick = %q, want %q", i, got[ids[i]], want)
		}
	}

	res, err = s.RunLoanRepayments(ctx, due.AddDate(0, 0, 1).Add(time.Second))
	if err != nil || res != (LoanRunResult{Missed: 1, Defaulted: 1}) {
		t.Fatalf("the retry a day on = %+v, %v; want Missed=1 Defaulted=1", res, err)
	}
	if l, err := s.GetLoan(ctx, "co-b", ids[0]); err != nil || l.Status != "defaulted" {
		t.Fatalf("the short loan after its retry = %+v, %v; want defaulted", l, err)
	}
}

// B56.22 — at the lending fee (1%), company A offers company B a 100 LXC loan: the offer shows the 1 LXC fee
// before B answers; accepting pays the principal out and takes the fee from what B received, as its own row on
// B's ledger and its own posting on B's agent; a retried accept takes nothing more; a declined offer takes nothing.
func TestCompanyLoans_LendingFeeTakenOnceFromTheBorrowerOnAccept(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	s.SetLendingFee(func() int64 { return 100 })
	const lxc = int64(1_000_000)
	for _, ws := range []string{"co-a", "co-b"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	agent := func(ws, handle string, credits int64) Agent {
		t.Helper()
		a, err := s.CreateAgent(ctx, ws, handle, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetAgentHandle(ctx, ws, a.ID, handle); err != nil {
			t.Fatal(err)
		}
		if credits > 0 {
			if _, err := s.CreditLXC(ctx, ws, credits, "stripe top-up", map[string]interface{}{"funding": FundingTest}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FundAgent(ctx, ws, a.ID, credits); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	lender := agent("co-a", "lender", 500*lxc)
	borrower := agent("co-b", "borrower", 0)
	terms := LoanTerms{PrincipalULXC: 100 * lxc, InterestBPS: 1000, Instalments: 3, Every: "day"}
	fees := func() (ledger, postings int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM lxc_ledger WHERE workspace_id = 'co-b' AND type = $1`,
			LXCTypeLendingFee).Scan(&ledger); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE workspace_id = 'co-b' AND account = $1 AND kind = $2`,
			agentAccount(borrower.ID), LXCTypeLendingFee).Scan(&postings); err != nil {
			t.Fatal(err)
		}
		return ledger, postings
	}

	// Declined: nothing is taken.
	declined, err := s.OfferLoan(ctx, "co-a", lender.ID, "@borrower", terms)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerLoan(ctx, "co-b", declined.ID, false); err != nil {
		t.Fatal(err)
	}
	if l, p := fees(); l != 0 || p != 0 {
		t.Fatalf("after a declined offer the fee rows sum to %d on the ledger and %d on the agent, want none", l, p)
	}

	// The offer shows the fee before the borrower answers, on both sides.
	offer, err := s.OfferLoan(ctx, "co-a", lender.ID, "@borrower", terms)
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"co-a", "co-b"} {
		if l, err := s.GetLoan(ctx, ws, offer.ID); err != nil || l.Status != "offered" || l.FeeBPS != 100 || l.FeeULXC != lxc {
			t.Fatalf("%s sees the offer as %+v, %v; want offered with a 1 LXC fee at 100 bps", ws, l, err)
		}
	}

	// Accepted: the fee is taken from what the borrower received, once.
	loan, err := s.AnswerLoan(ctx, "co-b", offer.ID, true)
	if err != nil || loan.Status != "active" || len(loan.Events) != 1 || loan.Events[0].FeeULXC != lxc {
		t.Fatalf("accept = %+v, %v; want active with the payout event naming the 1 LXC fee", loan, err)
	}
	if _, err := s.AnswerLoan(ctx, "co-b", offer.ID, true); !errors.Is(err, ErrLoanNotFound) {
		t.Fatalf("a retried accept = %v, want ErrLoanNotFound", err)
	}
	if l, p := fees(); l != -lxc || p != -lxc {
		t.Fatalf("the fee rows sum to %d on the ledger and %d on the agent, want −1 LXC on each", l, p)
	}
	var bal, held int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM lxc_balances WHERE workspace_id = 'co-b'`).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE workspace_id = 'co-b' AND account = $1`,
		agentAccount(borrower.ID)).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if bal != 99*lxc || held != 99*lxc {
		t.Fatalf("the borrower's workspace holds %d and its agent %d, want 99 LXC each", bal, held)
	}
	st, err := s.WorkspaceAgentStatement(ctx, "co-b", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, l := range st.Lines {
		if l.Kind == LXCTypeLendingFee {
			labels = append(labels, l.Label)
		}
	}
	if strings.Join(labels, ",") != "Lending fee 1%,Lending fee 1%" {
		t.Errorf("the borrower's statement fee lines are %q, want the agent's and the spend side's, each \"Lending fee 1%%\"", labels)
	}
}
