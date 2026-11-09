package economy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// capability_terms.go — B30.9: TERMS FOR EACH CAPABILITY, ACCEPTED BEFORE FIRST USE.
//
// A capability's terms are written in docs/terms/<capability>.md and published as numbered versions
// (capability_terms, migration 0235), each with the words themselves. Lens publishes version 1 of every text it
// carries when it starts, and the operator publishes a changed text as the next version (`lens terms publish`).
// Once a capability has terms, a workspace may not use it — test money or live — until a person of the workspace
// has accepted the latest version; a new version asks again. The texts start headed "Draft — for legal review".
//
// The gate stands beside the freeze's (refuseFrozen) wherever a capability is asked: PostMoney, credits spent on a
// capability (spendForCapability) and a Stripe bill (requireBilledCapability).

var (
	// ErrTermsNotAccepted: the capability's latest terms are not accepted for the workspace.
	ErrTermsNotAccepted = errors.New("economy: refused: this capability's terms are not accepted")
	// ErrNoTerms: no terms are published for the capability.
	ErrNoTerms = errors.New("economy: this capability has no terms")
	// ErrTermsVersionStale: an acceptance of a version that is no longer the latest.
	ErrTermsVersionStale = errors.New("economy: a newer version of these terms is published")
	// ErrTermsUnchanged: a publish of the text the latest version already has.
	ErrTermsUnchanged = errors.New("economy: the text is the latest version's already")
)

// TermsDraftHeading is how every terms text starts until a lawyer has reviewed it.
const TermsDraftHeading = "# Draft — for legal review"

// TermsText is a capability's terms as written: where the text is, and what it says.
type TermsText struct {
	Path string
	Body string
}

// CapabilityTermsTexts reads docs/terms/<capability>.md from fsys (docs/terms.FS) for every capability that has
// one.
func CapabilityTermsTexts(fsys fs.FS) map[string]TermsText {
	out := map[string]TermsText{}
	for _, c := range Capabilities {
		b, err := fs.ReadFile(fsys, c.Key+".md")
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		out[c.Key] = TermsText{Path: "docs/terms/" + c.Key + ".md", Body: string(b)}
	}
	return out
}

// CapabilityTerms is one version of a capability's terms.
type CapabilityTerms struct {
	Capability  string    `json:"capability"`
	Version     int       `json:"version"`
	TextPath    string    `json:"text_path"`
	Body        string    `json:"body,omitempty"`
	BodySHA256  string    `json:"body_sha256"`
	PublishedBy string    `json:"published_by"`
	PublishedAt time.Time `json:"published_at"`
}

