package dbmigrate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/migrations"
)

// B39.6: on the real schema, Lens's app role can write the money ledger but can neither rewrite it
// nor switch its trigger off, and the retention role can delete token_events and nothing else.
func TestApplyRoles_AppRoleCannotRewriteLedgerOrDisableItsTrigger(t *testing.T) {
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping DB integration test")
	}
	ctx := context.Background()
	// A database of its own: the full migration chain, and roles granted on its public schema.
	name := fmt.Sprintf("lens_b396_roles_%d", os.Getpid())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ac.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = ac.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = ac.Close(context.Background())
	})
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	q := u.Query()
	q.Del("options") // testschema's search_path names a schema this database does not have
	u.RawQuery = q.Encode()
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := ApplyRoles(ctx, conn, "", ""); err != nil {
		t.Fatalf("ApplyRoles: %v", err)
	}
	if err := ApplyRoles(ctx, conn, "", ""); err != nil {
		t.Fatalf("ApplyRoles must be re-runnable: %v", err)
	}

	as := func(role, sql string) error {
		t.Helper()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+role); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql)
		return err
	}
	denied := func(role, sql string) {
		t.Helper()
		var pg *pgconn.PgError
		if err := as(role, sql); !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("as %s, %q: got %v, want SQLSTATE 42501 (insufficient privilege)", role, sql, err)
		} else {
			t.Logf("as %s, %q: %s", role, sql, pg.Message)
		}
	}
	allowed := func(role, sql string) {
		t.Helper()
		if err := as(role, sql); err != nil {
			t.Errorf("as %s, %q: %v", role, sql, err)
		}
	}

	denied(AppRole, `UPDATE money_postings SET amount_minor = 1 WHERE false`)
	denied(AppRole, `DELETE FROM money_postings WHERE false`)
	denied(AppRole, `TRUNCATE money_postings`)
	denied(AppRole, `ALTER TABLE money_postings DISABLE TRIGGER audit_no_mutation`)
	denied(AppRole, `DROP TRIGGER audit_no_mutation ON money_postings`)
	denied(AppRole, `UPDATE agent_postings SET amount_ulxc = 1 WHERE false`)
	denied(AppRole, `DELETE FROM token_events WHERE false`)
	// the retention flag is no bypass without the DELETE privilege
	denied(AppRole, `SET LOCAL lens.audit_retention = 'on'; DELETE FROM token_events WHERE false`)

	allowed(AppRole, `SELECT count(*) FROM money_postings`)
	allowed(AppRole, `INSERT INTO money_postings SELECT * FROM money_postings WHERE false`)
	allowed(AppRole, `UPDATE lxc_purchases SET id = id WHERE false`) // an unguarded table keeps UPDATE

	denied(AppRole, `DELETE FROM schema_migrations WHERE false`)

	allowed(RetentionRole, `SET LOCAL lens.audit_retention = 'on'; DELETE FROM token_events WHERE false`)
	denied(RetentionRole, `SELECT count(*) FROM money_postings`)
	denied(RetentionRole, `UPDATE token_events SET model = model WHERE false`)

	// A password gives the role a login; no password leaves the login alone.
	if err := ApplyRoles(ctx, conn, "s3cret'pw", ""); err != nil {
		t.Fatalf("ApplyRoles with a password: %v", err)
	}
	var appLogin, retLogin bool
	if err := conn.QueryRow(ctx, `SELECT
		(SELECT rolcanlogin FROM pg_roles WHERE rolname = 'lens_app'),
		(SELECT rolcanlogin FROM pg_roles WHERE rolname = 'lens_retention')`).Scan(&appLogin, &retLogin); err != nil {
		t.Fatal(err)
	}
	if !appLogin {
		t.Error("lens_app must be able to log in once its password is set")
	}
	var stored string
	if err := conn.QueryRow(ctx, `SELECT rolpassword FROM pg_authid WHERE rolname = 'lens_app'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "SCRAM-SHA-256$4096:") {
		t.Errorf("lens_app's stored password = %.20q…, want the SCRAM verifier migrate sent", stored)
	}
	if _, err := conn.Exec(ctx, `ALTER ROLE lens_app NOLOGIN PASSWORD NULL`); err != nil {
		t.Fatal(err)
	}
	if retLogin {
		t.Error("lens_retention was given no password and must keep no login")
	}
}
