package secretstatic

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/byok"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/moderatorkey"
	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/redact"
	"github.com/talyvor/lens/internal/sessionkey"
	"github.com/talyvor/lens/migrations"
)

// TestNoKeyOrSecretIsStoredInTheClear mints every kind of key Lens hands out (a workspace API key, a session key, a
// moderator key) and stores a provider key a workspace brings, then reads every column of every table as text: none
// of the raw values is anywhere in the database. Keys a caller presents live only as hashes; a provider key Lens must
// use again lives only as AES-GCM ciphertext (internal/envelope, a fresh random nonce on every write).
func TestNoKeyOrSecretIsStoredInTheClear(t *testing.T) {
	const schema, ws = "secretstatic_b503", "ws-b503"
	ctx := context.Background()
	pool := realPool(t, schema)

	apiKey, _, err := auth.New(pool).GenerateKey(ctx, ws, "team", "b503", nil)
	if err != nil {
		t.Fatal(err)
	}
	sessKey, _, err := sessionkey.NewStore(pool).Mint(ctx, ws, "user-b503", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	modKey, _, err := moderatorkey.NewStore(pool).Create(ctx, "b503", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id,
		price_id, status, livemode, last_event_at, byok, plan)
		VALUES ($1, 'sub_b503', 'cus_b503', 'price_byok', 'active', false, NOW(), true, 'byok')`, ws); err != nil {
		t.Fatal(err)
	}
	ring, err := envelope.NewKeyring(bytes.Repeat([]byte{3}, envelope.KEKLen))
	if err != nil {
		t.Fatal(err)
	}
	const providerKey = "sk-b503-own-provider-key-0001"
	if _, err := byok.New(pool, ring).Put(ctx, ws, "openai", providerKey); err != nil {
		t.Fatal(err)
	}

	secrets := map[string]string{"API key": apiKey, "session key": sessKey, "moderator key": modKey, "provider key": providerKey}
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type = 'BASE TABLE'`, schema)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	sawControl := false
	for _, table := range tables {
		rows, err := pool.Query(ctx, `SELECT t::text FROM `+pgx.Identifier{schema, table}.Sanitize()+` t`)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for _, row := range texts {
			sawControl = sawControl || strings.Contains(row, "cus_b503")
			for what, raw := range secrets {
				// a bytea column reads as \x and hex
				if strings.Contains(row, raw) || strings.Contains(row, hex.EncodeToString([]byte(raw))) {
					t.Errorf("table %s stores the raw %s", table, what)
				}
			}
		}
	}
	if !sawControl {
		t.Fatal("the scan never saw the subscription row it wrote: it reads nothing")
	}
}

// realPool migrates a fresh schema on LENS_TEST_DATABASE_URL and returns a pool on it.
func realPool(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping the real-PG secrets tests")
	}
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
			t.Fatal(err)
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

// TestTheAuditTrailNeverKeepsASecret writes a credential into both audit trails the way a partner's error text or an
// operator's note could carry one: neither table keeps it, and each keeps the rest of what was said.
func TestTheAuditTrailNeverKeepsASecret(t *testing.T) {
	ctx := context.Background()
	pool := realPool(t, "secretstatic_b503_audit")
	const leaked = "sk_live_B503notarealkey00000000"
	if err := economy.NewDualTokenStore(nil, pool, nil).AppendAudit(ctx, "ws-b503", "preq_b503", "b503_payout",
		map[string]any{"error": "partner refused: key " + leaked + " is revoked", "call": map[string]any{"auth": "Bearer " + leaked}}); err != nil {
		t.Fatal(err)
	}
	if _, err := operatoraudit.NewStore(pool).Record(ctx, operatoraudit.Entry{Actor: "operator", Action: "b503.note",
		Target: "ws-b503", Detail: "rotated the old key " + leaked}); err != nil {
		t.Fatal(err)
	}
	var chain, op string
	if err := pool.QueryRow(ctx, `SELECT body FROM audit_chain WHERE subject = 'preq_b503'`).Scan(&chain); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT detail FROM operator_audit WHERE action = 'b503.note'`).Scan(&op); err != nil {
		t.Fatal(err)
	}
	for table, got := range map[string]string{"audit_chain": chain, "operator_audit": op} {
		if strings.Contains(got, leaked) || strings.Contains(got, "B503notarealkey") {
			t.Errorf("%s keeps the secret: %s", table, got)
		}
		if !strings.Contains(got, redact.Mark) || !(strings.Contains(got, "revoked") || strings.Contains(got, "rotated")) {
			t.Errorf("%s should keep the rest of the entry with the secret marked: %s", table, got)
		}
	}
}
