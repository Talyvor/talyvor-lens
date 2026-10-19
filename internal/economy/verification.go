package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
)

// verification.go — B30.4: VERIFICATION LEVELS FOR PEOPLE AND COMPANIES, AND THE LEVEL EACH CAPABILITY NEEDS.
//
//	L0  signed in
//	L1  email and phone confirmed
//	L2  identity checked
//	L3  company checked: its number, directors and people with significant control
//
// Every check past L0 goes to partners.KYCProvider — the registry's Verification: Persona for a person's identity
// and Companies House for a UK company once their keys are set (B30.112), the Test provider otherwise and for a
// synthetic workspace — and needs the level below it first. Lens keeps each check's level, method (the provider that
// checked), status, date and the provider's reference as its evidence, never a document (workspace_verifications,
// migration 0230, append-only). Lens knows a person only through their workspace (B19.11), so the record is the
// workspace's.
//
// Each capability declares the level its live money needs (Capability.Level): currency accounts, payments, FX and
// trading L2; credit and merchant acceptance L3; spending on Talyvor itself L0. Live money is judged by the
// workspace's live level — the checks a real provider passed. A pass by the Test provider is on the record and
// shows, and counts for nothing live; so does a pass by a provider's sandbox (a method ending _sandbox). Test money never asks for a level: with it every level may try everything.
//
// And a level takes live money only within the limits Nicolai sets for it (`lens verification-limits`): the most one
// movement of money may move, per currency. Until one is set no level takes live money; a level with no limit of
// its own in a currency has the highest one set below it.

// VerificationLevel is L0 to L3. It reads and writes as "L0" to "L3".
type VerificationLevel int

// The levels.
const (
	LevelSignedIn VerificationLevel = iota // L0
	LevelContact                           // L1
	LevelIdentity                          // L2
	LevelCompany                           // L3
)

var levelMeanings = []string{"signed in", "email and phone confirmed", "identity checked", "company checked"}

func (l VerificationLevel) String() string { return "L" + strconv.Itoa(int(l)) }

// Meaning is what the level says was checked.
func (l VerificationLevel) Meaning() string {
	if l < LevelSignedIn || l > LevelCompany {
		return ""
	}
	return levelMeanings[l]
}

// MarshalText writes "L2".
func (l VerificationLevel) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// UnmarshalText reads "L2", "l2" or "2".
func (l *VerificationLevel) UnmarshalText(b []byte) error {
	v, err := ParseVerificationLevel(string(b))
	if err != nil {
		return err
	}
	*l = v
	return nil
}

// ParseVerificationLevel reads "L2", "l2" or "2".
func ParseVerificationLevel(s string) (VerificationLevel, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "L"))
	if err != nil || n < int(LevelSignedIn) || n > int(LevelCompany) {
		return 0, fmt.Errorf("economy: a verification level is L0, L1, L2 or L3, not %q", s)
	}
	return VerificationLevel(n), nil
}

// subjects is what each level's check is on.
var subjects = map[VerificationLevel]string{LevelContact: partners.KYCContact, LevelIdentity: partners.KYCPerson,
	LevelCompany: partners.KYCCompany}

// testVerifier is the Test provider's name: what it passes counts for test money only.
const testVerifier = "test"

// testOnly says whether what method passes counts for test money only: the Test provider's, a sandbox's, and
// Persona's until it compares the name on the ID with the name given (partners.PersonaKYC).
func testOnly(method string) bool {
	return method == testVerifier || method == "persona" || strings.HasSuffix(method, "_sandbox")
}

var (
	// ErrVerificationInvalid: a check asked for without what it checks.
	ErrVerificationInvalid = errors.New("economy: invalid verification request")
	// ErrVerificationOrder: a check asked for before the level below it is reached.
	ErrVerificationOrder = errors.New("economy: verification levels are reached in order")
	// ErrVerificationNeeded: live money a capability needs a higher verification level for, or more of it than the
	// level's limit.
	ErrVerificationNeeded = errors.New("economy: this needs a higher verification level")
)

