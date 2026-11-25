package economy

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/workspace"
)

// people_found.go — B61.32: A PERSON ON TALYVOR IS FOUND BY THEIR NAME, PHONE NUMBER OR EMAIL.
//
// A person (B61.22's people row) may claim one Talyvor name — 3 to 20 letters a to z, digits, dots and underscores,
// unique whatever the case, refused when it reads like a name already held (look-alikes folded, nameKey) or is kept
// for Talyvor — and decides how they are found: by name, by their confirmed phone number, by the email the BFF
// confirmed, each its own switch, each off until they say yes. Their settings are the BFF's to read and change, on the
// provision secret, for the person whose session it holds.
//
// An account looks a person up by an exact name, number or email (FindPerson) and is answered the person's shown name
// — their first name and their last name's initial, from the identity check they passed — and nothing else. A name,
// number or email nobody holds, one whose person has not said yes, and a person whose identity check has not passed
// answer the same words, padded to the same time (personFindFloor). Every lookup, by find or by a payment, counts
// against the account's limit an hour.

var (
	// ErrPersonNotFound is the one answer for a name, number or email that finds nobody, for whatever reason; it is
	// said in PersonNotFoundWords.
	ErrPersonNotFound = errors.New("economy: nobody found")
	// ErrPersonLookupLimit: the account has looked up as many people as it may this hour.
	ErrPersonLookupLimit = errors.New("economy: too many people looked up")
	// ErrPersonName: a Talyvor name that may not be claimed as written.
	ErrPersonName = errors.New("economy: invalid Talyvor name")
	// ErrPersonNameTaken: a Talyvor name another person holds, or one that reads like it.
	ErrPersonNameTaken = errors.New("economy: that Talyvor name is taken: choose another")
	// ErrPersonEmail: an email that is not one address, or one another person on Talyvor holds.
	ErrPersonEmail = errors.New("economy: invalid email")
	// ErrPersonFinding: a switch turned on with nothing to be found by, or an account to receive into that is not theirs.
	ErrPersonFinding = errors.New("economy: invalid settings for being found")
	// ErrNoSuchPerson: the BFF named a person Lens does not hold.
	ErrNoSuchPerson = errors.New("economy: no such person")
)

// PersonNotFoundWords are what a person is told when a lookup finds nobody.
const PersonNotFoundWords = "Nobody found. Check the name or number, or ask them to turn on being found in Settings."

// defaultPersonLookupsPerHour is how many people an account may look up an hour until Nicolai sets another
// (LENS_PERSON_LOOKUPS_PER_HOUR).
const defaultPersonLookupsPerHour = 30

// personFindFloor is the least time a lookup answers in, found or not, so an answer's time says nothing of why.
var personFindFloor = 150 * time.Millisecond

// SetPersonLookupsPerHour sets the limit of lookups an account an hour; 0 or less keeps the default.
func (s *DualTokenStore) SetPersonLookupsPerHour(n int) { s.personLookupsPerHour = n }

func (s *DualTokenStore) lookupsPerHour() int {
	if s.personLookupsPerHour > 0 {
		return s.personLookupsPerHour
	}
	return defaultPersonLookupsPerHour
}

// PersonFinding is how a person is found, as their settings show it.
type PersonFinding struct {
	Name             string `json:"name"`
	Email            string `json:"email"`
	Phone            string `json:"phone"`
	ByName           bool   `json:"by_name"`
	ByPhone          bool   `json:"by_phone"`
	ByEmail          bool   `json:"by_email"`
	ReceiveAccountID string `json:"receive_account_id"`
	// CryptoWallets are the agent wallets they receive crypto into, a network and a token each (B48.32).
	CryptoWallets []PersonCryptoWallet `json:"crypto_wallets"`
}

