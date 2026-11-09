package market

// B32.50 — public collections: any workspace may publish a titled list of listings, and the operator marks Talyvor's own
// featured (market_collections, migration 0219).
//
// A collection lists only public listings the review approved, and reads them in the order its curator gave; a listing
// later taken down, held or made private drops out of the read without leaving the collection. A collection is its
// workspace's until it is made public; the public list puts the featured ones first, then the most recently changed.
// Only a public collection may be featured, and making a featured one private unfeatures it.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/operatoraudit"
)

// MaxCollectionListings is the most listings one collection holds.
const MaxCollectionListings = 100

// CollectionInput is what a curator writes.
type CollectionInput struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Public      bool     `json:"public"`
	ListingIDs  []string `json:"listing_ids"`
}

// Collection is one collection. Listings is present on a read of the collection itself, ListingCount on every read.
type Collection struct {
	ID           string     `json:"id"`
	WorkspaceID  string     `json:"workspace_id"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Public       bool       `json:"public"`
	Featured     bool       `json:"featured"`
	FeaturedAt   *time.Time `json:"featured_at,omitempty"`
	ListingCount int        `json:"listing_count"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	Listings     []Listing  `json:"listings,omitempty"`
}

// shownItems counts a collection's items anyone may see: public listings the review approved.
const shownItems = `(SELECT count(*) FROM market_collection_items i JOIN market_listings m ON m.id = i.listing_id
	WHERE i.collection_id = c.id AND m.visibility = 'public' AND m.review_status = 'approved')`

const collectionColumns = `c.id, c.workspace_id, c.title, c.description, c.public, c.featured, c.featured_at, ` + shownItems + `, c.created_at, c.updated_at`

