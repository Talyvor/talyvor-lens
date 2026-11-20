package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// earnVerified reads workspaces.earn_verified, the operator's verification of a workspace's people.
type earnVerified struct{}

func (earnVerified) MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT earn_verified FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&ok)
	return ok, err
}

// B22.3 — on the migrated schema, with test money: an agent of company A pays an agent of private user B by
// handle, B asks A for credits and A accepts, a recurring transfer runs twice, and B gives one transfer back;
// with live money and no clearance a transfer to another owner is refused as class AMBER, while one between an
// owner's own agents is GREEN. Every transfer is one entry of two postings that sum to zero.
func TestAgentTransfers_SendRequestRecurRefundAndTheClassOfEach(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	const lxc = int64(1_000_000)
	const company, person, other, companyToo = "ws-acme", "ws-bea", "ws-live", "ws-acme-2"
	for _, ws := range []string{company, person, other, companyToo} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
		sandboxConfirmed(t, pool, ws) // B61.52: it moves money, so it has confirmed who it is
	}
	agent := func(ws, name, owner string, funding string, credits int64) Agent {
		t.Helper()
		a, err := s.CreateAgent(ctx, ws, name, owner)
		if err != nil {
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
	buyer := agent(company, "buyer", "user-acme", FundingTest, 400*lxc) // test money
	bea := agent(person, "bea's agent", "user-bea", "", 0)
	if _, err := s.SetAgentHandle(ctx, company, buyer.ID, "@Acme.Buyer"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAgentHandle(ctx, person, bea.ID, "bea"); err != nil {
		t.Fatal(err)
	}
	if b, err := s.AgentBook(ctx, person); err != nil || b.Agents[0].Handle != "bea" {
		t.Fatalf("the book's agent handle = %+v, %v; want bea", b.Agents, err)
	}
	if _, err := s.SetAgentHandle(ctx, person, bea.ID+"x", "bea"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("a handle for no agent = %v", err)
	}

	// Company A's agent pays private user B's agent, by handle.
	paid, err := s.SendCredits(ctx, company, buyer.ID, "@bea", 50*lxc, "design work")
	if err != nil || paid.Class != "AMBER" || paid.TestFundedULXC != 50*lxc {
		t.Fatalf("send = %+v, %v; want an AMBER transfer of test money", paid, err)
	}
	// B asks A for 20 LXC; A accepts.
	req, err := s.RequestCredits(ctx, person, bea.ID, "acme.buyer", 20*lxc, "expenses")
	if err != nil {
		t.Fatal(err)
	}
	if req, err = s.AnswerMoneyRequest(ctx, company, req.ID, true); err != nil || req.Status != "accepted" || req.TransferID == "" {
		t.Fatalf("accept = %+v, %v", req, err)
	}
	if _, err := s.AnswerMoneyRequest(ctx, company, req.ID, true); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("answering a request twice = %v, want ErrRequestNotFound", err)
	}
	// A pays B 10 LXC a day: two ticks.
	first := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	sc, err := s.CreateAgentSchedule(ctx, company, buyer.ID, bea.ID, 10*lxc, "retainer", "day", first)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{first.Add(time.Minute), first.Add(24*time.Hour + time.Minute)} {
		if _, err := s.RunAgentSchedules(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	var ran int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_transfers WHERE schedule_id = $1`, sc.ID).Scan(&ran); err != nil || ran != 2 {
		t.Fatalf("the schedule made %d transfers (%v), want 2", ran, err)
	}
	// B gives the first transfer back, once.
	back, err := s.RefundTransfer(ctx, person, paid.ID)
	if err != nil || back.RefundOf != paid.ID || back.AmountULXC != 50*lxc {
		t.Fatalf("refund = %+v, %v", back, err)
	}
	if _, err := s.RefundTransfer(ctx, person, paid.ID); !errors.Is(err, ErrAlreadyRefunded) {
		t.Fatalf("a second refund = %v, want refused", err)
	}

	// Live money: C's agent may not pay another owner's agent without a clearance…
	live := agent(other, "live payer", "user-cara", FundingLive, 100*lxc)
	if _, err := s.SendCredits(ctx, other, live.ID, "@bea", 5*lxc, ""); !errors.Is(err, ErrCapabilityNotCleared) || !strings.Contains(err.Error(), "class AMBER") {
		t.Fatalf("a live transfer to another owner = %v, want refused as class AMBER", err)
	}
	// …but an owner may move live money between their own agents: GREEN.
	own := agent(companyToo, "acme's other agent", "user-acme", FundingLive, 100*lxc)
	green, err := s.SendCredits(ctx, companyToo, own.ID, buyer.ID, 5*lxc, "between my agents")
	if err != nil || green.Class != "GREEN" || green.TestFundedULXC != 0 {
		t.Fatalf("a transfer between one owner's agents = %+v, %v; want GREEN, live", green, err)
	}

	// Every transfer is one entry of two postings that sum to zero.
	var transfers, entries, unbalanced int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_transfers),
		(SELECT count(DISTINCT entry_id) FROM agent_postings WHERE kind = 'transfer'),
		(SELECT count(*) FROM (SELECT entry_id FROM agent_postings WHERE kind = 'transfer' GROUP BY entry_id
		  HAVING count(*) <> 2 OR sum(amount_ulxc) <> 0) x)`).Scan(&transfers, &entries, &unbalanced); err != nil {
		t.Fatal(err)
	}
	if transfers != 6 || entries != 6 || unbalanced != 0 {
		t.Fatalf("%d transfers, %d entries, %d not two postings summing to zero; want 6, 6, 0", transfers, entries, unbalanced)
	}
	// Where the money is: A's agent 400 − 50 − 20 − 10 − 10 + 50 + 5, B's 50 + 20 + 10 + 10 − 50, on the agents'
	// accounts and on the workspaces' balances; B's credits are the test money they came as.
	book := func(ws string) (agentBal, wsBal int64) {
		t.Helper()
		b, err := s.AgentBook(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		return b.Agents[0].BalanceULXC, b.WorkspaceBalanceULXC
	}
	if a, w := book(company); a != 365*lxc || w != 365*lxc {
		t.Errorf("company A: agent %d, workspace %d; want 365 LXC each", a, w)
	}
	if a, w := book(person); a != 40*lxc || w != 40*lxc {
		t.Errorf("private user B: agent %d, workspace %d; want 40 LXC each", a, w)
	}
	if tf, _ := s.TestFundedLXC(ctx, person); tf != 40*lxc {
		t.Errorf("B's test-funded credits = %d, want 40 LXC", tf)
	}
	// What B's agent received is held by it: B cannot fund another agent with it.
	if _, err := s.FundAgent(ctx, person, bea.ID, 1); !errors.Is(err, ErrAgentFunds) {
		t.Errorf("funding B's agent from credits its agent already holds = %v, want ErrAgentFunds", err)
	}
}
