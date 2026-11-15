package routefuzz

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// B61.7: one person holds two accounts, A and B, on the real Lens binary. A's session token reads nothing of B and
// writes nothing of B on any route registered in cmd/lens: every GET with B's workspace and agent in its path carries
// none of B's name, agent or key in its answer, and after every POST, PUT, PATCH and DELETE aimed at B the rows B holds
// in every workspace-keyed table are unchanged.

const b617Name = "B617 second account Qx7"

type b617Account struct {
	WorkspaceID string `json:"workspace_id"`
	Token       string `json:"token"`
	FirstAgent  *struct {
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
		KeyID string `json:"key_id"`
	} `json:"first_agent"`
}

// routesOf reads every route registered with a literal path under one of methods in cmd/lens's non-test source.
func routesOf(t *testing.T, dir string, methods ...string) []string {
	t.Helper()
	want := map[string]bool{}
	for _, m := range methods {
		want[m] = true
	}
	fset := token.NewFileSet()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !want[sel.Sel.Name] {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			path, _ := strconv.Unquote(lit.Value)
			if h, ok := call.Args[1].(*ast.SelectorExpr); ok && strings.HasPrefix(h.Sel.Name, modelCall) {
				return true
			}
			if strings.HasPrefix(path, "/v1/") && !strings.HasSuffix(path, "/*") {
				seen[strings.ToUpper(sel.Sel.Name)+" "+path] = true
			}
			return true
		})
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func (l *lens) provision(t *testing.T, body string) b617Account {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, l.base+"/v1/provision", strings.NewReader(body))
	req.Header.Set("X-Gateway-Auth", gateKey)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out b617Account
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		t.Fatalf("provision %s = %d: %s", body, resp.StatusCode, raw)
	}
	return out
}

// call sends one request with token, clearing the rate limiter's counters first; an answer that does not come in time
// is "" and status 0.
func (l *lens) call(t *testing.T, c *http.Client, method, path, token string, body []byte) (int, string) {
	t.Helper()
	ctx := context.Background()
	if keys, _ := l.redis.Keys(ctx, "lens:rl:*").Result(); len(keys) > 0 {
		l.redis.Del(ctx, keys...)
	}
	req, _ := http.NewRequest(method, l.base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(raw)
}

// fingerprint is an md5 of every row account ws holds in every table with a workspace_id column.
func fingerprint(t *testing.T, conn *pgx.Conn, ws string) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.Query(ctx, `SELECT c.table_name FROM information_schema.columns c
		JOIN information_schema.tables tb ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		WHERE c.table_schema = $1 AND c.column_name = 'workspace_id' AND tb.table_type = 'BASE TABLE' ORDER BY 1`, schema)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 50 {
		t.Fatalf("only %d workspace-keyed tables found in %s", len(tables), schema)
	}
	out := map[string]string{}
	for _, tb := range append(tables, "workspaces") {
		col := "workspace_id"
		if tb == "workspaces" {
			col = "id"
		}
		var sum string
		q := `SELECT coalesce(md5(string_agg(to_jsonb(t)::text, '|' ORDER BY to_jsonb(t)::text)), '') FROM ` +
			pgx.Identifier{tb}.Sanitize() + ` t WHERE ` + col + ` = $1`
		if err := conn.QueryRow(ctx, q, ws).Scan(&sum); err != nil {
			t.Fatalf("fingerprint %s: %v", tb, err)
		}
		out[tb] = sum
	}
	return out
}

func TestB617_OneAccountsTokenReadsAndWritesNothingOfThePersonsOther(t *testing.T) {
	l := startLens(t)
	const who = "b617-routefuzz-person"
	a := l.provision(t, `{"identity":"`+who+`","display_name":"B617 first account","account_type":"business","money_kind":"hybrid","country":"GB","registration_number":"01234567"}`)
	b := l.provision(t, `{"identity":"`+who+`","mode":"open","display_name":"`+b617Name+`","account_type":"personal","money_kind":"fiat","country":"GB"}`)
	if a.WorkspaceID == b.WorkspaceID || b.FirstAgent == nil || b.FirstAgent.Agent.ID == "" || b.FirstAgent.KeyID == "" {
		t.Fatalf("setup: A %q, B %q, B's first agent %+v", a.WorkspaceID, b.WorkspaceID, b.FirstAgent)
	}
	bAgent, bKey := b.FirstAgent.Agent.ID, b.FirstAgent.KeyID

	param := regexp.MustCompile(`\{[^}]*\}`)
	fill := func(route string) (method, path string) {
		method, path, _ = strings.Cut(route, " ")
		return method, param.ReplaceAllStringFunc(path, func(p string) string {
			if p = strings.ToLower(p); strings.HasPrefix(p, "{ws") || strings.HasPrefix(p, "{workspace") {
				return b.WorkspaceID
			}
			return bAgent
		})
	}
	// Public discovery (an agent's A2A card and keys) answers anyone by design and is not read with a session.
	public := func(path string) bool { return strings.HasPrefix(path, "/v1/a2a/") }
	short := &http.Client{Timeout: 5 * time.Second}

	// Reads. B's own token sees B's name, agent or key on its own routes, so the markers are visible to this sweep.
	gets := routesOf(t, "..", "Get")
	if len(gets) < 200 {
		t.Fatalf("only %d GET routes found in cmd/lens — the source scan lost its routes", len(gets))
	}
	var seenByB int
	for _, route := range gets {
		method, path := fill(route)
		if public(path) {
			continue
		}
		if _, body := l.call(t, short, method, path, b.Token, nil); strings.Contains(body, b617Name) || strings.Contains(body, bKey) {
			seenByB++
		}
		code, body := l.call(t, short, method, path, a.Token, nil)
		inPath := strings.Contains(path, bAgent)
		if strings.Contains(body, b617Name) || strings.Contains(body, bKey) || (!inPath && strings.Contains(body, bAgent)) {
			t.Errorf("%s with account A's token answered %d carrying account B's data: %.300s", route, code, body)
		}
	}
	if seenByB < 3 {
		t.Fatalf("B's own token showed B's name or key on only %d routes — the read sweep could not see a leak", seenByB)
	}

	// Writes.
	ctx := context.Background()
	dbURL := os.Getenv("LENS_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	before := fingerprint(t, conn, b.WorkspaceID)
	if before["workspaces"] == "" || before["agent_accounts"] == "" || before["money_accounts"] == "" {
		t.Fatalf("B's fingerprint holds no workspace, agent or money account row: %v", before)
	}
	writes := routesOf(t, "..", "Post", "Put", "Patch", "Delete")
	var sent int
	for _, route := range writes {
		method, path := fill(route)
		if public(path) || strings.HasPrefix(path, "/v1/synthetic/") || path == "/v1/provision" || path == "/v1/waitlist" ||
			strings.HasPrefix(path, "/v1/sso/") {
			continue
		}
		for _, body := range []string{`{}`, `{"agent_id":"` + bAgent + `","workspace_id":"` + b.WorkspaceID + `","name":"x"}`} {
			l.call(t, short, method, path, a.Token, []byte(body))
		}
		sent++
	}
	if sent < 200 {
		t.Fatalf("only %d write routes sent", sent)
	}
	after := fingerprint(t, conn, b.WorkspaceID)
	for tb, sum := range before {
		if after[tb] != sum {
			t.Errorf("account A's token changed account B's rows in %s", tb)
		}
	}
}
