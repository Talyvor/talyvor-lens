// Package market is the Talyvor marketplace's catalog (B20): listings a workspace publishes — an agent,
// a prompt, a skill, an evaluation or a pipeline — each a history of versions.
//
// B20.1 is publishing: a listing carries its owner workspace, kind, title, description, price per use
// (µLXC, 0 for free), visibility and its artifact; publishing a change adds a version and never rewrites
// one (migration 0148 refuses an UPDATE), so what was built on an earlier version keeps working. Every
// publish is scanned (scan.go) and refused for a secret, personal data, or — outside an evaluation — a
// prompt injection.
package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxArtifactBytes bounds one version's artifact.
const MaxArtifactBytes = 256 << 10

var (
	// ErrInvalid wraps every reason a listing or version is not well formed.
	ErrInvalid = errors.New("market: invalid listing")
	// ErrNotFound: no such listing (or version) this caller may see.
	ErrNotFound = errors.New("market: no such listing")
)

// RefusedError is a publish the scan refused; Scan says what it found.
type RefusedError struct{ Scan Scan }

func (e *RefusedError) Error() string {
	return "market: the listing cannot be published: " + e.Scan.Refused
}

// requiredField is the artifact field each kind cannot do without.
var requiredField = map[string]string{
	"agent":      "system_prompt", // with its model and tools, as the seller runs it
	"prompt":     "template",
	"skill":      "instructions",
	"evaluation": "cases", // a non-empty list of test cases
	"pipeline":   "steps", // a non-empty list of steps
}

// Listing is one published listing.
type Listing struct {
	ID              string    `json:"id"`
	WorkspaceID     string    `json:"workspace_id"`
	Kind            string    `json:"kind"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	PricePerUseULXC int64     `json:"price_per_use_ulxc"` // 0 is free
	Visibility      string    `json:"visibility"`         // public | unlisted | private | room
	RoomID          string    `json:"room_id,omitempty"`  // a room's contribution: the room whose members see it (B32.31)
	LatestVersion   int       `json:"latest_version"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	ReviewStatus    string    `json:"review_status"`           // approved | held | taken_down (review.go)
	ReviewReason    string    `json:"review_reason,omitempty"` // why it is held or was taken down
	HeldVersion     int       `json:"held_version,omitempty"`  // B17.206: the first version held for review; its owner's alone, with every later one
	RemixPolicy     string    `json:"remix_policy"`            // none | free | royalty (lineage.go)
	RemixShareBPS   int       `json:"remix_share_bps"`         // a royalty's share of each remix's sales
	Offers          []Offer   `json:"offers"`                  // how it is sold (offers.go); none: it is free
	PriceNote       string    `json:"price_note,omitempty"`    // how its offers' display prices are charged (B32.51)
	Capabilities    []string  `json:"capabilities"`            // what it can do, from market_capabilities (B32.50)
	Versions        []Version `json:"versions,omitempty"`
}

// Version is one version of a listing. Artifact is present only for the listing's owner (B20.2 is how
// anyone else uses it); what a use of it asks for — Needs and Model — is shown to everyone who may use it.
type Version struct {
	Version        int             `json:"version"`
	ArtifactSHA256 string          `json:"artifact_sha256"`
	Changelog      string          `json:"changelog,omitempty"`
	Scan           Scan            `json:"scan"`
	CreatedAt      time.Time       `json:"created_at"`
	Needs          Needs           `json:"needs"`
	Privacy        Privacy         `json:"privacy"`           // which models it calls, the tools it uses and the data it keeps (B41.193, privacy.go)
	Parents        []Parent        `json:"parents,omitempty"` // the listing versions it builds on (lineage.go)
	Artifact       json.RawMessage `json:"artifact,omitempty"`
}

// Needs is what a use of a version asks the buyer for (B20.3): an input (an agent or a skill), the
// prompt's {{variables}}, and the model it runs on unless the buyer names another ("" when none).
type Needs struct {
	Input     bool     `json:"input"`
	Variables []string `json:"variables"`
	Model     string   `json:"model"`
	Cases     int      `json:"cases,omitempty"` // an evaluation's cases
}

