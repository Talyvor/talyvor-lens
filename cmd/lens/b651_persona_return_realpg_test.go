package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// B65.1 — a Persona identity check sends the person back to the app and can be started again. Every link Lens hands
// out carries a return address from Lens's own list, never one the caller sends; an expired inquiry is replaced by a
// new one on a second start; the check's detail is in plain words, never Persona's; and a synthetic workspace never
// calls Persona. Persona is a local mock of its published inquiry API.
func TestVerificationRoutes_PersonaSendsThePersonBackAndStartsAgain(t *testing.T) {
	pool := agentRoutesDB(t)
	var mu sync.Mutex
	statuses, calls := map[string]string{}, 0
	persona := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/inquiries/"), "/generate-one-time-link")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/inquiries":
			id = fmt.Sprintf("inq_%d", len(statuses)+1)
			statuses[id] = "created"
			_, _ = w.Write([]byte(`{"data":{"type":"inquiry","id":"` + id + `","attributes":{"status":"created"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/generate-one-time-link") && statuses[id] != "":
			if statuses[id] == "expired" {
				http.Error(w, `{"errors":[{"title":"Inquiry is expired"}]}`, http.StatusConflict)
				return
			}
			_, _ = w.Write([]byte(`{"meta":{"one-time-link":"https://inquiry.withpersona.com/verify?code=` + id + `"}}`))
		case r.Method == http.MethodGet && statuses[id] != "":
			_, _ = w.Write([]byte(`{"data":{"type":"inquiry","id":"` + id + `","attributes":{"status":"` + statuses[id] + `"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer persona.Close()
	reg := partners.NewRegistry(nil)
	reg.UseVerifier(partners.KYCPerson, &partners.PersonaKYC{Key: "persona_sandbox_k", TemplateID: "itmpl_kyc", Base: persona.URL})
	r := chi.NewRouter()
	mountVerificationRoutes(r, economy.NewDualTokenStore(nil, pool, nil), reg.Verification)
	call := func(ws, method, path, body string) (int, string, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/workspaces/"+ws+"/verification"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, w.Body.String(), out
	}
	returnOf := func(link any) string {
		t.Helper()
		u, err := url.Parse(fmt.Sprint(link))
		if err != nil || u.Host != "inquiry.withpersona.com" {
			t.Fatalf("the link %v is not Persona's page", link)
		}
		return u.Query().Get("redirect-uri")
	}
	newest := func(out map[string]any) map[string]any {
		c, _ := out["checks"].([]any)[0].(map[string]any)
		return c
	}
	const ws = "ws-b651"
	if code, body, _ := call(ws, http.MethodPost, "/contact", `{"email": "ada@example.com", "phone": "+447700900123"}`); code != http.StatusCreated {
		t.Fatalf("the email and phone check = %d %s", code, body)
	}
	const ada = `"name": "Ada Lovelace", "country": "GB", "date_of_birth": "1990-12-10"`

	// The link carries the return address Lens names for the place the check was started from.
	code, body, out := call(ws, http.MethodPost, "/identity", `{`+ada+`, "return_to": "phone"}`)
	check, _ := out["check"].(map[string]any)
	if code != http.StatusCreated || check["status"] != "pending" || returnOf(check["link"]) != "talyvor://settings/verification" {
		t.Fatalf("an identity check started on the phone = %d %s; want pending, its link back to the phone's verification step", code, body)
	}
	// A caller's own address is never used: the link goes back to Settings, Verification.
	code, body, out = call(ws, http.MethodPost, "/identity",
		`{`+ada+`, "return_to": "https://evil.example/take", "redirect_uri": "https://evil.example/take"}`)
	check, _ = out["check"].(map[string]any)
	if code != http.StatusCreated || returnOf(check["link"]) != "https://app.talyvor.com/settings/verification" || strings.Contains(body, "evil.example") {
		t.Fatalf("an identity check with the caller's own return address = %d %s; want the link back to Settings, Verification", code, body)
	}
	// The fresh link a read hands out goes back to where the reader asks, from the same list.
	code, body, out = call(ws, http.MethodGet, "?return_to=signup", "")
	if code != http.StatusOK || returnOf(newest(out)["link"]) != "https://app.talyvor.com/signup/confirm" {
		t.Fatalf("the record read from sign-up = %d %s; want the waiting check's link back to /signup/confirm", code, body)
	}
	if detail := fmt.Sprint(newest(out)["detail"]); detail != "Your ID check is waiting for you. Open the link to finish it." {
		t.Fatalf("a waiting check's detail = %q; want it in plain words", detail)
	}

	// Persona expires the inquiry: the check fails in plain words, and Persona's own status is kept for support.
	mu.Lock()
	statuses["inq_2"] = "expired"
	mu.Unlock()
	code, body, out = call(ws, http.MethodGet, "", "")
	if c := newest(out); code != http.StatusOK || c["evidence_ref"] != "inq_2" || c["status"] != "failed" || c["link"] != nil ||
		c["detail"] != "Your ID check expired. Start it again." {
		t.Fatalf("the record once the inquiry expired = %d %s; want it failed, no link, \"Your ID check expired. Start it again.\"", code, body)
	}
	if strings.Contains(body, "Persona:") || strings.Contains(body, "expired\"") {
		t.Fatalf("the record carries Persona's own words: %s", body)
	}
	var kept string
	if err := pool.QueryRow(context.Background(), `SELECT provider_status FROM workspace_verifications
		WHERE workspace_id = $1 AND evidence_ref = 'inq_2' ORDER BY id DESC LIMIT 1`, ws).Scan(&kept); err != nil || kept != "expired" {
		t.Fatalf("Persona's status kept for support = %q, %v; want expired", kept, err)
	}

	// Started again, the check is a new inquiry with its own link: the person is never stuck on the old one.
	code, body, out = call(ws, http.MethodPost, "/identity", `{`+ada+`, "return_to": "settings"}`)
	check, _ = out["check"].(map[string]any)
	if code != http.StatusCreated || check["evidence_ref"] != "inq_3" || check["status"] != "pending" ||
		returnOf(check["link"]) != "https://app.talyvor.com/settings/verification" || strings.Contains(body, "Persona:") {
		t.Fatalf("starting the expired check again = %d %s; want a new pending inquiry, inq_3, with its link", code, body)
	}

	// A synthetic workspace's checks stay on the Test provider: Persona is never called.
	if _, err := pool.Exec(context.Background(), `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`,
		"ws-b651-synthetic"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	before := calls
	mu.Unlock()
	call("ws-b651-synthetic", http.MethodPost, "/contact", `{"email": "ada@example.com", "phone": "+447700900123"}`)
	code, body, out = call("ws-b651-synthetic", http.MethodPost, "/identity", `{`+ada+`, "return_to": "phone"}`)
	check, _ = out["check"].(map[string]any)
	call("ws-b651-synthetic", http.MethodGet, "?return_to=phone", "")
	mu.Lock()
	after := calls
	mu.Unlock()
	if code != http.StatusCreated || check["method"] != "test" || after != before {
		t.Fatalf("a synthetic workspace's identity check = %d %s, Persona called %d times; want the Test provider, Persona never called",
			code, body, after-before)
	}
}
