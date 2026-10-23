package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// provision_handler_test.go — the tenancy boundary for POST /v1/provision.
//
// This route is what turns an authenticated session into a tenant, so it is treated as a money
// path. Two properties carry the weight and each has a dedicated test:
//
//  1. THE CALLER CANNOT NAME A WORKSPACE. The id is derived from the identity inside Lens
//     (deriveWorkspaceID). A caller that cannot express "workspace X" cannot target another
//     tenant even with a bug, so a body-supplied workspace_id must be inert.
//  2. CHECK-THEN-CREATE, NOT UPSERT. insertWorkspaceSQL's ON CONFLICT rewrites spend_limit_usd,
//     both allowlists, max_tokens_* and logging_policy from the body — only the three CONSENT
//     columns are preserved. So calling RegisterWorkspace on an EXISTING workspace silently
//     resets that tenant's spend cap and reverts their logging policy: the #129 regression
//     shape. TestProvision_SecondCallPreservesNonConsentSettings is the lock on that.

const provisionSecret = "test-provision-secret"

// provisionSchema / provisionSetupLock mirror the sibling real-PG harness
// (register_workspace_handler_test.go): tables live in a PRIVATE schema so this test never
// populates public.schema_migrations nor perturbs another package's shared-table assertions.
// The pgvector extension is still provisioned into PUBLIC under the shared advisory lock —
// the `vector` type is database-global and lands in the first schema of search_path.
const provisionSchema = "lens_it_provision"
const provisionSetupLock = 727274 // same lock as the peer harness: serialises extension/catalog DDL

var provisionMigrateOnce sync.Once

func provisionManager(t *testing.T) (*workspace.Manager, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG provisioning test")
	}
	ctx := context.Background()

	provisionMigrateOnce.Do(func() {
		admin, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for setup: %v", err)
		}
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin setup tx: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, provisionSetupLock); err != nil {
			t.Fatalf("advisory lock: %v", err)
		}
		if _, err := tx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
			t.Fatalf("create extension: %v", err)
		}
		if _, err := tx.Exec(ctx, `DROP SCHEMA IF EXISTS `+provisionSchema+` CASCADE`); err != nil {
			t.Fatalf("drop private schema: %v", err)
		}
		if _, err := tx.Exec(ctx, `CREATE SCHEMA `+provisionSchema); err != nil {
			t.Fatalf("create private schema: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit setup tx: %v", err)
		}
		_ = admin.Close(ctx)

		ccfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse migrate config: %v", err)
		}
		ccfg.RuntimeParams["search_path"] = provisionSchema + ",public"
		conn, err := pgx.ConnectConfig(ctx, ccfg)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = provisionSchema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return workspace.New(pool), pool
}

// provisionRouter builds the route exactly as run() mounts it, so the fail-closed
// mounting decision is under test rather than reimplemented here.
func provisionRouter(t *testing.T, secret string, p provisioner, key *ecdsaKeyFixture) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	mountProvisionRoute(r, secret, p, nil, key.mint)
	return r
}

// ecdsaKeyFixture holds a real EC P-256 signing key plus the Manager that verifies tokens minted
// with it — so "the token cannot reach an admin route" is proven against real signature validation,
// not a stub.
type ecdsaKeyFixture struct {
	mgr *auth.Manager
}

func newKeyFixture(t *testing.T) *ecdsaKeyFixture {
	t.Helper()
	key, err := auth.GenerateECKey()
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	// globalKey deliberately set: the admin credential EXISTS in this fixture, so a test that
	// shows the provisioned token is refused by requireAdmin is showing a real refusal and not
	// the absence of any admin path at all.
	return &ecdsaKeyFixture{mgr: auth.NewManager("the-global-admin-key", key, nil, nil)}
}

func (f *ecdsaKeyFixture) mint(workspaceID, userID string, scopes []string, ttl time.Duration) (string, error) {
	return auth.GenerateToken(workspaceID, userID, scopes, f.mgr.PrivateKey(), ttl)
}

