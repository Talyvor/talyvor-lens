package partners

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// B30.112 — the real verification providers: Persona checks a person's identity (L2), Companies House a UK
// company (L3). Each is used once its key is in lens.env (Registry.UseVerifier); until then the Test provider
// checks.

// kycHTTP is the client both providers call with unless they are given one.
var kycHTTP = &http.Client{Timeout: 20 * time.Second}

// callJSON sends req with body as JSON and decodes a 2xx answer into out; it answers the HTTP status too.
func callJSON(client *http.Client, req *http.Request, body, out any) (int, error) {
	if client == nil {
		client = kycHTTP
	}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s answered %d: %.300s", req.Method, req.URL.Path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
		}
	}
	return resp.StatusCode, nil
}

// PersonaKYC checks a person's identity with Persona: an inquiry from the template of Persona's "KYC" solution — a
// government ID and a selfie — which the person completes themselves at the one-time link the check carries. A
// check still waiting for them carries a fresh link each time it is asked about.
//
// shortcut: the name and date of birth are prefilled and Persona's decision is taken as it is, not compared with
// the name on the ID, so economy counts every Persona pass for test money only; compare them before a production
// key's passes count live.
type PersonaKYC struct {
	Key        string // persona_sandbox_… or persona_production_…
	TemplateID string // itmpl_…
	Base       string // https://withpersona.com unless set
	HTTP       *http.Client
}

// Name is "persona", or "persona_sandbox" with a Sandbox key: what Persona's Sandbox passes counts for test money only.
func (p *PersonaKYC) Name() string {
	if strings.HasPrefix(p.Key, "persona_sandbox_") {
		return "persona_sandbox"
	}
	return "persona"
}

type personaInquiry struct {
	Data struct {
		ID         string `json:"id"`
		Attributes struct {
			Status string `json:"status"`
		} `json:"attributes"`
	} `json:"data"`
	Meta struct {
		OneTimeLink string `json:"one-time-link"`
	} `json:"meta"`
}

