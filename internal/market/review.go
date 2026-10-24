package market

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/billing"
)

// review.go — B20.4: MARKETPLACE SAFETY — REVIEW, REPORTING, TAKEDOWN.
//
//   - Every publish (and every new version) is reviewed automatically. What B20.1's scan refuses is still
//     refused; what only a person can judge HOLDS the listing: a pipeline step that would run a command,
//     reach the network or read the host's secrets, or text that reads as a prompt injection below the
//     level that refuses it. A held listing is seen and used by its owner only, until an admin approves it.
//   - Anyone may report a listing they can see. Held listings and reported ones make the admin's queue.
//   - An admin can take a listing down. Nobody may use it again, and every billed use of it whose seller's
//     earning is still inside the 14-day holdback — or has not cleared at all — is refunded: a
//     market_refunds row per use, the buyer credited its price on their marketplace bill, and the seller's
//     share reversed.

// Review statuses (market_listings.review_status).
const (
	ReviewApproved  = "approved"
	ReviewHeld      = "held"
	ReviewTakenDown = "taken_down"
)

// ErrTakenDown: the listing was taken down, and stays down.
var ErrTakenDown = errors.New("market: the listing was taken down")

// stepKinds are what a pipeline step may use: a listing of one of the kinds the marketplace runs.
var stepKinds = map[string]bool{"agent": true, "prompt": true, "skill": true, "evaluation": true, "listing": true}

// stepKeys are the keys that ask a step to do something other than use a listing.
var stepKeys = map[string]string{
	"exec": "run a command", "shell": "run a command", "command": "run a command", "cmd": "run a command",
	"script": "run code", "code": "run code", "eval": "run code",
	"http": "reach the network", "url": "reach the network", "webhook": "reach the network", "fetch": "reach the network", "request": "reach the network",
	"env": "read the host's environment", "secret": "read secrets", "secrets": "read secrets", "credentials": "read secrets",
	"file": "read the host's files", "path": "read the host's files",
}

// stepText are the same asks written into a step's text.
var stepText = []struct {
	what string
	re   *regexp.Regexp
}{
	{"reach the network", regexp.MustCompile(`(?i)\b(?:https?|ftp|wss?)://`)},
	{"run a command", regexp.MustCompile(`(?i)(?:^|[\s;&|\x60(])(?:curl|wget|bash|sh|zsh|powershell|netcat|nc|sudo|chmod)\s|\brm\s+-rf\b|\|\s*(?:ba)?sh\b|\$\(`)},
	{"read the host's environment", regexp.MustCompile(`\$\{[A-Za-z_]\w*\}|(?i)process\.env|os\.environ|\bgetenv\b`)},
	{"read the host's files", regexp.MustCompile(`(?i)/etc/(?:passwd|shadow)|~/\.ssh|\.aws/credentials|\.env\b`)},
}

// reviewPipeline says why a pipeline's steps must wait for a person, or "" when none must.
func reviewPipeline(obj map[string]any) string {
	steps, _ := obj["steps"].([]any)
	for i, raw := range steps {
		n := i + 1
		step, ok := raw.(map[string]any)
		if !ok {
			return fmt.Sprintf("step %d is not a step that uses a listing", n)
		}
		for k := range step {
			if what, bad := stepKeys[strings.ToLower(k)]; bad {
				return fmt.Sprintf("step %d would %s (%q)", n, what, k)
			}
		}
		for _, t := range stringsIn(step, nil) {
			for _, p := range stepText {
				if p.re.MatchString(t) {
					return fmt.Sprintf("step %d would %s", n, p.what)
				}
			}
		}
		use, _ := step["use"].(string)
		kind, ref, _ := strings.Cut(use, ":")
		if !stepKinds[kind] || strings.TrimSpace(ref) == "" {
			return fmt.Sprintf("step %d uses %q, which is not an agent, prompt, skill, evaluation or listing", n, use)
		}
	}
	return ""
}

// Report reasons (market_listing_reports.reason).
var reportReasons = map[string]bool{"malicious": true, "injection": true, "secret": true, "personal_data": true,
	"infringing": true, "misleading": true, "other": true}