type provisionResponseBody struct {
	WorkspaceID   string `json:"workspace_id"`
	Created       bool   `json:"created"`
	CachePoolable bool   `json:"cache_poolable"`
	Token         string `json:"token"`
	ExpiresAt     string `json:"expires_at"`
	FirstAgent    *struct {
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
		Key           string `json:"key"`
		TestMoneyULXC int64  `json:"test_money_ulxc"`
	} `json:"first_agent"`
}

func doProvision(t *testing.T, r chi.Router, secret, body string) (*httptest.ResponseRecorder, provisionResponseBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/provision", strings.NewReader(body))
	if secret != "" {
		req.Header.Set(provisionSecretHeader, secret)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out provisionResponseBody
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func workspaceRowCount(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM workspaces WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("count workspace rows: %v", err)
	}
	return n
}

// ─── 1. the gate ────────────────────────────────────────────────────────────

// A wrong, empty or absent shared secret must be refused BEFORE anything is provisioned —
// a 401 that still created the workspace would be a capability leak dressed as a refusal.
func TestProvision_GateRefusesBadSecret(t *testing.T) {
	mgr, pool := provisionManager(t)
	r := provisionRouter(t, provisionSecret, mgr, newKeyFixture(t))

	for _, c := range []struct{ name, sent string }{
		{"absent header", ""},
		{"wrong secret", "not-the-secret"},
		{"prefix of the real secret", provisionSecret[:8]},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, _ := doProvision(t, r, c.sent, `{"identity":"gate-probe@example.com"}`)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if n := workspaceRowCount(t, pool, deriveWorkspaceID("gate-probe@example.com")); n != 0 {
				t.Errorf("refused call still created %d workspace row(s) — the gate must precede provisioning", n)
			}
		})
	}
}

// ─── 2. fail closed: no secret ⇒ no capability ──────────────────────────────

