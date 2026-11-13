package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B59.40 — Lens keeps the sign-up profile: a workspace's country, and a business's registration number. Set on the
// call that makes the workspace, like its type; read back on GET /v1/workspaces/{wsID}/account.

// A new Business workspace reads back its country and registration number, through the store and the account route;
// a later sign-in naming others changes neither.
func TestB5940_BusinessWorkspaceKeepsItsCountryAndRegistrationNumber(t *testing.T) {
	mgr, pool := provisionManager(t)
	ctx := context.Background()
	bank := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountProvisionRoute(r, provisionSecret, mgr, nil, newKeyFixture(t).mint, nil)
	stamp := time.Now().Format("150405.000000")

	rec, out := doProvision(t, r, provisionSecret, `{"identity":"b5940-biz-`+stamp+`","display_name":"Acme Ltd",
		"account_type":"business","country":"gb","registration_number":"  12345678  "}`)
	if rec.Code != http.StatusOK || !out.Created {
		t.Fatalf("provision: %d %s", rec.Code, rec.Body.String())
	}
	want := economy.WorkspaceAccount{WorkspaceID: out.WorkspaceID, AccountType: "business", MoneyKind: "hybrid",
		Country: "GB", RegistrationNumber: "12345678"}
	if got, err := bank.WorkspaceAccount(ctx, out.WorkspaceID); err != nil || got != want {
		t.Fatalf("a new business workspace reads %+v (%v), want %+v", got, err, want)
	}

	acct := chi.NewRouter()
	mountWorkspaceMoneyKindRoutes(acct, bank)
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+out.WorkspaceID+"/account", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: out.WorkspaceID, AuthMethod: auth.MethodJWT,
		UserID: out.WorkspaceID, Scopes: []string{auth.ScopeKeys}}))
	got := httptest.NewRecorder()
	acct.ServeHTTP(got, req)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"country":"GB"`) ||
		!strings.Contains(got.Body.String(), `"registration_number":"12345678"`) {
		t.Fatalf("GET account = %d %s, want its country and registration number", got.Code, got.Body.String())
	}

	if rec, again := doProvision(t, r, provisionSecret, `{"identity":"b5940-biz-`+stamp+`","account_type":"business",
		"country":"FR","registration_number":"99999999"}`); rec.Code != http.StatusOK || again.Created {
		t.Fatalf("second sign-in: %d %s", rec.Code, rec.Body.String())
	}
	if got, err := bank.WorkspaceAccount(ctx, out.WorkspaceID); err != nil || got != want {
		t.Fatalf("after a second sign-in naming another country and number the workspace reads %+v (%v), want %+v", got, err, want)
	}
}

// A Personal workspace keeps its country; a registration number on a Personal one, a country that is not an ISO 3166-1
// alpha-2 code and a registration number over 64 characters are each a 400 that makes nothing.
func TestB5940_PersonalWorkspaceAndRefusedProfiles(t *testing.T) {
	mgr, pool := provisionManager(t)
	ctx := context.Background()
	bank := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountProvisionRoute(r, provisionSecret, mgr, nil, newKeyFixture(t).mint, nil)
	stamp := time.Now().Format("150405.000000")

	for name, body := range map[string]string{
		"personal with a registration number": `"account_type":"personal","country":"GB","registration_number":"12345678"`,
		"a country that is not a code":        `"account_type":"business","country":"Britain"`,
		"a registration number over 64":       `"account_type":"business","country":"GB","registration_number":"` + strings.Repeat("9", 65) + `"`,
	} {
		id := "b5940-bad-" + strings.ReplaceAll(name, " ", "-") + "-" + stamp
		if rec, _ := doProvision(t, r, provisionSecret, `{"identity":"`+id+`",`+body+`}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("provision %s = %d %s, want 400", name, rec.Code, rec.Body.String())
		}
		if n := workspaceRowCount(t, pool, deriveWorkspaceID(id)); n != 0 {
			t.Fatalf("a refused provision (%s) made %d workspaces", name, n)
		}
	}

	rec, out := doProvision(t, r, provisionSecret, `{"identity":"b5940-me-`+stamp+`","display_name":"Ana Pop","account_type":"personal","country":"MD"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("provision personal: %d %s", rec.Code, rec.Body.String())
	}
	want := economy.WorkspaceAccount{WorkspaceID: out.WorkspaceID, AccountType: "personal", MoneyKind: "hybrid", Country: "MD"}
	if got, err := bank.WorkspaceAccount(ctx, out.WorkspaceID); err != nil || got != want {
		t.Fatalf("a new personal workspace reads %+v (%v), want %+v", got, err, want)
	}
}
