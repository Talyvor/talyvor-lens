package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/areas"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// fixedAuthenticator answers requireAdmin with the caller the test names.
type fixedAuthenticator struct{ actx *auth.AuthContext }

func (f fixedAuthenticator) Authenticate(*http.Request) (*auth.AuthContext, error) {
	return f.actx, nil
}

func areasDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG areas test")
	}
	const schema = "cmd_areas_realpg"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema, `SET search_path = ` + schema + `,public`} {
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
	return pool
}

// B59.1 — every area is off for a workspace until the operator switches it on: one route of each area with routes
// in Lens answers 404 for a default workspace, the same as a route that is not there; once the operator switches
// the marketplace on for one workspace its route answers for that workspace only; a customer's admin key cannot
// switch it; launch-pack routes are untouched.
func TestAreas_AHiddenAreaAnswers404UntilTheOperatorSwitchesItOn(t *testing.T) {
	pool := areasDB(t)
	store := areas.NewStore(pool)

	// One route of each area, as Lens registers it, and one launch-pack route, behind the authed group's tenant
	// check and the gate in the order run() installs them; each answers 200 when it is reached.
	areaRoutes := map[areas.Area]string{
		areas.Marketplace: "/v1/marketplace/listings",
		areas.Rooms:       "/v1/rooms",
		areas.Chat:        "/v1/workspaces/{wsID}/chat-shares",
		areas.Scan:        "/v1/workspaces/{wsID}/scan-watches",
		areas.Counter:     "/v1/negotiations/{id}",
		areas.Trading:     "/v1/markets/simulated/quotes",
		areas.Credit:      "/v1/workspaces/{wsID}/loans",
		areas.Gateway:     "/v1/workspaces/{wsID}/stored-answers",
	}
	const launchRoute = "/v1/workspaces/{wsID}/agents"
	reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	var caller *auth.AuthContext
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), caller)))
			})
		})
		authed.Use(workspaceIsolationMiddleware)
		authed.Use(areaGate(store))
		for _, p := range areaRoutes {
			authed.Get(p, reached)
		}
		authed.Get(launchRoute, reached)
		mountAreaRoutes(authed, store)
	})
	operator := &auth.AuthContext{IsAdmin: true, AuthMethod: auth.MethodGlobalKey}
	customerAdmin := func(ws string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k-" + ws,
			Scopes: []string{auth.ScopeAdmin, auth.ScopeKeys}}
	}
	call := func(as *auth.AuthContext, path string) *httptest.ResponseRecorder {
		caller = as
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	pathFor := func(pattern, ws string) string {
		return strings.NewReplacer("{wsID}", ws, "{id}", "neg_1").Replace(pattern)
	}
	notThere := call(customerAdmin("ws-a"), "/v1/no-such-route")

	for a, p := range areaRoutes {
		w := call(customerAdmin("ws-a"), pathFor(p, "ws-a"))
		if w.Code != http.StatusNotFound || w.Body.String() != notThere.Body.String() {
			t.Errorf("%s: default workspace GET %s = %d %q; want the 404 a missing route gives, %q", a, p, w.Code, w.Body, notThere.Body)
		}
		if strings.Contains(strings.ToLower(w.Body.String()), string(a)) {
			t.Errorf("%s: the 404 names the area: %q", a, w.Body)
		}
	}
	if w := call(customerAdmin("ws-a"), pathFor(launchRoute, "ws-a")); w.Code != http.StatusOK {
		t.Fatalf("launch-pack route = %d %s; want 200, untouched", w.Code, w.Body)
	}

	// A customer's admin key cannot switch an area on, for its own workspace or any other.
	switchAs := func(as *auth.AuthContext, ws, area, body string) *httptest.ResponseRecorder {
		admin := chi.NewRouter()
		admin.Put("/v1/admin/workspaces/{wsID}/areas/{area}", requireAdmin(fixedAuthenticator{as}, newAreaSwitchHandler(store)))
		w := httptest.NewRecorder()
		admin.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/admin/workspaces/"+ws+"/areas/"+area, strings.NewReader(body)))
		return w
	}
	if w := switchAs(customerAdmin("ws-a"), "ws-a", "marketplace", `{"on":true,"actor":"ws-a"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("customer admin key switching its own area = %d %s; want 401", w.Code, w.Body)
	}
	if w := call(customerAdmin("ws-a"), "/v1/marketplace/listings"); w.Code != http.StatusNotFound {
		t.Fatalf("after the customer's attempt the marketplace answers %d; want 404", w.Code)
	}

	// The operator switches the marketplace on for ws-a: its route answers for ws-a, and only for ws-a.
	if w := switchAs(operator, "ws-a", "marketplace", `{"on":true,"actor":"nick@talyvor.com"}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"areas":["marketplace"]`) {
		t.Fatalf("operator switch = %d %s", w.Code, w.Body)
	}
	var actor, target, detail string
	if err := pool.QueryRow(context.Background(), `SELECT actor, target, detail FROM operator_audit WHERE action = $1`,
		areas.AuditAction).Scan(&actor, &target, &detail); err != nil {
		t.Fatalf("the switch is not in the operator audit trail: %v", err)
	}
	if actor != "nick@talyvor.com" || target != "ws-a/marketplace" || detail != "on" {
		t.Fatalf("audit row = %q %q %q", actor, target, detail)
	}
	if w := call(customerAdmin("ws-a"), "/v1/marketplace/listings"); w.Code != http.StatusOK {
		t.Fatalf("ws-a marketplace after the switch = %d %s; want 200", w.Code, w.Body)
	}
	if w := call(customerAdmin("ws-a"), "/v1/rooms"); w.Code != http.StatusNotFound {
		t.Fatalf("ws-a rooms after switching only the marketplace = %d; want 404", w.Code)
	}
	if w := call(customerAdmin("ws-b"), "/v1/marketplace/listings"); w.Code != http.StatusNotFound {
		t.Fatalf("ws-b marketplace after switching it on for ws-a = %d; want 404", w.Code)
	}

	// The read the web app and the phone use.
	if w := call(customerAdmin("ws-a"), "/v1/workspaces/ws-a/areas"); w.Code != http.StatusOK || w.Body.String() != `{"areas":["marketplace"]}`+"\n" {
		t.Fatalf("ws-a areas = %d %q", w.Code, w.Body)
	}
	if w := call(customerAdmin("ws-b"), "/v1/workspaces/ws-b/areas"); w.Code != http.StatusOK || w.Body.String() != `{"areas":[]}`+"\n" {
		t.Fatalf("ws-b areas = %d %q", w.Code, w.Body)
	}
}
