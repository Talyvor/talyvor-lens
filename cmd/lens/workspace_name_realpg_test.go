package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// B46.15 — the workspace's owner names the workspace and reads it back; nobody else can.
func TestWorkspaceName_TheOwnerNamesItAndNobodyElseCan(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG workspace name test")
	}
	const schema = "cmd_workspace_name_realpg"
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

	wsm := workspace.New(pool)
	for _, id := range []string{"ws-a", "ws-b"} {
		if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(workspaceIsolationMiddleware)
		mountWorkspaceNameRoutes(authed, pool, wsm)
	})
	ownerA := &auth.AuthContext{WorkspaceID: "ws-a", AuthMethod: auth.MethodJWT, UserID: "owner-a", Scopes: []string{auth.ScopeKeys}}
	ownerB := &auth.AuthContext{WorkspaceID: "ws-b", AuthMethod: auth.MethodJWT, UserID: "owner-b", Scopes: []string{auth.ScopeKeys}}
	proxyKeyA := &auth.AuthContext{WorkspaceID: "ws-a", AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	call := func(who *auth.AuthContext, method, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/workspaces/ws-a/name", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	if code, out := call(ownerA, http.MethodPut, `{"name":"  Acme Robotics  "}`); code != http.StatusOK || out["name"] != "Acme Robotics" {
		t.Fatalf("owner rename = %d %v", code, out)
	}
	if code, out := call(ownerA, http.MethodGet, ``); code != http.StatusOK || out["name"] != "Acme Robotics" {
		t.Fatalf("read back = %d %v", code, out)
	}
	if ws, _ := wsm.GetWorkspace("ws-a"); ws.Name != "Acme Robotics" {
		t.Fatalf("the roster still reads %q", ws.Name)
	}
	var actor, detail string
	if err := pool.QueryRow(ctx, `SELECT actor, detail FROM operator_audit WHERE action = 'workspace.rename' AND target = 'workspace:ws-a'`).Scan(&actor, &detail); err != nil {
		t.Fatalf("the audit trail has no rename: %v", err)
	}
	if actor != "jwt:user:owner-a" || detail != `"ws-a" → "Acme Robotics"` {
		t.Fatalf("audit row = %q %q", actor, detail)
	}

	for _, c := range []struct {
		who  *auth.AuthContext
		body string
		want int
	}{
		{ownerB, `{"name":"Taken"}`, http.StatusForbidden},
		{proxyKeyA, `{"name":"Taken"}`, http.StatusForbidden},
		{ownerA, `{"name":"   "}`, http.StatusBadRequest},
		{ownerA, `{"name":"` + strings.Repeat("é", 81) + `"}`, http.StatusBadRequest},
	} {
		if code, out := call(c.who, http.MethodPut, c.body); code != c.want {
			t.Errorf("PUT %s as %s = %d %v, want %d", c.body, c.who.WorkspaceID, code, out, c.want)
		}
	}
	if code, _ := call(ownerB, http.MethodGet, ``); code != http.StatusForbidden {
		t.Errorf("another workspace read the name: %d", code)
	}
	if code, out := call(ownerA, http.MethodPut, `{"name":"`+strings.Repeat("é", 80)+`"}`); code != http.StatusOK {
		t.Errorf("an 80-character name was refused: %d %v", code, out)
	}
	if code, out := call(ownerA, http.MethodGet, ``); out["name"] != strings.Repeat("é", 80) {
		t.Errorf("after the refusals the name reads %d %v", code, out)
	}
}