// PersonFindingChange is a change to a person's settings; a nil field is left as it is, and "" clears a name, an
// email or the account to receive into.
type PersonFindingChange struct {
	Name             *string `json:"name"`
	Email            *string `json:"email"`
	ByName           *bool   `json:"by_name"`
	ByPhone          *bool   `json:"by_phone"`
	ByEmail          *bool   `json:"by_email"`
	ReceiveAccountID *string `json:"receive_account_id"`
	// CryptoWallet names the agent wallet they receive its token into on its network; an agent_id of "" stops them
	// receiving there (B48.32).
	CryptoWallet *PersonCryptoWallet `json:"crypto_wallet"`
}

// reservedPersonNames are kept for Talyvor, compared folded; a name holding "talyvor" anywhere is kept too.
var reservedPersonNames = map[string]bool{}

func init() {
	for _, n := range []string{"talyvor", "support", "admin", "administrator", "help", "helpdesk", "root", "system", "security",
		"staff", "official", "team", "billing", "payments", "pay", "money", "bank", "lens", "pocket", "account", "accounts",
		"settings", "info", "contact", "abuse", "legal", "privacy", "compliance", "fraud", "operator", "moderator", "null",
		"undefined", "everyone", "me"} {
		reservedPersonNames[nameKey(n)] = true
	}
}

// personNameShape is a name's shape once lower-cased: a letter or digit at each end, and never two dots or
// underscores together.
var personNameShape = regexp.MustCompile(`^[a-z0-9]+([._][a-z0-9]+)*$`)

// nameKey folds what reads alike in a name, so two names that read alike share one key: dots and underscores go,
// 0 reads as o, 1 as l, rn as m and vv as w.
func nameKey(name string) string {
	k := strings.NewReplacer(".", "", "_", "").Replace(name)
	k = strings.NewReplacer("0", "o", "1", "l").Replace(k)
	return strings.NewReplacer("rn", "m", "vv", "w").Replace(k)
}

// PersonNameFor checks raw as a Talyvor name and answers it as it is kept, lower-case, with its folded key.
func PersonNameFor(raw string) (name, key string, err error) {
	name = strings.TrimPrefix(strings.TrimSpace(raw), "@")
	bad := func(msg string) (string, string, error) { return "", "", fmt.Errorf("%w: %s", ErrPersonName, msg) }
	for _, r := range name {
		if r > unicode.MaxASCII {
			return bad("a Talyvor name uses only the letters a to z, digits, dots and underscores; letters from other alphabets are refused, as they can look like these")
		}
	}
	name = strings.ToLower(name)
	switch {
	case len(name) < 3 || len(name) > 20:
		return bad("a Talyvor name is 3 to 20 characters")
	case strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789._") != "":
		return bad("a Talyvor name uses only the letters a to z, digits, dots and underscores")
	case !personNameShape.MatchString(name):
		return bad("a Talyvor name starts and ends with a letter or a digit, and never has two dots or underscores together")
	}
	key = nameKey(name)
	if reservedPersonNames[key] || strings.Contains(key, "talyvor") {
		return bad("that name is kept for Talyvor")
	}
	return name, key, nil
}

// personEmail is raw as one email address, lower-case.
func personEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if a, err := mail.ParseAddress(e); err != nil || a.Address != e || len(e) > 254 {
		return "", fmt.Errorf("%w: %q is not one email address", ErrPersonEmail, raw)
	}
	return e, nil
}

// personPhoneShape is a phone number in international form, as people.phone holds it.
var personPhoneShape = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// personPhone is raw as a phone number in international form, or "" when it is not one.
func personPhone(raw string) string {
	p := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "").Replace(strings.TrimSpace(raw))
	if strings.HasPrefix(p, "00") {
		p = "+" + p[2:]
	}
	if !personPhoneShape.MatchString(p) {
		return ""
	}
	return p
}

