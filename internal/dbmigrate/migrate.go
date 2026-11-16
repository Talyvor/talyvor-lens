// Package dbmigrate applies the repo's plain-SQL migrations (migrations/*.sql)
// to a Postgres database, tracking applied versions in a schema_migrations
// table so re-running is a no-op. The migrations are forward-only plain SQL
// (no golang-migrate/goose tooling), and several are not individually
// re-runnable (bare CREATE TABLE/INDEX), so the tracking table — not the SQL —
// is what makes the runner idempotent.
//
// A migration is named YYMMDDHHMMx_name.sql: the UTC minute it was written and
// the builder's letter, e.g. 2611151820a_agent_notes.sql, so two open branches
// cannot take the same version (B61.68). The files up to 0398 keep their
// sequential NNNN_name.sql names. Versions sort as text, and every NNNN starts
// with 0 while every timestamp starts with 2, so a timestamped migration always
// applies after every numbered one and the numbered ones keep their order.
package dbmigrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// noTxMarker is a SQL comment that opts a migration out of the automatic
// BEGIN/COMMIT wrapper.  Place it anywhere in the file body; the runner
// executes the SQL directly on the connection and records the version in
// a separate transaction afterward.
//
// Only use this for statements Postgres explicitly forbids inside a
// transaction block (DROP INDEX CONCURRENTLY, CREATE INDEX CONCURRENTLY,
// VACUUM, REINDEX CONCURRENTLY).  Because the SQL and the version-record
// steps are no longer atomic, the migration SQL must be idempotent
// (IF EXISTS / IF NOT EXISTS guards) so that a crash between them is
// safe to re-run.
const noTxMarker = "-- lens:no-transaction"

// Migration is one parsed *.sql file.
type Migration struct {
	Version string // the prefix before the first '_', e.g. "0001" or "2611151820a"
	Name    string // full filename, e.g. "0001_init.sql"
	SQL     string // file body
	// NoTx is true when the file body contains noTxMarker.  The runner
	// executes such migrations outside any transaction block, which is
	// required for statements like DROP/CREATE INDEX CONCURRENTLY.
	NoTx bool
}

// Parse reads every *.sql file at the root of fsys and returns them sorted by
// version. Non-.sql files are ignored. Errors on a malformed name (neither an
// NNNN_ nor a YYMMDDHHMMx_ prefix) or a duplicate version (ambiguous ordering).
func Parse(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: read migrations dir: %w", err)
	}
	var out []Migration
	seen := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, ok := versionPrefix(name)
		if !ok {
			return nil, fmt.Errorf("dbmigrate: %q has no version prefix: name it YYMMDDHHMMx_name.sql, the UTC minute and your builder letter", name)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("dbmigrate: duplicate version %s (%q and %q)", version, prev, name)
		}
		seen[version] = name
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("dbmigrate: read %q: %w", name, err)
		}
		sql := string(body)
		out = append(out, Migration{
			Version: version,
			Name:    name,
			SQL:     sql,
			NoTx:    strings.Contains(sql, noTxMarker),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// versionPrefix extracts the version before the first '_': a run of digits
// ("0001_init.sql" -> "0001") or a timestamp ("2611151820a_notes.sql" ->
// "2611151820a"). Returns ok=false for anything else.
func versionPrefix(name string) (string, bool) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return "", false
	}
	prefix := name[:i]
	if isTimestamp(prefix) {
		return prefix, true
	}
	for _, r := range prefix {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return prefix, true
}

// isTimestamp reports whether v is a UTC minute YYMMDDHHMM followed by one
// lower-case builder letter, e.g. "2611151820a".
func isTimestamp(v string) bool {
	if len(v) != 11 || v[10] < 'a' || v[10] > 'z' {
		return false
	}
	_, err := time.Parse("0601021504", v[:10])
	return err == nil
}

// Run applies every not-yet-recorded migration from fsys to the database in
// version order, each in its own transaction, recording the version in
// schema_migrations on success. It is idempotent: already-applied versions are
// skipped, so re-running is a no-op. Returns the versions newly applied this
// call. Any migration error aborts that migration's transaction and is
// returned (non-nil) so the caller can exit non-zero.
func Run(ctx context.Context, conn *pgx.Conn, fsys fs.FS) ([]string, error) {
	migrations, err := Parse(fsys)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("dbmigrate: ensure schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	var newlyApplied []string
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		var err error
		if m.NoTx {
			err = applyOneNoTx(ctx, conn, m)
		} else {
			err = applyOne(ctx, conn, m)
		}
		if err != nil {
			return newlyApplied, fmt.Errorf("dbmigrate: migration %s failed: %w", m.Name, err)
		}
		newlyApplied = append(newlyApplied, m.Version)
	}
	return newlyApplied, nil
}

// applyOne runs one migration and records it in a single transaction, so a
// failure leaves neither a half-applied schema nor a recorded version.
func applyOne(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// applyOneNoTx runs a no-transaction migration in two steps:
//
//  1. Execute the SQL directly on the connection (no BEGIN/COMMIT wrapper),
//     which is required for statements Postgres forbids inside a transaction
//     block (e.g. DROP INDEX CONCURRENTLY).
//
//  2. Record the applied version in schema_migrations inside its own
//     transaction, separate from step 1.
//
// Because steps 1 and 2 are not atomic, a crash between them leaves the
// schema change applied but the version unrecorded. On the next run the
// SQL executes again. The migration SQL must therefore be idempotent
// (IF EXISTS / IF NOT EXISTS) so the retry is a safe no-op.
func applyOneNoTx(ctx context.Context, conn *pgx.Conn, m Migration) error {
	if _, err := conn.Exec(ctx, m.SQL); err != nil {
		return err
	}
	// Record the version in its own transaction.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: read applied versions: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// ErrNoDatabaseURL is returned by the CLI wiring when LENS_DATABASE_URL is
// unset — surfaced as a clear non-zero exit rather than a nil-pointer panic.
var ErrNoDatabaseURL = errors.New("LENS_DATABASE_URL is not set")
