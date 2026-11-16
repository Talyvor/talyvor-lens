package partners

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// KYCProvider checks who a person or a company is, for the verification levels (B30.4): a person's email and
// phone, a person's identity, a company's number, directors and people with significant control. Lens keeps the
// provider's reference, never a document.
type KYCProvider interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// StartCheck starts a check. Idempotent on req.ID.
	StartCheck(ctx context.Context, req KYCRequest) (Result, error)
	// CheckResult says where a check stands now: completed is passed, failed is not, returned is a pass the
	// provider has since withdrawn.
	CheckResult(ctx context.Context, ref string) (Result, error)
}

// The subjects a check is about.
const (
	KYCContact = "contact" // a person's email and phone, each confirmed
	KYCPerson  = "person"
	KYCCompany = "company"
)

// KYCRequest asks for a check on a person's contact details, a person or a company.
type KYCRequest struct {
	ID            string      `json:"id"` // the caller's id for the check
	Subject       string      `json:"subject"`
	Name          string      `json:"name"`
	Country       string      `json:"country"`                  // ISO 3166-1 alpha-2
	DateOfBirth   string      `json:"date_of_birth,omitempty"`  // a person's, YYYY-MM-DD
	Address       *KYCAddress `json:"address,omitempty"`        // a person's home address, when given
	Email         string      `json:"email,omitempty"`          // a contact check's
	Phone         string      `json:"phone,omitempty"`          // a contact check's, E.164
	CompanyNumber string      `json:"company_number,omitempty"` // a company's
	// A company's directors and people with significant control, by name.
	Directors          []string `json:"directors,omitempty"`
	SignificantControl []string `json:"people_with_significant_control,omitempty"`
}

// KYCAddress is a person's home address, for an identity check: its lines, town, postcode and country.
type KYCAddress struct {
	Lines    []string `json:"lines"`
	Town     string   `json:"town"`
	Postcode string   `json:"postcode"`
	Country  string   `json:"country"` // ISO 3166-1 alpha-2
}

// lacks says what a home address lacks, or "" when it has its lines, town, postcode and country.
func (a KYCAddress) lacks() string {
	switch {
	case !slices.ContainsFunc(a.Lines, func(l string) bool { return strings.TrimSpace(l) != "" }):
		return "its first line"
	case strings.TrimSpace(a.Town) == "":
		return "its town"
	case strings.TrimSpace(a.Postcode) == "":
		return "its postcode"
	case len(strings.TrimSpace(a.Country)) != 2:
		return "its country, as an ISO 3166-1 alpha-2 code"
	}
	return ""
}

// TestKYCProvider is test mode for verification: it checks nobody. A check answers by the name (see the package
// comment) — a contact check by the email, a company check by its name, directors' and controllers' together:
// TESTFAIL and TESTSANCTION fail, TESTRETURN passes and is later withdrawn, TESTPENDING stays pending, anything
// else passes. A home address, once a check sends one, must be whole: its lines, town, postcode and country.
type TestKYCProvider struct {
	book testBook
}

// Name is "test".
func (*TestKYCProvider) Name() string { return "test" }

// StartCheck checks nobody: it answers by the name.
func (p *TestKYCProvider) StartCheck(_ context.Context, req KYCRequest) (Result, error) {
	decideBy := req.Name
	switch {
	case req.Subject != KYCContact && req.Subject != KYCPerson && req.Subject != KYCCompany:
		return Result{}, fmt.Errorf("%w: a check is on a person's contact details, a person or a company, not %q", ErrInvalid, req.Subject)
	case req.Subject == KYCContact:
		if strings.TrimSpace(req.Email) == "" || strings.TrimSpace(req.Phone) == "" {
			return Result{}, fmt.Errorf("%w: a contact check carries the email and the phone it confirms", ErrInvalid)
		}
		decideBy = req.Email
	case strings.TrimSpace(req.Name) == "":
		return Result{}, fmt.Errorf("%w: a check names who it is on", ErrInvalid)
	case req.Address != nil && req.Subject != KYCPerson:
		return Result{}, fmt.Errorf("%w: a home address is a person's, for an identity check", ErrInvalid)
	case req.Address != nil && req.Address.lacks() != "":
		return Result{}, fmt.Errorf("%w: the home address needs %s", ErrInvalid, req.Address.lacks())
	case req.Subject == KYCCompany && strings.TrimSpace(req.CompanyNumber) == "":
		return Result{}, fmt.Errorf("%w: a company check carries the company number", ErrInvalid)
	case req.Subject == KYCCompany && len(req.Directors) == 0:
		return Result{}, fmt.Errorf("%w: a company check names the company's directors", ErrInvalid)
	case req.Subject == KYCCompany:
		decideBy = strings.Join(append(append([]string{req.Name}, req.Directors...), req.SignificantControl...), " ")
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("kyc", req.ID, req, byName(decideBy))
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

// CheckResult is where a check stands now.
func (p *TestKYCProvider) CheckResult(_ context.Context, ref string) (Result, error) {
	return p.book.status("kyc", ref)
}
