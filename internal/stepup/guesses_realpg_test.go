package stepup

import (
	"context"
	"encoding/base32"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// B28.461 — the wrong-code count holds on every replica: two Verifiers on one database, from zero, share one count,
// and the wait begins exactly once.
func TestPostgresGuesses_OneCountAcrossReplicas(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG step-up guesses test")
	}
	const schema = "stepup_b28461_realpg"
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

	key := []byte("0123456789abcdef0123")
	at := time.Now().Truncate(time.Second)
	replica := func() *Verifier {
		v, err := New(base32.StdEncoding.EncodeToString(key), NewPostgresClaims(pool))
		if err != nil {
			t.Fatal(err)
		}
		v.now = func() time.Time { return at }
		return v
	}
	a, b := replica(), replica()
	wrong := CodeAt(key, at.Add(-time.Hour))

	for i := 1; i < MaxWrong; i++ {
		v := a
		if i%2 == 0 {
			v = b
		}
		if err := v.Verify(ctx, "nicolai", wrong); !errors.Is(err, ErrInvalid) {
			t.Fatalf("wrong code %d = %v; want ErrInvalid", i, err)
		}
	}
	if err := a.Verify(ctx, "nicolai", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no code = %v; want ErrInvalid", err)
	}
	started := 0
	var locked *ErrLocked
	for _, v := range []*Verifier{b, a, b} {
		if err := v.Verify(ctx, "nicolai", wrong); !errors.As(err, &locked) {
			t.Fatalf("a wrong code after four = %v; want ErrLocked", err)
		}
		if locked.Started {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("the wait began %d times across two replicas; want once", started)
	}
	if err := a.Verify(ctx, "nicolai", CodeAt(key, at)); !errors.As(err, &locked) {
		t.Fatalf("a good code on the other replica during the wait = %v; want ErrLocked", err)
	}
	var used int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operator_step_up_codes`).Scan(&used); err != nil || used != 0 {
		t.Fatalf("a code refused during the wait was claimed: %d (%v)", used, err)
	}
	at = at.Add(LockFor)
	if err := b.Verify(ctx, "nicolai", CodeAt(key, at)); err != nil {
		t.Fatalf("a good code after the wait: %v", err)
	}
}
