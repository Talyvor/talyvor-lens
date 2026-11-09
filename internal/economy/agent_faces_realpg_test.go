package economy

import (
	"context"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/url"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// agentPick is B57.6's pick from an agent's id: pair h mod 28 of FNV-1a 32-bit, colour = h mod 7, face = (h div 7) mod 4.
func agentPick(id string) (colour, face string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	n := h.Sum32() % 28
	return AgentColours[n%7], AgentFaces[n/7]
}

// B57.10 — on a database migrated from zero, agents that exist before migration 0360 are backfilled with exactly
// their id's pick: B57.6's three test vectors, and the same pick the apps compute.
func TestAgentFaces_BackfillIsTheIdsPick(t *testing.T) {
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_faces_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = ac.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = ac.Close(context.Background())
	})
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() < "0360" {
			b, _ := fs.ReadFile(migrations.FS, e.Name())
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, before); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name) VALUES
		('agt_research', 'ws-faces', 'Research'), ('agt_sales', 'ws-faces', 'Sales'), ('agt_ops', 'ws-faces', 'Ops')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct{ id, colour, face string }{
		{"agt_research", "mint", "hexagon"}, {"agt_sales", "mint", "flower"}, {"agt_ops", "rose", "squircle"},
	} {
		var colour, face string
		if err := conn.QueryRow(ctx, `SELECT colour, face FROM agent_accounts WHERE id = $1`, v.id).Scan(&colour, &face); err != nil {
			t.Fatal(err)
		}
		c, f := agentPick(v.id)
		if colour != v.colour || face != v.face || c != v.colour || f != v.face {
			t.Fatalf("%s backfilled %s %s, the apps pick %s %s, B57.6 says %s %s", v.id, colour, face, c, f, v.colour, v.face)
		}
	}
}

// B57.10 — a new agent takes its id's pick unless a live agent of its workspace wears that pair; then the next free
// pair in list order. An archived agent's pair is free.
func TestAgentFaces_ANewAgentTakesTheNextFreePair(t *testing.T) {
	pool := backfillPool(t)
	ctx := context.Background()
	const id = "agt_research" // its pick is pair 17, mint hexagon; 18 is lemon hexagon, 19 rose hexagon
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name, colour, face, archived_at) VALUES
		('agt_wears_it', 'ws-free', 'A', 'mint', 'hexagon', NULL),
		('agt_retired', 'ws-free', 'B', 'lemon', 'hexagon', now()),
		('agt_elsewhere', 'ws-other', 'C', 'lemon', 'hexagon', NULL)`); err != nil {
		t.Fatal(err)
	}
	var colour, face string
	if err := pool.QueryRow(ctx, `INSERT INTO agent_accounts (id, workspace_id, name) VALUES ($1, 'ws-free', 'Research')
		RETURNING colour, face`, id).Scan(&colour, &face); err != nil {
		t.Fatal(err)
	}
	if colour != "lemon" || face != "hexagon" {
		t.Fatalf("picked %s %s, want lemon hexagon: mint hexagon is worn, lemon hexagon only by an archived agent", colour, face)
	}
}