// needsOf reads what a use of kind's artifact asks for, without giving the artifact away.
func needsOf(kind string, raw []byte) Needs {
	n := Needs{Variables: []string{}}
	var a map[string]any
	if json.Unmarshal(raw, &a) != nil {
		return n
	}
	n.Model, _ = a["model"].(string)
	switch kind {
	case "agent", "skill", "pipeline":
		n.Input = true
	case "prompt":
		t, _ := a["template"].(string)
		seen := map[string]bool{}
		for _, m := range variable.FindAllStringSubmatch(t, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				n.Variables = append(n.Variables, m[1])
			}
		}
	case "evaluation":
		cases, _ := a["cases"].([]any)
		n.Cases = len(cases)
	}
	return n
}

// Draft is what a publish carries. Offers say how the listing is sold (offers.go); without them, a price per use
// is one per_use commercial offer at that price.
type Draft struct {
	Kind            string          `json:"kind"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	PricePerUseULXC int64           `json:"price_per_use_ulxc"`
	Offers          []Offer         `json:"offers"`
	Visibility      string          `json:"visibility"`
	Artifact        json.RawMessage `json:"artifact"`
	Changelog       string          `json:"changelog"`
	RemixPolicy     string          `json:"remix_policy"`    // none (default) | free | royalty
	RemixShareBPS   int             `json:"remix_share_bps"` // royalty: 1 to LENS_LINEAGE_MAX_SHARE_BPS
	Parents         []ParentRef     `json:"parents"`         // the listings it builds on (lineage.go)
	Capabilities    []string        `json:"capabilities"`    // what it can do, from market_capabilities (B32.50)
	// RoomID publishes the listing as a room's contribution, seen by its owner and the room's members only (B32.31).
	// internal/rooms sets it once it has checked the publisher is a member; a request body never does.
	RoomID string `json:"-"`
}

// draftOffers is the set of offers a draft publishes, its price per use folded in.
func draftOffers(d Draft, trialMax int) ([]Offer, error) {
	if d.PricePerUseULXC%ulxcPerUSDMicro != 0 {
		return nil, invalid("the price per use must be a whole number of µUSD (a multiple of %d µLXC)", ulxcPerUSDMicro)
	}
	if len(d.Offers) == 0 {
		if d.PricePerUseULXC == 0 {
			return nil, nil
		}
		return []Offer{{Kind: OfferPerUse, Licence: LicenceCommercial, PriceUSDMicros: d.PricePerUseULXC / ulxcPerUSDMicro}}, nil
	}
	if err := checkOffers(d.Offers, trialMax); err != nil {
		return nil, err
	}
	if price, _ := perUseULXC(d.Offers); d.PricePerUseULXC != 0 && d.PricePerUseULXC != price {
		return nil, invalid("price_per_use_ulxc says one price and the per_use commercial offer another: give the offer alone")
	}
	return d.Offers, nil
}

// Store reads and writes the catalog.
type Store struct {
	pool     *pgxpool.Pool
	clock    func() time.Time // nil: time.Now; when a licence starts, ends and is in force (B32.19)
	trialMax *int             // nil: DefaultTrialMax; the most trial uses an offer may give (B32.21)

	lineageMaxShare *int // nil: DefaultLineageMaxShareBPS (B32.24)
	lineageMaxDepth *int // nil: DefaultLineageMaxDepth (B32.24)
	lineageTotalCap *int // nil: DefaultLineageTotalCapBPS (B32.26)

	similarity *similarityCheck // nil: no version is fingerprinted or held as a copy (B32.46, similarity.go)

	ipCounterDays *int // nil: DefaultIPCounterDays; how long a seller has to counter an IP claim (B32.47)

	tax        *Tax // nil: no billed use carries tax (B32.39, tax.go)
	liveStripe bool // Lens's Stripe key is live: a sale Talyvor cannot account for the tax on is refused (B32.39)

	receipts *Receipts // nil: a paid marketplace bill gets no receipt (B32.40, receipts.go)

	selfBilling *SelfBilling // nil: no payout is a self-billed invoice (B32.43, selfbill.go)

	displayRates DisplayRates // nil: prices are shown in US dollars only (B32.51, display.go)

	moneyPayer     EarningsPayer // nil: sellers are paid through Stripe Connect only (B46.22, payout_money.go)
	moneyPayerFrom string        // the workspace whose money accounts pay them
}

func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// checkArtifact validates artifact for kind and scans it with the listing's words.
func checkArtifact(kind string, artifact json.RawMessage, words ...string) (canonical []byte, sum string, scan Scan, err error) {
	if len(artifact) == 0 || len(artifact) > MaxArtifactBytes {
		return nil, "", scan, invalid("the artifact must be a JSON object of at most %d KB", MaxArtifactBytes>>10)
	}
	var obj map[string]any
	if err := json.Unmarshal(artifact, &obj); err != nil {
		return nil, "", scan, invalid("the artifact must be a JSON object")
	}
	field := requiredField[kind]
	switch v := obj[field].(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, "", scan, invalid("a %s's artifact needs %q", kind, field)
		}
	case []any:
		if len(v) == 0 {
			return nil, "", scan, invalid("a %s's artifact needs a non-empty %q", kind, field)
		}
	default:
		return nil, "", scan, invalid("a %s's artifact needs %q", kind, field)
	}
	if err := checkPrivacy(obj); err != nil {
		return nil, "", scan, err
	}
	canonical, _ = json.Marshal(obj)
	h := sha256.Sum256(canonical)
	scan = ScanText(kind, stringsIn(obj, words))
	if scan.Refused != "" {
		return nil, "", scan, &RefusedError{Scan: scan}
	}
	if kind == "pipeline" && scan.Held == "" {
		scan.Held = reviewPipeline(obj)
	}
	return canonical, hex.EncodeToString(h[:]), scan, nil
}

// Publish creates a listing owned by workspaceID, with its first version.
func (s *Store) Publish(ctx context.Context, workspaceID string, d Draft) (Listing, error) {
	l, _, err := s.PublishOnce(ctx, workspaceID, "", d)
	return l, err
}

// PublishOnce is Publish under an idempotency key (B17.34): a key workspaceID has already published with
// answers that listing, as its owner reads it, with again true, and publishes nothing. An empty key always
// publishes.
func (s *Store) PublishOnce(ctx context.Context, workspaceID, key string, d Draft) (l Listing, again bool, err error) {
	if key != "" {
		if l, err := s.publishedWith(ctx, workspaceID, key); !errors.Is(err, ErrNotFound) {
			return l, err == nil, err
		}
	}
	l, err = s.publish(ctx, workspaceID, key, d)
	if errors.Is(err, errKeyTaken) {
		// The same key published concurrently and won the insert: answer its listing.
		l, err = s.publishedWith(ctx, workspaceID, key)
		return l, err == nil, err
	}
	return l, false, err
}

// errKeyTaken is publish's answer when another publish holds its key.
var errKeyTaken = errors.New("market: publish key taken")

// publishedWith reads the listing workspaceID published with key, or ErrNotFound.
func (s *Store) publishedWith(ctx context.Context, workspaceID, key string) (Listing, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM market_listings WHERE workspace_id = $1 AND publish_key = $2`, workspaceID, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, ErrNotFound
	}
	if err != nil {
		return Listing{}, fmt.Errorf("market: publish key: %w", err)
	}
	return s.Get(ctx, workspaceID, id)
}

