package dbmigrate

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/migrations"
)

// B61.68 — with four builders on Lens, branches opened in the same hour took the same next NNNN number, and the
// second to merge would have stopped Lens at start. A new migration is named by the UTC minute it is written and
// the builder's letter (2611151820a_agent_notes.sql), so two branches cannot collide; the old NNNN files keep their
// names and their order.

// lastNumbered is the last bare-number migration on main when timestamp names came in. No migration is numbered
// after it.
const lastNumbered = "0398"

// bareNumbersAfter lists the migrations in fsys named with a bare number after lastNumbered.
func bareNumbersAfter(fsys fs.FS) ([]string, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		v, ok := versionPrefix(n)
		if ok && !isTimestamp(v) && v > lastNumbered {
			out = append(out, n)
		}
	}
	return out, nil
}

func TestMigrations_NewOnesAreNamedByTheMinute(t *testing.T) {
	bad, err := bareNumbersAfter(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range bad {
		t.Errorf("migrations/%s takes a bare number after %s, and two open branches can take the same one. "+
			"Name it by the UTC minute and your builder letter instead: $(date -u +%%y%%m%%d%%H%%M)<letter>_%s, "+
			"for example 2611151820a_%s", n, lastNumbered, n[strings.IndexByte(n, '_')+1:], n[strings.IndexByte(n, '_')+1:])
	}
}

func TestMigrations_NamingCheckFailsOnANewBareNumber(t *testing.T) {
	bad, err := bareNumbersAfter(fstest.MapFS{
		"0398_two_plan_ladders.sql": {Data: []byte("SELECT 1;")},
		"0399_agent_notes.sql":      {Data: []byte("SELECT 1;")},
		"2611151820a_notes.sql":     {Data: []byte("SELECT 1;")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || bad[0] != "0399_agent_notes.sql" {
		t.Fatalf("bareNumbersAfter = %v, want [0399_agent_notes.sql]", bad)
	}
}

func TestParse_TimestampNamesSortAfterEveryNumberedOne(t *testing.T) {
	got, err := Parse(fstest.MapFS{
		"2611151820a_agent_notes.sql": {Data: []byte("SELECT 1;")},
		"0398_two_plan_ladders.sql":   {Data: []byte("SELECT 1;")},
		"2611151819d_cards.sql":       {Data: []byte("SELECT 1;")},
		"0001_init.sql":               {Data: []byte("SELECT 1;")},
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var order []string
	for _, m := range got {
		order = append(order, m.Version)
	}
	if want := "0001 0398 2611151819d 2611151820a"; strings.Join(order, " ") != want {
		t.Fatalf("order = %v, want %s", order, want)
	}
}

func TestParse_RejectsAMalformedTimestampName(t *testing.T) {
	for _, name := range []string{
		"2611151820A_upper.sql",  // the letter is lower case
		"261115182a_short.sql",   // nine digits
		"2613151820a_month.sql",  // no month 13
		"2611151860a_minute.sql", // no minute 60
		"2611151820ab_two.sql",   // one letter
	} {
		if _, err := Parse(fstest.MapFS{name: {Data: []byte("SELECT 1;")}}); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestParse_RejectsTwoMigrationsWithOneTimestamp(t *testing.T) {
	if _, err := Parse(fstest.MapFS{
		"2611151820a_one.sql": {Data: []byte("SELECT 1;")},
		"2611151820a_two.sql": {Data: []byte("SELECT 1;")},
	}); err == nil {
		t.Fatal("two migrations with version 2611151820a must be rejected (ambiguous ordering)")
	}
}

// freshDatabase creates an empty database for one test and drops it after.
func freshDatabase(t *testing.T) *pgx.Conn {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping DB integration test")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_b6168_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// realWith is the repo's migrations up to (not including) the one named before, when before is set, plus extra.
func realWith(t *testing.T, before string, extra fstest.MapFS) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if before != "" && n >= before {
			continue
		}
		b, err := fs.ReadFile(migrations.FS, n)
		if err != nil {
			t.Fatal(err)
		}
		out[n] = &fstest.MapFile{Data: b}
	}
	for n, f := range extra {
		out[n] = f
	}
	return out
}

// probe reads workspaces.plan, which 0398 adds, so it fails unless it runs after 0398. Its version is the last minute
// the form can name, so it sorts after every real migration, timestamped ones included (B61.81 added the first).
const probeVersion = "9912312359a"

var probe = fstest.MapFS{probeVersion + "_b6168_probe.sql": {Data: []byte(
	`CREATE TABLE dbmigrate_b6168_probe AS SELECT plan FROM workspaces`)}}

func TestRun_TimestampMigrationAppliesLastFromZero(t *testing.T) {
	conn := freshDatabase(t)
	ctx := context.Background()
	all := realWith(t, "", probe)

	applied, err := Run(ctx, conn, all)
	if err != nil {
		t.Fatalf("Run from zero: %v", err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != probeVersion {
		t.Fatalf("applied %v, want %s last", applied, probeVersion)
	}
	again, err := Run(ctx, conn, all)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Run applied %v (err %v), want nothing", again, err)
	}
}

func TestRun_TimestampMigrationAppliesAfterTheRestOnADatabaseAt0392(t *testing.T) {
	conn := freshDatabase(t)
	ctx := context.Background()
	if _, err := Run(ctx, conn, realWith(t, "0393", nil)); err != nil {
		t.Fatalf("Run to 0392: %v", err)
	}
	var top string
	if err := conn.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&top); err != nil || top != "0392" {
		t.Fatalf("database at %q (err %v), want 0392", top, err)
	}

	applied, err := Run(ctx, conn, realWith(t, "", probe))
	if err != nil {
		t.Fatalf("Run on a database at 0392: %v", err)
	}
	var numbered []string
	names, _ := fs.Glob(migrations.FS, "*.sql")
	for _, n := range names {
		if v, _ := versionPrefix(n); v > "0392" {
			numbered = append(numbered, v)
		}
	}
	if want := strings.Join(append(numbered, probeVersion), " "); strings.Join(applied, " ") != want {
		t.Fatalf("applied %v, want %s", applied, want)
	}
	again, err := Run(ctx, conn, realWith(t, "", probe))
	if err != nil || len(again) != 0 {
		t.Fatalf("second Run applied %v (err %v), want nothing", again, err)
	}
}