// VerificationRequest is what a check is asked to confirm: for L1 the email and phone; for L2 the person's name,
// country and date of birth; for L3 the company's name, country, number, directors and people with significant
// control. Lens passes the date of birth, email, phone and the people's names to the provider and keeps none of
// them.
type VerificationRequest struct {
	Level              VerificationLevel `json:"-"`
	Name               string            `json:"name"`
	Country            string            `json:"country"`
	DateOfBirth        string            `json:"date_of_birth"`
	Email              string            `json:"email"`
	Phone              string            `json:"phone"`
	CompanyNumber      string            `json:"company_number"`
	Directors          []string          `json:"directors"`
	SignificantControl []string          `json:"people_with_significant_control"`
}

// VerificationCheck is one check and where it stands.
type VerificationCheck struct {
	Level         VerificationLevel `json:"level"`
	Subject       string            `json:"subject"`
	Method        string            `json:"method"` // the provider that checked
	Test          bool              `json:"test"`   // checked by the Test provider or a sandbox: it counts for test money only
	Status        string            `json:"status"` // pending, completed, failed or returned (a pass withdrawn)
	EvidenceRef   string            `json:"evidence_ref"`
	VerifiedName  string            `json:"verified_name,omitempty"`
	Country       string            `json:"country,omitempty"`
	CompanyNumber string            `json:"company_number,omitempty"`
	Detail        string            `json:"detail,omitempty"`
	RequestedBy   string            `json:"requested_by,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	CheckedAt     time.Time         `json:"checked_at"` // when it reached its status
	// Link is where the person completes a check still waiting for them (Persona's one-time link); never stored.
	Link string `json:"link,omitempty"`
}

// WorkspaceVerification is the owner's record: the level the checks reach, the level live money is judged by, the
// country live money is used from (B30.10), and every check, newest first.
type WorkspaceVerification struct {
	WorkspaceID string              `json:"workspace_id"`
	Level       VerificationLevel   `json:"level"`
	Meaning     string              `json:"meaning"`
	LiveLevel   VerificationLevel   `json:"live_level"` // passes by a real provider only
	LiveMeaning string              `json:"live_meaning"`
	LiveCountry string              `json:"live_country,omitempty"` // a real provider confirmed it
	Checks      []VerificationCheck `json:"checks"`
}

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// checkVerificationRequest refuses a request without what its level's check confirms, and tidies it.
func checkVerificationRequest(in *VerificationRequest) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrVerificationInvalid, fmt.Sprintf(format, a...))
	}
	in.Name, in.CompanyNumber = strings.TrimSpace(in.Name), strings.TrimSpace(in.CompanyNumber)
	in.Email, in.Phone = strings.TrimSpace(in.Email), strings.ReplaceAll(strings.TrimSpace(in.Phone), " ", "")
	if in.Level == LevelContact {
		if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email {
			return bad("an email and phone check needs the email address, not %q", in.Email)
		}
		if !e164.MatchString(in.Phone) {
			return bad("an email and phone check needs the phone number in international form, like +447700900123, not %q", in.Phone)
		}
		return nil
	}
	if in.Level != LevelIdentity && in.Level != LevelCompany {
		return bad("checks are for L1, L2 and L3, not %s", in.Level)
	}
	countries, err := countryCodes([]string{in.Country})
	if err != nil {
		return bad("the country is an ISO 3166-1 alpha-2 code, like GB, not %q", in.Country)
	}
	in.Country = countries[0]
	if in.Name == "" {
		return bad("the check needs the full name of the %s", subjects[in.Level])
	}
	if in.Level == LevelIdentity {
		dob, err := time.Parse(time.DateOnly, strings.TrimSpace(in.DateOfBirth))
		if err != nil || dob.Year() < 1900 || !dob.Before(time.Now()) {
			return bad("an identity check needs the date of birth as YYYY-MM-DD, not %q", in.DateOfBirth)
		}
		in.DateOfBirth = dob.Format(time.DateOnly)
		return nil
	}
	tidy := func(names []string) []string {
		out := []string{}
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	in.Directors, in.SignificantControl = tidy(in.Directors), tidy(in.SignificantControl)
	switch {
	case in.CompanyNumber == "":
		return bad("a company check needs the company number")
	case len(in.Directors) == 0:
		return bad("a company check names the company's directors")
	}
	return nil
}

// StartVerification starts a check for in.Level on workspaceID through kyc and records it, by who asked: L1 needs L0,
// L2 needs L1, L3 needs L2 — each passed, by any provider. The check is recorded as the provider answers: passed,
// failed, or pending until a later read finds it decided.
func (s *DualTokenStore) StartVerification(ctx context.Context, verifiers partners.Verifiers, workspaceID, by string,
	in VerificationRequest) (VerificationCheck, error) {
	if err := checkVerificationRequest(&in); err != nil {
		return VerificationCheck{}, err
	}
	checks, err := readVerificationChecks(ctx, s.pool, workspaceID)
	if err != nil {
		return VerificationCheck{}, err
	}
	if have, _ := reachedLevels(checks); have < in.Level-1 {
		below := in.Level - 1
		return VerificationCheck{}, fmt.Errorf("%w: the %s check (%s) needs %s — %s — first; this workspace is at %s",
			ErrVerificationOrder, in.Level.Meaning(), in.Level, below, below.Meaning(), have)
	}
	if person, named := identityNamed(checks, slices.Concat(in.Directors, in.SignificantControl)); in.Level == LevelCompany && !named {
		return VerificationCheck{}, fmt.Errorf("%w: a company check names %s, whose identity was checked, among its directors or people with significant control",
			ErrVerificationInvalid, person)
	}
	subject := subjects[in.Level]
	synthetic, err := syntheticWorkspace(ctx, s.pool, workspaceID)
	if err != nil {
		return VerificationCheck{}, err
	}
	kyc := verifiers(subject, in.Country, synthetic)
	res, err := kyc.StartCheck(ctx, partners.KYCRequest{ID: "kyc_" + uuid.NewString(), Subject: subject, Name: in.Name,
		Country: in.Country, DateOfBirth: in.DateOfBirth, Email: in.Email, Phone: in.Phone, CompanyNumber: in.CompanyNumber,
		Directors: in.Directors, SignificantControl: in.SignificantControl})
	if errors.Is(err, partners.ErrInvalid) {
		return VerificationCheck{}, fmt.Errorf("%w: %v", ErrVerificationInvalid, err)
	}
	if err != nil {
		return VerificationCheck{}, fmt.Errorf("economy: verification: %w", err)
	}
	c := VerificationCheck{Level: in.Level, Subject: subject, Method: kyc.Name(), Status: string(res.Status), EvidenceRef: res.Ref,
		Country: in.Country, CompanyNumber: in.CompanyNumber, Detail: res.Detail, RequestedBy: by, Link: res.Link}
	if in.Level != LevelContact {
		c.VerifiedName = in.Name
	}
	if err := recordVerificationCheck(ctx, s.pool, workspaceID, &c); err != nil {
		return VerificationCheck{}, err
	}
	c.StartedAt = c.CheckedAt
	return c, nil
}

// identityNamed says whether names include the person whose identity check (L2) passed most recently, and who: a
// company is checked for the person who was.
func identityNamed(checks []VerificationCheck, names []string) (person string, named bool) {
	for _, c := range checks { // newest first
		if c.Level == LevelIdentity && c.Status == string(partners.StatusCompleted) {
			person = c.VerifiedName
			break
		}
	}
	return person, slices.ContainsFunc(names, func(n string) bool {
		return strings.EqualFold(strings.Join(strings.Fields(n), " "), strings.Join(strings.Fields(person), " "))
	})
}

// recordVerificationCheck appends c as it stands now, and stamps when.
func recordVerificationCheck(ctx context.Context, q pgxDB, workspaceID string, c *VerificationCheck) error {
	c.Test = testOnly(c.Method)
	if err := q.QueryRow(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref,
		verified_name, country, company_number, detail, requested_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at`, workspaceID, int(c.Level), c.Subject, c.Method, c.Status, c.EvidenceRef, c.VerifiedName, c.Country,
		c.CompanyNumber, c.Detail, c.RequestedBy).Scan(&c.CheckedAt); err != nil {
		return fmt.Errorf("economy: record verification: %w", err)
	}
	return nil
}

