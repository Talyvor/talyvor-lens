package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// B22.6 — on the migrated schema, with test money, between two owners' agents: one escrow is released when the
// payer confirms delivery, another at its deadline, and a disputed one stays held past its deadline until the
// operator decides (here: back to the payer). While held, the credits are in neither side's balance. Each
// escrow and its states are on both statements. Live money with no clearance is refused naming class AMBER.
func TestAgentEscrows_ConfirmDeadlineDisputeAndTheClass(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	const lxc = int64(1_000_000)
	for _, ws := range []string{"buyer", "seller", "live"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
		sandboxConfirmed(t, pool, ws) // B61.52: it moves money, so it has confirmed who it is
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
	buyer := agent("buyer", "buyer", FundingTest, 300*lxc)
	seller := agent("seller", "seller", "", 0)
	liveBuyer := agent("live", "live-buyer", FundingLive, 300*lxc)
	holds := func(ws, account string) int64 {
		t.Helper()
		var bal int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE workspace_id = $1 AND account = $2`,
			ws, account).Scan(&bal); err != nil {
			t.Fatal(err)
		}
		return bal
	}
	workspaceLXC := func(ws string) (bal, test int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM lxc_balances WHERE workspace_id = $1), 0)::bigint,
			COALESCE((SELECT test_funded_ulxc FROM lxc_balances WHERE workspace_id = $1), 0)::bigint`, ws).Scan(&bal, &test); err != nil {
			t.Fatal(err)
		}
		return bal, test
	}
	week, soon := time.Now().Add(7*24*time.Hour), time.Now().Add(time.Hour)

	if _, err := s.PayIntoEscrow(ctx, "live", liveBuyer.ID, "@seller", 10*lxc, "", week); !errors.Is(err, ErrCapabilityNotCleared) ||
		!strings.Contains(err.Error(), "class AMBER") {
		t.Fatalf("escrow of live money to another owner with no clearance = %v, want refused as class AMBER", err)
	}

	// 1. Paid in, held, released when the payer confirms delivery.
	a, err := s.PayIntoEscrow(ctx, "buyer", buyer.ID, "@seller", 100*lxc, "logo design", week)
	if err != nil || a.Status != "held" || a.Class != "AMBER" || a.TestFundedULXC != 100*lxc {
		t.Fatalf("pay into escrow = %+v, %v; want held, AMBER, all test money", a, err)
	}
	if bal, _ := workspaceLXC("buyer"); holds("buyer", agentAccount(buyer.ID)) != 200*lxc || holds("buyer", escrowAccount(a.ID)) != 100*lxc ||
		bal != 200*lxc || holds("seller", agentAccount(seller.ID)) != 0 {
		t.Fatalf("held: buyer's agent %d, escrow %d, buyer's workspace %d, seller's agent %d; want 200, 100, 200, 0 LXC",
			holds("buyer", agentAccount(buyer.ID)), holds("buyer", escrowAccount(a.ID)), bal, holds("seller", agentAccount(seller.ID)))
	}
	if _, err := s.ConfirmEscrow(ctx, "seller", a.ID); !errors.Is(err, ErrEscrowNotFound) {
		t.Fatalf("the payee confirming = %v, want refused", err)
	}
	if a, err = s.ConfirmEscrow(ctx, "buyer", a.ID); err != nil || a.Status != "released" {
		t.Fatalf("confirm = %+v, %v", a, err)
	}
	if bal, test := workspaceLXC("seller"); holds("seller", agentAccount(seller.ID)) != 100*lxc || holds("buyer", escrowAccount(a.ID)) != 0 ||
		bal != 100*lxc || test != 100*lxc {
		t.Fatalf("released: seller's agent %d, escrow %d, seller's workspace %d (test-funded %d); want 100, 0, 100 (100) LXC",
			holds("seller", agentAccount(seller.ID)), holds("buyer", escrowAccount(a.ID)), bal, test)
	}

	// 2. Released at its deadline, and not before.
	b, err := s.PayIntoEscrow(ctx, "buyer", buyer.ID, seller.ID, 50*lxc, "api credits", soon)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.RunEscrowDeadlines(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("before the deadline released %d, %v", n, err)
	}
	if n, err := s.RunEscrowDeadlines(ctx, soon.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("at the deadline released %d, %v; want 1", n, err)
	}
	if b, err = s.GetEscrow(ctx, "seller", b.ID); err != nil || b.Status != "released" || holds("seller", agentAccount(seller.ID)) != 150*lxc {
		t.Fatalf("after its deadline = %+v, %v, seller holds %d; want released and 150 LXC", b, err, holds("seller", agentAccount(seller.ID)))
	}

	// 3. Disputed: held past its deadline until the operator decides — here, back to the payer.
	c, err := s.PayIntoEscrow(ctx, "buyer", buyer.ID, "@seller", 40*lxc, "translation", soon)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisputeEscrow(ctx, "buyer", c.ID, " "); !errors.Is(err, ErrEscrowTerms) {
		t.Fatalf("a dispute with no reason = %v, want refused", err)
	}
	if c, err = s.DisputeEscrow(ctx, "buyer", c.ID, "never delivered"); err != nil || c.Status != "disputed" {
		t.Fatalf("dispute = %+v, %v", c, err)
	}
	if n, err := s.RunEscrowDeadlines(ctx, soon.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("a disputed escrow's deadline released %d, %v; want 0", n, err)
	}
	if _, err := s.ConfirmEscrow(ctx, "buyer", c.ID); !errors.Is(err, ErrEscrowNotFound) || holds("buyer", escrowAccount(c.ID)) != 40*lxc {
		t.Fatalf("confirming a disputed escrow = %v, escrow holds %d; want refused and still 40 LXC held", err, holds("buyer", escrowAccount(c.ID)))
	}
	if c, err = s.DecideEscrow(ctx, c.ID, false, "operator-cli:ng", "no delivery shown"); err != nil || c.Status != "returned" {
		t.Fatalf("decide = %+v, %v", c, err)
	}
	if bal, test := workspaceLXC("buyer"); holds("buyer", agentAccount(buyer.ID)) != 150*lxc || holds("buyer", escrowAccount(c.ID)) != 0 ||
		bal != 150*lxc || test != 150*lxc {
		t.Fatalf("returned: buyer's agent %d, escrow %d, buyer's workspace %d (test-funded %d); want 150, 0, 150 (150) LXC",
			holds("buyer", agentAccount(buyer.ID)), holds("buyer", escrowAccount(c.ID)), bal, test)
	}

	// Each escrow and each of its states is on both statements.
	want := map[string]string{a.ID: "held released", b.ID: "held released", c.ID: "held disputed returned"}
	for _, ws := range []string{"buyer", "seller"} {
		st, err := s.WorkspaceAgentStatement(ctx, ws, time.Now().Add(-time.Hour), time.Now().AddDate(0, 0, 10))
		if err != nil || len(st.Escrows) != 3 {
			t.Fatalf("%s's statement escrows = %+v, %v; want all three", ws, st.Escrows, err)
		}
		for _, e := range st.Escrows {
			var kinds []string
			for _, ev := range e.Events {
				kinds = append(kinds, ev.Kind)
			}
			if got := strings.Join(kinds, " "); got != want[e.ID] {
				t.Errorf("%s's statement: escrow %q states %q, want %q", ws, e.Memo, got, want[e.ID])
			}
		}
	}
}
