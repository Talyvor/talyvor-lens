package market

// B32.49 — the trust panel: verified publisher, reviews, eval score, claims and lineage in one read.
//
// Trust answers what a buyer weighs before paying for a listing:
//
//   - the publisher is verified when its payouts are enabled on market_sellers and no IP claim against any of its
//     listings was upheld in the last 12 months (B30.4's verification level joins this once it has shipped);
//   - reviews count only buyers who paid for a use or a licence of the listing (a billed use that ran and was not
//     refunded) and are not linked to its seller (Store.linked) — asked at every read, so a buyer later found linked
//     drops out of the count and the average;
//   - the eval score of the version a buyer would run, its latest (null until B28.174 and B28.175 record runs);
//   - the claims against the listing, open and decided;
//   - the originals it credits and how many remixes build on it, exactly as the lineage read gives them;
//   - B41.173: its performance over the last 30 days of uses — success rate, median latency and cost per use.
//
// A buyer that qualifies writes one review of a listing, a rating of 1 to 5 and its text, and may rewrite it; the
// seller may reply to each (market_reviews, migration 0218).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotReviewer: the workspace has not paid for a use or a licence of the listing, or is linked to its seller.
var ErrNotReviewer = errors.New("market: only a buyer who paid for a use or a licence of this listing, and is not linked to its seller, may review it")

// MaxReviewText is the longest a review's text or the seller's reply may be.
const MaxReviewText = 4000

// recentReviews is how many of the reviews that count the trust read returns, newest first.
const recentReviews = 20

// paidSQL asks whether the workspace the expression buyer names paid for a use or a licence of the listing the
// expression listing names: a billed use of it that ran and was not refunded.
func paidSQL(listing, buyer string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM market_uses u WHERE u.listing_id = %[1]s AND u.buyer_workspace_id = %[2]s
		AND u.charge = 'billed' AND u.ran_at IS NOT NULL AND NOT EXISTS (SELECT 1 FROM market_refunds f WHERE f.use_id = u.id))`, listing, buyer)
}

// ReviewInput is what a buyer writes: a rating of 1 to 5 and its text.
type ReviewInput struct {
	Rating int    `json:"rating"`
	Text   string `json:"text"`
}

// Review is one market_reviews row as anyone who may see the listing reads it; its writer is not named.
type Review struct {
	ID        string     `json:"id"`
	ListingID string     `json:"listing_id"`
	Rating    int        `json:"rating"`
	Text      string     `json:"text"`
	Reply     string     `json:"reply,omitempty"`
	RepliedAt *time.Time `json:"replied_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

const reviewColumns = `r.id, r.listing_id, r.rating, r.body, r.reply, r.replied_at, r.created_at, r.updated_at`

func scanReview(row pgx.Row) (Review, error) {
	var r Review
	err := row.Scan(&r.ID, &r.ListingID, &r.Rating, &r.Text, &r.Reply, &r.RepliedAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// Review writes buyer's review of a listing it may see, or rewrites the one it wrote: only a buyer that paid for a use
// or a licence of it and is not linked to its seller may.
func (s *Store) Review(ctx context.Context, buyer, listingID string, in ReviewInput) (Review, error) {
	in.Text = strings.TrimSpace(in.Text)
	switch {
	case in.Rating < 1 || in.Rating > 5:
		return Review{}, invalid("rating must be 1 to 5")
	case len(in.Text) > MaxReviewText:
		return Review{}, invalid("text must be at most %d characters", MaxReviewText)
	}
	l, err := visibleListing(ctx, s.pool, buyer, listingID, "")
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, ErrNotFound
	}
	if err != nil {
		return Review{}, fmt.Errorf("market: review: %w", err)
	}
	if l.WorkspaceID == buyer {
		return Review{}, ErrNotReviewer
	}
	var may bool
	if err := s.pool.QueryRow(ctx, `SELECT `+paidSQL("$1", "$2")+` AND NOT `+linkedSQL("$3", "$2"), listingID, buyer, l.WorkspaceID).Scan(&may); err != nil {
		return Review{}, fmt.Errorf("market: review: %w", err)
	}
	if !may {
		return Review{}, ErrNotReviewer
	}
	r, err := scanReview(s.pool.QueryRow(ctx, `INSERT INTO market_reviews AS r (id, listing_id, buyer_workspace_id, rating, body)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (listing_id, buyer_workspace_id) DO UPDATE SET rating = EXCLUDED.rating, body = EXCLUDED.body, updated_at = now()
		RETURNING `+reviewColumns, "mrv_"+uuid.NewString(), listingID, buyer, in.Rating, in.Text))
	if err != nil {
		return Review{}, fmt.Errorf("market: review: %w", err)
	}
	return r, nil
}

// ReplyToReview sets seller's reply to a review of one of its listings; an empty reply removes it.
func (s *Store) ReplyToReview(ctx context.Context, seller, listingID, reviewID, reply string) (Review, error) {
	reply = strings.TrimSpace(reply)
	if len(reply) > MaxReviewText {
		return Review{}, invalid("reply must be at most %d characters", MaxReviewText)
	}
	r, err := scanReview(s.pool.QueryRow(ctx, `UPDATE market_reviews r SET reply = $4, replied_at = CASE WHEN $4 = '' THEN NULL ELSE now() END
		FROM market_listings l WHERE r.id = $3 AND r.listing_id = $2 AND l.id = r.listing_id AND l.workspace_id = $1
		RETURNING `+reviewColumns, seller, listingID, reviewID, reply))
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, ErrNotFound
	}
	if err != nil {
		return Review{}, fmt.Errorf("market: reply: %w", err)
	}
	return r, nil
}