// personQuery says what to is — a "name", a "phone" number or an "email" — and answers it as it is kept, or "" when it
// cannot be anyone's.
func personQuery(to string) (kind, value string) {
	to = strings.TrimSpace(to)
	switch {
	case strings.Contains(to, "@") && !strings.HasPrefix(to, "@"):
		if e, err := personEmail(to); err == nil {
			return "email", e
		}
	case strings.HasPrefix(to, "+") || strings.HasPrefix(to, "00") || (to != "" && strings.Trim(to, "0123456789 -().") == ""):
		if p := personPhone(to); p != "" {
			return "phone", p
		}
	default:
		if n, _, err := PersonNameFor(to); err == nil {
			return "name", n
		}
	}
	return "", ""
}

// ShownName is what another person sees of fullName: the first name and the last name's initial.
func ShownName(fullName string) string {
	f := strings.Fields(fullName)
	switch len(f) {
	case 0:
		return ""
	case 1:
		return f[0]
	}
	last := []rune(f[len(f)-1])
	return f[0] + " " + string(unicode.ToUpper(last[0])) + "."
}

// PersonFinding reads personID's settings.
func (s *DualTokenStore) PersonFinding(ctx context.Context, personID string) (PersonFinding, error) {
	var f PersonFinding
	var name, email *string
	err := s.pool.QueryRow(ctx, `SELECT pe.phone, f.name, f.email, COALESCE(f.by_name, false), COALESCE(f.by_phone, false),
		COALESCE(f.by_email, false), COALESCE(f.receive_workspace_id, '')
		FROM people pe LEFT JOIN people_found f ON f.person_id = pe.id WHERE pe.id = $1`, personID).
		Scan(&f.Phone, &name, &email, &f.ByName, &f.ByPhone, &f.ByEmail, &f.ReceiveAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersonFinding{}, ErrNoSuchPerson
	}
	if err != nil {
		return PersonFinding{}, fmt.Errorf("economy: being found: %w", err)
	}
	if name != nil {
		f.Name = *name
	}
	if email != nil {
		f.Email = *email
	}
	if f.CryptoWallets, err = personCryptoWallets(ctx, s.pool, personID); err != nil {
		return PersonFinding{}, err
	}
	return f, nil
}

