package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
)

// authz_routes_test.go proves the WIRING of the Phase-1 fix (#146) route by
// route, over HTTP against the REAL handler registered on a real chi router: a
// tenant-A credential attacking tenant B must never reach B's data/action. The
// assertions are against the downstream dependency (the keystore, exporter,
// engine, trades reader) — not just the HTTP status.

// withIdentity injects an authenticated identity onto a request exactly as
// AuthMiddleware would: a non-admin DB key on the APIKey slot, the global admin
// on the AuthContext slot.
func withIdentity(r *http.Request, workspace string, admin bool) *http.Request {
	ctx := r.Context()
	switch {
	case admin:
		ctx = auth.WithAuthContext(ctx, &auth.AuthContext{WorkspaceID: workspace, IsAdmin: true})
	case workspace != "":
		ctx = auth.WithAPIKey(ctx, &auth.APIKey{WorkspaceID: workspace})
	}
	return r.WithContext(ctx)
}

// serveAuthed registers the real handler on a real chi router at its real path
// and serves one request carrying the given identity — exercising the actual
// route registration + routing, not a reimplementation.
func serveAuthed(t *testing.T, method, pattern, target, body, workspace string, admin bool, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Method(method, pattern, h)
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := withIdentity(httptest.NewRequest(method, target, rdr), workspace, admin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ── GET /v1/audit/export ──────────────────────────────────────────────────

type fakeAuditExporter struct{ lastWS string }

func (f *fakeAuditExporter) Export(_ context.Context, filter audit.ExportFilter, _ audit.ExportFormat, _ io.Writer) (int, error) {
	f.lastWS = filter.WorkspaceID
	return 0, nil
}

func TestAuthz_AuditExport_Wiring(t *testing.T) {
	fake := &fakeAuditExporter{}
	h := newAuditExportHandler(fake)

	// ATTACK 1: no workspace_id → must scope to ws-A, NEVER all-tenants ("").
	serveAuthed(t, http.MethodGet, "/v1/audit/export", "/v1/audit/export", "", "ws-A", false, h)
	if fake.lastWS != "ws-A" {
		t.Fatalf("EXFIL: empty param exported %q, want ws-A (empty must never mean all-tenants for a tenant)", fake.lastWS)
	}

	// ATTACK 2: workspace_id=ws-B → forced to ws-A.
	fake.lastWS = ""
	serveAuthed(t, http.MethodGet, "/v1/audit/export", "/v1/audit/export?workspace_id=ws-B", "", "ws-A", false, h)
	if fake.lastWS != "ws-A" {
		t.Fatalf("EXFIL: exported %q, want ws-A", fake.lastWS)
	}

	// ADMIN regression: empty param = all-tenants ("") is admin-only.
	fake.lastWS = "sentinel"
	serveAuthed(t, http.MethodGet, "/v1/audit/export", "/v1/audit/export", "", "ws-admin", true, h)
	if fake.lastWS != "" {
		t.Fatalf("admin empty: exported %q, want \"\" (all-tenants)", fake.lastWS)
	}

	// HONEST-OWN.
	fake.lastWS = ""
	serveAuthed(t, http.MethodGet, "/v1/audit/export", "/v1/audit/export?workspace_id=ws-A", "", "ws-A", false, h)
	if fake.lastWS != "ws-A" {
		t.Fatalf("honest-own: exported %q, want ws-A", fake.lastWS)
	}
}

// ── PUT /v1/guardrails/policy ─────────────────────────────────────────────

func TestAuthz_GuardrailsPolicy_Wiring(t *testing.T) {
	eng := guardrails.New(pii.New(), injection.New(injection.DefaultPolicy()))
	h := newGuardrailsPolicyPutHandler(eng)

	// Seed ws-B's policy with a marker.
	_ = eng.SetPolicy(context.Background(), "ws-B", guardrails.GuardrailPolicy{WorkspaceID: "ws-B", BlockedWords: []string{"secret-B"}})

	// ATTACK: ws-A PUTs a policy targeting ws-B.
	rec := serveAuthed(t, http.MethodPut, "/v1/guardrails/policy", "/v1/guardrails/policy",
		`{"workspace_id":"ws-B","blocked_words":["attacker"]}`, "ws-A", false, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := eng.GetPolicy("ws-B").BlockedWords; !reflect.DeepEqual(got, []string{"secret-B"}) {
		t.Fatalf("OVERWRITE: ws-B policy mutated by ws-A — BlockedWords=%v, want [secret-B]", got)
	}
	// The attacker's policy landed on ITS OWN workspace instead.
	if got := eng.GetPolicy("ws-A").BlockedWords; !reflect.DeepEqual(got, []string{"attacker"}) {
		t.Fatalf("honest-own: ws-A policy=%v, want [attacker] (the write should land on the caller)", got)
	}

	// ADMIN regression: admin may write ws-B.
	serveAuthed(t, http.MethodPut, "/v1/guardrails/policy", "/v1/guardrails/policy",
		`{"workspace_id":"ws-B","blocked_words":["admin-set"]}`, "ws-admin", true, h)
	if got := eng.GetPolicy("ws-B").BlockedWords; !reflect.DeepEqual(got, []string{"admin-set"}) {
		t.Fatalf("admin: ws-B policy=%v, want [admin-set] (admin honored)", got)
	}
}

// ── GET /v1/marketplace/trades ────────────────────────────────────────────

type fakeTradesReader struct{ lastWS string }

func (f *fakeTradesReader) GetTrades(_ context.Context, workspaceID string, _ int) ([]economy.MarketplaceTrade, error) {
	f.lastWS = workspaceID
	return []economy.MarketplaceTrade{{ID: "t1", SellerID: workspaceID}}, nil
}

func TestAuthz_MarketplaceTrades_Wiring(t *testing.T) {
	fake := &fakeTradesReader{}
	h := newMarketplaceTradesHandler(fake)

	// ATTACK: ws-A reads ?workspace_id=ws-B → query must be for ws-A.
	rec := serveAuthed(t, http.MethodGet, "/v1/marketplace/trades", "/v1/marketplace/trades?workspace_id=ws-B", "", "ws-A", false, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if fake.lastWS != "ws-A" {
		t.Fatalf("LEAK: trades queried for %q, want ws-A (#144)", fake.lastWS)
	}

	// ADMIN regression.
	fake.lastWS = ""
	serveAuthed(t, http.MethodGet, "/v1/marketplace/trades", "/v1/marketplace/trades?workspace_id=ws-B", "", "ws-admin", true, h)
	if fake.lastWS != "ws-B" {
		t.Fatalf("admin: queried %q, want ws-B (honored)", fake.lastWS)
	}

	// HONEST-OWN.
	fake.lastWS = ""
	serveAuthed(t, http.MethodGet, "/v1/marketplace/trades", "/v1/marketplace/trades?workspace_id=ws-A", "", "ws-A", false, h)
	if fake.lastWS != "ws-A" {
		t.Fatalf("honest-own: queried %q, want ws-A", fake.lastWS)
	}
}