// Trust is a listing's trust panel.
type Trust struct {
	ListingID   string       `json:"listing_id"`
	Version     int          `json:"version"` // the version a buyer would run: the latest
	Publisher   Publisher    `json:"publisher"`
	Reviews     ReviewCounts `json:"reviews"`
	Eval        *EvalScore   `json:"eval"` // null: no stored eval run of that version
	Claims      ClaimCounts  `json:"claims"`
	Originals   []Ancestor   `json:"originals"`   // the listings it builds on, as the lineage read gives them
	Remixes     int          `json:"remixes"`     // how many listings build on it, at any depth
	Performance Performance  `json:"performance"` // B41.173: the last 30 days of its uses
}

// performanceDays is the window a listing's performance is measured over.
const performanceDays = 30

// Performance is how a listing's runs went over the last 30 days (market_use_outcomes): every use that ran, answered or
// failed, but its seller's own and those of buyers linked to it, so a seller cannot pad its own record.
type Performance struct {
	Days                int      `json:"days"`
	Uses                int      `json:"uses"`
	Failed              int      `json:"failed"`
	SuccessRate         *float64 `json:"success_rate"`            // answered ÷ uses, 0 to 1 to four places; null with no uses
	MedianLatencyMS     *int64   `json:"median_latency_ms"`       // of the uses that answered; null with none
	CostPerUseUSDMicros *int64   `json:"cost_per_use_usd_micros"` // the mean price of its billed uses; null with none
}

// Publisher is whether the listing's seller is verified, and why not.
type Publisher struct {
	WorkspaceID          string   `json:"workspace_id"`
	Verified             bool     `json:"verified"`
	PayoutsEnabled       bool     `json:"payouts_enabled"`
	UpheldClaims12Months int      `json:"upheld_claims_12_months"` // claims upheld against any of its listings
	NotVerifiedBecause   []string `json:"not_verified_because,omitempty"`
}

// ReviewCounts are the reviews that count: from buyers who paid and are not linked to the seller.
type ReviewCounts struct {
	Count   int      `json:"count"`
	Average float64  `json:"average"` // to two places; 0 with none
	Stars   [5]int   `json:"stars"`   // how many gave 1, 2, 3, 4 and 5
	Recent  []Review `json:"recent"`  // the newest, at most 20
}

// EvalScore is a stored eval run of a listing version: how many of its cases passed.
type EvalScore struct {
	Version int       `json:"version"`
	Passed  int       `json:"passed"`
	Cases   int       `json:"cases"`
	RanAt   time.Time `json:"ran_at"`
}

// ClaimCounts are the IP claims against the listing by state: open (open or countered) and each decision.
type ClaimCounts struct {
	Open       int `json:"open"`
	Upheld     int `json:"upheld"`
	Attributed int `json:"attributed"`
	Rejected   int `json:"rejected"`
}