// Verification is workspaceID's record. A check still pending, or passed, is asked again first of the provider it
// went to, if verifiers still sends its kind of check there, and a change — a pending check decided, a pass
// withdrawn — is appended; a provider that cannot answer leaves the check as it was. With nil verifiers nothing is
// asked.
func (s *DualTokenStore) Verification(ctx context.Context, verifiers partners.Verifiers, workspaceID string) (WorkspaceVerification, error) {
	checks, err := readVerificationChecks(ctx, s.pool, workspaceID)
	if err != nil {
		return WorkspaceVerification{}, err
	}
	synthetic := false
	if verifiers != nil {
		if synthetic, err = syntheticWorkspace(ctx, s.pool, workspaceID); err != nil {
			return WorkspaceVerification{}, err
		}
	}
	for i, c := range checks {
		if verifiers == nil || (c.Status != string(partners.StatusPending) && c.Status != string(partners.StatusCompleted)) {
			continue
		}
		kyc := verifiers(c.Subject, c.Country, synthetic)
		if c.Method != kyc.Name() {
			continue
		}
		res, err := kyc.CheckResult(ctx, c.EvidenceRef)
		if err != nil {
			slog.Warn("economy: verification: the provider could not say where a check stands", "ref", c.EvidenceRef, "err", err)
			continue
		}
		checks[i].Link = res.Link
		if string(res.Status) == c.Status {
			continue
		}
		c.Status, c.Detail, c.Link = string(res.Status), res.Detail, res.Link
		if err := recordVerificationCheck(ctx, s.pool, workspaceID, &c); err != nil {
			return WorkspaceVerification{}, err
		}
		checks[i] = c
	}
	v := WorkspaceVerification{WorkspaceID: workspaceID, Checks: checks}
	v.Level, v.LiveLevel = reachedLevels(checks)
	v.Meaning, v.LiveMeaning = v.Level.Meaning(), v.LiveLevel.Meaning()
	v.LiveCountry = liveCountry(checks)
	return v, nil
}

