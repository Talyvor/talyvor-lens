package market

// B32.50 — discovery: trending by distinct buyers, search by capability and price, public collections.
//
// A listing declares what it can do from a controlled list kept as data (market_capabilities, migration 0219), at
// publish or with PUT …/listings/{id}/capabilities. Discover searches the public listings the review approved:
//
//   - q is Postgres full-text search on the title and description. A few words must all match; a sentence (sentenceWords
//     words or more) matches any of its words and is re-ranked by how near its embedding (internal/embedder, the one
//     the similarity check fingerprints versions with) is to each listing's latest version;
//   - capability, kind and licence narrow it; max_price_per_use keeps a listing that is free or whose per_use commercial
//     offer — the price one use is billed at — is at or under it; verified_only keeps a verified publisher's (trust.go);
//     min_eval keeps a listing whose latest version's stored eval score is at or above it;
//   - sort is relevance (the default with q), trending (the default without), new or price, and the result pages past
//     any cap, MaxSearchResults a page.
//
// RefreshDiscoveryStats is the nightly job: it rewrites the last eight days of market_listing_stats and every
// listing's trending score from market_uses. A use by the seller itself, or by a workspace linked to it (Store.linked's
// question, asked when the job runs), never counts, so a seller cannot move its own rank.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// MaxListingCapabilities is the most capabilities one listing may declare.
const MaxListingCapabilities = 8

// sentenceWords is how many words make q a sentence: matched by any word and re-ranked by meaning.
const sentenceWords = 4

// TrendingDays is the window a trending score counts buyers in; statsDays the days each nightly run rewrites.
const (
	TrendingDays = 7
	statsDays    = TrendingDays + 1
)

// trendingDecay is how much a buyer weighs for each day since its latest use: today's counts 1, yesterday's 0.8, and
// so on to 0.8⁶ for the seventh day.
const trendingDecay = 0.8

// Sort orders of a search.
const (
	SortRelevance = "relevance"
	SortTrending  = "trending"
	SortNew       = "new"
	SortPrice     = "price"
)

// Capability is one entry of the controlled list.
type Capability struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
}

// Capabilities reads the controlled list, in its order.
func (s *Store) Capabilities(ctx context.Context) ([]Capability, error) {
	rows, err := s.pool.Query(ctx, `SELECT slug, label FROM market_capabilities ORDER BY position, slug`)
	if err != nil {
		return nil, fmt.Errorf("market: capabilities: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Capability, error) {
		var c Capability
		return c, row.Scan(&c.Slug, &c.Label)
	})
}

// checkCapabilities answers caps without repeats, in the list's order, each one on the controlled list.
func checkCapabilities(ctx context.Context, q querier, caps []string) ([]string, error) {
	if len(caps) == 0 {
		return []string{}, nil
	}
	rows, err := q.Query(ctx, `SELECT slug FROM market_capabilities ORDER BY position, slug`)
	if err != nil {
		return nil, fmt.Errorf("market: capabilities: %w", err)
	}
	known, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("market: capabilities: %w", err)
	}
	for _, c := range caps {
		if !slices.Contains(known, c) {
			return nil, invalid("%q is not a capability; the capabilities are %s", c, strings.Join(known, ", "))
		}
	}
	out := []string{}
	for _, c := range known {
		if slices.Contains(caps, c) {
			out = append(out, c)
		}
	}
	if len(out) > MaxListingCapabilities {
		return nil, invalid("a listing declares at most %d capabilities", MaxListingCapabilities)
	}
	return out, nil
}

// writeCapabilities makes caps (checked) the capabilities of listingID, in tx.
func writeCapabilities(ctx context.Context, tx pgx.Tx, listingID string, caps []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM listing_capabilities WHERE listing_id = $1`, listingID); err != nil {
		return fmt.Errorf("market: capabilities: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO listing_capabilities (listing_id, capability) SELECT $1, unnest($2::text[])`, listingID, caps); err != nil {
		return fmt.Errorf("market: capabilities: %w", err)
	}
	return nil
}

