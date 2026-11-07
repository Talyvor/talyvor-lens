package market

import (
	"context"
	"errors"
	"testing"
)

// B54.30 — a playbook run whose bind fails (the process dying between the use and the bind) bills nothing and binds
// nothing, and asking again runs it, bound and billed once; a negotiation past its deadline runs no playbook.
func TestB5430_PlaybookRunBindsOrBillsNothing(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const creator, buyer = "ws-b5430-creator", "ws-b5430-buyer"
	l, err := s.Publish(ctx, creator, Draft{Kind: KindCounterPlaybook, Title: "Steady", Visibility: "public", PricePerUseULXC: 10_000_000,
		Artifact: b5216Playbook(3, "Thanks. For {what} we can do {offer}.")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO negotiation_mandates (id, workspace_id, deadline, canonical, credential_id, signature, created_by)
		VALUES ('neg_b5430', $1, now() + interval '7 days', '{"task": {"kind": "renewal", "what": "Acme CRM"}}', 'cred', 'sig', 'owner'),
		       ('neg_b5430_late', $1, now() - interval '1 minute', '{"task": {"kind": "renewal", "what": "Acme CRM"}}', 'cred', 'sig', 'owner')`, buyer); err != nil {
		t.Fatal(err)
	}
	meter := &b501Meter{}
	billedUses := func() (ran int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE listing_id = $1 AND ran_at IS NOT NULL`, l.ID).Scan(&ran); err != nil {
			t.Fatal(err)
		}
		return ran
	}

	if _, err := s.RunCounterPlaybook(ctx, UseDeps{Meter: meter}, buyer, "neg_b5430_late", l.ID, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a run on a negotiation past its deadline = %v; want it refused", err)
	}
	if n := billedUses(); n != 0 || len(meter.accepted) != 0 {
		t.Fatalf("after the refused run: %d uses ran, metered %v; want nothing", n, meter.accepted)
	}

	// The bind fails as a crash would leave it: no run row ever gets its use.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION b5430_die() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'b5430: the process died'; END $$;
		CREATE TRIGGER b5430_die BEFORE INSERT OR UPDATE ON negotiation_playbook_runs FOR EACH ROW WHEN (NEW.use_id IS NOT NULL) EXECUTE FUNCTION b5430_die()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunCounterPlaybook(ctx, UseDeps{Meter: meter}, buyer, "neg_b5430", l.ID, nil); err == nil {
		t.Fatal("a run whose bind failed succeeded; want the error")
	}
	if n := billedUses(); n != 0 || len(meter.accepted) != 0 {
		t.Fatalf("after a failed bind: %d uses ran, metered %v; want the buyer billed nothing", n, meter.accepted)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER b5430_die ON negotiation_playbook_runs`); err != nil {
		t.Fatal(err)
	}

	run, err := s.RunCounterPlaybook(ctx, UseDeps{Meter: meter}, buyer, "neg_b5430", l.ID, nil)
	if err != nil || run.Again || run.Charge != ChargeBilled || run.UseID == "" {
		t.Fatalf("asking again after the failed bind = %+v, %v; want the run made now and billed", run, err)
	}
	var bound string
	if err := pool.QueryRow(ctx, `SELECT use_id FROM negotiation_playbook_runs WHERE mandate_id = 'neg_b5430' AND playbook IS NOT NULL`).Scan(&bound); err != nil || bound != run.UseID {
		t.Fatalf("the negotiation's bound use = %q (%v); want %s", bound, err, run.UseID)
	}
	if n := billedUses(); n != 1 || len(meter.accepted) != 1 || meter.accepted[0] != run.UseID {
		t.Fatalf("%d uses ran, metered %v; want the one bound use billed once", n, meter.accepted)
	}
}
