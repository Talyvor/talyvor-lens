package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// B30.4 — the owner climbs the verification levels through the real routes: an identity check before the email and
// phone are confirmed is 409; the email and phone check makes the workspace L1; the identity check makes it L2 —
// on the Test provider, so live money still reads L0; the record lists both checks; an agent's key is 403.
func TestVerificationRoutes_TheOwnerReachesL2OnTheTestProvider(t *testing.T) {
	pool := agentRoutesDB(t)
	r := chi.NewRouter()
	kyc := &partners.TestKYCProvider{}
	mountVerificationRoutes(r, economy.NewDualTokenStore(nil, pool, nil), partners.OneVerifier(kyc))
	caller := &auth.AuthContext{WorkspaceID: "ws-b304", AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/v1/workspaces/ws-b304/verification"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), caller))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	identity := `{"name": "Ada Lovelace", "country": "GB", "date_of_birth": "1990-12-10"}`

	if code, out := call(http.MethodPost, "/identity", identity); code != http.StatusConflict {
		t.Fatalf("an identity check before L1 = %d %v; want 409", code, out)
	}
	if code, out := call(http.MethodPost, "/contact", `{"email": "ada@example.com"}`); code != http.StatusBadRequest {
		t.Fatalf("an email and phone check without the phone = %d %v; want 400", code, out)
	}
	code, out := call(http.MethodPost, "/contact", `{"email": "ada@example.com", "phone": "+447700900123"}`)
	if v, _ := out["verification"].(map[string]any); code != http.StatusCreated || v["level"] != "L1" {
		t.Fatalf("the email and phone check = %d %v; want 201 at L1", code, out)
	}
	code, out = call(http.MethodPost, "/identity", identity)
	check, _ := out["check"].(map[string]any)
	if v, _ := out["verification"].(map[string]any); code != http.StatusCreated || v["level"] != "L2" || v["live_level"] != "L0" ||
		check["status"] != "completed" || check["test"] != true || check["evidence_ref"] == "" || check["verified_name"] != "Ada Lovelace" {
		t.Fatalf("the identity check = %d %v; want 201 at L2, live L0, passed by the Test provider", code, out)
	}
	code, out = call(http.MethodGet, "", "")
	if checks, _ := out["checks"].([]any); code != http.StatusOK || out["level"] != "L2" || len(checks) != 2 {
		t.Fatalf("the record = %d %v; want L2 and both checks", code, out)
	}

	caller = &auth.AuthContext{WorkspaceID: "ws-b304", AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "agent"}
	if code, _ := call(http.MethodGet, "", ""); code != http.StatusForbidden {
		t.Fatalf("an agent's key reading the verification = %d; want 403", code)
	}
}

// B30.112 — with Persona's and Companies House's keys set, the owner's identity check is a Persona inquiry: the
// check answers pending with the one-time link the owner completes it at, the record carries a fresh link while it
// waits, and once Persona approves it the workspace is L2 — a Sandbox pass, so live money still reads L0. The company
// check must name that person among the company's directors or people with significant control; it then reads the
// Companies House register and makes the workspace L3, a pass that counts live. Both providers
// are local mocks of their published APIs.
func TestVerificationRoutes_PersonaAndCompaniesHouseCheckTheOwner(t *testing.T) {
	pool := agentRoutesDB(t)
	status := "created"
	persona := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/inquiries":
			_, _ = w.Write([]byte(`{"data":{"type":"inquiry","id":"inq_ada","attributes":{"status":"created"}}}`))
		case r.URL.Path == "/api/v1/inquiries/inq_ada/generate-one-time-link":
			_, _ = w.Write([]byte(`{"meta":{"one-time-link":"https://withpersona.com/verify?code=ada"}}`))
		case r.URL.Path == "/api/v1/inquiries/inq_ada":
			_, _ = w.Write([]byte(`{"data":{"type":"inquiry","id":"inq_ada","attributes":{"status":"` + status + `"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer persona.Close()
	companiesHouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/company/17299143":
			_, _ = w.Write([]byte(`{"company_name":"TALYVOR LTD","company_status":"active"}`))
		case "/company/17299143/officers":
			_, _ = w.Write([]byte(`{"items":[{"name":"LOVELACE, Ada","officer_role":"director"}]}`))
		default:
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	defer companiesHouse.Close()
	reg := partners.NewRegistry(nil)
	reg.UseVerifier(partners.KYCPerson, &partners.PersonaKYC{Key: "persona_sandbox_k", TemplateID: "itmpl_kyc", Base: persona.URL})
	reg.UseVerifier(partners.KYCCompany, &partners.CompaniesHouseKYC{Key: "ch_k", Base: companiesHouse.URL}, "GB")
	r := chi.NewRouter()
	mountVerificationRoutes(r, economy.NewDualTokenStore(nil, pool, nil), reg.Verification)
	caller := &auth.AuthContext{WorkspaceID: "ws-b30112", AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/v1/workspaces/ws-b30112/verification"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), caller))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	first := func(v map[string]any) map[string]any { c, _ := v["checks"].([]any)[0].(map[string]any); return c }

	if code, out := call(http.MethodPost, "/contact", `{"email": "ada@example.com", "phone": "+447700900123"}`); code != http.StatusCreated {
		t.Fatalf("the email and phone check = %d %v", code, out)
	}
	code, out := call(http.MethodPost, "/identity", `{"name": "Ada Lovelace", "country": "GB", "date_of_birth": "1990-12-10"}`)
	check, _ := out["check"].(map[string]any)
	if code != http.StatusCreated || check["method"] != "persona_sandbox" || check["status"] != "pending" || check["evidence_ref"] != "inq_ada" ||
		check["link"] != "https://withpersona.com/verify?code=ada&redirect-uri=https%3A%2F%2Fapp.talyvor.com%2Fsettings%2Fverification" {
		t.Fatalf("the identity check = %d %v; want 201, a pending Persona inquiry with its link", code, out)
	}
	if code, out = call(http.MethodGet, "", ""); code != http.StatusOK || out["level"] != "L1" || first(out)["link"] == nil {
		t.Fatalf("the record while the inquiry waits = %d %v; want L1 and the check's link", code, out)
	}
	status = "approved"
	if code, out = call(http.MethodGet, "", ""); code != http.StatusOK || out["level"] != "L2" || out["live_level"] != "L0" ||
		first(out)["status"] != "completed" || first(out)["test"] != true || first(out)["link"] != nil {
		t.Fatalf("the record once Persona approved = %d %v; want L2, live L0 (a Sandbox pass), no link", code, out)
	}
	if code, out = call(http.MethodPost, "/company", `{"name": "Talyvor Ltd", "country": "GB", "company_number": "17299143",
		"directors": ["Charles Babbage"]}`); code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out), "Ada Lovelace, whose identity was checked") {
		t.Fatalf("a company check not naming the person whose identity was checked = %d %v; want 400 naming them", code, out)
	}
	code, out = call(http.MethodPost, "/company", `{"name": "Talyvor Ltd", "country": "GB", "company_number": "17299143", "directors": ["Ada Lovelace"]}`)
	check, _ = out["check"].(map[string]any)
	if v, _ := out["verification"].(map[string]any); code != http.StatusCreated || v["level"] != "L3" || check["method"] != "companies_house" ||
		check["status"] != "completed" || check["test"] != false {
		t.Fatalf("the company check = %d %v; want 201 at L3, passed by Companies House", code, out)
	}
}
