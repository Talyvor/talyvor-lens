package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/economy"
)

const adminKeySweepUsage = `usage:
  lens admin-key-sweep            list every admin-scoped workspace key
  lens admin-key-sweep --revoke   revoke every one of them, each recorded on its workspace's audit chain`

// runAdminKeySweep is `lens admin-key-sweep` (B50.33), run inside the lens container like `lens moderator-keys`.
//
// Until B50.33 any workspace key, an agent's own included, could mint itself an admin-scoped key, and no key records
// who minted it: none of the admin keys made before the fix can be shown to be the owner's. So the sweep lists them
// all and --revoke revokes them all; an owner who still wants one mints it again from their signed-in session.
func runAdminKeySweep(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("admin-key-sweep: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("admin-key-sweep: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return adminKeySweep(ctx, pool, args, by, os.Stdout)
}

type sweptKey struct {
	id, workspaceID, prefix, name string
	createdAt                     time.Time
}

func adminKeySweep(ctx context.Context, pool *pgxpool.Pool, args []string, by string, out io.Writer) error {
	revoke := len(args) == 1 && args[0] == "--revoke"
	if len(args) > 0 && !revoke {
		return errors.New(adminKeySweepUsage)
	}
	rows, err := pool.Query(ctx, `SELECT id::text, workspace_id, key_prefix, name, created_at FROM workspace_api_keys
WHERE 'admin' = ANY(scopes) ORDER BY workspace_id, created_at`)
	if err != nil {
		return fmt.Errorf("admin-key-sweep: list: %w", err)
	}
	var keys []sweptKey
	for rows.Next() {
		var k sweptKey
		if err := rows.Scan(&k.id, &k.workspaceID, &k.prefix, &k.name, &k.createdAt); err != nil {
			rows.Close()
			return fmt.Errorf("admin-key-sweep: list: %w", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("admin-key-sweep: list: %w", err)
	}
	if len(keys) == 0 {
		fmt.Fprintln(out, "no admin-scoped workspace keys")
		return nil
	}
	for _, k := range keys {
		line := fmt.Sprintf("%s\t%s\t%s…\t%s\tcreated %s", k.workspaceID, k.id, k.prefix, k.name, k.createdAt.UTC().Format(time.RFC3339))
		if !revoke {
			fmt.Fprintln(out, line)
			continue
		}
		if err := revokeSweptKey(ctx, pool, k, by); err != nil {
			return err
		}
		fmt.Fprintln(out, "revoked\t"+line)
	}
	if !revoke {
		fmt.Fprintf(out, "\n%d admin-scoped key(s); run with --revoke to revoke them all\n", len(keys))
	}
	return nil
}

// revokeSweptKey deletes k and appends its revocation to the workspace's audit chain in one transaction.
func revokeSweptKey(ctx context.Context, pool *pgxpool.Pool, k sweptKey, by string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("admin-key-sweep: revoke %s: %w", k.id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM workspace_api_keys WHERE id = $1 AND workspace_id = $2`, k.id, k.workspaceID)
	if err != nil {
		return fmt.Errorf("admin-key-sweep: revoke %s: %w", k.id, err)
	}
	if tag.RowsAffected() == 0 {
		return nil // revoked since it was listed
	}
	if err := economy.AppendAuditTx(ctx, tx, k.workspaceID, "api_key:"+k.id, "api_key.revoked", map[string]any{
		"by": by, "prefix": k.prefix, "name": k.name, "created_at": k.createdAt.UTC().Format(time.RFC3339),
		"reason": "B50.33: an admin-scoped key minted before only the owner could mint keys",
	}); err != nil {
		return fmt.Errorf("admin-key-sweep: revoke %s: %w", k.id, err)
	}
	return tx.Commit(ctx)
}
