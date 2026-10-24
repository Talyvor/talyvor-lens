package market

// B32.24 — remix terms and the family tree.
//
// A listing says whether others may build on it (RemixPolicy): none, free, or royalty with the share of each sale a
// remix sends up to it (RemixShareBPS, 1 to LENS_LINEAGE_MAX_SHARE_BPS). A version that builds on other listings
// declares them as its parents when it is published; each parent must be one the publisher may see that allows
// remixes — or the publisher's own — and none may descend from the version's own listing. Each declaration is one
// market_lineage edge (migration 0206) holding the share the parent's terms gave when it was declared, so a later
// change to those terms never touches it. A new version keeps the parents of the one before it: a remix cannot drop
// its originals by publishing again.
//
// A room's contribution (B32.31) is built on by the members of its room under the room's terms instead of a remix
// grant: its edge is a room_fork at the share the room's current terms give the original.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The remix policies.
const (
	RemixNone    = "none"
	RemixFree    = "free"
	RemixRoyalty = "royalty"
)

// Where an edge of the family tree came from: a publisher declared it, a room fork made it (B32.31), or an upheld
// claim attributed it (B32.47).
const (
	LineageDeclared = "declared"
	LineageRoomFork = "room_fork"
	LineageClaim    = "claim"
)

const (
	// DefaultLineageMaxShareBPS is the largest remix share a royalty listing may ask (LENS_LINEAGE_MAX_SHARE_BPS):
	// 3000, 30% — a proposal for Nicolai.
	DefaultLineageMaxShareBPS = 3000
	// DefaultLineageMaxDepth is how many generations the lineage read walks up (LENS_LINEAGE_MAX_DEPTH): 5 — a
	// proposal for Nicolai.
	DefaultLineageMaxDepth = 5
)

var (
	// ErrLineageCycle is a parent that descends from the listing declaring it.
	ErrLineageCycle = fmt.Errorf("%w: a listing cannot build on one of its own remixes", ErrInvalid)
	// ErrNotRemixable is a parent whose owner does not allow remixes.
	ErrNotRemixable = fmt.Errorf("%w: that listing does not allow remixes", ErrInvalid)
)

// ParentRef is a listing version a publish declares it builds on; version 0 is the parent's latest.
type ParentRef struct {
	ListingID string `json:"listing_id"`
	Version   int    `json:"version"`
}

// Parent is one edge of the family tree: the listing version a version builds on and the share of its sales that
// parent was given when it was declared.
type Parent struct {
	ListingID string    `json:"listing_id"`
	Version   int       `json:"version"`
	ShareBPS  int       `json:"share_bps"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// RemixTerms is whether a listing may be remixed and the share of each remix's sales it asks.
type RemixTerms struct {
	Policy   string `json:"remix_policy"`
	ShareBPS int    `json:"remix_share_bps"`
}

// SetLineageLimits sets the largest remix share (LENS_LINEAGE_MAX_SHARE_BPS) and how many generations the lineage
// read walks (LENS_LINEAGE_MAX_DEPTH).
func (s *Store) SetLineageLimits(maxShareBPS, maxDepth int) {
	s.lineageMaxShare, s.lineageMaxDepth = &maxShareBPS, &maxDepth
}

func (s *Store) maxRemixShare() int {
	if s.lineageMaxShare == nil {
		return DefaultLineageMaxShareBPS
	}
	return *s.lineageMaxShare
}

func (s *Store) maxLineageDepth() int {
	if s.lineageMaxDepth == nil {
		return DefaultLineageMaxDepth
	}
	return *s.lineageMaxDepth
}

// checkRemixTerms answers t made whole: no policy is none, and only a royalty has a share.
func (s *Store) checkRemixTerms(t RemixTerms) (RemixTerms, error) {
	switch t.Policy {
	case "", RemixNone, RemixFree:
		if t.ShareBPS != 0 {
			return t, invalid("remix_share_bps is only for a royalty remix_policy")
		}
		if t.Policy == "" {
			t.Policy = RemixNone
		}
	case RemixRoyalty:
		if t.ShareBPS < 1 || t.ShareBPS > s.maxRemixShare() {
			return t, invalid("a royalty's remix_share_bps must be 1 to %d", s.maxRemixShare())
		}
	default:
		return t, invalid("remix_policy must be none, free or royalty")
	}
	return t, nil
}

// SetRemixTerms changes whether one of workspaceID's listings may be remixed and on what share. Remixes already
// declared keep the share they were given.
func (s *Store) SetRemixTerms(ctx context.Context, workspaceID, listingID string, t RemixTerms) (RemixTerms, error) {
	t, err := s.checkRemixTerms(t)
	if err != nil {
		return t, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE market_listings SET remix_policy = $3, remix_share_bps = $4, updated_at = now()
		WHERE id = $1 AND workspace_id = $2`, listingID, workspaceID, t.Policy, t.ShareBPS)
	if err != nil {
		return t, fmt.Errorf("market: remix terms: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return t, ErrNotFound
	}
	return t, nil
}

