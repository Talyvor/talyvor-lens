package market

// B41.185 — content labels and age rating (Apple guideline 4.7): every listing carries an age rating and the content
// labels its seller declares. A publish without a rating is rated 18+ — the strictest — so no listing is ever
// unlabelled, and none reaches a family filter until its seller rates it lower. A workspace's content filter keeps
// listings rated above its max_age_rating, or carrying a label it blocks, out of its catalog, search, collections and
// listing reads; a listing it blocks stays out of its catalog, search and collections. A workspace always sees its own.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// AgeRatings are the ratings a listing may carry, mildest first. A family filter is max_age_rating "all".
var AgeRatings = []string{"all", "13+", "16+", "18+"}

// ContentLabels are the labels a seller declares of what its listing gives.
var ContentLabels = []string{"adult", "violence", "medical_advice", "financial_advice", "legal_advice"}

// DefaultAgeRating is the rating of a listing published without one.
const DefaultAgeRating = "18+"

// checkRating refuses a rating that is not one of AgeRatings.
func checkRating(rating string) error {
	if !slices.Contains(AgeRatings, rating) {
		return invalid("age_rating must be all, 13+, 16+ or 18+")
	}
	return nil
}

// checkLabels answers labels sorted, once each, refusing one that is not a ContentLabel.
func checkLabels(labels []string) ([]string, error) {
	out := []string{}
	for _, l := range labels {
		if !slices.Contains(ContentLabels, l) {
			return nil, invalid("content label %q is not one of adult, violence, medical_advice, financial_advice, legal_advice", l)
		}
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out, nil
}

// checkContent validates a listing's rating ("" is DefaultAgeRating) and labels: one labelled adult is rated 18+.
func checkContent(rating string, labels []string) (string, []string, error) {
	if rating == "" {
		rating = DefaultAgeRating
	}
	if err := checkRating(rating); err != nil {
		return "", nil, err
	}
	labels, err := checkLabels(labels)
	if err != nil {
		return "", nil, err
	}
	if slices.Contains(labels, "adult") && rating != "18+" {
		return "", nil, invalid("a listing labelled adult is rated 18+")
	}
	return rating, labels, nil
}

// ratingRank is the SQL position of a rating column in AgeRatings, mildest 1.
func ratingRank(col string) string {
	return `array_position(ARRAY['all', '13+', '16+', '18+'], ` + col + `)`
}

// contentShown is the SQL condition that the listing aliased l passes the content filter of the workspace in viewerArg,
// and with blocks that it did not block it.
func contentShown(viewerArg string, blocks bool) string {
	cond := `NOT EXISTS (SELECT 1 FROM market_content_filters cf WHERE cf.workspace_id = ` + viewerArg + `
		AND (` + ratingRank("l.age_rating") + ` > ` + ratingRank("cf.max_age_rating") + ` OR l.content_labels && cf.blocked_labels))`
	if blocks {
		cond += ` AND NOT EXISTS (SELECT 1 FROM market_listing_blocks b WHERE b.workspace_id = ` + viewerArg + ` AND b.listing_id = l.id)`
	}
	return `(l.workspace_id = ` + viewerArg + ` OR (` + cond + `))`
}

// ContentFilter is what a workspace is shown of the marketplace, and the listings it blocked.
type ContentFilter struct {
	MaxAgeRating    string    `json:"max_age_rating"`
	BlockedLabels   []string  `json:"blocked_labels"`
	BlockedListings []string  `json:"blocked_listings"` // newest first
	AgeRatings      []string  `json:"age_ratings"`      // what max_age_rating may be, mildest first
	ContentLabels   []string  `json:"content_labels"`   // what blocked_labels may hold
	UpdatedAt       time.Time `json:"updated_at,omitzero"`
}

// ContentFilter reads workspaceID's content filter: 18+ with no label blocked until it sets one.
func (s *Store) ContentFilter(ctx context.Context, workspaceID string) (ContentFilter, error) {
	f := ContentFilter{MaxAgeRating: DefaultAgeRating, BlockedLabels: []string{}, AgeRatings: AgeRatings, ContentLabels: ContentLabels}
	err := s.pool.QueryRow(ctx, `SELECT max_age_rating, blocked_labels, updated_at FROM market_content_filters WHERE workspace_id = $1`,
		workspaceID).Scan(&f.MaxAgeRating, &f.BlockedLabels, &f.UpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return f, fmt.Errorf("market: content filter: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT listing_id FROM market_listing_blocks WHERE workspace_id = $1 ORDER BY created_at DESC, listing_id`, workspaceID)
	if err != nil {
		return f, fmt.Errorf("market: blocked listings: %w", err)
	}
	f.BlockedListings, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if f.BlockedListings == nil {
		f.BlockedListings = []string{}
	}
	return f, err
}

// SetContentFilter replaces workspaceID's content filter.
func (s *Store) SetContentFilter(ctx context.Context, workspaceID, maxAgeRating string, blockedLabels []string) (ContentFilter, error) {
	if err := checkRating(maxAgeRating); err != nil {
		return ContentFilter{}, err
	}
	labels, err := checkLabels(blockedLabels)
	if err != nil {
		return ContentFilter{}, err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO market_content_filters (workspace_id, max_age_rating, blocked_labels) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET max_age_rating = EXCLUDED.max_age_rating, blocked_labels = EXCLUDED.blocked_labels, updated_at = now()`,
		workspaceID, maxAgeRating, labels); err != nil {
		return ContentFilter{}, fmt.Errorf("market: content filter: %w", err)
	}
	return s.ContentFilter(ctx, workspaceID)
}

// BlockListing keeps a listing workspaceID can see out of its catalog, search and collections; blocking it again is no change.
func (s *Store) BlockListing(ctx context.Context, workspaceID, listingID string) error {
	if _, err := visibleListing(ctx, s.pool, workspaceID, listingID, ""); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("market: block: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO market_listing_blocks (workspace_id, listing_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		workspaceID, listingID); err != nil {
		return fmt.Errorf("market: block: %w", err)
	}
	return nil
}

// UnblockListing shows a blocked listing to workspaceID again; one it never blocked is no change.
func (s *Store) UnblockListing(ctx context.Context, workspaceID, listingID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM market_listing_blocks WHERE workspace_id = $1 AND listing_id = $2`, workspaceID, listingID); err != nil {
		return fmt.Errorf("market: unblock: %w", err)
	}
	return nil
}

// SetContentLabels replaces the age rating and content labels of one of workspaceID's listings.
func (s *Store) SetContentLabels(ctx context.Context, workspaceID, listingID, rating string, labels []string) (Listing, error) {
	if err := checkRating(rating); err != nil {
		return Listing{}, err
	}
	rating, labels, err := checkContent(rating, labels)
	if err != nil {
		return Listing{}, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE market_listings SET age_rating = $3, content_labels = $4, updated_at = now() WHERE id = $1 AND workspace_id = $2`,
		listingID, workspaceID, rating, labels)
	if err != nil {
		return Listing{}, fmt.Errorf("market: content labels: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Listing{}, ErrNotFound
	}
	return s.Get(ctx, workspaceID, listingID)
}
