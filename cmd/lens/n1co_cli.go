package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// n1co_cli.go — B45.33: `lens n1co` gives N1CO, Talyvor's AI CEO, its own workspace and agent wallet on test money,
// so its tool spend runs through Talyvor first. See docs/n1co-wallet.md.

const n1coUsage = `usage:
  lens n1co -issuer <sign-in issuer> -subject <sign-in subject> -monthly-budget <LXC> [-test-money <LXC>]
creates the N1CO workspace for that sign-in, with an agent wallet called N1CO on test money and a monthly budget;
a second run changes nothing. See docs/n1co-wallet.md.`

// N1COAgentName is the name of N1CO's agent wallet.
const N1COAgentName = "N1CO"

// runN1CO is `lens n1co`, run inside the lens container so it reaches the same Postgres the server reads.
func runN1CO(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("n1co: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("n1co: database: %w", err)
	}
	defer pool.Close()
	return n1coCommand(ctx, pool, economy.NewDualTokenStore(nil, pool, nil), cfg.StarterTestMoneyULXC, args, os.Stdout)
}

// n1coCommand creates, once, the workspace the sign-in issuer+subject lands in (the id /v1/provision derives for
// it, so that sign-in on the phone app is N1CO's and its approvals go there), named N1CO, with an agent N1CO on the
// first agent's rules and the monthly budget given, funded with test money (defaultTestMoneyULXC unless -test-money).
func n1coCommand(ctx context.Context, pool *pgxpool.Pool, bank *economy.DualTokenStore, defaultTestMoneyULXC int64, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("n1co", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	issuer := fs.String("issuer", "", "")
	subject := fs.String("subject", "", "")
	budget := fs.Float64("monthly-budget", 0, "")
	testMoney := fs.Float64("test-money", -1, "")
	if err := fs.Parse(args); err != nil || *issuer == "" || *subject == "" || fs.NArg() != 0 {
		return errors.New(n1coUsage)
	}
	if *budget <= 0 {
		return fmt.Errorf("n1co: -monthly-budget must be a positive number of LXC\n%s", n1coUsage)
	}
	money := defaultTestMoneyULXC
	if *testMoney >= 0 {
		money = int64(math.Round(*testMoney * 1e6))
	}
	// The same derivation and separator the app's sign-in uses (talyvor-suite apps/bff provisionIdentity).
	wsID := deriveWorkspaceID(*issuer + "\x00" + *subject)

	ws := workspace.New(pool)
	if err := ws.LoadAll(ctx); err != nil {
		return fmt.Errorf("n1co: workspaces: %w", err)
	}
	if _, existed := ws.GetWorkspace(wsID); !existed {
		if err := ws.RegisterWorkspace(ctx, workspace.Workspace{ID: wsID, Name: "N1CO"}); err != nil {
			return fmt.Errorf("n1co: create the workspace: %w", err)
		}
		fmt.Fprintf(out, "workspace %s (N1CO) created\n", wsID)
	}
	start, started, err := newWorkspaceStarterNamed(bank, N1COAgentName, func(r *economy.AgentRules) {
		r.MonthlyLimitULXC = int64(math.Round(*budget * 1e6))
	}, money)(ctx, wsID, wsID)
	if err != nil {
		return fmt.Errorf("n1co: %w", err)
	}
	if !started {
		fmt.Fprintf(out, "workspace %s already has its agent: nothing changed\n", wsID)
		return nil
	}
	lxc := func(ulxc int64) string { return fmt.Sprintf("%g LXC", float64(ulxc)/1e6) }
	fmt.Fprintf(out, "agent %s (%s): monthly budget %s, approval above %s, test money %s\nits key, shown this once: %s\n",
		start.Agent.ID, start.Agent.Name, lxc(start.Rules.MonthlyLimitULXC), lxc(start.Rules.ApprovalAboveULXC), lxc(start.TestMoneyULXC), start.Key)
	return nil
}