// declareParents records the parents of childID's version childVersion, published by workspaceID: those of the
// version before it, carried forward with the share they were given, and the ones refs declares. A ref naming a
// parent already carried forward moves it to that version and keeps its share. Someone else's listing is declared
// under the remix grant workspaceID holds for that version (B32.25, remix.go), at the share the grant locked; a room's
// contribution, by a live member of its room, is a room_fork at the room's remix share (B32.31).
func (s *Store) declareParents(ctx context.Context, tx pgx.Tx, workspaceID, childID string, childVersion int, refs []ParentRef) ([]Parent, error) {
	var carried []Parent
	if childVersion > 1 {
		var err error
		if carried, err = parentsOf(ctx, tx, childID, childVersion-1); err != nil {
			return nil, err
		}
	}
	if len(refs) == 0 && len(carried) == 0 {
		return nil, nil
	}
	// One family tree: two publishes declaring each other as parents must not both pass the cycle check.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('market_lineage', 0))`); err != nil {
		return nil, fmt.Errorf("market: lineage: %w", err)
	}
	at := map[string]int{}
	for i, p := range carried {
		at[p.ListingID] = i
	}
	declared := map[string]bool{}
	out := carried
	for _, ref := range refs {
		if ref.ListingID == "" {
			return nil, invalid("each parent needs its listing_id")
		}
		if declared[ref.ListingID] {
			return nil, invalid("parent %s is declared twice", ref.ListingID)
		}
		declared[ref.ListingID] = true
		if ref.ListingID == childID {
			return nil, fmt.Errorf("%w (%s)", ErrLineageCycle, ref.ListingID)
		}
		var owner, visibility, review, policy, roomID string
		var share, latest int
		// B17.206: a held version, and those after it, are not another workspace's to declare.
		err := tx.QueryRow(ctx, `SELECT workspace_id, visibility, review_status, remix_policy, remix_share_bps,
				CASE WHEN workspace_id <> $2 THEN coalesce(held_version - 1, latest_version) ELSE latest_version END, coalesce(room_id, '')
			FROM market_listings WHERE id = $1`, ref.ListingID, workspaceID).Scan(&owner, &visibility, &review, &policy, &share, &latest, &roomID)
		if err == nil {
			var h bool
			if h, err = hidden(ctx, tx, Listing{WorkspaceID: owner, Visibility: visibility, ReviewStatus: review, RoomID: roomID}, workspaceID); err == nil && h {
				err = pgx.ErrNoRows
			}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, invalid("parent %s: no such listing", ref.ListingID)
		}
		if err != nil {
			return nil, fmt.Errorf("market: lineage: %w", err)
		}
		var cycle bool
		if err := tx.QueryRow(ctx, `WITH RECURSIVE up(id) AS (
				SELECT parent_listing_id FROM market_lineage WHERE child_listing_id = $1
				UNION
				SELECT e.parent_listing_id FROM market_lineage e JOIN up ON e.child_listing_id = up.id)
			SELECT EXISTS (SELECT 1 FROM up WHERE id = $2)`, ref.ListingID, childID).Scan(&cycle); err != nil {
			return nil, fmt.Errorf("market: lineage: %w", err)
		}
		if cycle {
			return nil, fmt.Errorf("%w (%s descends from %s)", ErrLineageCycle, ref.ListingID, childID)
		}
		version := ref.Version
		var grant RemixGrant
		var fork bool
		if roomID != "" {
			var roomShare int
			if fork, roomShare, err = roomFork(ctx, tx, roomID, workspaceID); err != nil {
				return nil, err
			}
			if fork {
				share = roomShare
			}
		}
		if owner != workspaceID && !fork {
			// B32.25: someone else's listing is built on under the remix licence the publisher accepted for the
			// version declared (version 0: the newest it accepted), at the share locked then.
			if grant, err = remixGrant(ctx, tx, workspaceID, ref.ListingID, ref.Version); err != nil {
				if errors.Is(err, ErrNoRemixGrant) && policy == RemixNone {
					return nil, fmt.Errorf("%w (%s)", ErrNotRemixable, ref.ListingID)
				}
				return nil, err
			}
			version = grant.Version
		}
		if version == 0 {
			version = latest
		}
		if version < 1 || version > latest {
			return nil, invalid("parent %s has no version %d", ref.ListingID, ref.Version)
		}
		if i, ok := at[ref.ListingID]; ok {
			out[i].Version = version
			continue
		}
		source := LineageDeclared
		switch {
		case fork:
			source = LineageRoomFork
		case owner != workspaceID:
			share = grant.ShareBPS
		case policy != RemixRoyalty:
			share = 0
		}
		out = append(out, Parent{ListingID: ref.ListingID, Version: version, ShareBPS: share, Source: source})
	}
	for i := range out {
		if err := tx.QueryRow(ctx, `INSERT INTO market_lineage (id, child_listing_id, child_version, parent_listing_id, parent_version, share_bps, source)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`, "lin_"+uuid.NewString(), childID, childVersion,
			out[i].ListingID, out[i].Version, out[i].ShareBPS, out[i].Source).Scan(&out[i].CreatedAt); err != nil {
			return nil, fmt.Errorf("market: lineage: %w", err)
		}
	}
	return out, nil
}

