package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/sso"
)

// B56.18 — a Business workspace saves its identity provider and reads it back without the secret; each domain stays
// unproven, and the BFF is told of no connection for it, until its TXT record answers the workspace's value; then
// the BFF, and only the BFF, gets the secret. Free is refused in words; a domain another workspace proved is refused.
// On a migrated schema through the real handlers, asserted on the rows.
func TestB5618_SSOConnection(t *testing.T) {
	pool := b2613DB(t)
	ctx := context.Background()
	ring, err := envelope.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := sso.New(pool, ring)
	txt := map[string][]string{}
	store.LookupTXT = func(_ context.Context, name string) ([]string, error) { return txt[name], nil }
	for ws, plan := range map[string]string{"ws-b5618-biz": "business", "ws-b5618-biz2": "business"} {
		if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id,
			price_id, status, livemode, last_event_at, plan) VALUES ($1, 'sub_' || $1, 'cus_' || $1, 'price_' || $2, 'active', false, NOW(), $2)`,
			ws, plan); err != nil {
			t.Fatal(err)
		}
	}

	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				who := &auth.AuthContext{WorkspaceID: chi.URLParam(req, "wsID"), AuthMethod: auth.MethodJWT, Scopes: []string{auth.ScopeKeys}}
				if req.Header.Get("X-Test-Key") != "" {
					who = &auth.AuthContext{WorkspaceID: who.WorkspaceID, AuthMethod: auth.MethodWorkspaceKey, Scopes: []string{auth.ScopeKeys}}
				}
				next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), who)))
			})
		})
		authed.Get("/v1/workspaces/{wsID}/sso", newSSOGetHandler(store))
		authed.Put("/v1/workspaces/{wsID}/sso", newSSOPutHandler(store))
		authed.Delete("/v1/workspaces/{wsID}/sso", newSSODeleteHandler(store))
		authed.Post("/v1/workspaces/{wsID}/sso/domains/{domain}/check", newSSOCheckHandler(store))
	})
	mountSSOSignInRoute(r, "gw-secret", store)
	do := func(method, path, body string, header ...string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	const secret = "s3cret-client-value-b5618"
	save := `{"issuer":"https://login.acme-b5618.example/","client_id":"lens-app","client_secret":"` + secret + `","domains":["Acme-B5618.example"]}`

	// Free is refused in words naming the plan that has it.
	if code, body := do("PUT", "/v1/workspaces/ws-b5618-free/sso", save); code != http.StatusPaymentRequired ||
		!strings.Contains(body, "Free does not include single sign-on. Business includes it.") {
		t.Fatalf("Free PUT = %d %s", code, body)
	}
	// A workspace key is not the owner signed in.
	if code, body := do("PUT", "/v1/workspaces/ws-b5618-biz/sso", save, "X-Test-Key", "1"); code != http.StatusForbidden {
		t.Fatalf("workspace-key PUT = %d %s", code, body)
	}

	// Business saves, and reads back without the secret.
	if code, body := do("PUT", "/v1/workspaces/ws-b5618-biz/sso", save); code != http.StatusOK || strings.Contains(body, secret) {
		t.Fatalf("Business PUT = %d %s", code, body)
	}
	code, body := do("GET", "/v1/workspaces/ws-b5618-biz/sso", "")
	var got sso.Connection
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || strings.Contains(body, secret) ||
		got.Issuer != "https://login.acme-b5618.example" || got.ClientID != "lens-app" || len(got.Domains) != 1 ||
		got.Domains[0].Domain != "acme-b5618.example" || got.Domains[0].Record != "_talyvor-sso.acme-b5618.example" ||
		!strings.HasPrefix(got.Domains[0].Value, "talyvor-sso=") || got.Domains[0].ProvenAt != nil {
		t.Fatalf("GET = %d %s (%v)", code, body, err)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sso_connections
		WHERE workspace_id = 'ws-b5618-biz' AND position($1::bytea IN ciphertext) = 0`, []byte(secret)).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("sealed rows without the plaintext = %d (%v)", stored, err)
	}

	// Unproven: the BFF learns of no connection, and a check against a missing or wrong record leaves it unproven.
	signIn := `{"email":"Pat@ACME-b5618.example"}`
	if code, body := do("POST", "/v1/sso/sign-in", signIn, provisionSecretHeader, "gw-secret"); code != http.StatusNotFound {
		t.Fatalf("sign-in before proof = %d %s", code, body)
	}
	check := "/v1/workspaces/ws-b5618-biz/sso/domains/acme-b5618.example/check"
	txt[got.Domains[0].Record] = []string{"talyvor-sso=someone-else"}
	if code, body := do("POST", check, ""); code != http.StatusConflict || !strings.Contains(body, got.Domains[0].Value) {
		t.Fatalf("check, wrong record = %d %s", code, body)
	}
	var proven int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sso_domains WHERE proven_at IS NOT NULL`).Scan(&proven); err != nil || proven != 0 {
		t.Fatalf("proven domains = %d (%v), want 0", proven, err)
	}

	// The record answers: proven, and the BFF alone gets the secret.
	txt[got.Domains[0].Record] = []string{"v=spf1 -all", got.Domains[0].Value}
	if code, body := do("POST", check, ""); code != http.StatusOK || !strings.Contains(body, `"proven_at":"`) {
		t.Fatalf("check, right record = %d %s", code, body)
	}
	if code, body := do("POST", "/v1/sso/sign-in", signIn); code != http.StatusUnauthorized || strings.Contains(body, secret) {
		t.Fatalf("sign-in without the provision secret = %d %s", code, body)
	}
	code, body = do("POST", "/v1/sso/sign-in", signIn, provisionSecretHeader, "gw-secret")
	var in sso.SignIn
	if err := json.Unmarshal([]byte(body), &in); code != http.StatusOK || err != nil || in != (sso.SignIn{WorkspaceID: "ws-b5618-biz",
		Issuer: "https://login.acme-b5618.example", ClientID: "lens-app", ClientSecret: secret}) {
		t.Fatalf("sign-in = %d %s (%v)", code, body, err)
	}

	// A domain one workspace proved, another cannot claim.
	if code, body := do("PUT", "/v1/workspaces/ws-b5618-biz2/sso", save); code != http.StatusConflict {
		t.Fatalf("second claim = %d %s", code, body)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sso_connections WHERE workspace_id = 'ws-b5618-biz2'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("second workspace's connection rows = %d (%v), want 0", rows, err)
	}

	// Removing the connection takes its domains with it.
	if code, body := do("DELETE", "/v1/workspaces/ws-b5618-biz/sso", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", code, body)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sso_domains`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("domain rows after removal = %d (%v), want 0", rows, err)
	}
}