// SetPersonFinding changes personID's settings as ch says, and answers them as they are now.
func (s *DualTokenStore) SetPersonFinding(ctx context.Context, personID string, ch PersonFindingChange) (PersonFinding, error) {
	var identity string
	if err := s.pool.QueryRow(ctx, `SELECT identity FROM people WHERE id = $1`, personID).Scan(&identity); errors.Is(err, pgx.ErrNoRows) {
		return PersonFinding{}, ErrNoSuchPerson
	} else if err != nil {
		return PersonFinding{}, fmt.Errorf("economy: being found: %w", err)
	}
	identity = workspace.IdentityFromStored(identity)
	personKey, firstID := workspace.PersonKey(identity), workspace.FirstAccountID(identity)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PersonFinding{}, fmt.Errorf("economy: being found: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO people_found (person_id, person_key, first_workspace_id) VALUES ($1, $2, $3)
		ON CONFLICT (person_id) DO NOTHING`, personID, personKey, firstID); err != nil {
		return PersonFinding{}, fmt.Errorf("economy: being found: %w", err)
	}
	var name, key, email *string
	var byName, byPhone, byEmail bool
	var receive string
	if err := tx.QueryRow(ctx, `SELECT name, name_key, email, by_name, by_phone, by_email, receive_workspace_id FROM people_found
		WHERE person_id = $1 FOR UPDATE`, personID).Scan(&name, &key, &email, &byName, &byPhone, &byEmail, &receive); err != nil {
		return PersonFinding{}, fmt.Errorf("economy: being found: %w", err)
	}
	if ch.Name != nil {
		name, key = nil, nil
		if *ch.Name != "" {
			n, k, err := PersonNameFor(*ch.Name)
			if err != nil {
				return PersonFinding{}, err
			}
			name, key = &n, &k
		}
	}
	if ch.Email != nil {
		email = nil
		if *ch.Email != "" {
			e, err := personEmail(*ch.Email)
			if err != nil {
				return PersonFinding{}, err
			}
			email = &e
		}
	}
	for _, sw := range []struct {
		to  *bool
		set **bool
	}{{&byName, &ch.ByName}, {&byPhone, &ch.ByPhone}, {&byEmail, &ch.ByEmail}} {
		if *sw.set != nil {
			*sw.to = **sw.set
		}
	}
	if ch.ReceiveAccountID != nil {
		receive = strings.TrimSpace(*ch.ReceiveAccountID)
		var theirs bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $3
			AND ($3 = $2 OR EXISTS (SELECT 1 FROM person_accounts WHERE person_key = $1 AND workspace_id = $3)))`,
			personKey, firstID, receive).Scan(&theirs); err != nil {
			return PersonFinding{}, fmt.Errorf("economy: being found: %w", err)
		}
		if receive != "" && !theirs {
			return PersonFinding{}, fmt.Errorf("%w: receive_account_id is not one of the person's accounts", ErrPersonFinding)
		}
	}
	if ch.CryptoWallet != nil {
		if err := s.setPersonCryptoWallet(ctx, tx, personID, personKey, firstID, *ch.CryptoWallet); err != nil {
			return PersonFinding{}, err
		}
	}
	switch {
	case byName && name == nil:
		return PersonFinding{}, fmt.Errorf("%w: choose a Talyvor name before being found by it", ErrPersonFinding)
	case byEmail && email == nil:
		return PersonFinding{}, fmt.Errorf("%w: give a confirmed email before being found by it", ErrPersonFinding)
	}
	if _, err := tx.Exec(ctx, `UPDATE people_found SET name = $2, name_key = $3, email = $4, by_name = $5, by_phone = $6,
		by_email = $7, receive_workspace_id = $8, updated_at = now() WHERE person_id = $1`,
		personID, name, key, email, byName, byPhone, byEmail, receive); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if strings.Contains(pgErr.ConstraintName, "email") {
				return PersonFinding{}, fmt.Errorf("%w: another person on Talyvor gave this email", ErrPersonEmail)
			}
			return PersonFinding{}, ErrPersonNameTaken
		}
		return PersonFinding{}, fmt.Errorf("economy: being found: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PersonFinding{}, fmt.Errorf("economy: being found: %w", err)
	}
	return s.PersonFinding(ctx, personID)
}

// foundPerson is a person a lookup found: their shown name, which is all the asking account sees, and what paying
// them needs.
type foundPerson struct {
	ShownName    string
	fullName     string
	personID     string
	personKey    string
	firstID      string
	receiveID    string
	byPhone      bool // found by a phone number
	phoneSandbox bool
}

// FoundPerson is what an account sees of a person it looked up.
type FoundPerson struct {
	ShownName string `json:"shown_name"`
}

// FindPerson looks up to — an exact Talyvor name, phone number or email — for workspaceID, counting it against the
// account's limit an hour, and answers the person's shown name or ErrPersonNotFound.
func (s *DualTokenStore) FindPerson(ctx context.Context, workspaceID, to string) (FoundPerson, error) {
	p, err := s.lookUpPerson(ctx, workspaceID, to)
	if err != nil {
		return FoundPerson{}, err
	}
	return FoundPerson{ShownName: p.ShownName}, nil
}

// lookUpPerson is FindPerson with what paying the person needs.
func (s *DualTokenStore) lookUpPerson(ctx context.Context, workspaceID, to string) (foundPerson, error) {
	if strings.TrimSpace(to) == "" {
		return foundPerson{}, fmt.Errorf("%w: give a name, phone number or email", ErrPersonFinding)
	}
	if err := s.countLookup(ctx, workspaceID); err != nil {
		return foundPerson{}, err
	}
	floor := time.Now().Add(personFindFloor)
	defer func() { time.Sleep(time.Until(floor)) }()
	p, found, err := s.resolvePerson(ctx, to)
	switch {
	case err != nil:
		return foundPerson{}, err
	case !found:
		return foundPerson{}, ErrPersonNotFound
	}
	return p, nil
}

