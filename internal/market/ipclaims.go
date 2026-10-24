package market

// B32.47 — IP claims: notice, counter-notice, decision, and attribution as a remedy.
//
// A workspace that believes a listing copies its work files a claim (market_ip_claims, migration 0217) naming the
// original — one of its own listings, or an outside reference — with its evidence and a good-faith statement. The
// listing stays up, but every billed use of it from then on is held by the claim (a market_holds row, escrow.go):
// its earnings stay in holdback past their 14 days until the claim is decided. The seller reads the claim among
// their own (IPClaims) and may counter it until its counter_by, LENS_IP_COUNTER_DAYS after filing. Once they have,
// or once that window is over, the operator decides:
//
//   - upheld: the listing is taken down with its refunds (review.go), the held uses among them;
//   - attributed: a claim edge (lineage.go) from every version of the listing to the claimant's original, at the
//     share the operator sets, so every sale from then on pays the claimant a royalty; the holds are released;
//   - rejected: the holds are released.
//
// Filing, the counter and the decision each write a row in the operator audit trail, in the same transaction.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/operatoraudit"
)

// The states of a claim: undecided (open, countered) and the three decisions.
const (
	ClaimOpen       = "open"
	ClaimCountered  = "countered"
	ClaimUpheld     = "upheld"
	ClaimAttributed = "attributed"
	ClaimRejected   = "rejected"
)

// DefaultIPCounterDays is how long a seller has to counter a claim (LENS_IP_COUNTER_DAYS): 10 days — a proposal for
// Nicolai.
const DefaultIPCounterDays = 10

// SetIPCounterDays sets how many days after a claim is filed its seller may counter it (LENS_IP_COUNTER_DAYS).
func (s *Store) SetIPCounterDays(days int) { s.ipCounterDays = &days }

