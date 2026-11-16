package compliance_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/compliance"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/monitoring"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/testschema"
	"github.com/talyvor/lens/migrations"
)

// B30.8 — DONE: freezing a workspace makes a test payment out fail naming the freeze while Talyvor-services spend
// still succeeds; every case action has an audit row.

func casePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_casefile_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	// B61.52: the accounts these tests move money for have confirmed who they are.
	if _, err := mc.Exec(ctx, testschema.ConfirmedAccountsSQL); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

// clearScreener finds no one on a sanctions list: these payments are about the pattern, not the payee.
type clearScreener struct{}

func (clearScreener) ScreenPayment(context.Context, screening.Payment) error { return nil }

func TestFreezeStopsMoneyNotTalyvorServicesAndEveryActionIsAudited(t *testing.T) {
	ctx := context.Background()
	pool := casePool(t)
	money := economy.NewDualTokenStore(nil, pool, nil)
	monitor := monitoring.New(pool, monitoring.Defaults(), nil)
	money.SetScreener(clearScreener{})
	money.SetMonitor(monitor)
	files := compliance.New(pool, monitor)
	const ws, agent, op = "ws-b308", "agt_b308", "nicolai"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, 'Acme Agents Ltd', 'b308')`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name, owner_user_id) VALUES ($1, $2, 'buyer', 'usr_1')`,
		agent, ws); err != nil {
		t.Fatal(err)
	}
	var agentAcct, partner string
	for _, a := range []struct {
		into           *string
		purpose, agent string
	}{{&agentAcct, economy.MoneyAgent, agent}, {&partner, economy.MoneyPartner, ""}} {
		acct, err := money.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: ws, AgentID: a.agent, Currency: economy.CurrencyUSD,
			Purpose: a.purpose, Name: a.purpose})
		if err != nil {
			t.Fatal(err)
		}
		*a.into = acct.ID
	}
	pay := func(key, kind, capability, party string, from, to string, cents int64) error {
		_, err := money.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: capability, Kind: kind, IdempotencyKey: key,
			Funding: economy.FundingTest, Counterparty: party, Postings: []economy.MoneyPosting{{AccountID: from, AmountMinor: -cents},
				{AccountID: to, AmountMinor: cents}}})
		return err
	}
	payOut := func(key string, cents int64) error {
		return pay(key, "payment_out", economy.CapabilityPaymentsOut, "Bob's Supplies", agentAcct, partner, cents)
	}
	postings := func(key string) (n int) {
		if err := pool.QueryRow(ctx, `SELECT count(p.entry_id) FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.idempotency_key = $2`, ws, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// $1,000 in, then three payments of round hundreds out within minutes: transaction monitoring opens a case.
	if err := pay("in-1", "payment_in", economy.CapabilityPaymentsIn, "Ada Lovelace", partner, agentAcct, 100_000); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := payOut(fmt.Sprintf("round-%d", i), int64(i)*10_000); err != nil {
			t.Fatalf("round payment %d: %v", i, err)
		}
	}
	cases, err := files.Cases(ctx, compliance.Filter{WorkspaceID: ws, Kind: monitoring.CaseKind}, 10)
	if err != nil || len(cases) != 1 || cases[0].Status != monitoring.CaseOpen {
		t.Fatalf("monitoring cases = %+v, %v; want one open case", cases, err)
	}
	id := cases[0].ID
	f, err := files.File(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.Owner.Name != "Acme Agents Ltd" || len(f.Alerts) == 0 || len(f.Agents) != 1 || f.Agents[0].Name != "buyer" || f.Freeze != nil {
		t.Fatalf("case file = owner %+v, %d alerts, agents %+v, freeze %+v", f.Owner, len(f.Alerts), f.Agents, f.Freeze)
	}

	// Review and freeze.
	if err := files.AddNote(ctx, id, op, "Three round payments to one supplier within minutes; asking the owner."); err != nil {
		t.Fatal(err)
	}
	if _, err := files.FreezeWorkspace(ctx, id, op, "Pending the owner's explanation"); err != nil {
		t.Fatal(err)
	}

	// Frozen: a test payment out fails, naming the freeze, and posts nothing.
	err = payOut("frozen-1", 5_000)
	if !errors.Is(err, economy.ErrWorkspaceFrozen) || !strings.Contains(err.Error(), "frozen") || !strings.Contains(err.Error(), id) {
		t.Fatalf("a test payment out of a frozen workspace = %v; want ErrWorkspaceFrozen naming case %s", err, id)
	}
	if n := postings("frozen-1"); n != 0 {
		t.Fatalf("the refused payment wrote %d postings", n)
	}
	// Talyvor's own services still work: an AI call's spend of credits is on the ledger.
	if _, err := money.GrantLXC(ctx, ws, 5_000_000, "starting credits", nil); err != nil {
		t.Fatal(err)
	}
	if err := money.SpendLXC(ctx, ws, 1_000_000, "AI call while frozen"); err != nil {
		t.Fatalf("a Talyvor-services spend while frozen: %v", err)
	}
	var spends int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'spend'
		AND description = 'AI call while frozen'`, ws).Scan(&spends); err != nil || spends != 1 {
		t.Fatalf("lxc_ledger spend rows = %d, %v; want 1", spends, err)
	}
	if _, err := files.FreezeWorkspace(ctx, id, op, "again"); !errors.Is(err, compliance.ErrAlreadyFrozen) {
		t.Fatalf("a second freeze = %v; want ErrAlreadyFrozen", err)
	}

	// Decide: close the case, then lift the freeze; the money moves again.
	if c, err := files.Close(ctx, id, op, "The owner showed the supplier's monthly invoices"); err != nil || c.Status != compliance.CaseClosed {
		t.Fatalf("close = %+v, %v", c, err)
	}
	if err := files.UnfreezeWorkspace(ctx, id, op, "Explanation accepted"); err != nil {
		t.Fatal(err)
	}
	if err := payOut("after-1", 5_000); err != nil {
		t.Fatalf("a payment out after the unfreeze: %v", err)
	}
	if n := postings("after-1"); n != 2 {
		t.Fatalf("the payment after the unfreeze wrote %d postings; want 2", n)
	}

	// Export: a report draft with what a person needs to file it.
	report, err := files.Export(ctx, id, op)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DRAFT", id, "Acme Agents Ltd", "round_amounts", "asking the owner", "Not frozen.", "monthly invoices"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report draft does not say %q:\n%s", want, report)
		}
	}

	// Every action has its audit row, naming who, on this case.
	rows, err := pool.Query(ctx, `SELECT action, actor FROM operator_audit WHERE target = $1 ORDER BY id`, screening.AuditTarget(id))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var action, actor string
		if err := rows.Scan(&action, &actor); err != nil {
			t.Fatal(err)
		}
		if actor != op {
			t.Errorf("%s was recorded as %q; want %q", action, actor, op)
		}
		got = append(got, action)
	}
	want := []string{compliance.ActionNote, compliance.ActionFreeze, compliance.ActionClose, compliance.ActionUnfreeze, compliance.ActionExport}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit rows = %v; want %v", got, want)
	}
	// The timeline reads them back in order, after the case opened and its alerts.
	f, err = files.File(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range f.Timeline {
		if len(kinds) == 0 || kinds[len(kinds)-1] != e.Kind {
			kinds = append(kinds, e.Kind)
		}
	}
	if strings.Join(kinds, ",") != "opened,alert,note,freeze,close,unfreeze,export" || len(f.Notes) != 1 {
		t.Fatalf("timeline = %v, notes %+v", kinds, f.Notes)
	}
}

// A held screening case's release is a case action too: it has its audit row.
func TestScreeningDecisionIsAudited(t *testing.T) {
	ctx := context.Background()
	pool := casePool(t)
	id := "cc_" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO compliance_cases (id, workspace_id, kind, subject_kind, subject_id, name, outcome, status)
		VALUES ($1, 'ws-b308', 'screening', 'payee', 'pye_1', 'Jon Smyth', 'review', 'held')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := screening.NewScreener(pool, nil).Decide(ctx, id, true, "nicolai", "a different person"); err != nil {
		t.Fatal(err)
	}
	var action, detail string
	if err := pool.QueryRow(ctx, `SELECT action, detail FROM operator_audit WHERE target = $1`, screening.AuditTarget(id)).
		Scan(&action, &detail); err != nil || action != compliance.ActionRelease || detail != "a different person" {
		t.Fatalf("audit row = %q %q, %v", action, detail, err)
	}
}

// earnVerified reads workspaces.earn_verified, the operator's verification of a workspace's people.
type earnVerified struct{}

func (earnVerified) MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT earn_verified FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&ok)
	return ok, err
}

// A freeze is not stepped round through the owner's second workspace: credits between one owner's agents are
// GREEN, but none leave or enter the frozen workspace, either way.
func TestFreezeHoldsAcrossTheOwnersOtherWorkspace(t *testing.T) {
	ctx := context.Background()
	pool := casePool(t)
	money := economy.NewDualTokenStore(nil, pool, nil)
	money.SetOwnerVerifier(earnVerified{})
	const frozen, other, lxc = "ws-acme", "ws-acme-2", int64(1_000_000)
	agents := map[string]economy.Agent{}
	for _, ws := range []string{frozen, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
		a, err := money.CreateAgent(ctx, ws, "agent", "user-acme")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := money.CreditLXC(ctx, ws, 10*lxc, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
			t.Fatal(err)
		}
		if _, err := money.FundAgent(ctx, ws, a.ID, 10*lxc); err != nil {
			t.Fatal(err)
		}
		agents[ws] = a
	}
	id := "cc_" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO compliance_cases (id, workspace_id, kind, subject_kind, subject_id, name, outcome, status)
		VALUES ($1, $2, 'monitoring', 'workspace', $2, 'transaction monitoring', 'alert', 'open')`, id, frozen); err != nil {
		t.Fatal(err)
	}
	if _, err := compliance.New(pool, nil).FreezeWorkspace(ctx, id, "nicolai", "pending review"); err != nil {
		t.Fatal(err)
	}
	if _, err := money.SendCredits(ctx, frozen, agents[frozen].ID, agents[other].ID, lxc, "out"); !errors.Is(err, economy.ErrWorkspaceFrozen) {
		t.Fatalf("credits out of the frozen workspace to the owner's other one = %v; want ErrWorkspaceFrozen", err)
	}
	if _, err := money.SendCredits(ctx, other, agents[other].ID, agents[frozen].ID, lxc, "in"); !errors.Is(err, economy.ErrWorkspaceFrozen) {
		t.Fatalf("credits into the frozen workspace from the owner's other one = %v; want ErrWorkspaceFrozen", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_transfers`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("agent_transfers rows = %d, %v; want none", n, err)
	}
}