// listingCapabilities reads the capabilities of each of listingIDs, in the list's order.
func listingCapabilities(ctx context.Context, q querier, listingIDs ...string) (map[string][]string, error) {
	rows, err := q.Query(ctx, `SELECT c.listing_id, c.capability FROM listing_capabilities c JOIN market_capabilities m ON m.slug = c.capability
		WHERE c.listing_id = ANY($1) ORDER BY c.listing_id, m.position, m.slug`, listingIDs)
	if err != nil {
		return nil, fmt.Errorf("market: capabilities: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var listing, c string
		if err := rows.Scan(&listing, &c); err != nil {
			return nil, err
		}
		out[listing] = append(out[listing], c)
	}
	return out, rows.Err()
}

// orNoCapabilities answers a listing's capabilities as a list, empty when it declares none.
func orNoCapabilities(caps []string) []string {
	if caps == nil {
		return []string{}
	}
	return caps
}

// SetCapabilities replaces the capabilities of one of workspaceID's listings and answers them.
func (s *Store) SetCapabilities(ctx context.Context, workspaceID, listingID string, caps []string) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	err = tx.QueryRow(ctx, `SELECT review_status FROM market_listings WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, listingID, workspaceID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("market: capabilities: %w", err)
	}
	if status == ReviewTakenDown {
		return nil, ErrTakenDown
	}
	out, err := checkCapabilities(ctx, tx, caps)
	if err != nil {
		return nil, err
	}
	if err := writeCapabilities(ctx, tx, listingID, out); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE market_listings SET updated_at = now() WHERE id = $1`, listingID); err != nil {
		return nil, fmt.Errorf("market: capabilities: %w", err)
	}
	return out, tx.Commit(ctx)
}

// DiscoverQuery is a search of the catalog. Every field set narrows or orders it.
type DiscoverQuery struct {
	Q              string
	Capability     string
	Kind           string
	Licence        string
	MaxPricePerUse *int64 // µUSD: free, or a per_use commercial offer at or under it
	MinEval        int    // 0 to 100: the share of its latest version's eval cases that passed, in percent
	VerifiedOnly   bool
	Sort           string // relevance | trending | new | price; "" is relevance with Q, trending without
	Page           int    // from 1; 0 is 1
	Viewer         string // the workspace searching: its content filter and blocks apply (B41.185); "" none
}

// DiscoverHit is one listing a search found, with what it was ranked by.
type DiscoverHit struct {
	Listing
	PricePerUseUSDMicros *int64  `json:"price_per_use_usd_micros"` // what one use is billed; 0 free; null: not sold per use
	DistinctBuyers7d     int     `json:"distinct_buyers_7d"`
	TrendingScore        float64 `json:"trending_score"`
}

