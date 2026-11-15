package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// B55.30 — a refused funding or withdrawal reads its amounts in dollars and names the agent, never µLXC (B61.16:
// nor LXC).
func TestAgentFundRefusals_ReadDollarsAndNameTheAgent(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws = "ws-b5530"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 165000000, 165000000)`, ws); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, ws, "Research", "user-b5530")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, a.ID, 40_000_000); err != nil {
		t.Fatal(err)
	}

	_, err = s.FundAgent(ctx, ws, a.ID, 200_000_000)
	if want := "not enough for this move: the workspace has $12.50 that is not with an agent"; !errors.Is(err, ErrAgentFunds) ||
		!strings.HasSuffix(err.Error(), want) || strings.Contains(err.Error(), "µLXC") {
		t.Errorf("funding past the workspace's balance = %v, want it to end %q", err, want)
	}
	_, err = s.WithdrawAgent(ctx, ws, a.ID, 50_000_000)
	if want := "not enough for this move: Research holds $4.00"; !errors.Is(err, ErrAgentFunds) || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("withdrawing past the agent's balance = %v, want it to end %q", err, want)
	}

}