// An unset LENS_PROVISION_SECRET must mean the route does not exist. The failure mode this
// forbids is the ordinary one: an empty configured secret compared against an empty header
// matches, and the endpoint silently becomes world-writable.
func TestProvision_RouteNotMountedWhenSecretUnset(t *testing.T) {
	r := chi.NewRouter()
	mountProvisionRoute(r, "", stubProvisioner{}, nil, newKeyFixture(t).mint)

	rec, _ := doProvision(t, r, "", `{"identity":"anyone"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — an unset secret must mean the route is absent, not open", rec.Code)
	}

	// And the same empty secret presented as a header must not authenticate either.
	rec2, _ := doProvision(t, r, "", `{"identity":"anyone"}`)
	if rec2.Code == http.StatusOK || rec2.Code == http.StatusCreated {
		t.Errorf("status = %d — empty-secret-matches-empty-header is the exact fail-open to avoid", rec2.Code)
	}
}

// ─── 3. THE FINDING-A LOCK ──────────────────────────────────────────────────

// A non-consent setting written BETWEEN two provision calls must survive the second.
//
// This is the whole reason the handler is check-then-create. RegisterWorkspace's ON CONFLICT
// rewrites spend_limit_usd / allowlists / max_tokens_* / logging_policy from the request body,
// and the provisioning body carries none of them — so a blind re-register on every login would
// silently zero a paying tenant's spend cap and revert their logging policy to the default.
func TestProvision_SecondCallPreservesNonConsentSettings(t *testing.T) {
	mgr, pool := provisionManager(t)
	r := provisionRouter(t, provisionSecret, mgr, newKeyFixture(t))

	const identity = "settings-keeper@example.com"
	wsID := deriveWorkspaceID(identity)
	if _, err := pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, wsID); err != nil {
		t.Fatalf("clean: %v", err)
	}

	rec, first := doProvision(t, r, provisionSecret, `{"identity":"`+identity+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first provision status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !first.Created {
		t.Fatalf("first provision created = false, want true")
	}

	// The tenant then acquires real configuration — a spend cap and a strict logging policy.
	if err := mgr.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: wsID, Name: "settings keeper", SpendLimitUSD: 250, LoggingPolicy: workspace.LoggingNone,
	}); err != nil {
		t.Fatalf("apply tenant settings: %v", err)
	}

	// Second login.
	rec2, second := doProvision(t, r, provisionSecret, `{"identity":"`+identity+`"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second provision status = %d, want 200", rec2.Code)
	}
	if second.Created {
		t.Errorf("second provision created = true, want false — the workspace already existed")
	}
	if second.WorkspaceID != first.WorkspaceID {
		t.Errorf("workspace_id changed between logins: %q then %q", first.WorkspaceID, second.WorkspaceID)
	}
	if n := workspaceRowCount(t, pool, wsID); n != 1 {
		t.Errorf("workspace rows = %d, want exactly 1", n)
	}

	var spend float64
	var logging string
	if err := pool.QueryRow(context.Background(),
		`SELECT spend_limit_usd, logging_policy FROM workspaces WHERE id=$1`, wsID).Scan(&spend, &logging); err != nil {
		t.Fatalf("read settings back: %v", err)
	}
	if spend != 250 {
		t.Errorf("spend_limit_usd = %v, want 250 — the second provision reset the tenant's spend cap", spend)
	}
	if logging != string(workspace.LoggingNone) {
		t.Errorf("logging_policy = %q, want %q — the second provision reverted the tenant's logging policy", logging, workspace.LoggingNone)
	}
}

// ─── 4. two identities are two tenants ──────────────────────────────────────

// Two identities must produce two workspaces, and the token minted for one must not authorise
// the other. The cross-read is proven through the REAL isolation middleware against a REAL
// signed token, not by inspecting the claim.
func TestProvision_TwoIdentitiesAreIsolated(t *testing.T) {
	mgr, pool := provisionManager(t)
	keys := newKeyFixture(t)
	r := provisionRouter(t, provisionSecret, mgr, keys)

	const idA, idB = "alice@example.com", "bob@example.com"
	for _, id := range []string{idA, idB} {
		if _, err := pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, deriveWorkspaceID(id)); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}

	_, a := doProvision(t, r, provisionSecret, `{"identity":"`+idA+`"}`)
	_, b := doProvision(t, r, provisionSecret, `{"identity":"`+idB+`"}`)

	if a.WorkspaceID == "" || b.WorkspaceID == "" {
		t.Fatalf("empty workspace id: a=%q b=%q", a.WorkspaceID, b.WorkspaceID)
	}
	if a.WorkspaceID == b.WorkspaceID {
		t.Fatalf("two identities collapsed to ONE workspace %q — every tenant would share a ledger", a.WorkspaceID)
	}

	// A's token reaching A's own workspace: allowed.
	if code := isolationProbe(t, keys, a.Token, a.WorkspaceID); code != http.StatusOK {
		t.Errorf("A reading A = %d, want 200 (isolation must not break legitimate access)", code)
	}
	// A's token reaching B's workspace: forbidden.
	if code := isolationProbe(t, keys, a.Token, b.WorkspaceID); code != http.StatusForbidden {
		t.Errorf("A reading B = %d, want 403 — one tenant can read another's ledger", code)
	}
	if code := isolationProbe(t, keys, b.Token, a.WorkspaceID); code != http.StatusForbidden {
		t.Errorf("B reading A = %d, want 403 — one tenant can read another's ledger", code)
	}
}

// isolationProbe drives a {wsID} route through the REAL workspaceIsolationMiddleware, with the
// auth context populated by the REAL manager validating the presented token — the same two steps
// the server performs.
//
// ⚠ The middlewares MUST be mounted inside r.Group, exactly as run() does at main.go:1782-1798.
// This is not stylistic. chi resolves URL params during route matching, which happens AFTER a
// TOP-LEVEL r.Use chain but BEFORE a Group's — so `chi.URLParam(r, "wsID")` reads "" in a
// top-level r.Use middleware and the isolation check silently skips every request. Verified
// against chi v5.2.5 directly: Group+Use sees "ws_b", top-level Use sees "". Mirroring the
// production mount is what makes this test a proof about the server rather than about a router
// assembled here.
func isolationProbe(t *testing.T, keys *ecdsaKeyFixture, token, targetWS string) int {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				actx, err := keys.mgr.Authenticate(req)
				if err != nil {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), actx)))
			})
		})
		authed.Use(workspaceIsolationMiddleware)
		authed.Get("/v1/workspaces/{wsID}/ledger", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+targetWS+"/ledger", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

// ─── 5. the pooling decline ─────────────────────────────────────────────────

// An explicit decline at creation must be stored as declined, and must not be revocable by a
// later login — including one that explicitly asks for true. Consent is created once; only the
// dedicated setter changes it.
func TestProvision_DeclineHonouredAndNeverRetroactivelyGranted(t *testing.T) {
	mgr, pool := provisionManager(t)
	r := provisionRouter(t, provisionSecret, mgr, newKeyFixture(t))

	const identity = "privacy-conscious@example.com"
	wsID := deriveWorkspaceID(identity)
	if _, err := pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, wsID); err != nil {
		t.Fatalf("clean: %v", err)
	}

	_, created := doProvision(t, r, provisionSecret, `{"identity":"`+identity+`","cache_poolable":false}`)
	if created.CachePoolable {
		t.Fatalf("cache_poolable = true at creation, want false — an explicit decline was ignored")
	}
	if got := provisionStoredPoolable(t, pool, wsID); got {
		t.Fatalf("stored cache_poolable = true, want false — the decline was not persisted")
	}

	// Silence on a later login must not flip it back to the default-on.
	_, silent := doProvision(t, r, provisionSecret, `{"identity":"`+identity+`"}`)
	if silent.CachePoolable || provisionStoredPoolable(t, pool, wsID) {
		t.Errorf("a later silent login re-enabled pooling on a workspace that declined")
	}

	// An explicit true on a later login must NOT retroactively grant consent.
	_, asked := doProvision(t, r, provisionSecret, `{"identity":"`+identity+`","cache_poolable":true}`)
	if asked.CachePoolable || provisionStoredPoolable(t, pool, wsID) {
		t.Errorf("a later login retroactively GRANTED pooling consent the tenant declined")
	}
}

// A new workspace that says nothing takes the default, and the response reports the state the
// workspace actually ended up in — so a client can render it instead of assuming.
func TestProvision_SilenceTakesDefaultAndResponseReportsRecordedState(t *testing.T) {
	mgr, pool := provisionManager(t)
	r := provisionRouter(t, provisionSecret, mgr, newKeyFixture(t))

	const identity = "quiet@example.com"
	wsID := deriveWorkspaceID(identity)
	if _, err := pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, wsID); err != nil {
		t.Fatalf("clean: %v", err)
	}

	_, out := doProvision(t, r, provisionSecret, `{"identity":"`+identity+`"}`)
	stored := provisionStoredPoolable(t, pool, wsID)
	if out.CachePoolable != stored {
		t.Errorf("response cache_poolable = %v but stored = %v — the response must report RECORDED consent", out.CachePoolable, stored)
	}
	if !stored {
		t.Errorf("silence stored cache_poolable = false, want the new-workspace default true")
	}
}

func provisionStoredPoolable(t *testing.T, pool *pgxpool.Pool, wsID string) bool {
	t.Helper()
	var b bool
	if err := pool.QueryRow(context.Background(),
		`SELECT cache_poolable FROM workspaces WHERE id=$1`, wsID).Scan(&b); err != nil {
		t.Fatalf("read cache_poolable: %v", err)
	}
	return b
}

// ─── 6. what the minted token can reach ─────────────────────────────────────

// The provisioned token is a tenant session credential. It must not be admin, and it must carry
// neither the admin nor the proxy scope: a session token that could run billable inference would
// let the BFF spend a tenant's balance without the tenant's own key.
func TestProvision_TokenIsNotAdminAndCarriesMinimalScopes(t *testing.T) {
	mgr, pool := provisionManager(t)
	keys := newKeyFixture(t)
	r := provisionRouter(t, provisionSecret, mgr, keys)

	const identity = "scope-probe@example.com"
	if _, err := pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, deriveWorkspaceID(identity)); err != nil {
		t.Fatalf("clean: %v", err)
	}
	_, out := doProvision(t, r, provisionSecret, `{"identity":"`+identity+`"}`)
	if out.Token == "" {
		t.Fatalf("no token minted")
	}

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	actx, err := keys.mgr.Authenticate(req)
	if err != nil {
		t.Fatalf("minted token does not authenticate: %v", err)
	}
	if actx.IsAdmin {
		t.Fatalf("minted token is ADMIN — it would bypass workspaceAuthorized for every tenant")
	}
	if actx.WorkspaceID != out.WorkspaceID {
		t.Errorf("token workspace = %q, want %q", actx.WorkspaceID, out.WorkspaceID)
	}
	for _, forbidden := range []string{auth.ScopeAdmin, auth.ScopeProxy} {
		for _, got := range actx.Scopes {
			if got == forbidden {
				t.Errorf("minted token carries the %q scope; provisioned sessions must not", forbidden)
			}
		}
	}

	// Behavioural proof, not just a claim inspection: the token is refused by the real gate.
	spy := &spyHandler{}
	rec := httptest.NewRecorder()
	requireAdmin(keys.mgr, spy)(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("requireAdmin with a provisioned token = %d, want 401", rec.Code)
	}
	if spy.called {
		t.Errorf("requireAdmin ran the admin handler for a provisioned tenant token")
	}
}

// ─── 7. the caller cannot name a workspace ──────────────────────────────────

// A body-supplied workspace_id must be inert. This is the property that makes a caller bug
// non-catastrophic: if the BFF cannot express "workspace X", it cannot aim at another tenant.
func TestProvision_CallerSuppliedWorkspaceIDIgnored(t *testing.T) {
	mgr, pool := provisionManager(t)
	r := provisionRouter(t, provisionSecret, mgr, newKeyFixture(t))

	const identity = "mallory@example.com"
	const victim = "u-victim-should-not-be-touched"
	want := deriveWorkspaceID(identity)
	for _, id := range []string{want, victim} {
		if _, err := pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, id); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}

	_, out := doProvision(t, r, provisionSecret,
		`{"identity":"`+identity+`","workspace_id":"`+victim+`","id":"`+victim+`"}`)

	if out.WorkspaceID != want {
		t.Errorf("workspace_id = %q, want the DERIVED %q — a caller-named workspace was honoured", out.WorkspaceID, want)
	}
	if n := workspaceRowCount(t, pool, victim); n != 0 {
		t.Errorf("caller-named workspace %q was created (%d rows) — the caller can name a tenant", victim, n)
	}
}

// ─── derivation ─────────────────────────────────────────────────────────────

// The derivation is the tenancy key: it must be deterministic, collision-resistant in shape, and
// restricted to a charset safe for the cache_prefix Lens builds from it ("ws:" + id + ":").
func TestDeriveWorkspaceID(t *testing.T) {
	a1 := deriveWorkspaceID("issuer\x00subject-A")
	a2 := deriveWorkspaceID("issuer\x00subject-A")
	b := deriveWorkspaceID("issuer\x00subject-B")

	if a1 != a2 {
		t.Errorf("not deterministic: %q vs %q — a user's workspace would change between logins", a1, a2)
	}
	if a1 == b {
		t.Errorf("two identities derived the same id %q", a1)
	}
	if !strings.HasPrefix(a1, "u") {
		t.Errorf("id %q lacks the 'u' prefix that keeps derived ids out of the reserved namespace", a1)
	}
	if a1 == "default" || strings.HasPrefix(a1, "default") {
		t.Errorf("derived id %q collides with the reserved bootstrap workspace", a1)
	}
	if len(a1) != 27 { // "u" + 26 base32 chars
		t.Errorf("id length = %d, want 27", len(a1))
	}
	for _, c := range a1[1:] {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			t.Errorf("id %q contains %q, outside base32-lower — unsafe for the cache_prefix", a1, c)
		}
	}
	// The identity must not be recoverable from the id: it is a one-way derivation, and the id
	// is handed to clients.
	if strings.Contains(a1, "subject-A") || strings.Contains(a1, "issuer") {
		t.Errorf("derived id %q leaks the identity it came from", a1)
	}
}

// An empty identity must be refused: deriving from "" would map every anonymous caller to one
// shared workspace — the exact failure this whole feature exists to end.
func TestProvision_EmptyIdentityRefused(t *testing.T) {
	mgr, pool := provisionManager(t)
	r := provisionRouter(t, provisionSecret, mgr, newKeyFixture(t))

	rec, _ := doProvision(t, r, provisionSecret, `{"identity":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if n := workspaceRowCount(t, pool, deriveWorkspaceID("")); n != 0 {
		t.Errorf("an empty identity provisioned a workspace — every anonymous caller would share it")
	}
}

// stubProvisioner satisfies the seam for the no-DB mounting test.
type stubProvisioner struct{}

func (stubProvisioner) GetWorkspace(string) (*workspace.Workspace, bool) { return nil, false }
func (stubProvisioner) RegisterWorkspace(context.Context, workspace.Workspace, ...workspace.RegisterOption) error {
	return nil
}
func (stubProvisioner) CachePoolableConsent(string) (bool, bool) { return false, false }

// B45.7: a first sign-in starts the new workspace with test money and a first agent whose key works; signing in
// again changes nothing.
func TestProvision_NewWorkspaceStartsWithTestMoneyAndFirstAgent(t *testing.T) {
	mgr, pool := provisionManager(t)
	ctx := context.Background()
	bank := economy.NewDualTokenStore(nil, pool, nil)
	const money = int64(25_000_000)
	r := chi.NewRouter()
	mountProvisionRoute(r, provisionSecret, mgr, newWorkspaceStarter(bank, money), newKeyFixture(t).mint)
	body := `{"identity":"b457-start-` + time.Now().Format("150405.000000") + `"}`

	rec, out := doProvision(t, r, provisionSecret, body)
	if rec.Code != http.StatusOK || !out.Created || out.FirstAgent == nil || out.FirstAgent.Key == "" || out.FirstAgent.TestMoneyULXC != money {
		t.Fatalf("first sign-in: %d %s", rec.Code, rec.Body.String())
	}
	if k, err := tenant.NewStore(pool).ValidateAPIKey(ctx, out.FirstAgent.Key); err != nil || k.WorkspaceID != out.WorkspaceID {
		t.Fatalf("the first agent's key does not authenticate to its workspace: %+v %v", k, err)
	}
	var funding string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT amount, metadata->>'funding' FROM lxc_ledger WHERE workspace_id = $1 AND type = $2`,
		out.WorkspaceID, economy.LXCTypeStarterTest).Scan(&amount, &funding); err != nil || amount != money || funding != economy.FundingTest {
		t.Fatalf("starter ledger row: %d %q %v; want %d test", amount, funding, err, money)
	}

	rec, again := doProvision(t, r, provisionSecret, body)
	if rec.Code != http.StatusOK || again.Created || again.FirstAgent != nil {
		t.Fatalf("second sign-in: %d %s, want no new agent", rec.Code, rec.Body.String())
	}
	var rows, agents int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1),
		(SELECT count(*) FROM agent_accounts WHERE workspace_id = $1)`, out.WorkspaceID).Scan(&rows, &agents); err != nil || rows != 1 || agents != 1 {
		t.Fatalf("after a second sign-in: %d ledger rows, %d agents (%v); want 1 and 1", rows, agents, err)
	}
}
