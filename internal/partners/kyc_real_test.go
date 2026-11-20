package partners

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// personaMock is Persona's published inquiry API, as far as the KYC adapter uses it: create an inquiry from a
// template, generate its one-time link, read its status.
func personaMock(t *testing.T, status *string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer persona_sandbox_k" || r.Header.Get("Persona-Version") == "" {
			http.Error(w, `{"errors":[{"title":"Unauthorized"}]}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/inquiries":
			var in struct {
				Data struct {
					Attributes struct {
						Template string            `json:"inquiry-template-id"`
						Fields   map[string]string `json:"fields"`
					} `json:"attributes"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if a := in.Data.Attributes; a.Template != "itmpl_kyc" || a.Fields["name-first"] != "Ada" || a.Fields["name-last"] != "Lovelace" ||
				a.Fields["birthdate"] != "1990-12-10" || a.Fields["address-street-1"] != "12 Marlborough Road" ||
				a.Fields["address-city"] != "Twickenham" || a.Fields["address-postal-code"] != "TW1 3AA" || a.Fields["address-country-code"] != "GB" {
				http.Error(w, `{"errors":[{"title":"bad inquiry"}]}`, http.StatusUnprocessableEntity)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"type":"inquiry","id":"inq_1","attributes":{"status":"created"}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/inquiries/inq_1/generate-one-time-link":
			_, _ = w.Write([]byte(`{"data":{"type":"inquiry","id":"inq_1"},"meta":{"one-time-link":"https://withpersona.com/verify?code=one"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/inquiries/inq_1":
			_, _ = w.Write([]byte(`{"data":{"type":"inquiry","id":"inq_1","attributes":{"status":"` + *status + `"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// companiesHouseMock is the Companies House public data API for one company, 01234567: its profile, officers and
// people with significant control.
func companiesHouseMock(t *testing.T, status *string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "ch_k" || pass != "" {
			http.Error(w, `{"error":"Invalid Authorization"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/company/01234567":
			_, _ = w.Write([]byte(`{"company_name":"ACME WIDGETS LIMITED","company_number":"01234567","company_status":"` + *status + `"}`))
		case "/company/01234567/officers":
			_, _ = w.Write([]byte(`{"items":[{"name":"LOVELACE, Augusta Ada","officer_role":"director"},
				{"name":"BABBAGE, Charles","officer_role":"director","resigned_on":"2025-01-01"},
				{"name":"SOMERVILLE, Mary","officer_role":"secretary"}]}`))
		case "/company/01234567/persons-with-significant-control":
			_, _ = w.Write([]byte(`{"items":[{"name":"Mrs Augusta Ada Lovelace","name_elements":{"title":"Mrs","surname":"Lovelace"}},
				{"name":"Mr Charles Babbage","name_elements":{"surname":"Babbage"},"ceased_on":"2025-01-01"}]}`))
		default:
			http.Error(w, `{"errors":[{"error":"company-profile-not-found"}]}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// B30.112 — a Persona identity check is an inquiry from the KYC template, prefilled with the person's name, date
// of birth and home address (B61.45), that the person completes at its one-time link; it passes when Persona approves it and fails when
// Persona declines it. A Sandbox key names the method persona_sandbox.
func TestPersonaKYC_ThePersonCompletesTheInquiryAtItsLink(t *testing.T) {
	ctx, status := context.Background(), "created"
	p := &PersonaKYC{Key: "persona_sandbox_k", TemplateID: "itmpl_kyc", Base: personaMock(t, &status).URL}
	if p.Name() != "persona_sandbox" {
		t.Fatalf("a Sandbox key's method = %q; want persona_sandbox", p.Name())
	}
	res, err := p.StartCheck(ctx, KYCRequest{ID: "kyc_1", Subject: KYCPerson, Name: "Ada Lovelace", Country: "GB", DateOfBirth: "1990-12-10",
		Address: &KYCAddress{Lines: []string{"12 Marlborough Road"}, Town: "Twickenham", Postcode: "TW1 3AA", Country: "GB"}})
	if err != nil || res.Ref != "inq_1" || res.Status != StatusPending || res.Link != "https://withpersona.com/verify?code=one" {
		t.Fatalf("starting the check = %+v, %v; want inq_1 pending with its one-time link", res, err)
	}
	for persona, want := range map[string]Status{"pending": StatusPending, "approved": StatusCompleted, "declined": StatusFailed} {
		status = persona
		if res, err := p.CheckResult(ctx, "inq_1"); err != nil || res.Status != want || (want == StatusPending) != (res.Link != "") {
			t.Fatalf("an inquiry Persona has %s = %+v, %v; want %s, with a link only while pending", persona, res, err, want)
		}
	}
	if _, err := p.StartCheck(ctx, KYCRequest{ID: "kyc_2", Subject: KYCCompany, Name: "Acme"}); err == nil {
		t.Fatal("Persona was asked to check a company; want it refused")
	}
}

// B30.112 — a Companies House check passes when the company is active under the name given and the directors and
// people with significant control named are exactly its current ones; it fails naming what differs; and a pass is
// withdrawn when the company is no longer active.
func TestCompaniesHouseKYC_ChecksTheCompanyAgainstTheRegister(t *testing.T) {
	ctx, status := context.Background(), "active"
	c := &CompaniesHouseKYC{Key: "ch_k", Base: companiesHouseMock(t, &status).URL}
	check := func(name, number string, directors, pscs []string) Result {
		t.Helper()
		res, err := c.StartCheck(ctx, KYCRequest{ID: "kyc_" + name, Subject: KYCCompany, Name: name, Country: "GB", CompanyNumber: number,
			Directors: directors, SignificantControl: pscs})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	ada := []string{"Ada Lovelace"}
	pass := check("Acme Widgets Ltd", "1234567", ada, ada)
	if pass.Status != StatusCompleted {
		t.Fatalf("the check of a company as it is on the register = %+v; want passed", pass)
	}
	for why, res := range map[string]Result{
		"Charles Babbage is not a current director":                                                check("Acme Widgets Ltd", "01234567", []string{"Ada Lovelace", "Charles Babbage"}, ada),
		"the register lists Mrs Augusta Ada Lovelace as a current person with significant control": check("Acme Widgets Ltd", "01234567", ada, nil),
		"company 01234567 is ACME WIDGETS LIMITED, not Acme Gadgets Ltd":                           check("Acme Gadgets Ltd", "01234567", ada, ada),
		"there is no company 07654321":                                                             check("Acme Widgets Ltd", "07654321", ada, ada),
	} {
		if res.Status != StatusFailed || !strings.Contains(res.Detail, why) {
			t.Errorf("check = %+v; want failed: %s", res, why)
		}
		if again, err := c.CheckResult(ctx, res.Ref); err != nil || again.Status != StatusFailed {
			t.Errorf("asked again about a failed check = %+v, %v; want failed", again, err)
		}
	}
	if again, err := c.CheckResult(ctx, pass.Ref); err != nil || again.Status != StatusCompleted {
		t.Fatalf("asked again about the pass = %+v, %v; want passed", again, err)
	}
	status = "dissolved"
	if again, err := c.CheckResult(ctx, pass.Ref); err != nil || again.Status != StatusReturned {
		t.Fatalf("asked about the pass once the company is dissolved = %+v, %v; want returned", again, err)
	}
}

// B30.112 — each check goes to the provider set for its subject and country, a check no real provider takes to the
// Test provider, and every check of a synthetic workspace to the Test provider.
func TestRegistry_VerificationSendsEachCheckToItsProvider(t *testing.T) {
	r := NewRegistry(nil)
	r.UseVerifier(KYCPerson, &PersonaKYC{Key: "persona_sandbox_k"})
	r.UseVerifier(KYCCompany, &CompaniesHouseKYC{}, "GB")
	for _, c := range []struct {
		subject, country string
		synthetic        bool
		want             string
	}{
		{KYCContact, "", false, "test"},
		{KYCPerson, "FR", false, "persona_sandbox"},
		{KYCCompany, "GB", false, "companies_house"},
		{KYCCompany, "FR", false, "test"},
		{KYCPerson, "GB", true, "test"},
		{KYCCompany, "GB", true, "test"},
	} {
		if got := r.Verification(c.subject, c.country, c.synthetic).Name(); got != c.want {
			t.Errorf("a %s check in %s (synthetic %v) went to %s; want %s", c.subject, c.country, c.synthetic, got, c.want)
		}
	}
}