// roomFork answers whether workspaceID is a live member of the room a contribution was made in, and the share of each
// sale of what it builds on the room's current terms give that contribution (B32.31). A room that is gone forks nothing.
func roomFork(ctx context.Context, tx pgx.Tx, roomID, workspaceID string) (member bool, shareBPS int, err error) {
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM room_members m WHERE m.room_id = r.id AND m.workspace_id = $2 AND m.removed_at IS NULL),
			t.remix_share_bps
		FROM rooms r JOIN room_terms t ON t.room_id = r.id AND t.version = r.terms_version WHERE r.id = $1`, roomID, workspaceID).Scan(&member, &shareBPS)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("market: room fork: %w", err)
	}
	return member, shareBPS, nil
}

// parentsOf reads the parents of one version of a listing, in the order they were declared.
func parentsOf(ctx context.Context, q querier, listingID string, version int) ([]Parent, error) {
	byVersion, err := versionParents(ctx, q, listingID, version)
	return byVersion[version], err
}

// versionParents reads the parents of a listing's versions, keyed by version: one version when version is set, every
// version when it is 0.
func versionParents(ctx context.Context, q querier, listingID string, version int) (map[int][]Parent, error) {
	rows, err := q.Query(ctx, `SELECT child_version, parent_listing_id, parent_version, share_bps, source, created_at FROM market_lineage
		WHERE child_listing_id = $1 AND ($2 = 0 OR child_version = $2) ORDER BY child_version, created_at, id`, listingID, version)
	if err != nil {
		return nil, fmt.Errorf("market: lineage: %w", err)
	}
	defer rows.Close()
	out := map[int][]Parent{}
	for rows.Next() {
		var v int
		var p Parent
		if err := rows.Scan(&v, &p.ListingID, &p.Version, &p.ShareBPS, &p.Source, &p.CreatedAt); err != nil {
			return nil, err
		}
		out[v] = append(out[v], p)
	}
	return out, rows.Err()
}

// Lineage is one listing version's family tree as a viewer reads it.
type Lineage struct {
	ListingID   string     `json:"listing_id"`
	Version     int        `json:"version"`
	RemixTerms             // whether it may itself be remixed
	Ancestors   []Ancestor `json:"ancestors"`   // nearest first, up to MaxDepth generations
	Descendants int        `json:"descendants"` // how many listings build on it, at any depth
	MaxDepth    int        `json:"max_depth"`
}

// Ancestor is one edge up the family tree: ListingID's Version is a parent of ChildListingID's ChildVersion and was
// given ShareBPS of its sales. A listing the viewer may not see is named by its id alone.
type Ancestor struct {
	ListingID      string `json:"listing_id"`
	Version        int    `json:"version"`
	Title          string `json:"title,omitempty"`
	Hidden         bool   `json:"hidden,omitempty"`
	ChildListingID string `json:"child_listing_id"`
	ChildVersion   int    `json:"child_version"`
	ShareBPS       int    `json:"share_bps"`
	Source         string `json:"source"`
	Depth          int    `json:"depth"` // 1 a parent, 2 a grandparent, …
}

// Lineage reads a listing version's ancestors, up to LENS_LINEAGE_MAX_DEPTH generations, and how many listings descend
// from it, as viewerWorkspace may see it. Version 0 is the latest.
func (s *Store) Lineage(ctx context.Context, viewerWorkspace, listingID string, version int) (Lineage, error) {
	l, err := visibleListing(ctx, s.pool, viewerWorkspace, listingID, "")
	if errors.Is(err, pgx.ErrNoRows) {
		return Lineage{}, ErrNotFound
	}
	if err != nil {
		return Lineage{}, fmt.Errorf("market: lineage: %w", err)
	}
	if version == 0 {
		version = l.LatestVersion
	}
	if version < 1 || version > l.LatestVersion {
		return Lineage{}, ErrNotFound
	}
	out := Lineage{ListingID: l.ID, Version: version, RemixTerms: RemixTerms{Policy: l.RemixPolicy, ShareBPS: l.RemixShareBPS},
		Ancestors: []Ancestor{}, MaxDepth: s.maxLineageDepth()}
	rows, err := s.pool.Query(ctx, `WITH RECURSIVE up AS (
			SELECT id, child_listing_id, child_version, parent_listing_id, parent_version, share_bps, source, created_at, 1 AS depth
			FROM market_lineage WHERE child_listing_id = $1 AND child_version = $2
			UNION ALL
			SELECT e.id, e.child_listing_id, e.child_version, e.parent_listing_id, e.parent_version, e.share_bps, e.source, e.created_at, up.depth + 1
			FROM market_lineage e JOIN up ON e.child_listing_id = up.parent_listing_id AND e.child_version = up.parent_version
			WHERE up.depth < $3)
		SELECT up.parent_listing_id, up.parent_version, l.title, l.workspace_id, l.visibility, l.review_status, coalesce(l.room_id, ''),
		       up.child_listing_id, up.child_version, up.share_bps, up.source, up.depth
		FROM up JOIN market_listings l ON l.id = up.parent_listing_id
		ORDER BY up.depth, up.created_at, up.id`, listingID, version, out.MaxDepth)
	if err != nil {
		return Lineage{}, fmt.Errorf("market: lineage: %w", err)
	}
	defer rows.Close()
	var seen []Listing // each ancestor as hidden reads it, asked once the rows are read
	for rows.Next() {
		var a Ancestor
		var l Listing
		if err := rows.Scan(&a.ListingID, &a.Version, &a.Title, &l.WorkspaceID, &l.Visibility, &l.ReviewStatus, &l.RoomID,
			&a.ChildListingID, &a.ChildVersion, &a.ShareBPS, &a.Source, &a.Depth); err != nil {
			return Lineage{}, err
		}
		out.Ancestors = append(out.Ancestors, a)
		seen = append(seen, l)
	}
	if err := rows.Err(); err != nil {
		return Lineage{}, err
	}
	rows.Close()
	for i := range out.Ancestors {
		if h, err := hidden(ctx, s.pool, seen[i], viewerWorkspace); err != nil {
			return Lineage{}, fmt.Errorf("market: lineage: %w", err)
		} else if h {
			out.Ancestors[i].Title, out.Ancestors[i].Hidden = "", true
		}
	}
	if err := s.pool.QueryRow(ctx, `WITH RECURSIVE down(id) AS (
			SELECT child_listing_id FROM market_lineage WHERE parent_listing_id = $1
			UNION
			SELECT e.child_listing_id FROM market_lineage e JOIN down ON e.parent_listing_id = down.id)
		SELECT count(*) FROM down`, listingID).Scan(&out.Descendants); err != nil {
		return Lineage{}, fmt.Errorf("market: lineage: %w", err)
	}
	return out, nil
}