// readVerificationChecks is each of workspaceID's checks as it stands now — the newest row for its reference — newest
// first.
func readVerificationChecks(ctx context.Context, q pgxDB, workspaceID string) ([]VerificationCheck, error) {
	rows, err := q.Query(ctx, `SELECT level, subject, method, status, evidence_ref, verified_name, country, company_number, detail,
		requested_by, started_at, created_at FROM (
			SELECT DISTINCT ON (evidence_ref) *, min(created_at) OVER (PARTITION BY evidence_ref) AS started_at
			FROM workspace_verifications WHERE workspace_id = $1 ORDER BY evidence_ref, id DESC) latest
		ORDER BY started_at DESC, evidence_ref`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: verification: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (VerificationCheck, error) {
		var c VerificationCheck
		var level int16
		err := row.Scan(&level, &c.Subject, &c.Method, &c.Status, &c.EvidenceRef, &c.VerifiedName, &c.Country, &c.CompanyNumber,
			&c.Detail, &c.RequestedBy, &c.StartedAt, &c.CheckedAt)
		c.Level, c.Test = VerificationLevel(level), testOnly(c.Method)
		return c, err
	})
}

// reachedLevels is the highest level whose check and every check below it has passed — by any provider, and by a
// real provider only.
func reachedLevels(checks []VerificationCheck) (shown, live VerificationLevel) {
	passed, passedLive := map[VerificationLevel]bool{}, map[VerificationLevel]bool{}
	for _, c := range checks {
		if c.Status == string(partners.StatusCompleted) {
			passed[c.Level] = true
			passedLive[c.Level] = passedLive[c.Level] || !c.Test
		}
	}
	for shown < LevelCompany && passed[shown+1] {
		shown++
	}
	for live < LevelCompany && passedLive[live+1] {
		live++
	}
	return shown, live
}

// syntheticWorkspace says whether workspaceID is a tester's: its checks stay with the Test provider.
func syntheticWorkspace(ctx context.Context, q pgxDB, workspaceID string) (synthetic bool, err error) {
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&synthetic); err != nil {
		return false, fmt.Errorf("economy: verification: %w", err)
	}
	return synthetic, nil
}

// liveVerificationLevel is the level workspaceID's live money is judged by.
func liveVerificationLevel(ctx context.Context, q pgxDB, workspaceID string) (VerificationLevel, error) {
	checks, err := readVerificationChecks(ctx, q, workspaceID)
	if err != nil {
		return 0, err
	}
	_, live := reachedLevels(checks)
	return live, nil
}

