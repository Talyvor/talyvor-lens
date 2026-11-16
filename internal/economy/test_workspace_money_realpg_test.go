package economy

import (
	"context"
	"testing"
)

// B17.105 — on the migrated schema: two test companies whose credits came from their starting grant (no
// test-funded counter at all) move money to each other and cash it out. Every move is test money to the last µLXC,
// as recorded on its row.
func TestTestWorkspaces_GrantFundedMovesAreTestMoney(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	s.SetCashOutPartner(TestCashOutPartner{})
	const lxc = int64(1_000_000)
	agent := func(ws string) Agent {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, synthetic) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
		sandboxConfirmed(t, pool, ws) // B61.52: it moves money, so it has confirmed who it is
		a, err := s.CreateAgent(ctx, ws, "agent", "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.GrantLXC(ctx, ws, 100*lxc, "starting grant", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FundAgent(ctx, ws, a.ID, 100*lxc); err != nil {
			t.Fatal(err)
		}
		return a
	}
	a, b := agent("ws-test-a"), agent("ws-test-b")

	sent, err := s.SendCredits(ctx, "ws-test-a", a.ID, b.ID, 40*lxc, "test money")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.RequestCashOut(ctx, "ws-test-b", b.ID, 130*lxc, "test bank ••1234", "owner")
	if err != nil {
		t.Fatal(err)
	}
	var class string
	var xferTest, cashTest int64
	if err := pool.QueryRow(ctx, `SELECT class, test_funded_ulxc, (SELECT test_funded_ulxc FROM agent_cash_outs WHERE id = $2)
		FROM agent_transfers WHERE id = $1`, sent.ID, c.ID).Scan(&class, &xferTest, &cashTest); err != nil {
		t.Fatal(err)
	}
	if class != "AMBER" || xferTest != 40*lxc || cashTest != 130*lxc {
		t.Fatalf("the send is %s with %d µLXC test money, the cash-out %d; want AMBER, %d and %d", class, xferTest, cashTest, 40*lxc, 130*lxc)
	}
}
