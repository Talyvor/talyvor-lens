package monitoring_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/monitoring"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/testschema"
	"github.com/talyvor/lens/migrations"
)

// B30.7 — DONE: each rule opens a case from a constructed history and stays quiet on a normal one. The history is
// written into the money ledger as it was, then the last payment is posted through PostMoney, whose monitor judges it.

func monitoringPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_monitoring_%d", time.Now().UnixNano())
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

// book is one workspace's money in dollars: the company's, an agent's, and the partner account money comes in and
// goes out through.
type book struct {
	t                       *testing.T
	pool                    *pgxpool.Pool
	money                   *economy.DualTokenStore
	monitor                 *monitoring.Monitor
	ws, agent               string
	company, agentAcct, prt string
}

func newBook(t *testing.T, pool *pgxpool.Pool, approvalUSD int64) *book {
	t.Helper()
	ctx := context.Background()
	b := &book{t: t, pool: pool, money: economy.NewDualTokenStore(nil, pool, nil), ws: "ws-" + uuid.NewString()[:8],
		agent: "agt_" + uuid.NewString()}
	b.monitor = monitoring.New(pool, monitoring.Defaults(), nil)
	b.money.SetScreener(clearScreener{})
	b.money.SetMonitor(b.monitor)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name, owner_user_id) VALUES ($1, $2, 'buyer', 'usr_1')`,
		b.agent, b.ws); err != nil {
		t.Fatal(err)
	}
	if approvalUSD > 0 {
		if _, err := pool.Exec(ctx, `INSERT INTO agent_rules (agent_id, workspace_id, approval_above_ulxc) VALUES ($1, $2, $3)`,
			b.agent, b.ws, approvalUSD*1_000_000*economy.ULXCPerUSDMicro); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []struct {
		into    *string
		purpose string
		agent   string
	}{{&b.company, economy.MoneyCompany, ""}, {&b.agentAcct, economy.MoneyAgent, b.agent}, {&b.prt, economy.MoneyPartner, ""}} {
		acct, err := b.money.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: b.ws, AgentID: a.agent, Currency: economy.CurrencyUSD,
			Purpose: a.purpose, Name: a.purpose})
		if err != nil {
			t.Fatal(err)
		}
		*a.into = acct.ID
	}
	return b
}

// step is one payment: in to the company, or out of the company's account or the agent's.
type step struct {
	ago   time.Duration
	in    bool
	agent bool
	party string
	cents int64
}

// legs is a step's postings: money out puts it into the partner account, money in takes it out.
func (b *book) legs(s step) (from, to string) {
	own := b.company
	if s.agent {
		own = b.agentAcct
	}
	if s.in {
		return b.prt, own
	}
	return own, b.prt
}

// wrote writes s into the ledger as it was, s.ago before now: history the monitor did not see happen.
func (b *book) wrote(s step) string {
	b.t.Helper()
	ctx := context.Background()
	id, kind := "mle_"+uuid.NewString(), "payment_out"
	capability := economy.CapabilityPaymentsOut
	if s.in {
		kind, capability = "payment_in", economy.CapabilityPaymentsIn
	}
	from, to := b.legs(s)
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO money_entries (id, workspace_id, capability, kind, idempotency_key, counterparty, created_at)
		VALUES ($1, $2, $3, $4, $1, $5, now() - $6::interval)`, id, b.ws, capability, kind, s.party, fmt.Sprintf("%d seconds", int(s.ago.Seconds()))); err != nil {
		b.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO money_postings (entry_id, line, account_id, amount_minor, currency, funding)
		VALUES ($1, 1, $2, $3, 'USD', 'test'), ($1, 2, $4, $5, 'USD', 'test')`, id, from, -s.cents, to, s.cents); err != nil {
		b.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		b.t.Fatal(err)
	}
	return id
}

// paid posts s now through PostMoney, whose monitor judges it.
func (b *book) paid(s step) economy.MoneyEntry {
	b.t.Helper()
	from, to := b.legs(s)
	kind, capability := "payment_out", economy.CapabilityPaymentsOut
	if s.in {
		kind, capability = "payment_in", economy.CapabilityPaymentsIn
	}
	e, err := b.money.PostMoney(context.Background(), economy.MoneyEntry{WorkspaceID: b.ws, Capability: capability, Kind: kind,
		IdempotencyKey: "now-" + uuid.NewString(), Funding: economy.FundingTest, Counterparty: s.party,
		Postings: []economy.MoneyPosting{{AccountID: from, AmountMinor: -s.cents}, {AccountID: to, AmountMinor: s.cents}}})
	if err != nil {
		b.t.Fatalf("the payment did not move: %v", err)
	}
	return e
}

// cases is the workspace's monitoring cases, by status.
func (b *book) cases() map[string]int {
	b.t.Helper()
	rows, err := b.pool.Query(context.Background(), `SELECT status, count(*) FROM compliance_cases WHERE workspace_id = $1 AND kind = 'monitoring'
		GROUP BY status`, b.ws)
	if err != nil {
		b.t.Fatal(err)
	}
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			b.t.Fatal(err)
		}
		out[s] = n
	}
	if err := rows.Err(); err != nil {
		b.t.Fatal(err)
	}
	return out
}

// alerts is the workspace's alerts, oldest first.
func (b *book) alerts() []monitoring.Alert {
	b.t.Helper()
	all, err := b.monitor.Alerts(context.Background(), "", 500)
	if err != nil {
		b.t.Fatal(err)
	}
	var out []monitoring.Alert
	for _, a := range all {
		if a.WorkspaceID == b.ws {
			out = append(out, a)
		}
	}
	slices.Reverse(out)
	return out
}

// entries is how many money entries the workspace has: the payments made, and nothing a hit moved.
func (b *book) entries() int {
	b.t.Helper()
	var n int
	if err := b.pool.QueryRow(context.Background(), `SELECT count(*) FROM money_entries WHERE workspace_id = $1`, b.ws).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

func TestEachRule_OpensACaseFromItsPatternAndStaysQuietOnANormalHistory(t *testing.T) {
	pool := monitoringPool(t)
	const approvalUSD = 500
	for _, c := range []struct {
		rule          string
		pattern, calm []step // the history before, then the payment posted now
	}{
		{rule: "under_approval",
			pattern: []step{{ago: 5 * time.Hour, agent: true, party: "Supplier A", cents: 480_00}, {ago: 2 * time.Hour, agent: true, party: "Supplier B", cents: 490_00},
				{agent: true, party: "Supplier C", cents: 495_00}},
			calm: []step{{ago: 5 * time.Hour, agent: true, party: "Supplier A", cents: 300_00}, {ago: 2 * time.Hour, agent: true, party: "Supplier B", cents: 490_00},
				{agent: true, party: "Supplier C", cents: 495_00}}},
		{rule: "in_and_out",
			pattern: []step{{ago: 30 * time.Minute, in: true, party: "Customer X", cents: 1_000_00}, {party: "Supplier Y", cents: 600_00}},
			calm:    []step{{ago: 3 * time.Hour, in: true, party: "Customer X", cents: 1_000_00}, {party: "Supplier Y", cents: 600_00}}},
		{rule: "new_payee_large",
			pattern: []step{{ago: 72 * time.Hour, agent: true, party: "Regular Co", cents: 100_00}, {ago: 48 * time.Hour, agent: true, party: "Regular Co", cents: 100_00},
				{ago: 26 * time.Hour, agent: true, party: "Regular Co", cents: 100_00}, {agent: true, party: "Brand New Ltd", cents: 1_000_00}},
			calm: []step{{ago: 72 * time.Hour, agent: true, party: "Regular Co", cents: 100_00}, {ago: 48 * time.Hour, agent: true, party: "Regular Co", cents: 100_00},
				{ago: 26 * time.Hour, agent: true, party: "Regular Co", cents: 100_00}, {agent: true, party: "Brand New Ltd", cents: 150_00}}},
		{rule: "round_amounts",
			pattern: []step{{ago: 40 * time.Minute, party: "P1", cents: 200_00}, {ago: 20 * time.Minute, party: "P2", cents: 500_00}, {party: "P3", cents: 1_000_00}},
			calm:    []step{{ago: 40 * time.Minute, party: "P1", cents: 200_00}, {ago: 20 * time.Minute, party: "P2", cents: 500_00}, {party: "P3", cents: 1_234_56}}},
		{rule: "new_payees",
			pattern: []step{{ago: 20 * time.Hour, party: "N1", cents: 12_34}, {ago: 15 * time.Hour, party: "N2", cents: 23_45},
				{ago: 10 * time.Hour, party: "N3", cents: 34_56}, {ago: 5 * time.Hour, party: "N4", cents: 45_67}, {party: "N5", cents: 56_78}},
			calm: []step{{ago: 30 * 24 * time.Hour, party: "N1", cents: 12_34}, {ago: 29 * 24 * time.Hour, party: "N2", cents: 23_45},
				{ago: 28 * 24 * time.Hour, party: "N3", cents: 34_56}, {ago: 27 * 24 * time.Hour, party: "N4", cents: 45_67},
				{ago: 20 * time.Hour, party: "N1", cents: 12_34}, {ago: 15 * time.Hour, party: "N2", cents: 23_45},
				{ago: 10 * time.Hour, party: "N3", cents: 34_56}, {ago: 5 * time.Hour, party: "N4", cents: 45_67}, {party: "N5", cents: 56_78}}},
	} {
		t.Run(c.rule, func(t *testing.T) {
			b := newBook(t, pool, approvalUSD)
			for _, s := range c.pattern[:len(c.pattern)-1] {
				b.wrote(s)
			}
			last := b.paid(c.pattern[len(c.pattern)-1])
			if got := b.cases(); got[monitoring.CaseOpen] != 1 || len(got) != 1 {
				t.Fatalf("the pattern left monitoring cases %v; want one open", got)
			}
			alerts := b.alerts()
			var rules []string
			for _, a := range alerts {
				rules = append(rules, a.Rule)
			}
			if len(alerts) != 1 || alerts[0].Rule != c.rule || alerts[0].EntryID != last.ID || alerts[0].RaisedBy != monitoring.RaisedByMovement ||
				alerts[0].Summary == "" || !slices.Contains(alerts[0].Entries, last.ID) {
				t.Fatalf("the pattern raised %v (%+v); want one %s alert on the payment just posted, %s", rules, alerts, c.rule, last.ID)
			}
			if got := b.entries(); got != len(c.pattern) {
				t.Fatalf("the workspace has %d money entries; want the %d payments made — a hit moves no money", got, len(c.pattern))
			}

			calm := newBook(t, pool, approvalUSD)
			for _, s := range c.calm[:len(c.calm)-1] {
				calm.wrote(s)
			}
			calm.paid(c.calm[len(c.calm)-1])
			if got, alerts := calm.cases(), calm.alerts(); len(got) != 0 || len(alerts) != 0 {
				t.Fatalf("a normal history opened cases %v with alerts %+v; want none", got, alerts)
			}
		})
	}
}

func TestAHit_ExtendsTheOpenCase_AndTheNightlyRunAddsOnlyWhatWasMissed(t *testing.T) {
	pool := monitoringPool(t)
	b := newBook(t, pool, 0)
	ctx := context.Background()
	// Three round payments the monitor never saw: the nightly run finds the burst.
	for i, ago := range []time.Duration{50 * time.Minute, 40 * time.Minute, 30 * time.Minute} {
		b.wrote(step{ago: ago, party: fmt.Sprintf("R%d", i), cents: 300_00})
	}
	if n, err := b.monitor.Sweep(ctx, time.Now().Add(-25*time.Hour), time.Now()); err != nil || n != 1 {
		t.Fatalf("the nightly run raised %d, %v; want the one burst it missed", n, err)
	}
	// A fourth, posted now, extends the case the nightly run opened.
	b.paid(step{party: "R3", cents: 400_00})
	// The nightly run again: everything is judged already.
	if n, err := b.monitor.Sweep(ctx, time.Now().Add(-25*time.Hour), time.Now().Add(time.Second)); err != nil || n != 0 {
		t.Fatalf("a second nightly run raised %d, %v; want nothing new", n, err)
	}
	if got := b.cases(); got[monitoring.CaseOpen] != 1 || len(got) != 1 {
		t.Fatalf("monitoring cases %v; want the one open case, extended", got)
	}
	alerts := b.alerts()
	var by []string
	for _, a := range alerts {
		by = append(by, a.Rule+"/"+a.RaisedBy)
		if a.CaseID != alerts[0].CaseID {
			t.Fatalf("alerts on two cases: %+v", alerts)
		}
	}
	sort.Strings(by)
	if want := []string{"round_amounts/movement", "round_amounts/nightly"}; !slices.Equal(by, want) {
		t.Fatalf("alerts %v; want %v", by, want)
	}
}
