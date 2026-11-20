package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// B61.52 — the accounts a test moves money for confirm who they are, as a person does once.

// confirmedAccounts records the Test provider's passes at L1, L2 and L3 for each ws: an account of either type has
// confirmed who it is, and its live money is judged as before, as Test passes count for nothing live. A test whose
// first money call opens an account needs it, as agentRoutesDB's fixture (testschema.ConfirmedAccountsSQL) waits for
// the first money an account holds.
func confirmedAccounts(t *testing.T, pool *pgxpool.Pool, workspaces ...string) {
	t.Helper()
	for _, ws := range workspaces {
		for level, subject := range map[int]string{1: "contact", 2: "person", 3: "company"} {
			if _, err := pool.Exec(context.Background(), `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref)
				VALUES ($1, $2, $3, 'test', 'completed', $4)`, ws, level, subject, fmt.Sprintf("test-%s-%d", ws, level)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// unconfirmedAccounts takes agentRoutesDB's fixture off: a test of what an account's own checks say starts at L0.
func unconfirmedAccounts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"agent_accounts", "lxc_balances", "money_accounts"} {
		if _, err := pool.Exec(context.Background(), `DROP TRIGGER test_confirmed_on_first_money ON `+table); err != nil {
			t.Fatal(err)
		}
	}
}
