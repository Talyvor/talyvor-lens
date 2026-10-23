package economy

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/tenant"
)

// B45.7 — a new workspace starts with test money and a first agent, in one transaction, once.

func TestStartWorkspace_TestMoneyFirstAgentKeyAndRulesOnce(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	keys := tenant.NewStore(pool)
	issue := func(ctx context.Context, tx pgx.Tx, ws, name string) (string, string, error) {
		raw, k, err := tenant.CreateAPIKeyTx(ctx, tx, ws, name, []string{"proxy"}, nil)
		if err != nil {
			return "", "", err
		}
		return raw, k.ID, nil
	}
	const ws, money = "ws-start", int64(50_000_000)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}

	out, started, err := s.StartWorkspace(ctx, ws, "user-start", money, issue)
	if err != nil || !started {
		t.Fatalf("start: started=%v err=%v", started, err)
	}

	// The ledger: one starter row of test money, counted test-funded, never cash-backed, all of it on the agent.
	var amount, testFunded, backed, balance int64
	var funding string
	if err := pool.QueryRow(ctx, `SELECT amount, metadata->>'funding' FROM lxc_ledger WHERE workspace_id = $1 AND type = $2`,
		ws, LXCTypeStarterTest).Scan(&amount, &funding); err != nil {
		t.Fatalf("starter ledger row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT balance, test_funded_ulxc, cash_backed_ulxc FROM lxc_balances WHERE workspace_id = $1`,
		ws).Scan(&balance, &testFunded, &backed); err != nil {
		t.Fatal(err)
	}
	if amount != money || funding != FundingTest || balance != money || testFunded != money || backed != 0 {
		t.Fatalf("ledger: amount=%d funding=%q balance=%d test_funded=%d cash_backed=%d; want %d test, all test-funded, none backed",
			amount, funding, balance, testFunded, backed, money)
	}
	book, err := s.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(book.Agents) != 1 || book.Agents[0].ID != out.Agent.ID || book.Agents[0].BalanceULXC != money {
		t.Fatalf("agents = %+v, want the one first agent holding %d", book.Agents, money)
	}

	// The key works, and spends as the agent.
	k, err := keys.ValidateAPIKey(ctx, out.Key)
	if err != nil || k.WorkspaceID != ws || k.ID != out.KeyID {
		t.Fatalf("first agent's key: %+v, %v", k, err)
	}
	if agentID, _, err := s.AgentOfKey(ctx, out.KeyID); err != nil || agentID != out.Agent.ID {
		t.Fatalf("the key spends as %q (%v), want %q", agentID, err, out.Agent.ID)
	}
	// The rules are the first-agent template's.
	rules, err := s.GetAgentRules(ctx, ws, out.Agent.ID)
	tmpl, _ := RuleTemplateByID(FirstAgentTemplate)
	if err != nil || rules.MaxPerRequestULXC != tmpl.Rules.MaxPerRequestULXC || rules.ApprovalAboveULXC != tmpl.Rules.ApprovalAboveULXC ||
		!rules.PauseOnUnusualSpend {
		t.Fatalf("rules = %+v (%v), want the %s template", rules, err, FirstAgentTemplate)
	}

	// A retried start changes nothing.
	if _, started, err := s.StartWorkspace(ctx, ws, "user-start", money, issue); err != nil || started {
		t.Fatalf("retry: started=%v err=%v, want nothing done", started, err)
	}
	var rows, agents, keysN int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1),
		(SELECT count(*) FROM agent_accounts WHERE workspace_id = $1), (SELECT count(*) FROM workspace_api_keys WHERE workspace_id = $1),
		(SELECT balance FROM lxc_balances WHERE workspace_id = $1)`, ws).Scan(&rows, &agents, &keysN, &balance); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || agents != 1 || keysN != 1 || balance != money {
		t.Fatalf("after a retry: %d ledger rows, %d agents, %d keys, balance %d; want 1, 1, 1, %d", rows, agents, keysN, balance, money)
	}
}

func TestStartWorkspace_AFailedKeyLeavesNothing(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws = "ws-start-fail"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	refuse := func(context.Context, pgx.Tx, string, string) (string, string, error) {
		return "", "", errors.New("no key")
	}
	if _, started, err := s.StartWorkspace(ctx, ws, "user-start", 50_000_000, refuse); err == nil || started {
		t.Fatalf("started=%v err=%v, want the start refused", started, err)
	}
	var rows, agents int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1),
		(SELECT count(*) FROM agent_accounts WHERE workspace_id = $1)`, ws).Scan(&rows, &agents); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || agents != 0 {
		t.Fatalf("a failed start left %d ledger rows and %d agents, want none", rows, agents)
	}
}