// TermsAcceptance is a person's acceptance of one version of a capability's terms for their workspace.
type TermsAcceptance struct {
	WorkspaceID string    `json:"workspace_id"`
	Capability  string    `json:"capability"`
	Version     int       `json:"version"`
	Person      string    `json:"person"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

// WorkspaceTerms is a capability's latest terms and whether the workspace has accepted them.
type WorkspaceTerms struct {
	Name  string          `json:"name"`
	Class CapabilityClass `json:"class"`
	CapabilityTerms
	Accepted *TermsAcceptance `json:"accepted,omitempty"` // of the latest version; nil until it is accepted
	// PreviouslyAccepted is the latest earlier version the workspace accepted, when it has not accepted this one.
	PreviouslyAccepted int `json:"previously_accepted_version,omitempty"`
}

// TermsNeeded is the terms version a refused use must accept first.
type TermsNeeded struct {
	Version int `json:"version"`
}

func termsSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// PublishFirstTerms publishes version 1 of each capability's text that has no terms yet, and says which it published.
// Lens runs it when it starts: a capability's terms ask from the moment Lens carries its text.
func (s *DualTokenStore) PublishFirstTerms(ctx context.Context, texts map[string]TermsText) ([]string, error) {
	var published []string
	for _, c := range Capabilities {
		t, ok := texts[c.Key]
		if !ok {
			continue
		}
		tag, err := s.pool.Exec(ctx, `INSERT INTO capability_terms (capability, version, text_path, body, body_sha256, published_by)
			SELECT $1, 1, $2, $3, $4, 'lens' WHERE NOT EXISTS (SELECT 1 FROM capability_terms WHERE capability = $1)
			ON CONFLICT (capability, version) DO NOTHING`, c.Key, t.Path, t.Body, termsSHA(t.Body))
		if err != nil {
			return published, fmt.Errorf("economy: publish %s's terms: %w", c.Key, err)
		}
		if tag.RowsAffected() == 1 {
			published = append(published, c.Key)
		}
	}
	return published, nil
}

// PublishTerms publishes text as capability's next terms version, which every workspace must accept before its next
// use. A text the latest version already has is ErrTermsUnchanged.
func (s *DualTokenStore) PublishTerms(ctx context.Context, capability, by string, text TermsText) (CapabilityTerms, error) {
	if _, ok := CapabilityByKey(capability); !ok {
		return CapabilityTerms{}, fmt.Errorf("economy: no wallet capability is called %q", capability)
	}
	if strings.TrimSpace(by) == "" || strings.TrimSpace(text.Body) == "" || text.Path == "" {
		return CapabilityTerms{}, errors.New("economy: publishing terms needs who publishes them, the text and where it is")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CapabilityTerms{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('capability_terms:' || $1, 0))`, capability); err != nil {
		return CapabilityTerms{}, err
	}
	t := CapabilityTerms{Capability: capability, TextPath: text.Path, Body: text.Body, BodySHA256: termsSHA(text.Body), PublishedBy: by}
	var latest string
	err = tx.QueryRow(ctx, `SELECT body_sha256 FROM capability_terms WHERE capability = $1 ORDER BY version DESC LIMIT 1`,
		capability).Scan(&latest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CapabilityTerms{}, fmt.Errorf("economy: publish terms: %w", err)
	}
	if latest == t.BodySHA256 {
		return CapabilityTerms{}, fmt.Errorf("%w: %s", ErrTermsUnchanged, capability)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO capability_terms (capability, version, text_path, body, body_sha256, published_by)
		SELECT $1, COALESCE(max(version), 0) + 1, $2, $3, $4, $5 FROM capability_terms WHERE capability = $1
		RETURNING version, published_at`, capability, t.TextPath, t.Body, t.BodySHA256, by).Scan(&t.Version, &t.PublishedAt); err != nil {
		return CapabilityTerms{}, fmt.Errorf("economy: publish terms: %w", err)
	}
	return t, tx.Commit(ctx)
}

// workspaceTermsSQL is each capability's latest terms, with workspaceID's acceptance of them and the latest earlier
// version it accepted.
const workspaceTermsSQL = `SELECT t.capability, t.version, t.text_path, CASE WHEN $3 THEN t.body ELSE '' END, t.body_sha256,
		t.published_by, t.published_at, a.person, a.accepted_at,
		COALESCE((SELECT max(p.version) FROM capability_terms_acceptances p WHERE p.workspace_id = $1 AND p.capability = t.capability
			AND p.version < t.version), 0)
	FROM (SELECT DISTINCT ON (capability) * FROM capability_terms ORDER BY capability, version DESC) t
	LEFT JOIN capability_terms_acceptances a ON a.workspace_id = $1 AND a.capability = t.capability AND a.version = t.version
	WHERE $2 = '' OR t.capability = $2`

func (s *DualTokenStore) workspaceTerms(ctx context.Context, workspaceID, capability string, body bool) ([]WorkspaceTerms, error) {
	rows, err := s.pool.Query(ctx, workspaceTermsSQL, workspaceID, capability, body)
	if err != nil {
		return nil, fmt.Errorf("economy: terms: %w", err)
	}
	byKey, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkspaceTerms, error) {
		var w WorkspaceTerms
		var person *string
		var at *time.Time
		if err := row.Scan(&w.Capability, &w.Version, &w.TextPath, &w.Body, &w.BodySHA256, &w.PublishedBy, &w.PublishedAt,
			&person, &at, &w.PreviouslyAccepted); err != nil {
			return w, err
		}
		if person != nil && at != nil {
			w.Accepted = &TermsAcceptance{WorkspaceID: workspaceID, Capability: w.Capability, Version: w.Version, Person: *person, AcceptedAt: *at}
			w.PreviouslyAccepted = 0
		}
		return w, nil
	})
	if err != nil {
		return nil, fmt.Errorf("economy: terms: %w", err)
	}
	found := make(map[string]WorkspaceTerms, len(byKey))
	for _, w := range byKey {
		found[w.Capability] = w
	}
	// In the capabilities' order, each named and classed.
	out := make([]WorkspaceTerms, 0, len(byKey))
	for _, c := range Capabilities {
		if w, ok := found[c.Key]; ok {
			w.Name, w.Class = c.Name, c.Class
			out = append(out, w)
		}
	}
	return out, nil
}

// WorkspaceTermsList is every capability with terms, its latest version and whether workspaceID has accepted it.
func (s *DualTokenStore) WorkspaceTermsList(ctx context.Context, workspaceID string) ([]WorkspaceTerms, error) {
	return s.workspaceTerms(ctx, workspaceID, "", false)
}

// WorkspaceTermsFor is capability's latest terms, with their text, and whether workspaceID has accepted them; ErrNoTerms
// when it has none.
func (s *DualTokenStore) WorkspaceTermsFor(ctx context.Context, workspaceID, capability string) (WorkspaceTerms, error) {
	ts, err := s.workspaceTerms(ctx, workspaceID, capability, true)
	if err != nil {
		return WorkspaceTerms{}, err
	}
	if len(ts) == 0 {
		return WorkspaceTerms{}, fmt.Errorf("%w: %q", ErrNoTerms, capability)
	}
	return ts[0], nil
}

// AcceptTerms records that person accepted version of capability's terms for workspaceID, from ip (kept only as an
// HMAC). Only the latest version may be accepted — an earlier one is ErrTermsVersionStale — and accepting it again
// answers the first acceptance.
func (s *DualTokenStore) AcceptTerms(ctx context.Context, workspaceID, capability string, version int, person, ip string) (TermsAcceptance, error) {
	if workspaceID == "" || strings.TrimSpace(person) == "" {
		return TermsAcceptance{}, errors.New("economy: an acceptance needs the workspace and the person accepting")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TermsAcceptance{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var latest int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM capability_terms WHERE capability = $1`, capability).Scan(&latest); err != nil {
		return TermsAcceptance{}, fmt.Errorf("economy: accept terms: %w", err)
	}
	switch {
	case latest == 0:
		return TermsAcceptance{}, fmt.Errorf("%w: %q", ErrNoTerms, capability)
	case version != latest:
		return TermsAcceptance{}, fmt.Errorf("%w: version %d of %s's terms is the latest, not %d. Read it and accept it",
			ErrTermsVersionStale, latest, capability, version)
	}
	hash, err := ipHash(ctx, tx, ip)
	if err != nil {
		return TermsAcceptance{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO capability_terms_acceptances (workspace_id, capability, version, person, ip_hash)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (workspace_id, capability, version) DO NOTHING`,
		workspaceID, capability, version, person, hash); err != nil {
		return TermsAcceptance{}, fmt.Errorf("economy: accept terms: %w", err)
	}
	a := TermsAcceptance{WorkspaceID: workspaceID, Capability: capability, Version: version}
	if err := tx.QueryRow(ctx, `SELECT person, accepted_at FROM capability_terms_acceptances
		WHERE workspace_id = $1 AND capability = $2 AND version = $3`, workspaceID, capability, version).Scan(&a.Person, &a.AcceptedAt); err != nil {
		return TermsAcceptance{}, fmt.Errorf("economy: accept terms: %w", err)
	}
	return a, tx.Commit(ctx)
}

// ipHash is the HMAC-SHA256 of ip under capability_terms_ip_key, which it makes on first use; "" for no known address.
func ipHash(ctx context.Context, tx pgx.Tx, ip string) (string, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "", nil
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return "", err
	}
	var key []byte
	err := tx.QueryRow(ctx, `WITH made AS (INSERT INTO capability_terms_ip_key (key) VALUES ($1) ON CONFLICT (id) DO NOTHING RETURNING key)
		SELECT key FROM made UNION ALL SELECT key FROM capability_terms_ip_key LIMIT 1`, fresh).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another acceptance made the key while this statement ran: a new statement sees it.
		err = tx.QueryRow(ctx, `SELECT key FROM capability_terms_ip_key`).Scan(&key)
	}
	if err != nil {
		return "", fmt.Errorf("economy: terms ip key: %w", err)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(ip))
	return hex.EncodeToString(m.Sum(nil)), nil
}

// termsRefusal refuses capability c for workspaceID while c has terms whose latest version the workspace has not
// accepted — test money or live, whatever c's class. A capability with no terms asks nothing.
func termsRefusal(ctx context.Context, q pgxDB, workspaceID string, c Capability) (*CapabilityRefusal, error) {
	// B17.354: the terms are what a customer accepts from Talyvor, so Talyvor's own workspace — which pays the marketplace's
	// sellers — has none to accept. No customer's workspace is named it: theirs are "u" and a hash (deriveWorkspaceID).
	if workspaceID == TalyvorWorkspace {
		return nil, nil
	}
	var version int
	var accepted bool
	err := q.QueryRow(ctx, `SELECT t.version, EXISTS (SELECT 1 FROM capability_terms_acceptances a
			WHERE a.workspace_id = $1 AND a.capability = t.capability AND a.version = t.version)
		FROM capability_terms t WHERE t.capability = $2 ORDER BY t.version DESC LIMIT 1`, workspaceID, c.Key).Scan(&version, &accepted)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && accepted) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("economy: capability terms: %w", err)
	}
	return &CapabilityRefusal{Capability: c, Terms: &TermsNeeded{Version: version}}, nil
}

// refuseUnaccepted is termsRefusal as an error: nil when workspaceID has accepted c's latest terms, or c has none.
func refuseUnaccepted(ctx context.Context, q pgxDB, workspaceID string, c Capability) error {
	return refusalErr(termsRefusal(ctx, q, workspaceID, c))
}