func (s *Store) counterWindow() time.Duration {
	days := DefaultIPCounterDays
	if s.ipCounterDays != nil {
		days = *s.ipCounterDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// IPClaimFiling is what a claimant files: the listing, the original it copies, the evidence and a statement that
// the claim is made in good faith.
type IPClaimFiling struct {
	ListingID         string `json:"listing_id"`
	OriginalListingID string `json:"original_listing_id,omitempty"` // one of the claimant's own listings
	OriginalReference string `json:"original_reference,omitempty"`  // or where the original is published outside the marketplace
	Evidence          string `json:"evidence"`
	GoodFaith         string `json:"good_faith"`
}

// IPClaim is one market_ip_claims row, with the listing's seller and how many of its uses the claim holds now.
type IPClaim struct {
	ID                  string     `json:"id"`
	ListingID           string     `json:"listing_id"`
	ListingTitle        string     `json:"listing_title"`
	SellerWorkspaceID   string     `json:"seller_workspace_id"`
	ClaimantWorkspaceID string     `json:"claimant_workspace_id"`
	OriginalListingID   string     `json:"original_listing_id,omitempty"`
	OriginalReference   string     `json:"original_reference,omitempty"`
	Evidence            string     `json:"evidence"`
	GoodFaith           string     `json:"good_faith"`
	Status              string     `json:"status"`
	FiledAt             time.Time  `json:"filed_at"`
	CounterBy           time.Time  `json:"counter_by"`
	CounterStatement    string     `json:"counter_statement,omitempty"`
	CounteredAt         *time.Time `json:"countered_at,omitempty"`
	ShareBPS            int        `json:"share_bps,omitempty"`
	DecisionReason      string     `json:"decision_reason,omitempty"`
	DecidedBy           string     `json:"decided_by,omitempty"`
	DecidedAt           *time.Time `json:"decided_at,omitempty"`
	HeldUses            int        `json:"held_uses"`
}

// IPClaimDecision is the operator's decision on a claim. ShareBPS is the share of each sale an attributed original
// receives from then on, 1 to 10000; the other outcomes take none.
type IPClaimDecision struct {
	Outcome  string `json:"outcome"`
	ShareBPS int    `json:"share_bps"`
	Reason   string `json:"reason"`
}

// IPClaimDecided is what deciding a claim did: the claim, the takedown an upheld claim made, and how many holds it
// released.
type IPClaimDecided struct {
	Claim         IPClaim   `json:"claim"`
	Takedown      *Takedown `json:"takedown,omitempty"`
	HoldsReleased int       `json:"holds_released"`
}

const ipClaimColumns = `c.id, c.listing_id, l.title, l.workspace_id, c.claimant_workspace_id, coalesce(c.original_listing_id, ''),
	c.original_reference, c.evidence, c.good_faith, c.status, c.filed_at, c.counter_by, c.counter_statement, c.countered_at,
	c.share_bps, c.decision_reason, c.decided_by, c.decided_at,
	(SELECT count(*) FROM market_holds h WHERE h.opened_by = c.id AND h.reason = 'ip_claim' AND h.released_at IS NULL)`

const ipClaimFrom = ` FROM market_ip_claims c JOIN market_listings l ON l.id = c.listing_id `

func scanIPClaim(row pgx.Row) (IPClaim, error) {
	var c IPClaim
	err := row.Scan(&c.ID, &c.ListingID, &c.ListingTitle, &c.SellerWorkspaceID, &c.ClaimantWorkspaceID, &c.OriginalListingID,
		&c.OriginalReference, &c.Evidence, &c.GoodFaith, &c.Status, &c.FiledAt, &c.CounterBy, &c.CounterStatement, &c.CounteredAt,
		&c.ShareBPS, &c.DecisionReason, &c.DecidedBy, &c.DecidedAt, &c.HeldUses)
	return c, err
}

func collectIPClaims(rows pgx.Rows, err error) ([]IPClaim, error) {
	if err != nil {
		return nil, fmt.Errorf("market: ip claims: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IPClaim, error) { return scanIPClaim(row) })
	if err != nil {
		return nil, fmt.Errorf("market: ip claims: %w", err)
	}
	if out == nil {
		out = []IPClaim{}
	}
	return out, nil
}

// FileIPClaim files claimant's claim that a listing it may see copies its original. From then on every billed use
// of the listing is held until the claim is decided; the listing stays up.
func (s *Store) FileIPClaim(ctx context.Context, claimant string, f IPClaimFiling, now time.Time) (IPClaim, error) {
	f.ListingID, f.OriginalListingID = strings.TrimSpace(f.ListingID), strings.TrimSpace(f.OriginalListingID)
	f.OriginalReference, f.Evidence, f.GoodFaith = strings.TrimSpace(f.OriginalReference), strings.TrimSpace(f.Evidence), strings.TrimSpace(f.GoodFaith)
	switch {
	case claimant == "":
		return IPClaim{}, invalid("a claim is filed by a workspace")
	case f.ListingID == "":
		return IPClaim{}, invalid("listing_id names the listing the claim is about")
	case (f.OriginalListingID == "") == (f.OriginalReference == ""):
		return IPClaim{}, invalid("name the original once: original_listing_id, one of your own listings, or original_reference, where it is published outside the marketplace")
	case len(f.OriginalReference) > 1000:
		return IPClaim{}, invalid("original_reference must be at most 1000 characters")
	case f.Evidence == "" || len(f.Evidence) > 8000:
		return IPClaim{}, invalid("evidence must say why the listing copies the original, in at most 8000 characters")
	case f.GoodFaith == "" || len(f.GoodFaith) > 2000:
		return IPClaim{}, invalid("good_faith must state that the claim is made in good faith, in at most 2000 characters")
	}
	var id string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		l, err := visibleListing(ctx, tx, claimant, f.ListingID, "")
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if l.WorkspaceID == claimant {
			return invalid("a workspace cannot claim its own listing")
		}
		if f.OriginalListingID != "" {
			var owner string
			err := tx.QueryRow(ctx, `SELECT workspace_id FROM market_listings WHERE id = $1`, f.OriginalListingID).Scan(&owner)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (owner != claimant || f.OriginalListingID == f.ListingID)) {
				return invalid("original_listing_id must be one of your own listings")
			}
			if err != nil {
				return err
			}
		}
		id = "mic_" + uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO market_ip_claims (id, listing_id, claimant_workspace_id, original_listing_id, original_reference,
			evidence, good_faith, filed_at, counter_by) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9)`,
			id, f.ListingID, claimant, f.OriginalListingID, f.OriginalReference, f.Evidence, f.GoodFaith, now, now.Add(s.counterWindow()))
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return invalid("you already have an undecided claim on this listing")
		}
		if err != nil {
			return err
		}
		original := f.OriginalListingID
		if original == "" {
			original = f.OriginalReference
		}
		_, err = operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: "workspace:" + claimant, Action: "market.ip_claim.file", Target: id,
			Detail: fmt.Sprintf("listing %s of %s; original %s", f.ListingID, l.WorkspaceID, original)})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			return IPClaim{}, err
		}
		return IPClaim{}, fmt.Errorf("market: file ip claim: %w", err)
	}
	return s.IPClaim(ctx, id)
}

// IPClaim reads one claim.
func (s *Store) IPClaim(ctx context.Context, claimID string) (IPClaim, error) {
	c, err := scanIPClaim(s.pool.QueryRow(ctx, `SELECT `+ipClaimColumns+ipClaimFrom+`WHERE c.id = $1`, claimID))
	if errors.Is(err, pgx.ErrNoRows) {
		return IPClaim{}, ErrNotFound
	}
	if err != nil {
		return IPClaim{}, fmt.Errorf("market: ip claim: %w", err)
	}
	return c, nil
}

// IPClaims reads a workspace's claims, newest first: those against its listings — how a seller is told of one —
// and those it filed.
func (s *Store) IPClaims(ctx context.Context, workspaceID string) (against, filed []IPClaim, err error) {
	if against, err = collectIPClaims(s.pool.Query(ctx, `SELECT `+ipClaimColumns+ipClaimFrom+`WHERE l.workspace_id = $1
		ORDER BY c.filed_at DESC, c.id`, workspaceID)); err != nil {
		return nil, nil, err
	}
	filed, err = collectIPClaims(s.pool.Query(ctx, `SELECT `+ipClaimColumns+ipClaimFrom+`WHERE c.claimant_workspace_id = $1
		ORDER BY c.filed_at DESC, c.id`, workspaceID))
	return against, filed, err
}

// IPClaimQueue reads the claims the operator looks at, oldest first: every undecided one when status is "", or
// those in status.
func (s *Store) IPClaimQueue(ctx context.Context, status string) ([]IPClaim, error) {
	switch status {
	case "", ClaimOpen, ClaimCountered, ClaimUpheld, ClaimAttributed, ClaimRejected:
	default:
		return nil, invalid("status must be open, countered, upheld, attributed or rejected")
	}
	return collectIPClaims(s.pool.Query(ctx, `SELECT `+ipClaimColumns+ipClaimFrom+`
		WHERE CASE WHEN $1 = '' THEN c.status IN ('open', 'countered') ELSE c.status = $1 END
		ORDER BY c.filed_at, c.id LIMIT 500`, status))
}

// CounterIPClaim is the seller's counter-notice: why the listing does not copy the original. It is taken until the
// claim's counter_by, once.
func (s *Store) CounterIPClaim(ctx context.Context, seller, claimID, statement string, now time.Time) (IPClaim, error) {
	statement = strings.TrimSpace(statement)
	if statement == "" || len(statement) > 8000 {
		return IPClaim{}, invalid("statement must answer the claim, in at most 8000 characters")
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		var counterBy time.Time
		err := tx.QueryRow(ctx, `SELECT c.status, c.counter_by FROM market_ip_claims c JOIN market_listings l ON l.id = c.listing_id
			WHERE c.id = $1 AND l.workspace_id = $2 FOR UPDATE OF c`, claimID, seller).Scan(&status, &counterBy)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case status != ClaimOpen:
			return invalid("the claim is %s: it takes one counter, before it is decided", status)
		case now.After(counterBy):
			return invalid("the counter window closed at %s", counterBy.UTC().Format(time.RFC3339))
		}
		if _, err := tx.Exec(ctx, `UPDATE market_ip_claims SET status = 'countered', counter_statement = $2, countered_at = $3 WHERE id = $1`,
			claimID, statement, now); err != nil {
			return err
		}
		_, err = operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: "workspace:" + seller, Action: "market.ip_claim.counter", Target: claimID})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			return IPClaim{}, err
		}
		return IPClaim{}, fmt.Errorf("market: counter ip claim %s: %w", claimID, err)
	}
	return s.IPClaim(ctx, claimID)
}

// DecideIPClaim records actor's decision on a claim the seller has countered, or whose counter window is over, and
// carries it out (see the top of this file). The same decision sent again finishes what the first could not — a
// takedown's refunds, a hold not yet released — and changes nothing else.
func (s *Store) DecideIPClaim(ctx context.Context, refunder Refunder, claimID, actor string, d IPClaimDecision, now time.Time) (IPClaimDecided, error) {
	d.Reason, actor = strings.TrimSpace(d.Reason), strings.TrimSpace(actor)
	switch {
	case d.Outcome != ClaimUpheld && d.Outcome != ClaimAttributed && d.Outcome != ClaimRejected:
		return IPClaimDecided{}, invalid("outcome must be upheld, attributed or rejected")
	case d.Outcome == ClaimAttributed && (d.ShareBPS < 1 || d.ShareBPS > 10000):
		return IPClaimDecided{}, invalid("an attribution's share_bps must be 1 to 10000")
	case d.Outcome != ClaimAttributed && d.ShareBPS != 0:
		return IPClaimDecided{}, invalid("share_bps is only for an attributed claim")
	case d.Reason == "" || len(d.Reason) > 400:
		return IPClaimDecided{}, invalid("a decision needs a reason of at most 400 characters")
	case actor == "":
		return IPClaimDecided{}, invalid("a decision names the operator who made it")
	}
	var listingID string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status, original string
		var counterBy time.Time
		err := tx.QueryRow(ctx, `SELECT listing_id, status, counter_by, coalesce(original_listing_id, '') FROM market_ip_claims WHERE id = $1 FOR UPDATE`,
			claimID).Scan(&listingID, &status, &counterBy, &original)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case status == d.Outcome:
			return nil // decided already: only what follows the decision is left to finish
		case status != ClaimOpen && status != ClaimCountered:
			return invalid("the claim was already decided: %s", status)
		case status == ClaimOpen && !now.After(counterBy):
			return invalid("the seller may counter until %s: decide once they have, or after that", counterBy.UTC().Format(time.RFC3339))
		case d.Outcome == ClaimAttributed && original == "":
			return invalid("an original outside the marketplace has no listing to pay: uphold or reject the claim")
		}
		if d.Outcome == ClaimAttributed {
			if err := attributeTx(ctx, tx, listingID, original, d.ShareBPS); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE market_ip_claims SET status = $2, share_bps = $3, decision_reason = $4, decided_by = $5, decided_at = $6
			WHERE id = $1`, claimID, d.Outcome, d.ShareBPS, d.Reason, actor, now); err != nil {
			return err
		}
		detail := d.Reason
		if d.Outcome == ClaimAttributed {
			detail = fmt.Sprintf("%s to %s at %d bps: %s", listingID, original, d.ShareBPS, d.Reason)
		}
		_, err = operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: actor, Action: "market.ip_claim." + d.Outcome, Target: claimID, Detail: detail})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			return IPClaimDecided{}, err
		}
		return IPClaimDecided{}, fmt.Errorf("market: decide ip claim %s: %w", claimID, err)
	}
	var out IPClaimDecided
	if d.Outcome == ClaimUpheld {
		t, err := s.TakeDown(ctx, refunder, listingID, "IP claim upheld: "+d.Reason)
		if err != nil {
			return out, err
		}
		out.Takedown = &t
	}
	if out.HoldsReleased, err = s.releaseClaimHolds(ctx, claimID, d.Outcome, now); err != nil {
		return out, err
	}
	out.Claim, err = s.IPClaim(ctx, claimID)
	return out, err
}

