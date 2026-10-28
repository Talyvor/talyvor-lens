// Package routefuzz is B50.14's request-body fuzz over the real Lens binary. FuzzRequestBodies (cmd/lens)
// mounts the route families it can build in process; about 33 write routes are closures inside run() and
// cannot be mounted alone, so this one builds cmd/lens, boots it on a migrated database, a Redis and an
// in-process NATS, and sends every body to every POST, PUT and PATCH route registered in cmd/lens's source,
// once as a synthetic workspace's owner and once with the admin key: no 500, no hang, no SQL, stack trace
// or internal address in any answer. `go test` runs the seeds; `go test -fuzz FuzzEveryWriteRoute` searches further.
package routefuzz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

const (
	schema    = "cmd_b5014_route_fuzz"
	adminKey  = "b5014-route-fuzz-admin"
	synthKey  = "b5014-route-fuzz-synthetic"
	gateKey   = "b5014-route-fuzz-gateway" // LENS_PROVISION_SECRET: the provision and waitlist routes
	anyID     = "00000000-0000-4000-8000-000000000001"
	modelCall = "Handle" // p.HandleOpenAI and friends: proxy routes, whose bodies go to a model, not to Lens's stores
)

// leak is what an answer to a caller may never carry; the same pattern as FuzzRequestBodies.
var leak = regexp.MustCompile(`SQLSTATE|pgx:|pgconn|pq: |goroutine \d|runtime error|\.go:\d|dial tcp|violates (?:foreign key|unique|not-null|check)|duplicate key value|syntax error at`)

var seeds = []string{
	``, `null`, `[]`, `"x"`, `{`, `{}`, `{"a":`, "\xff\xfe{}", `{"amount":-1,"amount_minor":-1,"amount_ulxc":-1}`,
	`{"amount":1e309,"amount_minor":9223372036854775808,"currency":"\u0000"}`, `{"amount_minor":1.5,"currency":"XXX"}`,
	`{"name":"` + strings.Repeat("A", 70000) + `"}`, strings.Repeat(`[`, 100000), `{"id":{"$gt":""},"name":["a"]}`,
}

type lens struct {
	base, wsID, token string
	proc              *exec.Cmd
	redis             *redis.Client
	routes            []string // "METHOD /path" with every parameter filled
}

