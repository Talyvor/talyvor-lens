package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// business_details.go — B59.30: A BUSINESS WORKSPACE'S DETAILS, READY FOR THE PARTNER'S IDENTITY FLOW (launch pack 6 (4)).
//
// The company's name, registration number and country, and the people with control: each a name, a role (director,
// shareholder or other) and a share. The number and country are the workspace's sign-up profile (B59.40), so saving
// the details changes them there too. A Personal workspace has none (ErrNotBusiness). Every change is in the audit
// trail, with which fields changed and how many people there are, never their names: the trail is kept for good and a
// deletion request must be able to remove a person. No identity document is collected or stored; the company check
// (L3, verification.go) is handed these details for whatever it is not given itself.

// The roles a person with control holds.
const (
	BusinessRoleDirector    = "director"
	BusinessRoleShareholder = "shareholder"
	BusinessRoleOther       = "other" // control some other way: votes, the right to appoint the board, or influence
)

// maxBusinessPeople is how many people with control one company may list; the migration's position check agrees.
const maxBusinessPeople = 50

var (
	// ErrNotBusiness: business details asked of a Personal workspace.
	ErrNotBusiness = errors.New("economy: a Personal workspace has no business details")
	// ErrBusinessDetailsInvalid: business details without what they need, or with a value out of range.
	ErrBusinessDetailsInvalid = errors.New("economy: invalid business details")
)

// BusinessPerson is one person with control of the company.
type BusinessPerson struct {
	Name string `json:"name"`
	Role string `json:"role"` // director, shareholder or other
	// ShareBps is their share of the company in hundredths of a percent: 2500 is 25%, 0 a director who holds none.
	ShareBps int `json:"share_bps"`
}

// BusinessDetails is a Business workspace's company record. A workspace that never saved one reads with an empty
// company name and no people.
type BusinessDetails struct {
	WorkspaceID        string           `json:"workspace_id"`
	CompanyName        string           `json:"company_name"`
	RegistrationNumber string           `json:"registration_number"`
	Country            string           `json:"country"` // ISO 3166-1 alpha-2
	People             []BusinessPerson `json:"people"`
	UpdatedBy          string           `json:"updated_by,omitempty"`
	UpdatedAt          *time.Time       `json:"updated_at,omitempty"`
}

// checkBusinessDetails tidies in and refuses it without a company name, registration number and country, or with a
// person unnamed, of an unknown role, or with shares outside 0 to 100% alone or together.
func checkBusinessDetails(in *BusinessDetails) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrBusinessDetailsInvalid, fmt.Sprintf(format, a...))
	}
	in.CompanyName, in.RegistrationNumber = strings.TrimSpace(in.CompanyName), strings.TrimSpace(in.RegistrationNumber)
	switch {
	case in.CompanyName == "" || len([]rune(in.CompanyName)) > 200:
		return bad("the company's name is 1 to 200 characters")
	case in.RegistrationNumber == "" || len([]rune(in.RegistrationNumber)) > 64:
		return bad("the company's registration number is 1 to 64 characters")
	}
	countries, err := countryCodes([]string{in.Country})
	if err != nil {
		return bad("the country is an ISO 3166-1 alpha-2 code, like GB, not %q", in.Country)
	}
	in.Country = countries[0]
	if len(in.People) > maxBusinessPeople {
		return bad("a company lists at most %d people with control", maxBusinessPeople)
	}
	total := 0
	for i := range in.People {
		p := &in.People[i]
		p.Name = strings.Join(strings.Fields(p.Name), " ")
		switch {
		case p.Name == "" || len([]rune(p.Name)) > 200:
			return bad("each person with control has a name of 1 to 200 characters")
		case p.Role != BusinessRoleDirector && p.Role != BusinessRoleShareholder && p.Role != BusinessRoleOther:
			return bad("a person's role is director, shareholder or other, not %q", p.Role)
		case p.ShareBps < 0 || p.ShareBps > 10000:
			return bad("a person's share is 0 to 10000 hundredths of a percent, not %d", p.ShareBps)
		}
		total += p.ShareBps
	}
	if total > 10000 {
		return bad("the people's shares come to %d hundredths of a percent, more than the whole company", total)
	}
	if in.People == nil {
		in.People = []BusinessPerson{}
	}
	return nil
}

// BusinessDetails reads workspaceID's company record: ErrWorkspaceNotFound for no such workspace, ErrNotBusiness for a
// Personal one.
func (s *DualTokenStore) BusinessDetails(ctx context.Context, workspaceID string) (BusinessDetails, error) {
	return readBusinessDetails(ctx, s.pool, workspaceID, false)
}