func (s *Store) publish(ctx context.Context, workspaceID, key string, d Draft) (Listing, error) {
	if _, ok := requiredField[d.Kind]; !ok {
		return Listing{}, invalid("kind must be agent, prompt, skill, evaluation or pipeline")
	}
	d.Title = strings.TrimSpace(d.Title)
	if d.Title == "" || len(d.Title) > 200 {
		return Listing{}, invalid("a listing needs a title of at most 200 characters")
	}
	if d.PricePerUseULXC < 0 {
		return Listing{}, invalid("the price per use cannot be negative (0 is free)")
	}
	offers, err := draftOffers(d, s.maxTrials())
	if err != nil {
		return Listing{}, err
	}
	d.PricePerUseULXC, _ = perUseULXC(offers)
	switch {
	case d.RoomID != "":
		d.Visibility = VisibilityRoom
	case d.Visibility == "":
		d.Visibility = "public"
	case d.Visibility == VisibilityRoom:
		return Listing{}, invalid("a room's listing is published as a contribution to the room: POST /v1/rooms/{id}/contributions")
	case d.Visibility != "public" && d.Visibility != "unlisted" && d.Visibility != "private":
		return Listing{}, invalid("visibility must be public, unlisted or private")
	}
	terms, err := s.checkRemixTerms(RemixTerms{Policy: d.RemixPolicy, ShareBPS: d.RemixShareBPS})
	if err != nil {
		return Listing{}, err
	}
	caps, err := checkCapabilities(ctx, s.pool, d.Capabilities)
	if err != nil {
		return Listing{}, err
	}
	artifact, sum, scan, err := checkArtifact(d.Kind, d.Artifact, d.Title, d.Description, d.Changelog)
	if err != nil {
		return Listing{}, err
	}
	fp := s.fingerprintOf(ctx, artifact, d.Title, d.Description)
	if err := s.checkCopy(ctx, s.pool, &scan, fp, workspaceID, d.RoomID, parentIDs(d.Parents)); err != nil {
		return Listing{}, err
	}
	scanJSON, _ := json.Marshal(scan)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Listing{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	l := Listing{ID: "lst_" + uuid.NewString(), WorkspaceID: workspaceID, Kind: d.Kind, Title: d.Title, Description: d.Description,
		PricePerUseULXC: d.PricePerUseULXC, Visibility: d.Visibility, RoomID: d.RoomID, LatestVersion: 1, ReviewStatus: ReviewApproved,
		RemixPolicy: terms.Policy, RemixShareBPS: terms.ShareBPS}
	if scan.Held != "" {
		l.ReviewStatus, l.ReviewReason = ReviewHeld, scan.Held
	}
	err = tx.QueryRow(ctx, `INSERT INTO market_listings (id, workspace_id, kind, title, description, price_per_use_ulxc, visibility, review_status, review_reason, publish_key,
			remix_policy, remix_share_bps, room_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12, NULLIF($13, ''))
		ON CONFLICT (workspace_id, publish_key) WHERE publish_key IS NOT NULL DO NOTHING RETURNING created_at, updated_at`,
		l.ID, workspaceID, d.Kind, d.Title, d.Description, d.PricePerUseULXC, d.Visibility, l.ReviewStatus, l.ReviewReason, key,
		l.RemixPolicy, l.RemixShareBPS, d.RoomID).Scan(&l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, errKeyTaken
	}
	if err != nil {
		return Listing{}, fmt.Errorf("market: publish: %w", err)
	}
	v := Version{Version: 1, ArtifactSHA256: sum, Changelog: d.Changelog, Scan: scan, Needs: needsOf(d.Kind, artifact), Artifact: artifact}
	// jsonb goes as text: behind PgBouncer (simple protocol) a []byte goes out as bytea, which jsonb refuses (B17.16).
	if err := tx.QueryRow(ctx, `INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256, changelog, scan)
		VALUES ($1, 1, $2, $3, $4, $5) RETURNING created_at`, l.ID, string(artifact), sum, d.Changelog, string(scanJSON)).Scan(&v.CreatedAt); err != nil {
		return Listing{}, fmt.Errorf("market: publish: %w", err)
	}
	if err := writeFingerprint(ctx, tx, l.ID, 1, fp); err != nil {
		return Listing{}, err
	}
	if l.Offers, err = writeOffers(ctx, tx, l.ID, offers); err != nil {
		return Listing{}, err
	}
	if err := writeCapabilities(ctx, tx, l.ID, caps); err != nil {
		return Listing{}, err
	}
	l.Capabilities = caps
	if v.Parents, err = s.declareParents(ctx, tx, workspaceID, l.ID, 1, d.Parents); err != nil {
		return Listing{}, err
	}
	l.Versions = []Version{v}
	return l, tx.Commit(ctx)
}

// PublishVersion adds a version to one of workspaceID's listings; the earlier ones stay as they were. A
// version the review holds is held with every later one, the earlier ones staying live (B17.206); a held
// listing stays held whole. A clean one never releases a hold (an admin does), and a taken-down listing
// takes no versions. The new version keeps the parents of the one before it and adds
// those parents declares (B32.24).
func (s *Store) PublishVersion(ctx context.Context, workspaceID, listingID string, artifact json.RawMessage, changelog string, parents []ParentRef) (Version, error) {
	// B32.46: fingerprinted before the listing is locked, so the embeddings call never holds its row.
	var fp *fingerprint
	if s.similarity != nil {
		var title, description string
		err := s.pool.QueryRow(ctx, `SELECT title, description FROM market_listings WHERE id = $1 AND workspace_id = $2`, listingID, workspaceID).
			Scan(&title, &description)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Version{}, fmt.Errorf("market: version: %w", err)
		}
		fp = s.fingerprintOf(ctx, artifact, title, description)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Version{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind, title, description, review, roomID string
	var latest int
	err = tx.QueryRow(ctx, `SELECT kind, title, description, latest_version, review_status, coalesce(room_id, '') FROM market_listings
		WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, listingID, workspaceID).Scan(&kind, &title, &description, &latest, &review, &roomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	if review == ReviewTakenDown {
		return Version{}, ErrTakenDown
	}
	canonical, sum, scan, err := checkArtifact(kind, artifact, title, description, changelog)
	if err != nil {
		return Version{}, err
	}
	// B32.46: the parents it declares and those it carries forward may be copied.
	carried, err := parentsOf(ctx, tx, listingID, latest)
	if err != nil {
		return Version{}, err
	}
	declared := parentIDs(parents)
	for _, p := range carried {
		declared = append(declared, p.ListingID)
	}
	if err := s.checkCopy(ctx, tx, &scan, fp, workspaceID, roomID, declared); err != nil {
		return Version{}, err
	}
	scanJSON, _ := json.Marshal(scan)
	v := Version{Version: latest + 1, ArtifactSHA256: sum, Changelog: changelog, Scan: scan, Needs: needsOf(kind, canonical), Artifact: canonical}
	if err := tx.QueryRow(ctx, `INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256, changelog, scan)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`, listingID, v.Version, string(canonical), sum, changelog, string(scanJSON)).Scan(&v.CreatedAt); err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	if err := writeFingerprint(ctx, tx, listingID, v.Version, fp); err != nil {
		return Version{}, err
	}
	if v.Parents, err = s.declareParents(ctx, tx, workspaceID, listingID, v.Version, parents); err != nil {
		return Version{}, err
	}
	if scan.Held != "" {
		hold, args := `UPDATE market_listings SET review_status = 'held', review_reason = $2 WHERE id = $1`, []any{listingID, fmt.Sprintf("version %d: %s", v.Version, scan.Held)}
		if review == ReviewApproved {
			hold, args = `UPDATE market_listings SET held_version = COALESCE(held_version, $3), review_reason = $2 WHERE id = $1`, append(args, v.Version)
		}
		if _, err := tx.Exec(ctx, hold, args...); err != nil {
			return Version{}, fmt.Errorf("market: version: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE market_listings SET latest_version = $2, updated_at = now() WHERE id = $1`, listingID, v.Version); err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	return v, tx.Commit(ctx)
}

const listingColumns = `id, workspace_id, kind, title, description, price_per_use_ulxc, visibility, latest_version, created_at, updated_at, review_status, review_reason, remix_policy, remix_share_bps, room_id, held_version`

// scanListing reads listingColumns, then into extra any columns the query selects after them.
func scanListing(row pgx.Row, extra ...any) (Listing, error) {
	var l Listing
	var room *string
	var held *int
	err := row.Scan(append([]any{&l.ID, &l.WorkspaceID, &l.Kind, &l.Title, &l.Description, &l.PricePerUseULXC, &l.Visibility, &l.LatestVersion, &l.CreatedAt, &l.UpdatedAt,
		&l.ReviewStatus, &l.ReviewReason, &l.RemixPolicy, &l.RemixShareBPS, &room, &held}, extra...)...)
	if room != nil {
		l.RoomID = *room
	}
	if held != nil {
		l.HeldVersion = *held
	}
	return l, err
}

// asSeenBy is l as viewer sees it: to anyone but its owner, a held version and those after it are not there (B17.206).
func (l *Listing) asSeenBy(viewer string) {
	if l.HeldVersion > 0 && viewer != l.WorkspaceID {
		l.LatestVersion, l.HeldVersion, l.ReviewReason = l.HeldVersion-1, 0, ""
	}
}

func (s *Store) list(ctx context.Context, where string, args ...any) ([]Listing, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+listingColumns+` FROM market_listings WHERE `+where+` ORDER BY created_at DESC, id LIMIT 500`, args...)
	if err != nil {
		return nil, fmt.Errorf("market: listings: %w", err)
	}
	defer rows.Close()
	out := []Listing{}
	ids := []string{}
	for rows.Next() {
		l, err := scanListing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
		ids = append(ids, l.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := s.withOffersAndCapabilities(ctx, ids, func(i int) *Listing { return &out[i] }); err != nil {
		return nil, err
	}
	return out, nil
}

// orNone answers a listing's offers as a list, empty when it has none.
func orNone(offers []Offer) []Offer {
	if offers == nil {
		return []Offer{}
	}
	return offers
}

// OwnListings reads workspaceID's listings, whatever their visibility.
func (s *Store) OwnListings(ctx context.Context, workspaceID string) ([]Listing, error) {
	return s.list(ctx, `workspace_id = $1`, workspaceID)
}

// Catalog reads the public listings the review approved, of one kind when kind is set.
func (s *Store) Catalog(ctx context.Context, kind string) ([]Listing, error) {
	where, args := `visibility = 'public' AND review_status = 'approved'`, []any{}
	if kind != "" {
		where, args = where+` AND kind = $1`, append(args, kind)
	}
	out, err := s.list(ctx, where, args...)
	for i := range out {
		out[i].asSeenBy("")
	}
	return out, err
}

// SearchQuery is what a buyer looks for in the catalog (B32.23). Each field set narrows it.
type SearchQuery struct {
	Text              string // the words to search the title and description for
	Kind              string
	Capability        string // what it should do: one of market_capabilities (B32.50)
	Licence           string // sold under this licence
	MaxPriceUSDMicros *int64 // one use costs at most this: free, or a per_use commercial offer at or under it
}

// MaxSearchResults is the most listings Search answers, and a page of Discover.
const MaxSearchResults = 50

// Search reads the first page of what Discover finds for q: by relevance with words, by trending without (B32.50).
func (s *Store) Search(ctx context.Context, q SearchQuery) ([]Listing, error) {
	page, err := s.Discover(ctx, DiscoverQuery{Q: q.Text, Kind: q.Kind, Capability: q.Capability, Licence: q.Licence, MaxPricePerUse: q.MaxPriceUSDMicros})
	if err != nil {
		return nil, err
	}
	out := make([]Listing, len(page.Listings))
	for i, h := range page.Listings {
		out[i] = h.Listing
	}
	return out, nil
}

// VisibilityRoom is a room's contribution (B32.31): seen by its owner and its room's live members, never in the catalog.
const VisibilityRoom = "room"

// rowQuerier is a pool or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// hidden says whether viewer may not see l: a private listing, or one held or taken down, is its owner's alone, and a
// room's contribution is its owner's and its room's live members' — one EXISTS query, asked of a room's listing only.
func hidden(ctx context.Context, q rowQuerier, l Listing, viewer string) (bool, error) {
	switch {
	case l.WorkspaceID == viewer:
		return false, nil
	case l.Visibility == "private" || l.ReviewStatus != ReviewApproved:
		return true, nil
	case l.Visibility != VisibilityRoom:
		return false, nil
	}
	var member bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM room_members WHERE room_id = $1 AND workspace_id = $2 AND removed_at IS NULL)`,
		l.RoomID, viewer).Scan(&member)
	return !member, err
}

// visibleListing reads a listing as viewer may see it, its row locked by lock (a FOR clause, or ""): pgx.ErrNoRows
// when there is none or viewer may not see it.
func visibleListing(ctx context.Context, q rowQuerier, viewer, listingID, lock string) (Listing, error) {
	l, err := scanListing(q.QueryRow(ctx, `SELECT `+listingColumns+` FROM market_listings WHERE id = $1 `+lock, listingID))
	if err != nil {
		return l, err
	}
	if h, err := hidden(ctx, q, l, viewer); err != nil {
		return l, err
	} else if h {
		return l, pgx.ErrNoRows
	}
	l.asSeenBy(viewer)
	return l, nil
}

// Get reads a listing and its versions as viewerWorkspace sees it: a private, held or taken-down listing
// only by its owner, and each version's artifact only for its owner — or, a room's contribution, for its room's
// members too, who build on it together (B32.31).
func (s *Store) Get(ctx context.Context, viewerWorkspace, listingID string) (Listing, error) {
	l, err := visibleListing(ctx, s.pool, viewerWorkspace, listingID, "")
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, ErrNotFound
	}
	if err != nil {
		return Listing{}, fmt.Errorf("market: listing: %w", err)
	}
	owner := l.WorkspaceID == viewerWorkspace || l.Visibility == VisibilityRoom
	offers, err := activeOffers(ctx, s.pool, listingID)
	if err != nil {
		return Listing{}, err
	}
	l.Offers = orNone(offers[listingID])
	caps, err := listingCapabilities(ctx, s.pool, listingID)
	if err != nil {
		return Listing{}, err
	}
	l.Capabilities = orNoCapabilities(caps[listingID])
	parents, err := versionParents(ctx, s.pool, listingID, 0)
	if err != nil {
		return Listing{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT version, artifact_sha256, changelog, scan, created_at, artifact FROM market_listing_versions
		WHERE listing_id = $1 AND version <= $2 ORDER BY version`, listingID, l.LatestVersion)
	if err != nil {
		return Listing{}, fmt.Errorf("market: versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v Version
		var scan, artifact []byte
		if err := rows.Scan(&v.Version, &v.ArtifactSHA256, &v.Changelog, &scan, &v.CreatedAt, &artifact); err != nil {
			return Listing{}, err
		}
		_ = json.Unmarshal(scan, &v.Scan)
		v.Needs = needsOf(l.Kind, artifact)
		v.Parents = parents[v.Version]
		v.Artifact = artifact
		l.Versions = append(l.Versions, v)
	}
	if err := rows.Err(); err != nil {
		return Listing{}, err
	}
	rows.Close() // a pipeline's label reads its steps' listings
	for i, v := range l.Versions {
		var a map[string]any
		_ = json.Unmarshal(v.Artifact, &a)
		l.Versions[i].Privacy = s.privacyOf(ctx, l, a)
		if !owner {
			l.Versions[i].Artifact = nil
		}
	}
	return l, nil
}