// Trust reads a listing's trust panel as viewer may see the listing.
func (s *Store) Trust(ctx context.Context, viewer, listingID string) (Trust, error) {
	l, err := visibleListing(ctx, s.pool, viewer, listingID, "")
	if errors.Is(err, pgx.ErrNoRows) {
		return Trust{}, ErrNotFound
	}
	if err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}
	out := Trust{ListingID: l.ID, Version: l.LatestVersion, Publisher: Publisher{WorkspaceID: l.WorkspaceID}}

	p := &out.Publisher
	if err := s.pool.QueryRow(ctx, `SELECT
			coalesce((SELECT payouts_enabled FROM market_sellers WHERE workspace_id = $1), false),
			(SELECT count(*) FROM market_ip_claims c JOIN market_listings m ON m.id = c.listing_id
			  WHERE m.workspace_id = $1 AND c.status = 'upheld' AND c.decided_at > $2::timestamptz - interval '12 months')`,
		l.WorkspaceID, s.now()).Scan(&p.PayoutsEnabled, &p.UpheldClaims12Months); err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}
	if !p.PayoutsEnabled {
		p.NotVerifiedBecause = append(p.NotVerifiedBecause, "payouts are not enabled")
	}
	if p.UpheldClaims12Months > 0 {
		p.NotVerifiedBecause = append(p.NotVerifiedBecause, "an IP claim against its listings was upheld in the last 12 months")
	}
	p.Verified = len(p.NotVerifiedBecause) == 0

	counts := `FROM market_reviews r WHERE r.listing_id = $1 AND ` + paidSQL("r.listing_id", "r.buyer_workspace_id") +
		` AND NOT ` + linkedSQL("$2", "r.buyer_workspace_id")
	rows, err := s.pool.Query(ctx, `SELECT r.rating, count(*) `+counts+` GROUP BY r.rating`, l.ID, l.WorkspaceID)
	if err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}
	sum := 0
	for rows.Next() {
		var rating, n int
		if err := rows.Scan(&rating, &n); err != nil {
			rows.Close()
			return Trust{}, err
		}
		out.Reviews.Stars[rating-1] = n
		out.Reviews.Count += n
		sum += rating * n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}
	if out.Reviews.Count > 0 {
		out.Reviews.Average = math.Round(float64(sum)/float64(out.Reviews.Count)*100) / 100
	}
	rows, err = s.pool.Query(ctx, `SELECT `+reviewColumns+` `+counts+` ORDER BY r.updated_at DESC, r.id LIMIT $3`, l.ID, l.WorkspaceID, recentReviews)
	if err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}
	if out.Reviews.Recent, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Review, error) { return scanReview(row) }); err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}
	if out.Reviews.Recent == nil {
		out.Reviews.Recent = []Review{}
	}

	rows, err = s.pool.Query(ctx, `SELECT status, count(*) FROM market_ip_claims WHERE listing_id = $1 GROUP BY status`, l.ID)
	if err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return Trust{}, err
		}
		switch status {
		case ClaimOpen, ClaimCountered:
			out.Claims.Open += n
		case ClaimUpheld:
			out.Claims.Upheld = n
		case ClaimAttributed:
			out.Claims.Attributed = n
		case ClaimRejected:
			out.Claims.Rejected = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}

	pf := &out.Performance
	pf.Days = performanceDays
	var median *float64
	var cost *float64
	if err := s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE NOT ok),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE ok),
			avg(price_ulxc) FILTER (WHERE ok AND charge = 'billed')
		FROM market_use_outcomes WHERE listing_id = $1 AND ran_at > $2::timestamptz - make_interval(days => $3)
		  AND charge NOT IN ('own', 'linked')`,
		l.ID, s.now(), performanceDays).Scan(&pf.Uses, &pf.Failed, &median, &cost); err != nil {
		return Trust{}, fmt.Errorf("market: trust: %w", err)
	}
	if pf.Uses > 0 {
		rate := math.Round(float64(pf.Uses-pf.Failed)/float64(pf.Uses)*10000) / 10000
		pf.SuccessRate = &rate
	}
	if median != nil {
		ms := int64(math.Round(*median))
		pf.MedianLatencyMS = &ms
	}
	if cost != nil {
		usd := int64(math.Round(*cost)) / ulxcPerUSDMicro
		pf.CostPerUseUSDMicros = &usd
	}

	lineage, err := s.Lineage(ctx, viewer, l.ID, l.LatestVersion)
	if err != nil {
		return Trust{}, err
	}
	out.Originals, out.Remixes = lineage.Ancestors, lineage.Descendants
	return out, nil
}