// readBusinessDetails reads workspaceID's record through q, locking the workspace's row first when forUpdate.
func readBusinessDetails(ctx context.Context, q pgxDB, workspaceID string, forUpdate bool) (BusinessDetails, error) {
	out := BusinessDetails{WorkspaceID: workspaceID, People: []BusinessPerson{}}
	var row pgx.Row
	if forUpdate {
		row = q.QueryRow(ctx, `SELECT account_type, country, registration_number FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID)
	} else {
		row = q.QueryRow(ctx, `SELECT account_type, country, registration_number FROM workspaces WHERE id = $1`, workspaceID)
	}
	var accountType string
	err := row.Scan(&accountType, &out.Country, &out.RegistrationNumber)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return BusinessDetails{}, ErrWorkspaceNotFound
	case err != nil:
		return BusinessDetails{}, fmt.Errorf("economy: business details: %w", err)
	case accountType != AccountTypeBusiness:
		return BusinessDetails{}, ErrNotBusiness
	}
	var at time.Time
	err = q.QueryRow(ctx, `SELECT company_name, updated_by, updated_at FROM workspace_business_details WHERE workspace_id = $1`,
		workspaceID).Scan(&out.CompanyName, &out.UpdatedBy, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return BusinessDetails{}, fmt.Errorf("economy: business details: %w", err)
	}
	out.UpdatedAt = &at
	rows, err := q.Query(ctx, `SELECT name, role, share_bps FROM workspace_business_people WHERE workspace_id = $1 ORDER BY position`,
		workspaceID)
	if err != nil {
		return BusinessDetails{}, fmt.Errorf("economy: business details: %w", err)
	}
	people, err := pgx.CollectRows(rows, pgx.RowToStructByPos[BusinessPerson])
	if err != nil {
		return BusinessDetails{}, fmt.Errorf("economy: business details: %w", err)
	}
	out.People = append(out.People, people...)
	return out, nil
}

// SetBusinessDetails replaces workspaceID's company record with in, by the person by, and records the change in the
// audit trail: ErrNotBusiness for a Personal workspace, ErrBusinessDetailsInvalid for details it refuses.
func (s *DualTokenStore) SetBusinessDetails(ctx context.Context, workspaceID, by string, in BusinessDetails) (BusinessDetails, error) {
	if err := checkBusinessDetails(&in); err != nil {
		return BusinessDetails{}, err
	}
	if by == "" {
		return BusinessDetails{}, fmt.Errorf("%w: a change names who made it", ErrBusinessDetailsInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BusinessDetails{}, fmt.Errorf("economy: save business details: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	was, err := readBusinessDetails(ctx, tx, workspaceID, true)
	if err != nil {
		return BusinessDetails{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workspaces SET country = $2, registration_number = $3, updated_at = now() WHERE id = $1`,
		workspaceID, in.Country, in.RegistrationNumber); err != nil {
		return BusinessDetails{}, fmt.Errorf("economy: save business details: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_business_details (workspace_id, company_name, updated_by) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET company_name = EXCLUDED.company_name, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		workspaceID, in.CompanyName, by); err != nil {
		return BusinessDetails{}, fmt.Errorf("economy: save business details: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_business_people WHERE workspace_id = $1`, workspaceID); err != nil {
		return BusinessDetails{}, fmt.Errorf("economy: save business details: %w", err)
	}
	for i, p := range in.People {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_business_people (workspace_id, position, name, role, share_bps)
			VALUES ($1, $2, $3, $4, $5)`, workspaceID, i, p.Name, p.Role, p.ShareBps); err != nil {
			return BusinessDetails{}, fmt.Errorf("economy: save business details: %w", err)
		}
	}
	changed := []string{}
	for _, f := range []struct {
		name     string
		was, now string
	}{{"company_name", was.CompanyName, in.CompanyName}, {"registration_number", was.RegistrationNumber, in.RegistrationNumber},
		{"country", was.Country, in.Country}} {
		if f.was != f.now {
			changed = append(changed, f.name)
		}
	}
	if !slices.Equal(was.People, in.People) {
		changed = append(changed, "people")
	}
	if err := appendAudit(ctx, tx, workspaceID, "workspace:"+workspaceID, "workspace.business_details",
		map[string]any{"by": by, "changed": changed, "people": len(in.People)}); err != nil {
		return BusinessDetails{}, err
	}
	out, err := readBusinessDetails(ctx, tx, workspaceID, false)
	if err != nil {
		return BusinessDetails{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BusinessDetails{}, fmt.Errorf("economy: save business details: %w", err)
	}
	return out, nil
}

// fillCompanyCheck gives a company check (L3) what it was not given from workspaceID's saved business details: the
// company's name, country and number, and its directors and people with significant control when it names neither.
// Directors are the people whose role is director; people with significant control every other person, and a
// director who holds a share. A workspace with no saved details, or a Personal one, leaves the check as it was asked.
func fillCompanyCheck(ctx context.Context, q pgxDB, workspaceID string, in *VerificationRequest) error {
	d, err := readBusinessDetails(ctx, q, workspaceID, false)
	if errors.Is(err, ErrNotBusiness) || errors.Is(err, ErrWorkspaceNotFound) || (err == nil && d.UpdatedAt == nil) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(in.Name) == "" {
		in.Name = d.CompanyName
	}
	if strings.TrimSpace(in.Country) == "" {
		in.Country = d.Country
	}
	if strings.TrimSpace(in.CompanyNumber) == "" {
		in.CompanyNumber = d.RegistrationNumber
	}
	if len(in.Directors) == 0 && len(in.SignificantControl) == 0 {
		for _, p := range d.People {
			if p.Role == BusinessRoleDirector {
				in.Directors = append(in.Directors, p.Name)
			}
			if p.Role != BusinessRoleDirector || p.ShareBps > 0 {
				in.SignificantControl = append(in.SignificantControl, p.Name)
			}
		}
	}
	return nil
}