func scanCollection(row pgx.Row) (Collection, error) {
	var c Collection
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.Title, &c.Description, &c.Public, &c.Featured, &c.FeaturedAt, &c.ListingCount, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (s *Store) collections(ctx context.Context, where string, args ...any) ([]Collection, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+collectionColumns+` FROM market_collections c WHERE `+where+`
		ORDER BY c.featured DESC, c.featured_at DESC NULLS LAST, c.updated_at DESC, c.id LIMIT 500`, args...)
	if err != nil {
		return nil, fmt.Errorf("market: collections: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Collection, error) { return scanCollection(row) })
	if err != nil {
		return nil, fmt.Errorf("market: collections: %w", err)
	}
	if out == nil {
		out = []Collection{}
	}
	return out, nil
}

// PublicCollections reads the public collections, the featured ones first.
func (s *Store) PublicCollections(ctx context.Context) ([]Collection, error) {
	return s.collections(ctx, `c.public`)
}

// OwnCollections reads workspaceID's collections, public or not.
func (s *Store) OwnCollections(ctx context.Context, workspaceID string) ([]Collection, error) {
	return s.collections(ctx, `c.workspace_id = $1`, workspaceID)
}

// GetCollection reads a collection with its listings in its order, as viewer may see it: a collection that is not
// public is its workspace's alone.
func (s *Store) GetCollection(ctx context.Context, viewer, id string) (Collection, error) {
	c, err := scanCollection(s.pool.QueryRow(ctx, `SELECT `+collectionColumns+` FROM market_collections c WHERE c.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !c.Public && c.WorkspaceID != viewer) {
		return Collection{}, fmt.Errorf("%w: no such collection", ErrNotFound)
	}
	if err != nil {
		return Collection{}, fmt.Errorf("market: collection: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+qualifiedListingColumns+` FROM market_collection_items i JOIN market_listings l ON l.id = i.listing_id
		WHERE i.collection_id = $1 AND l.visibility = 'public' AND l.review_status = 'approved' AND `+contentShown("$2", true)+` ORDER BY i.position`, id, viewer)
	if err != nil {
		return Collection{}, fmt.Errorf("market: collection: %w", err)
	}
	c.Listings, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Listing, error) {
		l, err := scanListing(row)
		l.asSeenBy("")
		return l, err
	})
	if err != nil {
		return Collection{}, fmt.Errorf("market: collection: %w", err)
	}
	ids := make([]string, len(c.Listings))
	for i, l := range c.Listings {
		ids[i] = l.ID
	}
	if err := s.withOffersAndCapabilities(ctx, ids, func(i int) *Listing { return &c.Listings[i] }); err != nil {
		return Collection{}, err
	}
	if c.Listings == nil {
		c.Listings = []Listing{}
	}
	return c, nil
}

// SaveCollection creates a collection of workspaceID's (id "") or replaces one of its own, and reads it back.
func (s *Store) SaveCollection(ctx context.Context, workspaceID, id string, in CollectionInput) (Collection, error) {
	in.Title, in.Description = strings.TrimSpace(in.Title), strings.TrimSpace(in.Description)
	if in.Title == "" || len([]rune(in.Title)) > 120 {
		return Collection{}, invalid("a collection needs a title of at most 120 characters")
	}
	if len(in.Description) > 2000 {
		return Collection{}, invalid("a collection's description is at most 2000 characters")
	}
	if len(in.ListingIDs) > MaxCollectionListings {
		return Collection{}, invalid("a collection holds at most %d listings", MaxCollectionListings)
	}
	seen := map[string]bool{}
	for _, l := range in.ListingIDs {
		if seen[l] {
			return Collection{}, invalid("listing %s is in the collection twice", l)
		}
		seen[l] = true
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Collection{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var shown []string
	rows, err := tx.Query(ctx, `SELECT id FROM market_listings WHERE id = ANY($1) AND visibility = 'public' AND review_status = 'approved'`, in.ListingIDs)
	if err != nil {
		return Collection{}, fmt.Errorf("market: collection: %w", err)
	}
	if shown, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return Collection{}, fmt.Errorf("market: collection: %w", err)
	}
	for _, l := range in.ListingIDs {
		if !slices.Contains(shown, l) {
			return Collection{}, invalid("listing %s is not a public listing anyone may find", l)
		}
	}
	if id == "" {
		id = "col_" + uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO market_collections (id, workspace_id, title, description, public) VALUES ($1, $2, $3, $4, $5)`,
			id, workspaceID, in.Title, in.Description, in.Public); err != nil {
			return Collection{}, fmt.Errorf("market: collection: %w", err)
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE market_collections SET title = $3, description = $4, public = $5, updated_at = now(),
				featured = featured AND $5, featured_at = CASE WHEN $5 THEN featured_at END, featured_by = CASE WHEN $5 THEN featured_by ELSE '' END
			WHERE id = $1 AND workspace_id = $2`, id, workspaceID, in.Title, in.Description, in.Public)
		if err != nil {
			return Collection{}, fmt.Errorf("market: collection: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return Collection{}, fmt.Errorf("%w: no such collection", ErrNotFound)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM market_collection_items WHERE collection_id = $1`, id); err != nil {
			return Collection{}, fmt.Errorf("market: collection: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO market_collection_items (collection_id, listing_id, position)
		SELECT $1, l, n FROM unnest($2::text[]) WITH ORDINALITY AS u(l, n)`, id, in.ListingIDs); err != nil {
		return Collection{}, fmt.Errorf("market: collection: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Collection{}, err
	}
	return s.GetCollection(ctx, workspaceID, id)
}

// DeleteCollection removes one of workspaceID's collections; its listings stay as they are.
func (s *Store) DeleteCollection(ctx context.Context, workspaceID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM market_collections WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("market: collection: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such collection", ErrNotFound)
	}
	return nil
}

// FeatureCollection is the operator marking a public collection featured, or no longer featured, recorded under actor
// in the operator audit trail.
func (s *Store) FeatureCollection(ctx context.Context, id, actor string, featured bool) (Collection, error) {
	if strings.TrimSpace(actor) == "" {
		return Collection{}, invalid("name the operator: the X-Talyvor-Operator header or actor in the body")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Collection{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var public bool
	err = tx.QueryRow(ctx, `SELECT public FROM market_collections WHERE id = $1 FOR UPDATE`, id).Scan(&public)
	if errors.Is(err, pgx.ErrNoRows) {
		return Collection{}, fmt.Errorf("%w: no such collection", ErrNotFound)
	}
	if err != nil {
		return Collection{}, fmt.Errorf("market: collection: %w", err)
	}
	if featured && !public {
		return Collection{}, invalid("only a public collection may be featured")
	}
	if _, err := tx.Exec(ctx, `UPDATE market_collections SET featured = $2, featured_by = CASE WHEN $2 THEN $3 ELSE '' END,
			featured_at = CASE WHEN $2 THEN $4::timestamptz END WHERE id = $1`, id, featured, actor, s.now()); err != nil {
		return Collection{}, fmt.Errorf("market: collection: %w", err)
	}
	action := "market.collection.feature"
	if !featured {
		action = "market.collection.unfeature"
	}
	if _, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: actor, Action: action, Target: id}); err != nil {
		return Collection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Collection{}, err
	}
	return scanCollection(s.pool.QueryRow(ctx, `SELECT `+collectionColumns+` FROM market_collections c WHERE c.id = $1`, id))
}
