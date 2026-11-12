package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// B59.30 — a Business workspace's details for the partner's identity flow: the company's name, registration number
// and country, and the people with control (name, role, share). Read and changed by the owner, signed in, alone; each
// change in the audit trail; a Personal workspace has none.

// b5930Router is the authed group as main.go mounts it: the caller's credential, the isolation middleware that binds
// {wsID} to it, then the business details and verification routes.
func b5930Router(t *testing.T, bank *economy.DualTokenStore, caller **auth.AuthContext) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), *caller)))
			})
		})
		authed.Use(workspaceIsolationMiddleware)
		mountBusinessDetailsRoutes(authed, bank)
		mountVerificationRoutes(authed, bank, partners.OneVerifier(&partners.TestKYCProvider{}))
	})
	return r
}

func b5930Owner(ws string) *auth.AuthContext {
	return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
}

const b5930Details = `{"company_name":" Acme Robotics Ltd ","registration_number":" 12345678 ","country":"gb","people":[
	{"name":"Ada Lovelace","role":"director","share_bps":6000},
	{"name":"Charles Babbage","role":"shareholder","share_bps":4000}]}`

// The owner saves and reads back the details; a member, an admin-scoped workspace key, an agent's key and another
// workspace's owner can neither read nor change them; a Personal workspace has no such record and cannot make one;
// the change is in the audit trail without the people's names.
func TestB5930_OnlyTheOwnerReadsAndChangesBusinessDetails(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000")
	wsA, wsB, wsP := "ws-b5930-a-"+stamp, "ws-b5930-b-"+stamp, "ws-b5930-p-"+stamp
	for ws, typ := range map[string]string{wsA: "business", wsB: "business", wsP: "personal"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, $2, 'fiat')`, ws, typ); err != nil {
			t.Fatal(err)
		}
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	var caller *auth.AuthContext
	r := b5930Router(t, bank, &caller)
	call := func(as *auth.AuthContext, method, ws, body string) (int, string) {
		t.Helper()
		caller = as
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, "/v1/workspaces/"+ws+"/business", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}

	code, body := call(b5930Owner(wsA), http.MethodPut, wsA, b5930Details)
	if code != http.StatusOK {
		t.Fatalf("the owner saving the details = %d %s; want 200", code, body)
	}
	code, body = call(b5930Owner(wsA), http.MethodGet, wsA, "")
	var got economy.BusinessDetails
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("the owner reading the details = %d %s", code, body)
	}
	want := []economy.BusinessPerson{{Name: "Ada Lovelace", Role: "director", ShareBps: 6000},
		{Name: "Charles Babbage", Role: "shareholder", ShareBps: 4000}}
	if got.CompanyName != "Acme Robotics Ltd" || got.RegistrationNumber != "12345678" || got.Country != "GB" ||
		len(got.People) != 2 || got.People[0] != want[0] || got.People[1] != want[1] {
		t.Fatalf("the details read back as %+v; want the company, its number, GB and both people in order", got)
	}
	if acct, err := bank.WorkspaceAccount(ctx, wsA); err != nil || acct.RegistrationNumber != "12345678" || acct.Country != "GB" {
		t.Fatalf("the workspace's own profile reads %+v (%v); want the number and country just saved", acct, err)
	}

	member := &auth.AuthContext{WorkspaceID: wsA, AuthMethod: auth.MethodJWT, UserID: "member-a"}
	adminKey := &auth.AuthContext{WorkspaceID: wsA, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k-admin", Scopes: []string{auth.ScopeAdmin}}
	agentKey := &auth.AuthContext{WorkspaceID: wsA, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k-agent", Scopes: []string{"proxy"}}
	for name, as := range map[string]*auth.AuthContext{"a member who is not the owner": member, "an admin-scoped workspace key": adminKey,
		"an agent's key": agentKey, "another workspace's owner": b5930Owner(wsB)} {
		if code, body := call(as, http.MethodGet, wsA, ""); code != http.StatusForbidden || strings.Contains(body, "Ada") {
			t.Errorf("%s reading the details = %d %s; want 403 and no names", name, code, body)
		}
		if code, body := call(as, http.MethodPut, wsA, `{"company_name":"Hijack Ltd","registration_number":"1","country":"GB","people":[]}`); code != http.StatusForbidden {
			t.Errorf("%s changing the details = %d %s; want 403", name, code, body)
		}
	}
	if code, body := call(b5930Owner(wsB), http.MethodGet, wsB, ""); code != http.StatusOK || strings.Contains(body, "Ada") || strings.Contains(body, "Acme") {
		t.Fatalf("another business reading its own details = %d %s; want 200 and nothing of the first", code, body)
	}
	if code, body = call(b5930Owner(wsA), http.MethodGet, wsA, ""); !strings.Contains(body, "Acme Robotics Ltd") {
		t.Fatalf("after the refused changes the details read %d %s; want them unchanged", code, body)
	}

	if code, body := call(b5930Owner(wsP), http.MethodGet, wsP, ""); code != http.StatusNotFound || !strings.Contains(body, `"code":"not_business"`) {
		t.Errorf("a Personal workspace's owner reading business details = %d %s; want 404 not_business", code, body)
	}
	if code, body := call(b5930Owner(wsP), http.MethodPut, wsP, b5930Details); code != http.StatusNotFound || !strings.Contains(body, `"code":"not_business"`) {
		t.Errorf("a Personal workspace's owner saving business details = %d %s; want 404 not_business", code, body)
	}
	var personalRows int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_business_details WHERE workspace_id = $1) +
		(SELECT count(*) FROM workspace_business_people WHERE workspace_id = $1)`, wsP).Scan(&personalRows); err != nil || personalRows != 0 {
		t.Fatalf("a Personal workspace holds %d business rows (%v); want none", personalRows, err)
	}

	for name, bad := range map[string]string{
		"shares over 100%":   `{"company_name":"Acme","registration_number":"1","country":"GB","people":[{"name":"A","role":"shareholder","share_bps":6000},{"name":"B","role":"shareholder","share_bps":5000}]}`,
		"an unknown role":    `{"company_name":"Acme","registration_number":"1","country":"GB","people":[{"name":"A","role":"king","share_bps":0}]}`,
		"no company name":    `{"company_name":" ","registration_number":"1","country":"GB","people":[]}`,
		"a country not ISO":  `{"company_name":"Acme","registration_number":"1","country":"Britain","people":[]}`,
		"a person unnamed":   `{"company_name":"Acme","registration_number":"1","country":"GB","people":[{"name":" ","role":"director","share_bps":0}]}`,
		"a negative share":   `{"company_name":"Acme","registration_number":"1","country":"GB","people":[{"name":"A","role":"director","share_bps":-1}]}`,
		"no registration no": `{"company_name":"Acme","registration_number":"","country":"GB","people":[]}`,
	} {
		if code, body := call(b5930Owner(wsA), http.MethodPut, wsA, bad); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s; want 400", name, code, body)
		}
	}

	var entries int
	var names bool
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(bool_or(body::text LIKE '%Lovelace%' OR body::text LIKE '%Babbage%'), false)
		FROM audit_chain WHERE workspace_id = $1 AND kind = 'workspace.business_details'`, wsA).Scan(&entries, &names); err != nil {
		t.Fatal(err)
	}
	if entries != 1 || names {
		t.Fatalf("the audit trail holds %d business details entries (names in them: %v); want the one change, without names", entries, names)
	}
}

// The company check (L3) takes what it is not given from the saved details: the company's name, number and country,
// its directors, and its people with control.
func TestB5930_TheCompanyCheckTakesTheBusinessDetails(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	ws := "ws-b5930-kyc-" + time.Now().Format("150405.000000")
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', 'fiat')`, ws); err != nil {
		t.Fatal(err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	caller := b5930Owner(ws)
	r := b5930Router(t, bank, &caller)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, "/v1/workspaces/"+ws+path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := call(http.MethodPut, "/business", b5930Details); code != http.StatusOK {
		t.Fatalf("saving the details = %d %v", code, out)
	}
	if code, out := call(http.MethodPost, "/verification/contact", `{"email":"ada@example.com","phone":"+447700900123"}`); code != http.StatusCreated {
		t.Fatalf("L1 = %d %v", code, out)
	}
	if code, out := call(http.MethodPost, "/verification/identity", `{"name":"Ada Lovelace","country":"GB","date_of_birth":"1990-12-10"}`); code != http.StatusCreated {
		t.Fatalf("L2 = %d %v", code, out)
	}
	code, out := call(http.MethodPost, "/verification/company", `{}`)
	check, _ := out["check"].(map[string]any)
	if v, _ := out["verification"].(map[string]any); code != http.StatusCreated || v["level"] != "L3" || check["company_number"] != "12345678" ||
		check["verified_name"] != "Acme Robotics Ltd" || check["country"] != "GB" {
		t.Fatalf("the company check with nothing given = %d %v; want 201 at L3 on the saved company", code, out)
	}
}
