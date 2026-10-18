package dbmigrate

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// B39.6 role separation (docs/AUDIT-POLICY.md §3). The login that runs migrations owns the schema and
// the audit_block_mutation triggers; Lens itself may run as AppRole, which can never UPDATE, DELETE or
// TRUNCATE a guarded table and, not owning it, can never DISABLE or DROP its trigger. RetentionRole
// may only read and delete token_events, for the retention sweeper.
const (
	AppRole       = "lens_app"
	RetentionRole = "lens_retention"
)

// roleGrantsSQL re-derives every grant from the live schema, so a table a later migration adds is
// covered by the next migrate run with no list to keep in step. Runs in the migrating session's
// current schema.
const roleGrantsSQL = `DO $$
DECLARE
    s text := current_schema();
    t regclass;
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO lens_app, lens_retention', s);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO lens_app', s);
    EXECUTE format('GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA %I TO lens_app', s);
    IF to_regclass(format('%I.schema_migrations', s)) IS NOT NULL THEN
        EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON %I.schema_migrations FROM lens_app', s);
    END IF;
    FOR t IN
        SELECT DISTINCT tg.tgrelid::regclass
        FROM pg_trigger tg
        JOIN pg_proc p ON p.oid = tg.tgfoid
        JOIN pg_class c ON c.oid = tg.tgrelid
        WHERE p.proname = 'audit_block_mutation' AND c.relnamespace = s::regnamespace
    LOOP
        EXECUTE format('REVOKE UPDATE, DELETE, TRUNCATE ON %s FROM lens_app', t);
    END LOOP;
    IF to_regclass(format('%I.token_events', s)) IS NOT NULL THEN
        EXECUTE format('GRANT SELECT, DELETE ON %I.token_events TO lens_retention', s);
    END IF;
    IF to_regclass(format('%I.audit_export_state', s)) IS NOT NULL THEN
        EXECUTE format('GRANT SELECT ON %I.audit_export_state TO lens_retention', s);
    END IF;
END
$$`

// ApplyRoles creates AppRole and RetentionRole when missing (NOLOGIN), re-applies their grants, and
// gives a role LOGIN with the password passed for it. An empty password leaves that role's login as
// it is, so a password an operator set with psql survives. When the migrating login may not create
// roles and no password was asked for, it logs and returns nil: role separation is opt-in, and a
// deploy that never uses it must not fail on it.
func ApplyRoles(ctx context.Context, conn *pgx.Conn, appPassword, retentionPassword string) error {
	for _, role := range []string{AppRole, RetentionRole} {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
			return fmt.Errorf("roles: look up %s: %w", role, err)
		}
		if exists {
			continue
		}
		// A concurrent migrate may create it between the lookup and here; that is not an error.
		_, err := conn.Exec(ctx, `DO $$ BEGIN CREATE ROLE `+role+` NOLOGIN; EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`)
		if err != nil {
			if appPassword == "" && retentionPassword == "" {
				slog.Warn("migrate: cannot create the database roles Lens can run as; it keeps the migration login",
					slog.String("role", role), slog.String("err", err.Error()))
				return nil
			}
			return fmt.Errorf("roles: create %s: %w", role, err)
		}
	}
	if _, err := conn.Exec(ctx, roleGrantsSQL); err != nil {
		return fmt.Errorf("roles: grants: %w", err)
	}
	for role, pw := range map[string]string{AppRole: appPassword, RetentionRole: retentionPassword} {
		if pw == "" {
			continue
		}
		// The verifier, never the password, goes over the wire, so no server log can hold it.
		var stmt string
		if err := conn.QueryRow(ctx, `SELECT format('ALTER ROLE %I LOGIN PASSWORD %L', $1::text, $2::text)`, role, scramVerifier(pw)).Scan(&stmt); err != nil {
			return fmt.Errorf("roles: password for %s: %w", role, err)
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("roles: password for %s: %w", role, err)
		}
	}
	return nil
}

// scramVerifier is the SCRAM-SHA-256 verifier Postgres stores for password (RFC 5802/7677), which
// ALTER ROLE ... PASSWORD takes as is. shortcut: no SASLprep, so a non-ASCII password may not log in;
// the documented passwords are hex.
func scramVerifier(password string) string {
	const iter = 4096
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	salted, _ := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	mac := func(msg string) []byte { h := hmac.New(sha256.New, salted); h.Write([]byte(msg)); return h.Sum(nil) }
	stored := sha256.Sum256(mac("Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iter, b64(salt), b64(stored[:]), b64(mac("Server Key")))
}
