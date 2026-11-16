package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// B61.53 — sign-up refuses Russia, Belarus and a country outside the list Talyvor serves, making nothing, with a code
// the apps read and one sentence; a listed country still opens an account. An account that already exists still signs
// in, whatever country the call names. The list is served on GET /v1/public/countries.
func TestB6153_SignUpRefusesACountryTalyvorDoesNotServe(t *testing.T) {
	mgr, pool := provisionManager(t)
	r := chi.NewRouter()
	mountProvisionRoute(r, provisionSecret, mgr, nil, newKeyFixture(t).mint)
	stamp := time.Now().Format("150405.000000")

	for _, country := range []string{"RU", "by", "KZ"} {
		id := "b6153-" + country + "-" + stamp
		rec, _ := doProvision(t, r, provisionSecret, `{"identity":"`+id+`","account_type":"personal","country":"`+country+`"}`)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out["code"] != economy.CountryNotServedCode || out["error"] != economy.CountryNotServedSentence {
			t.Fatalf("sign-up in %s = %d %s; want 400 %s", country, rec.Code, rec.Body.String(), economy.CountryNotServedCode)
		}
		if n := workspaceRowCount(t, pool, deriveWorkspaceID(id)); n != 0 {
			t.Fatalf("a refused sign-up in %s made %d workspaces", country, n)
		}
	}

	id := "b6153-gb-" + stamp
	if rec, out := doProvision(t, r, provisionSecret, `{"identity":"`+id+`","account_type":"personal","country":"GB"}`); rec.Code != http.StatusOK || !out.Created {
		t.Fatalf("sign-up in GB = %d %s; want an account", rec.Code, rec.Body.String())
	}
	if rec, out := doProvision(t, r, provisionSecret, `{"identity":"`+id+`","account_type":"personal","country":"RU"}`); rec.Code != http.StatusOK || out.Created {
		t.Fatalf("signing in to an existing account naming RU = %d %s; want the sign-in, nothing made", rec.Code, rec.Body.String())
	}
	if rec, _ := doProvision(t, r, provisionSecret, `{"identity":"`+id+`","mode":"open","account_type":"personal","country":"RU"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), economy.CountryNotServedCode) {
		t.Fatalf("opening another account in RU = %d %s; want 400 %s", rec.Code, rec.Body.String(), economy.CountryNotServedCode)
	}

	pub := chi.NewRouter()
	pub.Get("/v1/public/countries", publicCountriesHandler)
	w := httptest.NewRecorder()
	pub.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/public/countries", nil))
	var list struct {
		Countries []string `json:"countries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK || len(list.Countries) != 44 ||
		!slices.Contains(list.Countries, "GB") || slices.Contains(list.Countries, "RU") {
		t.Fatalf("GET /v1/public/countries = %d %s; want the 44 countries Talyvor serves", w.Code, w.Body.String())
	}
}

// The identity check route answers a country Talyvor does not serve with the same code and sentence, recording nothing.
func TestB6153_TheIdentityCheckRouteRefusesACountryTalyvorDoesNotServe(t *testing.T) {
	pool := agentRoutesDB(t)
	r := chi.NewRouter()
	bank := economy.NewDualTokenStore(nil, pool, nil)
	mountVerificationRoutes(r, bank, partners.OneVerifier(&partners.TestKYCProvider{}))
	ws := "ws-b6153-" + time.Now().Format("150405.000000")
	caller := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/verification"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), caller))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := call("/contact", `{"email": "ada@example.com", "phone": "+447700900123"}`); code != http.StatusCreated {
		t.Fatalf("the email and phone check = %d %v", code, out)
	}
	for _, country := range []string{"RU", "BY", "KZ"} {
		code, out := call("/identity", `{"name": "Ada Lovelace", "country": "`+country+`", "date_of_birth": "1990-12-10"}`)
		if code != http.StatusBadRequest || out["code"] != economy.CountryNotServedCode || out["error"] != economy.CountryNotServedSentence {
			t.Fatalf("an identity check in %s = %d %v; want 400 %s", country, code, out, economy.CountryNotServedCode)
		}
	}
	if code, out := call("/identity", `{"name": "Ada Lovelace", "country": "GB", "date_of_birth": "1990-12-10"}`); code != http.StatusCreated {
		t.Fatalf("an identity check in GB = %d %v; want 201", code, out)
	}
	v, err := bank.Verification(t.Context(), nil, ws)
	if err != nil || len(v.Checks) != 2 {
		t.Fatalf("the record holds %d checks (%v); want the email and phone check and GB's", len(v.Checks), err)
	}
}