// writeRoutes reads every POST, PUT and PATCH registered with a literal path in cmd/lens's non-test source.
func writeRoutes(t testing.TB, dir string) []string {
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
			if !ok || (sel.Sel.Name != "Post" && sel.Sel.Name != "Put" && sel.Sel.Name != "Patch") {
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
			if strings.HasPrefix(path, "/") && !strings.HasSuffix(path, "/*") { // "/*": a provider pass-through
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

func startLens(t testing.TB) *lens {
	dbURL, redisURL := os.Getenv("LENS_TEST_DATABASE_URL"), os.Getenv("LENS_TEST_REDIS_URL")
	if dbURL == "" || redisURL == "" {
		t.Skip("LENS_TEST_DATABASE_URL and LENS_TEST_REDIS_URL not both set — skipping the real-Lens route fuzz")
	}
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(dbURL)
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
	sep := "?"
	if strings.Contains(dbURL, "?") {
		sep = "&"
	}

	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats did not start")
	}

	bin := filepath.Join(t.TempDir(), "lens")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/lens: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	proc := exec.Command(bin)
	proc.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"LENS_LISTEN_ADDR=" + addr,
		"LENS_DATABASE_URL=" + dbURL + sep + "search_path=" + schema,
		"LENS_REDIS_URL=" + redisURL,
		"LENS_NATS_URL=" + ns.ClientURL(),
		"LENS_OPENAI_API_KEY=sk-b5014-not-a-key", "LENS_ANTHROPIC_API_KEY=sk-b5014-not-a-key",
		"LENS_API_KEY=" + adminKey, "LENS_SYNTHETIC_KEY=" + synthKey, "LENS_PROVISION_SECRET=" + gateKey,
		"LENS_GLOBAL_RPM=100000000",
		// Nothing a fuzzed body reaches may leave this machine: every proxy-aware client dials a closed port.
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost",
	}
	var logs bytes.Buffer
	proc.Stdout, proc.Stderr = &logs, &logs
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
		if t.Failed() {
			t.Logf("lens log (tail):\n%s", tail(logs.String(), 4000))
		}
	})
	base := "http://" + addr
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("lens did not come up:\n%s", tail(logs.String(), 4000))
		}
	}

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/synthetic/workspaces", strings.NewReader(`{"count":1}`))
	req.Header.Set("X-Talyvor-Synthetic-Key", synthKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var made struct {
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
			Token       string `json:"token"`
		} `json:"workspaces"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&made)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || len(made.Workspaces) != 1 {
		t.Fatalf("synthetic workspace: %d", resp.StatusCode)
	}
	ws := made.Workspaces[0]

	param := regexp.MustCompile(`\{[^}]*\}`)
	var routes []string
	for _, r := range writeRoutes(t, "..") {
		routes = append(routes, param.ReplaceAllStringFunc(r, func(p string) string {
			if p = strings.ToLower(p); strings.HasPrefix(p, "{ws") || strings.HasPrefix(p, "{workspace") {
				return ws.WorkspaceID
			}
			return anyID
		}))
	}
	// The synthetic routes go last: their own limiter allows 10 (create, reset) and 60 (due) calls a minute,
	// and a reset must not take the workspace the routes before it are fuzzed as.
	sort.SliceStable(routes, func(i, j int) bool {
		return !strings.Contains(routes[i], " /v1/synthetic/") && strings.Contains(routes[j], " /v1/synthetic/")
	})
	if len(routes) < 200 {
		t.Fatalf("only %d write routes found in cmd/lens — the source scan lost its routes", len(routes))
	}
	ropts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(ropts)
	t.Cleanup(func() { _ = rdb.Close() })
	return &lens{base: base, wsID: ws.WorkspaceID, token: ws.Token, proc: proc, routes: routes, redis: rdb}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

var client = &http.Client{Timeout: 10 * time.Second}

// check sends body to route with each credential and fails on a 500, a leak, a hang or a dead server.
func (l *lens) check(t *testing.T, route string, body []byte) {
	method, path, _ := strings.Cut(route, " ")
	for _, cred := range []string{l.token, adminKey} {
		// Every request here comes from one address, whose bucket allows 1,000 a minute; the fuzz sends
		// thousands, so it clears Lens's counters first and treats any 429 as a route it failed to reach.
		ctx := context.Background()
		keys, _ := l.redis.Keys(ctx, "lens:rl:*").Result()
		if len(keys) > 0 {
			l.redis.Del(ctx, keys...)
		}
		req, _ := http.NewRequest(method, l.base+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cred)
		req.Header.Set("X-Talyvor-Synthetic-Key", synthKey)
		req.Header.Set("X-Gateway-Auth", gateKey)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s on body %.80q: %v", route, body, err)
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		resp.Body.Close()
		limited := resp.StatusCode == http.StatusTooManyRequests && !strings.HasPrefix(path, "/v1/synthetic/")
		if resp.StatusCode == http.StatusInternalServerError || limited {
			t.Fatalf("%s answered %d on body %.80q: %.300s", route, resp.StatusCode, body, b.String())
		}
		if m := leak.FindString(b.String()); m != "" {
			t.Fatalf("%s answered %d carrying %q on body %.80q: %s", route, resp.StatusCode, m, body, fmt.Sprintf("%.300s", b.String()))
		}
	}
}

func FuzzEveryWriteRoute(f *testing.F) {
	l := startLens(f)
	for i := range l.routes {
		for _, s := range seeds {
			f.Add(uint16(i), []byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, at uint16, body []byte) {
		l.check(t, l.routes[int(at)%len(l.routes)], body)
	})
}
