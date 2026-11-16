package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// B22.9 — on the migrated schema, through the test partner: a verified owner cashes 40 of an agent's 100
// test-funded LXC out; the credits leave the agent and the workspace's balance at once and are held, and on the
// tick the partner reports the payment paid and they are gone for good. A cash-out the partner fails comes back
// to the agent, test-funded. A request of live money is refused naming class RED.
func TestCashOuts_HeldPaidFailedAndTheClass(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	s.SetCashOutPartner(TestCashOutPartner{})
	const lxc = int64(1_000_000)
	agent := func(ws, funding string) Agent {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
		sandboxConfirmed(t, pool, ws) // B61.52: it moves money, so it has confirmed who it is
		a, err := s.CreateAgent(ctx, ws, "earner", "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreditLXC(ctx, ws, 100*lxc, "stripe top-up", map[string]interface{}{"funding": funding}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FundAgent(ctx, ws, a.ID, 100*lxc); err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := agent("ws-c", FundingTest)
	live := agent("ws-live", FundingLive)
	book := func(ws string, account string) (posted, balance, test int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT sum(amount_ulxc) FROM agent_postings WHERE workspace_id = $1 AND account = $2), 0)::bigint,
			balance, test_funded_ulxc FROM lxc_balances WHERE workspace_id = $1`, ws, account).Scan(&posted, &balance, &test); err != nil {
			t.Fatal(err)
		}
		return
	}

	if _, err := s.RequestCashOut(ctx, "ws-live", live.ID, 10*lxc, "bank ••9999", "owner"); !errors.Is(err, ErrCapabilityNotCleared) ||
		!strings.Contains(err.Error(), "class RED") {
		t.Fatalf("cashing out live money with no clearance = %v, want refused as class RED", err)
	}

	c, err := s.RequestCashOut(ctx, "ws-c", a.ID, 40*lxc, "test bank ••1234", "owner")
	if err != nil || c.Status != "submitted" || c.Partner != "test" || c.AmountUUSD != 40*lxc/ULXCPerUSDMicro || c.TestFundedULXC != 40*lxc {
		t.Fatalf("request = %+v, %v; want submitted to the test partner, all test money", c, err)
	}
	held, _, _ := book("ws-c", cashOutAccount(c.ID))
	if agentHolds, balance, _ := book("ws-c", agentAccount(a.ID)); agentHolds != 60*lxc || held != 40*lxc || balance != 60*lxc {
		t.Fatalf("held: agent %d, cash-out %d, workspace balance %d; want 60, 40, 60 LXC", agentHolds, held, balance)
	}
	if res, err := s.RunCashOuts(ctx); err != nil || res.Paid != 1 {
		t.Fatalf("tick = %+v, %v; want the cash-out paid", res, err)
	}
	gone, _, _ := book("ws-c", "cashed_out")
	held, _, _ = book("ws-c", cashOutAccount(c.ID))
	if list, _ := s.ListCashOuts(ctx, "ws-c"); len(list) != 1 || list[0].Status != "paid" || held != 0 || gone != 40*lxc {
		t.Fatalf("after the partner paid: %+v, held %d, cashed out %d; want paid, 0, 40 LXC", list, held, gone)
	}

	f, err := s.RequestCashOut(ctx, "ws-c", a.ID, 20*lxc, "test-fail account", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := s.RunCashOuts(ctx); err != nil || res.Failed != 1 {
		t.Fatalf("tick = %+v, %v; want the cash-out failed", res, err)
	}
	if agentHolds, balance, test := book("ws-c", agentAccount(a.ID)); agentHolds != 60*lxc || balance != 60*lxc || test != 60*lxc {
		t.Fatalf("after the partner failed %s: agent %d, workspace balance %d (test-funded %d); want 60, 60 (60) LXC back", f.ID, agentHolds, balance, test)
	}
}
