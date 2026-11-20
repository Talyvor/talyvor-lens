package main

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/phonesignin"
	"github.com/talyvor/lens/internal/stepup"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// B61.30 — the operator's number move, mounted as main.go mounts it, on a migrated schema: refused without step-up,
// without the identity check passing again, and for another person; its audit row names the operator; and for a day
// the person's account cannot move money, read on the ledger.

func b6130DB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG number move test")
	}
	const schema = "cmd_b6130_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// b6130FailingKYC is a provider that fails every check.
type b6130FailingKYC struct{}

func (b6130FailingKYC) Name() string { return "test" }
func (b6130FailingKYC) StartCheck(_ context.Context, req partners.KYCRequest) (partners.Result, error) {
	return partners.Result{Ref: req.ID, Status: partners.StatusFailed, Detail: "not the person"}, nil
}
func (b6130FailingKYC) CheckResult(_ context.Context, ref string) (partners.Result, error) {
	return partners.Result{Ref: ref, Status: partners.StatusFailed}, nil
}

func TestB6130_TheOperatorMovesANumberOnlyWithStepUpAndThePersonsIdentity(t *testing.T) {
	ctx := context.Background()
	pool := b6130DB(t)
	bank, _ := b596Bank(t, pool, "whsec_b6130")
	var provider partners.KYCProvider = &partners.TestKYCProvider{}
	verifiers := func(string, string, bool) partners.KYCProvider { return provider }
	people := &phonesignin.Store{DB: pool, Sender: partners.NewRegistry(nil).TextMessages, Key: []byte("b6130"), PerAddressHour: 100,
		PasscodeSecret: []byte("passcode-secret-for-tests-0123456789")}
	stepKey := []byte("0123456789abcdef0123")
	stepUp, err := stepup.New(base32.StdEncoding.EncodeToString(stepKey), stepup.NewPostgresClaims(pool))
	if err != nil {
		t.Fatal(err)
	}
	audit := operatoraudit.NewStore(pool)
	r := chi.NewRouter()
	r.Post("/v1/admin/people/move-number", requireAdminStepUp(headerAdmin{}, stepUp, audit, "person.move_number",
		newNumberMoveHandler(people, workspace.New(pool), bank, verifiers)))
	told := make(chan []string, 1)
	tellInBackground = func(tell func(ctx context.Context)) { tell(ctx); told <- nil }
	t.Cleanup(func() {
		tellInBackground = func(tell func(ctx context.Context)) { go tell(context.Background()) }
	})

	// Two people, each with an account and an identity check on record.
	join := func(phone, identity, name, dob string) (phonesignin.Person, string) {
		t.Helper()
		st, err := people.Start(ctx, phone, "203.0.113.60")
		if err != nil {
			t.Fatal(err)
		}
		fin, err := people.Finish(ctx, phone, st.SandboxCode, "203.0.113.60")
		if err != nil {
			t.Fatal(err)
		}
		p, _, err := people.RedeemWithPasscode(ctx, fin.Ticket, identity, "583920", "203.0.113.60")
		if err != nil {
			t.Fatal(err)
		}
		ws := deriveWorkspaceID(identity)
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type) VALUES ($1, $2, $1, 'personal')`, ws, name); err != nil {
			t.Fatal(err)
		}
		for _, in := range []economy.VerificationRequest{
			{Level: economy.LevelContact, Email: "person@example.com", Phone: phone},
			{Level: economy.LevelIdentity, Name: name, Country: "GB", DateOfBirth: dob},
		} {
			if c, err := bank.StartVerification(ctx, verifiers, ws, "owner", in); err != nil || c.Status != "completed" {
				t.Fatalf("%s check for %s: %+v %v", in.Level, name, c, err)
			}
		}
		return p, ws
	}
	const lost, fresh = "+447700900601", "+447700900602"
	alice, aliceWS := join(lost, "b6130-alice", "Alice Example", "1990-04-01")
	join("+447700900603", "b6130-bob", "Bob Example", "1985-11-30")

	// Alice's account holds money, and pays out today.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name, owner_user_id) VALUES ('agt_b6130', $1, 'buyer', 'usr_1')`,
		aliceWS); err != nil {
		t.Fatal(err)
	}
	var agentAcct, partner string
	for _, a := range []struct {
		into           *string
		purpose, agent string
	}{{&agentAcct, economy.MoneyAgent, "agt_b6130"}, {&partner, economy.MoneyPartner, ""}} {
		acct, err := bank.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: aliceWS, AgentID: a.agent, Currency: economy.CurrencyUSD,
			Purpose: a.purpose, Name: a.purpose})
		if err != nil {
			t.Fatal(err)
		}
		*a.into = acct.ID
	}
	pay := func(key, kind, capability, party, from, to string, cents int64) error {
		_, err := bank.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: aliceWS, Capability: capability, Kind: kind, IdempotencyKey: key,
			Funding: economy.FundingTest, Counterparty: party, Postings: []economy.MoneyPosting{{AccountID: from, AmountMinor: -cents},
				{AccountID: to, AmountMinor: cents}}})
		return err
	}
	postings := func(key string) (n int) {
		if err := pool.QueryRow(ctx, `SELECT count(p.entry_id) FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.idempotency_key = $2`, aliceWS, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := pay("in-1", "payment_in", economy.CapabilityPaymentsIn, "Ada Lovelace", partner, agentAcct, 50_000); err != nil {
		t.Fatal(err)
	}
	if err := pay("before", "payment_out", economy.CapabilityPaymentsOut, "Bob's Supplies", agentAcct, partner, 1_000); err != nil || postings("before") != 2 {
		t.Fatalf("a payment out before the move: %v, %d postings", err, postings("before"))
	}

	// freshCode is a code nobody has used: one code is one action, so the test forgets the codes used before, as an
	// operator waits for the next one.
	freshCode := func() string {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM operator_step_up_codes`); err != nil {
			t.Fatal(err)
		}
		return stepup.CodeAt(stepKey, time.Now())
	}
	move := func(operator string, code string, body map[string]string) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/people/move-number", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer admin-key")
		req.Header.Set(moderatorOperatorHeader, operator)
		req.Header.Set(stepUpHeader, code)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	aliceAsks := map[string]string{"phone": lost, "new_phone": fresh, "name": "alice  example", "date_of_birth": "1990-04-01", "country": "GB"}
	unmoved := func(when string) {
		t.Helper()
		if p, ok, err := people.PersonByPhone(ctx, lost); err != nil || !ok || p.ID != alice.ID {
			t.Fatalf("%s: the lost number is held by %+v (%v %v); want Alice still", when, p, ok, err)
		}
	}
	checks := func() (n int) {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_verifications WHERE workspace_id = $1`, aliceWS).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := checks()

	// Without step-up: refused, and nothing is asked or moved.
	if code, out := move("nicolai", "", aliceAsks); code != http.StatusForbidden {
		t.Fatalf("without step-up: %d %v, want 403", code, out)
	}
	unmoved("without step-up")

	// For another person — Bob's name and date of birth for Alice's number: refused before any provider is asked.
	bobAsks := map[string]string{"phone": lost, "new_phone": fresh, "name": "Bob Example", "date_of_birth": "1985-11-30", "country": "GB"}
	if code, out := move("mihai", freshCode(), bobAsks); code != http.StatusForbidden {
		t.Fatalf("another person's identity: %d %v, want 403", code, out)
	}
	unmoved("for another person")
	// Alice's name with the wrong date of birth: the same.
	wrongDOB := map[string]string{"phone": lost, "new_phone": fresh, "name": "Alice Example", "date_of_birth": "1990-04-02", "country": "GB"}
	if code, out := move("ops-3", freshCode(), wrongDOB); code != http.StatusForbidden {
		t.Fatalf("the wrong date of birth: %d %v, want 403", code, out)
	}
	unmoved("with the wrong date of birth")
	if got := checks(); got != before {
		t.Fatalf("refused moves wrote %d identity checks into Alice's account; want none", got-before)
	}

	// Without a passed identity check: the provider fails the check asked again.
	provider = b6130FailingKYC{}
	if code, out := move("ops-4", freshCode(), aliceAsks); code != http.StatusForbidden ||
		!strings.Contains(out["error"].(string), "did not pass") {
		t.Fatalf("an identity check that does not pass again: %d %v, want 403", code, out)
	}
	unmoved("when the identity check did not pass")
	provider = &partners.TestKYCProvider{}

	// With step-up and Alice's own name, date of birth and country: moved, the audit row names the operator.
	code, out := move("nicolai", freshCode(), aliceAsks)
	if code != http.StatusOK || out["new_phone"] != fresh || out["accounts_held"] != float64(1) {
		t.Fatalf("the move: %d %v", code, out)
	}
	<-told
	if p, ok, _ := people.PersonByPhone(ctx, fresh); !ok || p.ID != alice.ID {
		t.Fatalf("the new number finds %+v, want Alice", p)
	}
	rows, err := audit.List(ctx, operatoraudit.Filter{Action: "person.move_number"})
	if err != nil {
		t.Fatal(err)
	}
	var named []string
	for _, e := range rows {
		named = append(named, e.Actor)
	}
	if len(rows) != 4 || !strings.Contains(strings.Join(named, ","), "nicolai") || rows[0].Target != "POST /v1/admin/people/move-number" {
		t.Fatalf("operator audit rows = %+v; want one for each use behind step-up, the last naming nicolai", rows)
	}
	var moveOperator string
	if err := pool.QueryRow(ctx, `SELECT operator FROM person_number_moves WHERE person_id = $1`, alice.ID).Scan(&moveOperator); err != nil ||
		moveOperator != "nicolai" {
		t.Fatalf("the move's row names %q (%v), want nicolai", moveOperator, err)
	}

	// For a day the account can look and cannot move money: no posting on the ledger, Talyvor's own services go on.
	err = pay("held", "payment_out", economy.CapabilityPaymentsOut, "Bob's Supplies", agentAcct, partner, 1_000)
	if !errors.Is(err, economy.ErrNumberMoveHold) || !strings.Contains(err.Error(), "phone number was changed") {
		t.Fatalf("a payment out during the day's hold: %v; want ErrNumberMoveHold", err)
	}
	if n := postings("held"); n != 0 {
		t.Fatalf("the held payment wrote %d postings; want none", n)
	}
	if _, err := bank.WorkspaceAccount(ctx, aliceWS); err != nil {
		t.Fatalf("reading the account during the hold: %v", err)
	}

	// After the day it moves again.
	if _, err := pool.Exec(ctx, `UPDATE number_move_holds SET held_until = now() - interval '1 second' WHERE workspace_id = $1`, aliceWS); err != nil {
		t.Fatal(err)
	}
	if err := pay("after", "payment_out", economy.CapabilityPaymentsOut, "Bob's Supplies", agentAcct, partner, 1_000); err != nil || postings("after") != 2 {
		t.Fatalf("a payment out after the day: %v, %d postings", err, postings("after"))
	}
}