// Report is one report of a listing.
type Report struct {
	ID          string    `json:"id"`
	ListingID   string    `json:"listing_id"`
	Reason      string    `json:"reason"`
	Details     string    `json:"details,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	AlreadyMade bool      `json:"already_reported,omitempty"` // this reporter's earlier report is still open
}

// Report records reporterWorkspace's report of a listing it can see. A second report while the first is
// open changes nothing.
func (s *Store) Report(ctx context.Context, reporterWorkspace, listingID, reason, details string) (Report, error) {
	if !reportReasons[reason] {
		return Report{}, invalid("reason must be malicious, injection, secret, personal_data, infringing, misleading or other")
	}
	details = strings.TrimSpace(details)
	if len(details) > 2000 {
		return Report{}, invalid("details must be at most 2000 characters")
	}
	if _, err := s.Get(ctx, reporterWorkspace, listingID); err != nil {
		return Report{}, err
	}
	r := Report{ID: "rpt_" + uuid.NewString(), ListingID: listingID, Reason: reason, Details: details}
	err := s.pool.QueryRow(ctx, `INSERT INTO market_listing_reports (id, listing_id, reporter_workspace_id, reason, details)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (listing_id, reporter_workspace_id) WHERE resolved_at IS NULL DO NOTHING
		RETURNING created_at`, r.ID, listingID, reporterWorkspace, reason, details).Scan(&r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `SELECT id, reason, details, created_at FROM market_listing_reports
			WHERE listing_id = $1 AND reporter_workspace_id = $2 AND resolved_at IS NULL`, listingID, reporterWorkspace).
			Scan(&r.ID, &r.Reason, &r.Details, &r.CreatedAt)
		r.AlreadyMade = true
	}
	if err != nil {
		return Report{}, fmt.Errorf("market: report: %w", err)
	}
	return r, nil
}

// QueueItem is one listing an admin must look at: held by the review, reported, or both.
type QueueItem struct {
	Listing     Listing  `json:"listing"`
	OpenReports int      `json:"open_reports"`
	Reasons     []string `json:"report_reasons"` // the open reports' reasons, most frequent first
	Details     []string `json:"report_details"` // the latest open reports' details, newest first
}

// ReviewQueue reads the listings waiting for an admin: every held listing and every one with an open
// report, most reported first.
func (s *Store) ReviewQueue(ctx context.Context) ([]QueueItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("l.", listingColumns)+`,
		       count(r.id)::int,
		       COALESCE(array_agg(r.reason ORDER BY r.reason) FILTER (WHERE r.id IS NOT NULL), '{}'),
		       COALESCE((array_agg(r.details ORDER BY r.created_at DESC) FILTER (WHERE r.details <> ''))[1:5], '{}')
		FROM market_listings l LEFT JOIN market_listing_reports r ON r.listing_id = l.id AND r.resolved_at IS NULL
		WHERE (l.review_status = 'held' OR l.held_version IS NOT NULL OR r.id IS NOT NULL) AND l.review_status <> 'taken_down'
		GROUP BY l.id
		ORDER BY count(r.id) DESC, l.updated_at DESC LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("market: review queue: %w", err)
	}
	defer rows.Close()
	out := []QueueItem{}
	for rows.Next() {
		var q QueueItem
		var reasons []string
		var err error
		if q.Listing, err = scanListing(rows, &q.OpenReports, &reasons, &q.Details); err != nil {
			return nil, err
		}
		q.Reasons = byFrequency(reasons)
		out = append(out, q)
	}
	return out, rows.Err()
}

func prefixed(p, columns string) string {
	cols := strings.Split(columns, ", ")
	for i := range cols {
		cols[i] = p + cols[i]
	}
	return strings.Join(cols, ", ")
}

