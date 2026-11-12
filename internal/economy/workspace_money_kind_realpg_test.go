package economy

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// B59.6 — a workspace made before migration 0373 reads business and hybrid after it, so nothing it uses today goes.

// migrationsBefore is migrations.FS without the migrations from first on.
type migrationsBefore struct{ first string }

func (m migrationsBefore) Open(name string) (fs.File, error) { return migrations.FS.Open(name) }

func (m migrationsBefore) ReadDir(name string) ([]fs.DirEntry, error) {
	all, err := fs.ReadDir(migrations.FS, name)
	if err != nil {
		return nil, err
	}
	var out []fs.DirEntry
	for _, e := range all {
		if e.Name() < m.first {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestB596_ExistingWorkspaceIsBusinessAndHybridAfterTheMigration(t *testing.T) {
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_b596_%d", time.Now().UnixNano())
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
	defer conn.Close(ctx)

	if _, err := dbmigrate.Run(ctx, conn, migrationsBefore{first: "0373_"}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, company) VALUES ('ws-old', 'Old', 'ws:old:', false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var accountType, moneyKind string
	if err := conn.QueryRow(ctx, `SELECT account_type, money_kind FROM workspaces WHERE id = 'ws-old'`).Scan(&accountType, &moneyKind); err != nil {
		t.Fatalf("read the workspace's type and kind: %v", err)
	}
	if accountType != AccountTypeBusiness || moneyKind != MoneyKindHybrid {
		t.Fatalf("a workspace made before the migration is %s and %s, want business and hybrid", accountType, moneyKind)
	}
	// Neither column takes a value outside its list.
	for _, bad := range []string{`UPDATE workspaces SET account_type = 'charity' WHERE id = 'ws-old'`,
		`UPDATE workspaces SET money_kind = 'gold' WHERE id = 'ws-old'`} {
		if _, err := conn.Exec(ctx, bad); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
}