func (p *PersonaKYC) call(ctx context.Context, method, path string, body, out any, idempotencyKey string) (int, error) {
	base := p.Base
	if base == "" {
		base = "https://withpersona.com"
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/api/v1"+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Key)
	req.Header.Set("Persona-Version", "2023-01-05")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	code, err := callJSON(p.HTTP, req, body, out)
	if err != nil {
		err = fmt.Errorf("partners: persona: %w", err)
		switch code {
		case http.StatusNotFound:
			err = fmt.Errorf("%w: %v", ErrNotFound, err)
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			err = fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	return code, err
}

// personaStatus is where an inquiry in Persona's status stands as a check, and whether the person has still to act.
func personaStatus(s string) (Status, bool) {
	switch s {
	case "approved", "completed":
		return StatusCompleted, false
	case "declined", "failed", "expired":
		return StatusFailed, false
	case "created", "pending":
		return StatusPending, true
	}
	return StatusPending, false // needs_review, and any status Persona adds: decided later
}

// StartCheck creates the inquiry, its fields prefilled with the person's name, date of birth and home address, and answers it
// pending with the link the person completes it at.
func (p *PersonaKYC) StartCheck(ctx context.Context, req KYCRequest) (Result, error) {
	if req.Subject != KYCPerson {
		return Result{}, fmt.Errorf("%w: Persona checks a person's identity, not a %s", ErrInvalid, req.Subject)
	}
	if err := checkID(req.ID); err != nil {
		return Result{}, err
	}
	first, last, _ := strings.Cut(strings.TrimSpace(req.Name), " ")
	fields := map[string]string{"name-first": first, "name-last": strings.TrimSpace(last), "birthdate": req.DateOfBirth}
	if a := req.Address; a != nil && len(a.Lines) > 0 {
		fields["address-street-1"], fields["address-street-2"] = a.Lines[0], strings.Join(a.Lines[1:], ", ")
		fields["address-city"], fields["address-postal-code"], fields["address-country-code"] = a.Town, a.Postcode, a.Country
	}
	body := map[string]any{"data": map[string]any{"attributes": map[string]any{
		"inquiry-template-id": p.TemplateID, "reference-id": req.ID, "fields": fields}}}
	var inq personaInquiry
	if _, err := p.call(ctx, http.MethodPost, "/inquiries", body, &inq, req.ID); err != nil {
		return Result{}, err
	}
	return p.answer(ctx, inq)
}

// CheckResult reads the inquiry's status.
func (p *PersonaKYC) CheckResult(ctx context.Context, ref string) (Result, error) {
	if !strings.HasPrefix(ref, "inq_") {
		return Result{}, fmt.Errorf("%w: %q is not a Persona inquiry", ErrNotFound, ref)
	}
	var inq personaInquiry
	if _, err := p.call(ctx, http.MethodGet, "/inquiries/"+url.PathEscape(ref), nil, &inq, ""); err != nil {
		return Result{}, err
	}
	return p.answer(ctx, inq)
}

// answer is inq as a check: with a one-time link while the person has still to act.
func (p *PersonaKYC) answer(ctx context.Context, inq personaInquiry) (Result, error) {
	st, waiting := personaStatus(inq.Data.Attributes.Status)
	r := Result{Ref: inq.Data.ID, Status: st, Detail: "Persona: " + inq.Data.Attributes.Status}
	if !waiting {
		return r, nil
	}
	var link personaInquiry
	if _, err := p.call(ctx, http.MethodPost, "/inquiries/"+url.PathEscape(inq.Data.ID)+"/generate-one-time-link", nil, &link, ""); err != nil {
		return Result{}, err
	}
	r.Link = link.Meta.OneTimeLink
	r.Detail += " — the person completes it at the link"
	return r, nil
}

// CompaniesHouseKYC checks a UK company against the Companies House register: it is active, its registered name is
// the one given, and the directors and people with significant control given are exactly its current ones. It
// answers at once; asked again about a pass, it reads the company's status, and a company no longer active
// withdraws the pass.
type CompaniesHouseKYC struct {
	Key  string // a Companies House REST API key: HTTP Basic, the key as the user name and no password
	Base string // https://api.company-information.service.gov.uk unless set
	HTTP *http.Client
}

// Name is "companies_house".
func (*CompaniesHouseKYC) Name() string { return "companies_house" }

var companyNumber = regexp.MustCompile(`^[A-Z0-9]{1,8}$`)

func (c *CompaniesHouseKYC) get(ctx context.Context, path string, out any) (int, error) {
	base := c.Base
	if base == "" {
		base = "https://api.company-information.service.gov.uk"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(c.Key, "")
	code, err := callJSON(c.HTTP, req, nil, out)
	if err != nil && code != http.StatusNotFound {
		return code, fmt.Errorf("partners: companies house: %w", err)
	}
	return code, nil
}

type chProfile struct {
	Name   string `json:"company_name"`
	Status string `json:"company_status"`
}

// profile is the company's register entry; found is false when Companies House has no such company.
func (c *CompaniesHouseKYC) profile(ctx context.Context, number string) (p chProfile, found bool, err error) {
	code, err := c.get(ctx, "/company/"+number, &p)
	return p, code != http.StatusNotFound, err
}

// The ref of a check: "companies_house:<passed|failed>:<number>:<id>".
func chRef(passed bool, number, id string) string {
	return fmt.Sprintf("companies_house:%s:%s:%s", map[bool]string{true: "passed", false: "failed"}[passed], number, id)
}

// StartCheck checks the company on the register.
func (c *CompaniesHouseKYC) StartCheck(ctx context.Context, req KYCRequest) (Result, error) {
	if req.Subject != KYCCompany {
		return Result{}, fmt.Errorf("%w: Companies House checks a company, not a %s", ErrInvalid, req.Subject)
	}
	if err := checkID(req.ID); err != nil {
		return Result{}, err
	}
	number := strings.ToUpper(strings.ReplaceAll(req.CompanyNumber, " ", ""))
	if !companyNumber.MatchString(number) {
		return Result{}, fmt.Errorf("%w: a UK company number is up to 8 letters and digits, like 01234567, not %q", ErrInvalid, req.CompanyNumber)
	}
	if strings.Trim(number, "0123456789") == "" {
		number = fmt.Sprintf("%08s", number)
	}
	profile, found, err := c.profile(ctx, number)
	if err != nil {
		return Result{}, err
	}
	fail := func(why string) (Result, error) {
		return Result{Ref: chRef(false, number, req.ID), Status: StatusFailed, Detail: "Companies House: " + why}, nil
	}
	switch {
	case !found:
		return fail("there is no company " + number)
	case profile.Status != "active":
		return fail(fmt.Sprintf("%s (%s) is %s, not active", profile.Name, number, profile.Status))
	case companyWords(profile.Name) != companyWords(req.Name):
		return fail(fmt.Sprintf("company %s is %s, not %s", number, profile.Name, req.Name))
	}
	var officers struct {
		Items []struct {
			Name       string `json:"name"`
			Role       string `json:"officer_role"`
			ResignedOn string `json:"resigned_on"`
		} `json:"items"`
	}
	// shortcut: reads the first 100 officers and 100 people with significant control; page when a customer has more.
	if _, err := c.get(ctx, "/company/"+number+"/officers?items_per_page=100", &officers); err != nil {
		return Result{}, err
	}
	var pscs struct {
		Items []struct {
			Name         string `json:"name"`
			NameElements struct {
				Surname string `json:"surname"`
			} `json:"name_elements"`
			CeasedOn string `json:"ceased_on"`
		} `json:"items"`
	}
	if _, err := c.get(ctx, "/company/"+number+"/persons-with-significant-control?items_per_page=100", &pscs); err != nil {
		return Result{}, err
	}
	var directors, controllers []registered
	for _, o := range officers.Items {
		if o.ResignedOn == "" && strings.HasSuffix(o.Role, "director") {
			surname, _, comma := strings.Cut(o.Name, ",") // a person is "SURNAME, Forenames"
			if !comma {
				surname = ""
			}
			directors = append(directors, register(o.Name, surname))
		}
	}
	for _, p := range pscs.Items {
		if p.CeasedOn == "" {
			controllers = append(controllers, register(p.Name, p.NameElements.Surname))
		}
	}
	if why := sameNames("director", req.Directors, directors); why != "" {
		return fail(why)
	}
	if why := sameNames("person with significant control", req.SignificantControl, controllers); why != "" {
		return fail(why)
	}
	return Result{Ref: chRef(true, number, req.ID), Status: StatusCompleted,
		Detail: fmt.Sprintf("Companies House: %s (%s) is active, and its directors and people with significant control are the ones named", profile.Name, number)}, nil
}

// CheckResult answers a failed check failed, and a passed one by whether the company is still active.
func (c *CompaniesHouseKYC) CheckResult(ctx context.Context, ref string) (Result, error) {
	parts := strings.SplitN(ref, ":", 4)
	if len(parts) != 4 || parts[0] != "companies_house" || !companyNumber.MatchString(parts[2]) {
		return Result{}, fmt.Errorf("%w: %q is not a Companies House check", ErrNotFound, ref)
	}
	if parts[1] != "passed" {
		return Result{Ref: ref, Status: StatusFailed}, nil
	}
	profile, found, err := c.profile(ctx, parts[2])
	switch {
	case err != nil:
		return Result{}, err
	case !found || profile.Status != "active":
		return Result{Ref: ref, Status: StatusReturned, Detail: fmt.Sprintf("Companies House: company %s is no longer active", parts[2])}, nil
	}
	return Result{Ref: ref, Status: StatusCompleted}, nil
}

// registered is a name on the register, its words, and its surname when it is a person's.
type registered struct {
	name    string
	words   []string
	surname string
}

func register(name, surname string) registered {
	return registered{name: name, words: nameWords(name), surname: strings.Join(nameWords(surname), " ")}
}

// matches says whether a name given for the check is this one: a person's when it has their surname and every word
// of it is in theirs ("Ada Lovelace" is "LOVELACE, Augusta Ada"); a company's when it has the same words.
func (r registered) matches(given string) bool {
	if r.surname == "" {
		return companyWords(given) == companyWords(r.name)
	}
	g := nameWords(given)
	if !slices.Contains(g, r.surname) {
		return false
	}
	for _, w := range g {
		if !slices.Contains(r.words, w) {
			return false
		}
	}
	return true
}

// sameNames is why the names given are not exactly the ones on the register, as role; "" when they are.
func sameNames(role string, given []string, on []registered) string {
	for _, g := range given {
		if !slices.ContainsFunc(on, func(r registered) bool { return r.matches(g) }) {
			return fmt.Sprintf("%s is not a current %s", g, role)
		}
	}
	for _, r := range on {
		if !slices.ContainsFunc(given, r.matches) {
			return fmt.Sprintf("the register lists %s as a current %s, and the check does not name them", r.name, role)
		}
	}
	return ""
}

var titles = map[string]bool{"MR": true, "MRS": true, "MS": true, "MISS": true, "MX": true, "DR": true, "SIR": true, "DAME": true, "PROF": true}

// nameWords is a person's or company's name as upper-case words, without punctuation or a title.
func nameWords(name string) []string {
	name = strings.Map(func(r rune) rune {
		if r == ',' || r == '.' || r == '\'' || r == '(' || r == ')' {
			return ' '
		}
		return r
	}, strings.ToUpper(strings.ReplaceAll(name, "&", " AND ")))
	words := []string{}
	for _, w := range strings.Fields(name) {
		if !titles[w] {
			words = append(words, w)
		}
	}
	return words
}

// companyWords is a company's name to compare: its words, with LIMITED as LTD and PUBLIC LIMITED COMPANY as PLC.
func companyWords(name string) string {
	s := " " + strings.Join(nameWords(name), " ") + " "
	s = strings.ReplaceAll(s, " PUBLIC LIMITED COMPANY ", " PLC ")
	return strings.TrimSpace(strings.ReplaceAll(s, " LIMITED ", " LTD "))
}