// liveCountry is the country the owner is verified in for live money (B30.10): the one the highest-level check a real
// provider passed confirmed — the company's at L3, the person's at L2 — the newest at that level. "" when none names
// one.
func liveCountry(checks []VerificationCheck) string {
	country, at := "", LevelSignedIn
	for _, c := range checks { // newest first
		if c.Status == string(partners.StatusCompleted) && !c.Test && c.Country != "" && c.Level > at {
			country, at = c.Country, c.Level
		}
	}
	return country
}

// ownerCountry is the country workspaceID's live money is used from: its owner's verified one (liveCountry).
func ownerCountry(ctx context.Context, q pgxDB, workspaceID string) (string, error) {
	checks, err := readVerificationChecks(ctx, q, workspaceID)
	if err != nil {
		return "", err
	}
	return liveCountry(checks), nil
}

// LevelLimit is the most one movement of live money may move at a level, in a currency's minor units.
type LevelLimit struct {
	Level      VerificationLevel `json:"level"`
	Currency   string            `json:"currency"`
	LimitMinor int64             `json:"limit_minor"`
	Operator   string            `json:"operator"`
	Reference  string            `json:"reference"`
	At         time.Time         `json:"at"`
}

// SetLevelLimit records the most one movement of live money may move at level, in currency — by the operator, on
// their reference. 0 takes no live money in that currency at that level.
func (s *DualTokenStore) SetLevelLimit(ctx context.Context, level VerificationLevel, currency string, limitMinor int64,
	operator, reference string) (LevelLimit, error) {
	l := LevelLimit{Level: level, Currency: strings.ToUpper(strings.TrimSpace(currency)), LimitMinor: limitMinor,
		Operator: strings.TrimSpace(operator), Reference: strings.TrimSpace(reference)}
	_, known := MoneyCurrencies[l.Currency]
	switch {
	case level < LevelContact || level > LevelCompany:
		return LevelLimit{}, fmt.Errorf("economy: limits are set for L1, L2 and L3; L0 takes no live money that needs a level")
	case !known:
		return LevelLimit{}, fmt.Errorf("economy: a limit is in GBP, EUR, USD or USDC, not %q", currency)
	case limitMinor < 0:
		return LevelLimit{}, errors.New("economy: a limit is 0 or more")
	case l.Operator == "" || l.Reference == "":
		return LevelLimit{}, errors.New("economy: a limit names who sets it and why")
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO verification_level_limits (level, currency, limit_minor, operator, reference)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`, int(level), l.Currency, limitMinor, l.Operator, l.Reference).Scan(&l.At); err != nil {
		return LevelLimit{}, fmt.Errorf("economy: set level limit: %w", err)
	}
	return l, nil
}

// LevelLimits is each level's limit in force in each currency it has one in.
func (s *DualTokenStore) LevelLimits(ctx context.Context) ([]LevelLimit, error) {
	return levelLimitsInForce(ctx, s.pool)
}

func levelLimitsInForce(ctx context.Context, q pgxDB) ([]LevelLimit, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (level, currency) level, currency, limit_minor, operator, reference, created_at
		FROM verification_level_limits ORDER BY level, currency, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("economy: level limits: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LevelLimit, error) {
		var l LevelLimit
		var level int16
		err := row.Scan(&level, &l.Currency, &l.LimitMinor, &l.Operator, &l.Reference, &l.At)
		l.Level = VerificationLevel(level)
		return l, err
	})
}

// limitFor is the limit a workspace at level have moves live money in currency under: its level's, or the highest
// one set below it. ok is false when none is.
func limitFor(limits []LevelLimit, have VerificationLevel, currency string) (l LevelLimit, ok bool) {
	for _, c := range limits {
		if c.Currency == currency && c.Level <= have && (!ok || c.Level > l.Level) {
			l, ok = c, true
		}
	}
	return l, ok
}

// LevelRefusal says why a workspace's verification level keeps a capability's live money out: the level is below
// the one the capability needs; or no limit is set for it; or the movement is over its limit.
type LevelRefusal struct {
	Needed      VerificationLevel `json:"level_needed"`
	Have        VerificationLevel `json:"level"`
	NoLimit     bool              `json:"no_limit,omitempty"`
	Currency    string            `json:"currency,omitempty"`
	Limit       *LevelLimit       `json:"limit,omitempty"`
	AmountMinor int64             `json:"amount_minor,omitempty"`
}

func (r *LevelRefusal) message(c Capability) string {
	switch {
	case r.NoLimit && r.Currency == "":
		return fmt.Sprintf("%s: Talyvor has set no live limit for verification level %s or below yet, and no level takes real money until it does",
			c.Name, r.Have)
	case r.NoLimit:
		return fmt.Sprintf("%s: Talyvor has set no live limit in %s for verification level %s or below yet, and no level takes real money in %s until it does",
			c.Name, r.Currency, r.Have, r.Currency)
	case r.Limit != nil:
		return fmt.Sprintf("%s: this moves %s of real money, over the %s limit for one movement at verification level %s",
			c.Name, FormatMinor(r.AmountMinor, r.Currency), FormatMinor(r.Limit.LimitMinor, r.Currency), r.Limit.Level)
	}
	return fmt.Sprintf("%s needs verification level %s (%s) for real money; this workspace is at %s (%s)",
		c.Name, r.Needed, r.Needed.Meaning(), r.Have, r.Have.Meaning())
}

// FormatMinor writes an amount in minor units as the currency's major units: 150000 GBP is "1500.00 GBP".
func FormatMinor(minor int64, currency string) string {
	places := MoneyCurrencies[currency]
	neg := minor < 0
	if neg {
		minor = -minor
	}
	digits := fmt.Sprintf("%0*d", places+1, minor)
	out := digits
	if places > 0 {
		out = digits[:len(digits)-places] + "." + digits[len(digits)-places:]
	}
	if neg {
		out = "-" + out
	}
	return out + " " + currency
}

// levelRefusal is why workspaceID's verification keeps capability c's live money out — nil when it does not: its
// live level is below c's, or no limit is set at or below it.
func levelRefusal(ctx context.Context, q pgxDB, workspaceID string, c Capability) (*LevelRefusal, error) {
	if c.Level == LevelSignedIn {
		return nil, nil
	}
	have, err := liveVerificationLevel(ctx, q, workspaceID)
	if err != nil {
		return nil, err
	}
	if have < c.Level {
		return &LevelRefusal{Needed: c.Level, Have: have}, nil
	}
	limits, err := levelLimitsInForce(ctx, q)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(limits, func(l LevelLimit) bool { return l.Level <= have && l.LimitMinor > 0 }) {
		return &LevelRefusal{Needed: c.Level, Have: have, NoLimit: true}, nil
	}
	return nil, nil
}

// overLevelLimit is why a live movement of postings for capability c is over workspaceID's limits — nil when it is
// within them. A movement's size in a currency is what its postings add in it, which, as they balance, is what
// they take.
func overLevelLimit(ctx context.Context, q pgxDB, workspaceID string, c Capability, postings []MoneyPosting) (*LevelRefusal, error) {
	if c.Level == LevelSignedIn {
		return nil, nil
	}
	have, err := liveVerificationLevel(ctx, q, workspaceID)
	if err != nil {
		return nil, err
	}
	limits, err := levelLimitsInForce(ctx, q)
	if err != nil {
		return nil, err
	}
	moved := map[string]int64{} // nosemgrep: trailofbits.go.iterate-over-empty-map.iterate-over-empty-map -- moved is filled from postings just above; empty means nothing moves, so no limit applies
	for _, p := range postings {
		if p.AmountMinor > 0 {
			moved[p.Currency] += p.AmountMinor
		}
	}
	currencies := make([]string, 0, len(moved))
	for cur := range moved {
		currencies = append(currencies, cur)
	}
	slices.Sort(currencies)
	for _, cur := range currencies {
		l, ok := limitFor(limits, have, cur)
		if !ok {
			return &LevelRefusal{Needed: c.Level, Have: have, NoLimit: true, Currency: cur}, nil
		}
		if moved[cur] > l.LimitMinor {
			return &LevelRefusal{Needed: c.Level, Have: have, Currency: cur, Limit: &l, AmountMinor: moved[cur]}, nil
		}
	}
	return nil, nil
}