// DiscoverPage is one page of a search.
type DiscoverPage struct {
	Listings []DiscoverHit `json:"listings"`
	Sort     string        `json:"sort"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Total    int           `json:"total"`
	HasMore  bool          `json:"has_more"`
}

// qualifiedListingColumns are listingColumns read through the alias l.
var qualifiedListingColumns = "l." + strings.ReplaceAll(listingColumns, ", ", ", l.")

// searchWords are the words of q, without the characters full-text search would read as operators.
func searchWords(q string) []string {
	return strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// Discover answers one page of the public listings the review approved that q matches.
func (s *Store) Discover(ctx context.Context, q DiscoverQuery) (DiscoverPage, error) {
	q.Q = strings.TrimSpace(q.Q)
	if q.Sort == "" {
		q.Sort = SortTrending
		if q.Q != "" {
			q.Sort = SortRelevance
		}
	}
	switch q.Sort {
	case SortRelevance:
		if q.Q == "" {
			return DiscoverPage{}, invalid("sort relevance needs q, the words to search for")
		}
	case SortTrending, SortNew, SortPrice:
	default:
		return DiscoverPage{}, invalid("sort must be relevance, trending, new or price")
	}
	if q.Page == 0 {
		q.Page = 1
	}
	if q.Page < 1 || q.Page > 10_000 {
		return DiscoverPage{}, invalid("page must be a whole number from 1 to 10000")
	}
	if q.MinEval < 0 || q.MinEval > 100 {
		return DiscoverPage{}, invalid("min_eval must be a whole number from 0 to 100")
	}
	if q.MaxPricePerUse != nil && *q.MaxPricePerUse < 0 {
		return DiscoverPage{}, invalid("max_price_per_use cannot be negative")
	}
	if _, ok := requiredField[q.Kind]; q.Kind != "" && !ok {
		return DiscoverPage{}, invalid("kind must be agent, prompt, skill, evaluation or pipeline")
	}
	if _, ok := LicenceTerms[q.Licence]; q.Licence != "" && !ok {
		return DiscoverPage{}, invalid("licence must be personal, commercial or enterprise")
	}
	if q.Capability != "" {
		if _, err := checkCapabilities(ctx, s.pool, []string{q.Capability}); err != nil {
			return DiscoverPage{}, err
		}
	}

	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where := []string{`l.visibility = 'public'`, `l.review_status = 'approved'`}
	joins := `LEFT JOIN market_listing_trending t ON t.listing_id = l.id`
	rank, near := `0::float8`, `NULL::float8`
	if q.Q != "" {
		words := searchWords(q.Q)
		text := strings.Join(words, " ")
		if len(words) >= sentenceWords {
			text = strings.Join(words, " or ")
		}
		query := `websearch_to_tsquery('english', ` + arg(text) + `)`
		doc := `to_tsvector('english', l.title || ' ' || l.description)`
		where = append(where, doc+` @@ `+query)
		rank = `ts_rank(` + doc + `, ` + query + `)::float8`
		if len(words) >= sentenceWords {
			if model, v := s.embedQuery(ctx, q.Q); v != nil {
				joins += ` LEFT JOIN market_listing_fingerprints f ON f.listing_id = l.id AND f.version = coalesce(l.held_version - 1, l.latest_version)`
				near = `CASE WHEN f.embedding_model = ` + arg(model) + ` AND f.embedding IS NOT NULL THEN 1 - (f.embedding <=> ` + arg(vectorText(v)) + `::vector) END`
			}
		}
	}
	if q.Viewer != "" {
		where = append(where, contentShown(arg(q.Viewer), true))
	}
	if q.Kind != "" {
		where = append(where, `l.kind = `+arg(q.Kind))
	}
	if q.Capability != "" {
		where = append(where, `EXISTS (SELECT 1 FROM listing_capabilities c WHERE c.listing_id = l.id AND c.capability = `+arg(q.Capability)+`)`)
	}
	if q.Licence != "" {
		where = append(where, `EXISTS (SELECT 1 FROM market_offers o WHERE o.listing_id = l.id AND o.active AND o.licence = `+arg(q.Licence)+`)`)
	}
	if q.VerifiedOnly {
		// trust.go's verified publisher: payouts enabled, and no IP claim against its listings upheld in 12 months.
		where = append(where, `EXISTS (SELECT 1 FROM market_sellers ms WHERE ms.workspace_id = l.workspace_id AND ms.payouts_enabled)`,
			`NOT EXISTS (SELECT 1 FROM market_ip_claims c JOIN market_listings m ON m.id = c.listing_id
				WHERE m.workspace_id = l.workspace_id AND c.status = 'upheld' AND c.decided_at > `+arg(s.now())+`::timestamptz - interval '12 months')`)
	}
	if q.MinEval > 0 {
		// The trust panel's eval score is null for every listing until B28.174 and B28.175 record eval runs: none is at
		// or above a positive minimum yet.
		where = append(where, `false`)
	}
	// price: what one use is billed (0 free, NULL when it is sold but not per use).
	price := `CASE WHEN NOT EXISTS (SELECT 1 FROM market_offers o WHERE o.listing_id = l.id AND o.active) THEN 0
		ELSE (SELECT o.price_usd_micros FROM market_offers o WHERE o.listing_id = l.id AND o.active AND o.kind = 'per_use' AND o.licence = 'commercial') END`
	cand := `SELECT ` + qualifiedListingColumns + `, ` + price + ` AS price, coalesce(t.score, 0) AS score, coalesce(t.distinct_buyers_7d, 0) AS buyers,
			` + rank + ` AS rank, ` + near + ` AS near
		FROM market_listings l ` + joins + ` WHERE ` + strings.Join(where, " AND ")
	outer := ``
	if q.MaxPricePerUse != nil {
		outer = ` WHERE price <= ` + arg(*q.MaxPricePerUse)
	}
	order := map[string]string{
		SortRelevance: `near DESC NULLS LAST, rank DESC, score DESC, created_at DESC, id`,
		SortTrending:  `score DESC, created_at DESC, id`,
		SortNew:       `created_at DESC, id`,
		SortPrice:     `price ASC NULLS LAST, score DESC, created_at DESC, id`,
	}[q.Sort]
	limit, offset := arg(MaxSearchResults), arg((q.Page-1)*MaxSearchResults)
	rows, err := s.pool.Query(ctx, `WITH cand AS (`+cand+`)
		SELECT `+listingColumns+`, price, score, buyers, count(*) OVER () FROM cand`+outer+`
		ORDER BY `+order+` LIMIT `+limit+` OFFSET `+offset, args...)
	if err != nil {
		return DiscoverPage{}, fmt.Errorf("market: search: %w", err)
	}
	defer rows.Close()
	out := DiscoverPage{Listings: []DiscoverHit{}, Sort: q.Sort, Page: q.Page, PageSize: MaxSearchResults}
	ids := []string{}
	for rows.Next() {
		var h DiscoverHit
		if h.Listing, err = scanListing(rows, &h.PricePerUseUSDMicros, &h.TrendingScore, &h.DistinctBuyers7d, &out.Total); err != nil {
			return DiscoverPage{}, err
		}
		h.asSeenBy("")
		out.Listings = append(out.Listings, h)
		ids = append(ids, h.ID)
	}
	if err := rows.Err(); err != nil {
		return DiscoverPage{}, fmt.Errorf("market: search: %w", err)
	}
	rows.Close()
	if err := s.withOffersAndCapabilities(ctx, ids, func(i int) *Listing { return &out.Listings[i].Listing }); err != nil {
		return DiscoverPage{}, err
	}
	if out.Total == 0 && q.Page > 1 {
		// A page past the last one still counts what there is.
		if err := s.pool.QueryRow(ctx, `WITH cand AS (`+cand+`) SELECT count(*) FROM cand`+outer, args[:len(args)-2]...).Scan(&out.Total); err != nil {
			return DiscoverPage{}, fmt.Errorf("market: search: %w", err)
		}
	}
	out.HasMore = q.Page*MaxSearchResults < out.Total
	return out, nil
}

// withOffersAndCapabilities fills in the offers and capabilities of the listings ids names, the i-th at at(i).
func (s *Store) withOffersAndCapabilities(ctx context.Context, ids []string, at func(i int) *Listing) error {
	offers, err := activeOffers(ctx, s.pool, ids...)
	if err != nil {
		return err
	}
	caps, err := listingCapabilities(ctx, s.pool, ids...)
	if err != nil {
		return err
	}
	for i, id := range ids {
		l := at(i)
		l.Offers, l.Capabilities = orNone(offers[id]), orNoCapabilities(caps[id])
	}
	return nil
}

// embedQuery is q's embedding and its model, or nil when there is no embedder or it failed.
func (s *Store) embedQuery(ctx context.Context, q string) (string, []float32) {
	if s.similarity == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	v, err := s.similarity.embedder.Embed(ctx, truncated(q, maxEmbedBytes))
	if err != nil || !unitLength(v) {
		return "", nil
	}
	return s.similarity.embedder.Model(), v
}

// countedUses are the market_uses rows discovery counts, as SQL over the alias u: a use of a listing that ran, or a
// licence bought, rented, subscribed to or renewed; never refunded, never the seller's own and never a linked
// workspace's — linked as of now, not only when the use was charged.
func countedUses() string {
	return `u.payee_agent_id = '' AND u.charge NOT IN ('own', 'linked') AND u.buyer_workspace_id <> u.seller_workspace_id
		AND ((u.use_kind IN ('use', 'trial') AND u.ran_at IS NOT NULL) OR u.use_kind IN ('buy', 'rent', 'subscribe', 'renewal'))
		AND NOT EXISTS (SELECT 1 FROM market_refunds f WHERE f.use_id = u.id)
		AND NOT ` + linkedSQL("u.seller_workspace_id", "u.buyer_workspace_id")
}

// DiscoveryStats is what one run of the nightly job wrote.
type DiscoveryStats struct {
	StatsRows int `json:"stats_rows"`
	Trending  int `json:"trending"`
}

// RefreshDiscoveryStats rewrites market_listing_stats for the statsDays UTC days up to now's, and every listing's
// trending score: its distinct buyers of the TrendingDays up to now's, each weighted trendingDecay to the power of the
// days since its latest counted use.
func (s *Store) RefreshDiscoveryStats(ctx context.Context, now time.Time) (DiscoveryStats, error) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	statsFrom, trendFrom := today.AddDate(0, 0, -(statsDays-1)), today.AddDate(0, 0, -(TrendingDays-1))
	end := today.AddDate(0, 0, 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DiscoveryStats{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out DiscoveryStats
	if _, err := tx.Exec(ctx, `DELETE FROM market_listing_stats WHERE day >= $1::date`, statsFrom); err != nil {
		return out, fmt.Errorf("market: discovery stats: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO market_listing_stats (listing_id, day, uses, distinct_buyers, revenue_usd_micros, computed_at)
		SELECT u.listing_id, (u.used_at AT TIME ZONE 'UTC')::date,
			count(*) FILTER (WHERE u.use_kind IN ('use', 'trial')), count(DISTINCT u.buyer_workspace_id),
			coalesce(sum(u.price_ulxc) FILTER (WHERE u.charge = 'billed'), 0) / $3, $4
		FROM market_uses u JOIN market_listings l ON l.id = u.listing_id
		WHERE u.used_at >= $1 AND u.used_at < $2 AND `+countedUses()+`
		GROUP BY 1, 2`, statsFrom, end, ulxcPerUSDMicro, now)
	if err != nil {
		return out, fmt.Errorf("market: discovery stats: %w", err)
	}
	out.StatsRows = int(tag.RowsAffected())
	if _, err := tx.Exec(ctx, `DELETE FROM market_listing_trending`); err != nil {
		return out, fmt.Errorf("market: trending: %w", err)
	}
	tag, err = tx.Exec(ctx, `INSERT INTO market_listing_trending (listing_id, score, distinct_buyers_7d, computed_at)
		SELECT listing_id, sum(power($4::float8, $3::date - latest)), count(*), $5
		FROM (SELECT u.listing_id, u.buyer_workspace_id, max((u.used_at AT TIME ZONE 'UTC')::date) AS latest
			FROM market_uses u JOIN market_listings l ON l.id = u.listing_id
			WHERE u.used_at >= $1 AND u.used_at < $2 AND `+countedUses()+`
			GROUP BY 1, 2) buyers
		GROUP BY listing_id`, trendFrom, end, today, trendingDecay, now)
	if err != nil {
		return out, fmt.Errorf("market: trending: %w", err)
	}
	out.Trending = int(tag.RowsAffected())
	return out, tx.Commit(ctx)
}