// countLookup records one lookup by workspaceID, or refuses it when the account has made its limit in the last hour.
func (s *DualTokenStore) countLookup(ctx context.Context, workspaceID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: look up a person: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('person_lookups:' || $1, 0))`, workspaceID); err != nil {
		return fmt.Errorf("economy: look up a person: %w", err)
	}
	limit := s.lookupsPerHour()
	var n int
	var oldest *time.Time
	if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) FROM (SELECT created_at FROM person_lookups
		WHERE workspace_id = $1 AND created_at > now() - interval '1 hour' ORDER BY created_at DESC LIMIT $2) l`,
		workspaceID, limit).Scan(&n, &oldest); err != nil {
		return fmt.Errorf("economy: look up a person: %w", err)
	}
	if n >= limit && oldest != nil {
		return fmt.Errorf("%w: an account looks up at most %d people an hour. Try again after %s UTC", ErrPersonLookupLimit,
			limit, oldest.Add(time.Hour+time.Minute).UTC().Format("15:04"))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO person_lookups (workspace_id) VALUES ($1)`, workspaceID); err != nil {
		return fmt.Errorf("economy: look up a person: %w", err)
	}
	return tx.Commit(ctx)
}

// resolvePerson finds the person to names who said yes to being found that way and whose identity check has passed.
func (s *DualTokenStore) resolvePerson(ctx context.Context, to string) (foundPerson, bool, error) {
	kind, value := personQuery(to)
	if kind == "" {
		return foundPerson{}, false, nil
	}
	var p foundPerson
	err := s.pool.QueryRow(ctx, `SELECT f.person_id, f.person_key, f.first_workspace_id, f.receive_workspace_id, pe.phone_sandbox
		FROM people_found f JOIN people pe ON pe.id = f.person_id
		WHERE CASE $1 WHEN 'name' THEN f.by_name AND f.name = $2 WHEN 'phone' THEN f.by_phone AND pe.phone = $2
			WHEN 'email' THEN f.by_email AND f.email = $2 ELSE false END`, kind, value).
		Scan(&p.personID, &p.personKey, &p.firstID, &p.receiveID, &p.phoneSandbox)
	if errors.Is(err, pgx.ErrNoRows) {
		return foundPerson{}, false, nil
	}
	if err != nil {
		return foundPerson{}, false, fmt.Errorf("economy: look up a person: %w", err)
	}
	p.byPhone = kind == "phone"
	if p.fullName, err = s.personVerifiedName(ctx, p.personKey, p.firstID); err != nil || p.fullName == "" {
		return foundPerson{}, false, err
	}
	p.ShownName = ShownName(p.fullName)
	return p, true, nil
}

// personVerifiedName is the name the person's newest identity check that stands passed confirmed, on any of their
// accounts, or "".
func (s *DualTokenStore) personVerifiedName(ctx context.Context, personKey, firstID string) (string, error) {
	return verifiedPersonName(ctx, s.pool, firstID, personKey)
}

// verifiedPersonName is the verified name of the newest standing passed identity check of workspaceID and, when
// personKey is not "", of every account the person opened after it.
func verifiedPersonName(ctx context.Context, q pgxDB, workspaceID, personKey string) (string, error) {
	var name string
	err := q.QueryRow(ctx, `SELECT v.verified_name FROM workspace_verifications v
		WHERE (v.workspace_id = $1 OR v.workspace_id IN (SELECT workspace_id FROM person_accounts WHERE person_key = $2 AND $2 <> ''))
		  AND v.level = 2 AND v.subject = 'person' AND v.status = 'completed' AND v.verified_name <> ''
		  AND v.id = (SELECT max(w.id) FROM workspace_verifications w WHERE w.workspace_id = v.workspace_id AND w.evidence_ref = v.evidence_ref)
		ORDER BY v.id DESC LIMIT 1`, workspaceID, personKey).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("economy: a person's verified name: %w", err)
	}
	return strings.TrimSpace(name), nil
}