// attributeTx adds a claim edge from every version of listingID to the latest version of original at shareBPS, on
// tx: from then on each sale of the listing pays the original's owner a royalty (royalties.go), and a new version
// keeps the edge (declareParents). A version that already builds on original keeps the edge it has.
func attributeTx(ctx context.Context, tx pgx.Tx, listingID, original string, shareBPS int) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('market_lineage', 0))`); err != nil {
		return err
	}
	var cycle bool
	if err := tx.QueryRow(ctx, `WITH RECURSIVE up(id) AS (
			SELECT parent_listing_id FROM market_lineage WHERE child_listing_id = $1
			UNION
			SELECT e.parent_listing_id FROM market_lineage e JOIN up ON e.child_listing_id = up.id)
		SELECT EXISTS (SELECT 1 FROM up WHERE id = $2)`, original, listingID).Scan(&cycle); err != nil {
		return err
	}
	if cycle {
		return fmt.Errorf("%w (%s descends from %s): uphold or reject the claim", ErrLineageCycle, original, listingID)
	}
	_, err := tx.Exec(ctx, `INSERT INTO market_lineage (id, child_listing_id, child_version, parent_listing_id, parent_version, share_bps, source)
		SELECT 'lin_' || gen_random_uuid(), v.listing_id, v.version, o.id, coalesce(o.held_version - 1, o.latest_version), $3, 'claim'
		  FROM market_listing_versions v CROSS JOIN market_listings o
		 WHERE v.listing_id = $1 AND o.id = $2
		ORDER BY v.version
		ON CONFLICT (child_listing_id, child_version, parent_listing_id) DO NOTHING`, listingID, original, shareBPS)
	return err
}

// releaseClaimHolds releases the holds a decided claim opened, each with its earning if its 14 days are over
// (ReleaseHold). An upheld claim releases only the holds of uses its takedown refunded: a refund already took the
// earning back out of the holdback, and a use not refunded yet stays held until RefundTakenDown reaches it.
func (s *Store) releaseClaimHolds(ctx context.Context, claimID, outcome string, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT h.id FROM market_holds h WHERE h.opened_by = $1 AND h.reason = 'ip_claim' AND h.released_at IS NULL
		AND ($2 <> 'upheld' OR EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = h.use_id)) ORDER BY h.opened_at, h.id`, claimID, outcome)
	if err != nil {
		return 0, fmt.Errorf("market: holds of ip claim %s: %w", claimID, err)
	}
	holds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("market: holds of ip claim %s: %w", claimID, err)
	}
	n := 0
	for _, id := range holds {
		if _, err := s.ReleaseHold(ctx, id, "ip_claim "+outcome+" ("+claimID+")", now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