// byFrequency is the distinct values of sorted, most frequent first.
func byFrequency(sorted []string) []string {
	count := map[string]int{}
	var out []string
	for _, v := range sorted {
		if count[v] == 0 {
			out = append(out, v)
		}
		count[v]++
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && count[out[j]] > count[out[j-1]]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Approve keeps a listing up: a held listing, or its held versions (B17.206), is released to everyone who may see it, and its open reports
// are resolved as kept. A taken-down listing stays down.
func (s *Store) Approve(ctx context.Context, listingID string) (Listing, error) {
	var l Listing
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		l, err = scanListing(tx.QueryRow(ctx, `UPDATE market_listings SET review_status = 'approved', review_reason = '', held_version = NULL, updated_at = now()
			WHERE id = $1 AND review_status <> 'taken_down' RETURNING `+listingColumns, listingID))
		if errors.Is(err, pgx.ErrNoRows) {
			if gone, _ := s.takenDown(ctx, tx, listingID); gone {
				return ErrTakenDown
			}
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE market_listing_reports SET resolved_at = now(), resolution = 'kept'
			WHERE listing_id = $1 AND resolved_at IS NULL`, listingID)
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrTakenDown) {
		err = fmt.Errorf("market: approve: %w", err)
	}
	return l, err
}

func (s *Store) takenDown(ctx context.Context, q pgx.Tx, listingID string) (bool, error) {
	var status string
	err := q.QueryRow(ctx, `SELECT review_status FROM market_listings WHERE id = $1`, listingID).Scan(&status)
	return status == ReviewTakenDown, err
}

// Refunder credits a buyer one refunded use on their marketplace bill, once per use.
// *billing.Service satisfies it.
type Refunder interface {
	CreditMarketRefund(ctx context.Context, buyerWorkspaceID, useID string, ulxc int64, description string) (creditID string, err error)
}

// Takedown is what taking a listing down did.
type Takedown struct {
	Listing Listing  `json:"listing"`
	Refunds []Refund `json:"refunds"`
	// CreditError: a buyer's credit Stripe did not accept yet; RefundTakenDown retries it on its next pass.
	CreditError string `json:"credit_error,omitempty"`
}

// Refund is one refunded use (a market_refunds row).
type Refund struct {
	UseID                  string     `json:"use_id"`
	ListingID              string     `json:"listing_id"`
	BuyerWorkspaceID       string     `json:"buyer_workspace_id"`
	SellerWorkspaceID      string     `json:"seller_workspace_id"`
	PriceULXC              int64      `json:"price_ulxc"`
	GrossUSDMicros         int64      `json:"gross_usd_micros"`
	ReversedShareUSDMicros int64      `json:"reversed_share_usd_micros"`
	Reason                 string     `json:"reason"`
	RefundedAt             time.Time  `json:"refunded_at"`
	StripeCreditID         string     `json:"stripe_credit_id,omitempty"`
	CreditedAt             *time.Time `json:"credited_at,omitempty"`
}

// TakeDown takes a listing down for reason: nobody may use it again, its open reports are resolved, and
// every billed use of it inside the holdback is refunded through refunder (nil: the refunds are recorded
// and credited by a later RefundTakenDown).
func (s *Store) TakeDown(ctx context.Context, refunder Refunder, listingID, reason string) (Takedown, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 500 {
		return Takedown{}, invalid("a takedown needs a reason of at most 500 characters")
	}
	var t Takedown
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		t.Listing, err = scanListing(tx.QueryRow(ctx, `UPDATE market_listings
			SET review_status = 'taken_down', review_reason = $2, taken_down_at = COALESCE(taken_down_at, now()), updated_at = now()
			WHERE id = $1 RETURNING `+listingColumns, listingID, reason))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE market_listing_reports SET resolved_at = now(), resolution = 'taken_down'
			WHERE listing_id = $1 AND resolved_at IS NULL`, listingID)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return t, err
	}
	if err != nil {
		return t, fmt.Errorf("market: take down %s: %w", listingID, err)
	}
	if _, err := s.refundUses(ctx, listingID); err != nil {
		return t, err
	}
	if refunder != nil {
		if _, err := s.creditRefunds(ctx, refunder, listingID); err != nil {
			t.CreditError = err.Error()
		}
	}
	t.Refunds, err = s.Refunds(ctx, listingID)
	return t, err
}

// RefundTakenDown refunds what TakeDown could not finish: uses of taken-down listings not yet refunded (a
// use that was running when its listing came down) and credits Stripe has not yet accepted.
func (s *Store) RefundTakenDown(ctx context.Context, refunder Refunder) (refunded, credited int, err error) {
	if refunded, err = s.refundUses(ctx, ""); err != nil {
		return refunded, 0, err
	}
	credited, err = s.creditRefunds(ctx, refunder, "")
	return refunded, credited, err
}

// refundUses writes a refund for every billed use of listingID (every taken-down listing when "") that ran
// and whose earning, if it has cleared, was still inside its holdback when the listing came down — its 14 days
// not over, or kept there by a hold (B32.17).
func (s *Store) refundUses(ctx context.Context, listingID string) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id FROM market_uses u
		JOIN market_listings l ON l.id = u.listing_id
		LEFT JOIN market_earnings e ON e.use_id = u.id AND e.kind = 'sale'
		WHERE l.review_status = 'taken_down' AND ($1 = '' OR l.id = $1)
		  AND u.charge = 'billed' AND u.ran_at IS NOT NULL
		  AND (e.use_id IS NULL OR e.payable_at > l.taken_down_at
		       OR EXISTS (SELECT 1 FROM market_holds h WHERE h.use_id = u.id AND h.released_at IS NULL))
		  AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = u.id)
		ORDER BY u.used_at, u.id LIMIT 1000`, listingID)
	if err != nil {
		return 0, fmt.Errorf("market: uses to refund: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("market: uses to refund: %w", err)
	}
	n := 0
	for _, id := range ids {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			// The use's lock orders this against ClearInvoice: whichever comes second sees the other's row.
			var listing, buyer, seller string
			var price int64
			if err := tx.QueryRow(ctx, `SELECT listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc FROM market_uses
				WHERE id = $1 FOR UPDATE`, id).Scan(&listing, &buyer, &seller, &price); err != nil {
				return err
			}
			var share int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(share_usd_micros), 0)::bigint FROM market_earnings WHERE use_id = $1`, id).Scan(&share); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO market_refunds (use_id, listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc,
				gross_usd_micros, reversed_share_usd_micros, reason)
				SELECT $1, $2, $3, $4, $5, $6, $7, 'the listing was taken down: ' || review_reason FROM market_listings WHERE id = $2
				ON CONFLICT (use_id) DO NOTHING`, id, listing, buyer, seller, price, price/ulxcPerUSDMicro, share)
			n += int(tag.RowsAffected())
			return err
		})
		if err != nil {
			return n, fmt.Errorf("market: refund use %s: %w", id, err)
		}
	}
	return n, nil
}

// creditRefunds asks Stripe to credit each refund of a use that was billed (metered) and is not credited yet.
func (s *Store) creditRefunds(ctx context.Context, refunder Refunder, listingID string) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.use_id, r.buyer_workspace_id, r.price_ulxc, COALESCE(l.title, ''),
		       CASE WHEN u.tax_metered_at IS NOT NULL AND r.tax_credited_at IS NULL THEN COALESCE(t.tax_usd_micros, 0) ELSE 0 END
		FROM market_refunds r JOIN market_uses u ON u.id = r.use_id LEFT JOIN market_listings l ON l.id = r.listing_id
		LEFT JOIN market_tax_lines t ON t.use_id = r.use_id
		WHERE r.credited_at IS NULL AND r.cause = 'takedown' AND u.metered_at IS NOT NULL AND ($1 = '' OR r.listing_id = $1)
		ORDER BY r.refunded_at, r.use_id LIMIT 200`, listingID)
	if err != nil {
		return 0, fmt.Errorf("market: refunds to credit: %w", err)
	}
	type due struct {
		use, buyer, title string
		ulxc, tax         int64
	}
	todo, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.use, &d.buyer, &d.ulxc, &d.title, &d.tax)
	})
	if err != nil {
		return 0, fmt.Errorf("market: refunds to credit: %w", err)
	}
	n := 0
	for _, d := range todo {
		// B32.39: its tax is credited back too, as its own line, once per use.
		if d.tax > 0 {
			tr, ok := refunder.(TaxRefunder)
			if !ok {
				return n, fmt.Errorf("market: credit refund of %s: %w", d.use, billing.ErrNoMarketTax)
			}
			if _, err := tr.CreditMarketRefundTax(ctx, d.buyer, d.use, d.tax*ulxcPerUSDMicro,
				fmt.Sprintf("Refund of tax: %q was taken down (use %s)", d.title, d.use)); err != nil {
				return n, fmt.Errorf("market: credit refund of %s's tax: %w", d.use, err)
			}
			if _, err := s.pool.Exec(ctx, `UPDATE market_refunds SET tax_credited_at = now() WHERE use_id = $1 AND tax_credited_at IS NULL`,
				d.use); err != nil {
				return n, fmt.Errorf("market: credit refund of %s's tax: %w", d.use, err)
			}
		}
		creditID, err := refunder.CreditMarketRefund(ctx, d.buyer, d.use, d.ulxc, fmt.Sprintf("Refund: %q was taken down (use %s)", d.title, d.use))
		if err != nil {
			return n, fmt.Errorf("market: credit refund of %s: %w", d.use, err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE market_refunds SET stripe_credit_id = $2, credited_at = now()
			WHERE use_id = $1 AND credited_at IS NULL`, d.use, creditID); err != nil {
			return n, fmt.Errorf("market: credit refund of %s: %w", d.use, err)
		}
		n++
	}
	return n, nil
}

// Refunds reads a listing's refunds, oldest first.
func (s *Store) Refunds(ctx context.Context, listingID string) ([]Refund, error) {
	rows, err := s.pool.Query(ctx, `SELECT use_id, listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc, gross_usd_micros,
		       reversed_share_usd_micros, reason, refunded_at, COALESCE(stripe_credit_id, ''), credited_at
		FROM market_refunds WHERE listing_id = $1 ORDER BY refunded_at, use_id`, listingID)
	if err != nil {
		return nil, fmt.Errorf("market: refunds: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Refund, error) {
		var r Refund
		return r, row.Scan(&r.UseID, &r.ListingID, &r.BuyerWorkspaceID, &r.SellerWorkspaceID, &r.PriceULXC, &r.GrossUSDMicros,
			&r.ReversedShareUSDMicros, &r.Reason, &r.RefundedAt, &r.StripeCreditID, &r.CreditedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("market: refunds: %w", err)
	}
	return out, nil
}
