package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// prizes.go — B32.35: ROOM PRIZES, PAID AS A PURCHASE OF THE WINNING CONTRIBUTION.
//
// A room's owner posts a prize — a title, what wins it, an amount and a deadline — no larger than what the room
// wallet's monthly limit has left once the room's other open prizes are counted. Before the deadline the owner awards
// it to one of the room's contributions: the owner's workspace buys that contribution at the prize's amount, with the
// room's wallet as the agent whose rules judge it (market.AwardPrize) — one billed prize row on the owner's marketplace
// bill and a perpetual commercial licence to the version that won — so the contribution's author earns their share
// through the normal clearing and payout. Nothing is escrowed and nothing moves between owners. A prize not awarded by
// its deadline closes and charges nothing. The room is told when a prize is posted, awarded and closed.

// Prize statuses.
const (
	PrizeOpen    = "open"
	PrizeAwarded = "awarded"
	PrizeClosed  = "closed"
)

// maxPrizes bounds GET /v1/rooms/{id}/prizes.
const maxPrizes = 200

// ErrOverBudget refuses a prize larger than what the room's budget has left this month.
var ErrOverBudget = errors.New("rooms: over the room's budget")

// PrizeDraft is a prize as its owner posts it.
type PrizeDraft struct {
	Title           string    `json:"title"`
	Criteria        string    `json:"criteria"`
	AmountUSDMicros int64     `json:"amount_usd_micros"`
	Deadline        time.Time `json:"deadline"`
}

// Prize is a room's prize: what it pays and by when, and — once awarded — the contribution that won it, its author,
// the billed prize row and the owner's licence.
type Prize struct {
	ID                string     `json:"id"`
	RoomID            string     `json:"room_id"`
	PosterWorkspaceID string     `json:"poster_workspace_id"`
	Title             string     `json:"title"`
	Criteria          string     `json:"criteria"`
	AmountUSDMicros   int64      `json:"amount_usd_micros"`
	Deadline          time.Time  `json:"deadline"`
	Status            string     `json:"status"`
	ContributionID    string     `json:"contribution_id,omitempty"`
	WinnerWorkspaceID string     `json:"winner_workspace_id,omitempty"`
	UseID             string     `json:"use_id,omitempty"`
	LicenceID         string     `json:"licence_id,omitempty"`
	AwardedAt         *time.Time `json:"awarded_at,omitempty"`
	ClosedAt          *time.Time `json:"closed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	// Message is the message the room got for what just happened to the prize: its posting or its award.
	Message *Message `json:"message,omitempty"`
}

// Award is a prize's award as its owner sees it: the prize, and the licence the owner now holds to what won.
type Award struct {
	Prize   Prize          `json:"prize"`
	Licence market.Licence `json:"licence"`
}

const prizeCols = `p.id, p.room_id, p.poster_workspace_id, p.title, p.criteria, p.amount_usd_micros, p.deadline, p.status,
	COALESCE(p.contribution_id, ''), p.winner_workspace_id, COALESCE(p.use_id, ''), COALESCE(p.licence_id, ''), p.awarded_at, p.closed_at, p.created_at`

func scanPrize(row pgx.Row) (Prize, error) {
	var p Prize
	err := row.Scan(&p.ID, &p.RoomID, &p.PosterWorkspaceID, &p.Title, &p.Criteria, &p.AmountUSDMicros, &p.Deadline, &p.Status,
		&p.ContributionID, &p.WinnerWorkspaceID, &p.UseID, &p.LicenceID, &p.AwardedAt, &p.ClosedAt, &p.CreatedAt)
	return p, err
}

func prizeErr(op string, err error) error {
	var refusal *ScanRefusal
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrConflict) || errors.Is(err, ErrPlanLimit) || errors.Is(err, ErrOverBudget) || errors.As(err, &refusal) {
		return err
	}
	return fmt.Errorf("rooms: %s: %w", op, err)
}

// prizeOwner answers the room's spender when ws is its owner, who alone posts and awards the room's prizes.
func (s *Store) prizeOwner(ctx context.Context, ws, roomID, what string) (Spender, error) {
	sp, err := s.MaySpend(ctx, ws, roomID)
	if err != nil {
		return sp, err
	}
	if sp.OwnerWorkspaceID != ws {
		return sp, forbidden("only the room's owner %s a prize: it is bought on the owner's marketplace bill", what)
	}
	return sp, nil
}

// PostPrize posts ws's prize to the room, by user: ws must be the room's owner, and the prize no larger than what the
// room wallet's monthly limit has left this month less the room's other open prizes. The room is told.
func (s *Store) PostPrize(ctx context.Context, ws, user, roomID string, d PrizeDraft) (Prize, error) {
	d.Title, d.Criteria = strings.TrimSpace(d.Title), strings.TrimSpace(d.Criteria)
	switch {
	case d.Title == "":
		return Prize{}, invalid("a prize needs a title")
	case utf8.RuneCountInString(d.Title) > maxTitle:
		return Prize{}, invalid("a prize's title is at most %d characters", maxTitle)
	case utf8.RuneCountInString(d.Criteria) > maxDescription:
		return Prize{}, invalid("a prize's criteria are at most %d characters", maxDescription)
	case d.AmountUSDMicros <= 0:
		return Prize{}, invalid("a prize pays more than nothing: amount_usd_micros is its amount in millionths of a dollar")
	case d.AmountUSDMicros > math.MaxInt64/economy.ULXCPerUSDMicro:
		return Prize{}, invalid("amount_usd_micros is more than any bill can carry")
	case d.Deadline.IsZero() || !d.Deadline.After(time.Now()):
		return Prize{}, invalid("a prize's deadline is in the future")
	}
	sp, err := s.prizeOwner(ctx, ws, roomID, "posts")
	if err != nil {
		return Prize{}, prizeErr("prize", err)
	}
	var out Prize
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoomAny(ctx, tx, roomID) // the room's lock orders its prizes against its budget
		if err != nil {
			return err
		}
		b, err := economy.ReadAgentBudget(ctx, tx, sp.OwnerWorkspaceID, sp.WalletAgentID, time.Now())
		if err != nil {
			return err
		}
		if b.MonthlyLimitULXC > 0 {
			var promised int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_usd_micros), 0)::bigint FROM room_prizes
				WHERE room_id = $1 AND status = 'open' AND deadline > now()`, roomID).Scan(&promised); err != nil {
				return err
			}
			left := b.MonthlyLimitULXC - b.SpentThisMonthULXC - promised*economy.ULXCPerUSDMicro
			if amount := d.AmountUSDMicros * economy.ULXCPerUSDMicro; amount > left {
				return fmt.Errorf("%w: the prize is %s and the room's budget has %s left this month. Its wallet's monthly limit is %s, "+
					"%s of it is spent and %s is promised to the room's open prizes", ErrOverBudget, usd(d.AmountUSDMicros),
					usd(max(left, 0)/economy.ULXCPerUSDMicro), usd(b.MonthlyLimitULXC/economy.ULXCPerUSDMicro),
					usd(b.SpentThisMonthULXC/economy.ULXCPerUSDMicro), usd(promised))
			}
		}
		id := "rprz_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if out, err = scanPrize(tx.QueryRow(ctx, `INSERT INTO room_prizes AS p (id, room_id, poster_workspace_id, poster_user_id, title, criteria,
				amount_usd_micros, deadline) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+prizeCols,
			id, roomID, ws, user, d.Title, d.Criteria, d.AmountUSDMicros, d.Deadline)); err != nil {
			return err
		}
		body := fmt.Sprintf("posted a prize of %s: “%s”, awarded by %s UTC", usd(out.AmountUSDMicros), out.Title,
			out.Deadline.UTC().Format("2 January 2006 15:04"))
		if out.Criteria != "" {
			body += "\n\nWhat wins it: " + out.Criteria
		}
		m, err := postPrizeMessage(ctx, tx, r, ws, user, body, false, out, "posted")
		out.Message = &m
		return err
	})
	return out, prizeErr("prize", err)
}

// postPrizeMessage posts a prize message to the room r, whose row tx holds. In a public room it is scanned: what the
// owner wrote is refused for a secret or personal data, as any message is; an award or a close, which repeats what was
// scanned before, is posted without its titles instead (plain).
func postPrizeMessage(ctx context.Context, tx pgx.Tx, r Room, ws, user, body string, plain bool, p Prize, what string) (Message, error) {
	refs := map[string]any{"prize": what, "prize_id": p.ID, "amount_usd_micros": p.AmountUSDMicros, "status": p.Status}
	if p.ContributionID != "" {
		refs["contribution_id"], refs["winner_workspace_id"], refs["use_id"], refs["licence_id"] =
			p.ContributionID, p.WinnerWorkspaceID, p.UseID, p.LicenceID
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return Message{}, err
	}
	body = clip(body)
	var scan []byte
	if r.Visibility == Public {
		scan, err = scanPublic(body)
		var refusal *ScanRefusal
		if plain && errors.As(err, &refusal) {
			body = fmt.Sprintf("the prize of %s was %s", usd(p.AmountUSDMicros), what)
			scan, err = scanPublic(body)
		}
		if err != nil {
			return Message{}, err
		}
	}
	id := "rmsg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	m, err := scanMessage(tx.QueryRow(ctx, `INSERT INTO room_messages AS m (id, room_id, author_workspace_id, author_user_id, kind, body, refs, scan)
		VALUES ($1, $2, $3, $4, 'prize', $5, $6, $7) RETURNING `+messageCols, id, r.ID, ws, user, body, string(refsJSON), jsonText(scan)), ws)
	if err != nil {
		return m, err
	}
	if err := appendEvent(ctx, tx, r.ID, EventPosted, id); err != nil {
		return m, err
	}
	return m, touch(ctx, tx, r.ID)
}

// Prizes lists the room's prizes as viewer reads them, newest first, once those past their deadline are closed.
func (s *Store) Prizes(ctx context.Context, viewer, roomID string) ([]Prize, error) {
	if err := readTx(ctx, s, func(tx pgx.Tx) error { return readable(ctx, tx, viewer, roomID) }); err != nil {
		return nil, prizeErr("prizes", err)
	}
	if _, err := s.ClosePrizes(ctx, roomID, time.Now()); err != nil {
		return nil, err
	}
	var out []Prize
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+prizeCols+` FROM room_prizes p WHERE p.room_id = $1 ORDER BY p.created_at DESC, p.id LIMIT $2`,
			roomID, maxPrizes)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Prize, error) { return scanPrize(row) })
		return err
	})
	if out == nil {
		out = []Prize{}
	}
	return out, prizeErr("prizes", err)
}

// ClosePrizes closes the open prizes past their deadline as of now — roomID's, or every room's when it is "" — and
// tells each room: nothing is charged for them. It answers how many it closed.
func (s *Store) ClosePrizes(ctx context.Context, roomID string, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, room_id FROM room_prizes WHERE status = 'open' AND deadline <= $1 AND ($2 = '' OR room_id = $2)
		ORDER BY deadline LIMIT 500`, now, roomID)
	if err != nil {
		return 0, fmt.Errorf("rooms: close prizes: %w", err)
	}
	type due struct{ id, room string }
	expired, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.id, &d.room)
	})
	if err != nil {
		return 0, fmt.Errorf("rooms: close prizes: %w", err)
	}
	n := 0
	for _, d := range expired {
		closed := false
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			r, err := lockRoomAny(ctx, tx, d.room)
			if err != nil {
				return err
			}
			p, err := scanPrize(tx.QueryRow(ctx, `UPDATE room_prizes AS p SET status = 'closed', closed_at = now()
				WHERE p.id = $1 AND p.status = 'open' AND p.deadline <= $2 RETURNING `+prizeCols, d.id, now))
			if errors.Is(err, pgx.ErrNoRows) { // awarded or closed by another meanwhile
				return nil
			}
			if err != nil {
				return err
			}
			closed = true
			_, err = postPrizeMessage(ctx, tx, r, p.PosterWorkspaceID, "", fmt.Sprintf("the prize “%s” closed at its deadline without a winner: "+
				"nothing was charged for its %s", p.Title, usd(p.AmountUSDMicros)), true, p, "closed")
			return err
		})
		if err != nil {
			return n, fmt.Errorf("rooms: close prize %s: %w", d.id, err)
		}
		if closed {
			n++
		}
	}
	return n, nil
}

// AwardPrize awards the room's prize to one of its contributions, for ws — the room's owner — by user: the owner's
// workspace buys the contribution at the prize's amount with the room's wallet as the agent its rules judge, and holds
// a perpetual commercial licence to the version that won. The award, its purchase and the room's message are one
// transaction: a prize already awarded or past its deadline writes nothing, and a prize past its deadline closes.
func (s *Store) AwardPrize(ctx context.Context, deps market.LicenceDeps, ws, user, roomID, prizeID, contributionID string) (Award, error) {
	if s.market == nil {
		return Award{}, errors.New("rooms: prizes are not wired to the marketplace")
	}
	contributionID = strings.TrimSpace(contributionID)
	if contributionID == "" {
		return Award{}, invalid("name the contribution that wins: contribution_id is one of the room's contributions")
	}
	sp, err := s.prizeOwner(ctx, ws, roomID, "awards")
	if err != nil {
		return Award{}, prizeErr("award", err)
	}
	var p Prize
	var listingID, author, title string
	var version int
	err = readTx(ctx, s, func(tx pgx.Tx) error {
		var err error
		p, err = scanPrize(tx.QueryRow(ctx, `SELECT `+prizeCols+` FROM room_prizes p WHERE p.id = $1 AND p.room_id = $2`, prizeID, roomID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the room has no prize %s", ErrNotFound, prizeID)
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT c.listing_id, c.version, c.author_workspace_id, COALESCE(l.title, '') FROM room_contributions c
			LEFT JOIN market_listings l ON l.id = c.listing_id WHERE c.id = $1 AND c.room_id = $2`, contributionID, roomID).
			Scan(&listingID, &version, &author, &title)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the room has no contribution %s", ErrNotFound, contributionID)
		}
		return err
	})
	if err != nil {
		return Award{}, prizeErr("award", err)
	}
	switch {
	case p.Status == PrizeAwarded:
		return Award{}, fmt.Errorf("%w: the prize was awarded already, to %s", ErrConflict, p.ContributionID)
	case p.Status == PrizeClosed:
		return Award{}, fmt.Errorf("%w: the prize closed at its deadline without a winner, and nothing was charged", ErrConflict)
	case !p.Deadline.After(time.Now()):
		if _, err := s.ClosePrizes(ctx, roomID, time.Now()); err != nil {
			return Award{}, err
		}
		return Award{}, fmt.Errorf("%w: the prize's deadline has passed: it closed without a winner, and nothing was charged", ErrConflict)
	case author == ws:
		return Award{}, invalid("the room's owner cannot win their own prize: award it to another member's contribution")
	}
	var out Award
	lic, err := s.market.AwardPrize(sp.Context(ctx), deps, sp.OwnerWorkspaceID, sp.WalletAgentID, market.PrizeAward{
		ListingID: listingID, Version: version, AmountUSDMicros: p.AmountUSDMicros, RoomID: roomID, ActorWorkspaceID: ws,
		What: fmt.Sprintf("room prize %s: %s to %s", p.ID, usd(p.AmountUSDMicros), contributionID),
	}, func(tx pgx.Tx, lic market.Licence) error {
		r, err := lockRoomAny(ctx, tx, roomID)
		if err != nil {
			return err
		}
		won, err := scanPrize(tx.QueryRow(ctx, `UPDATE room_prizes AS p SET status = 'awarded', contribution_id = $2, winner_workspace_id = $3,
				use_id = $4, licence_id = $5, awarded_at = now()
			WHERE p.id = $1 AND p.status = 'open' AND p.deadline > now() RETURNING `+prizeCols, p.ID, contributionID, author, lic.UseID, lic.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the prize is no longer open: it was awarded or closed meanwhile", ErrConflict)
		}
		if err != nil {
			return err
		}
		m, err := postPrizeMessage(ctx, tx, r, ws, user, fmt.Sprintf("awarded the prize “%s” — %s — to the contribution “%s”, "+
			"bought on the room's budget", won.Title, usd(won.AmountUSDMicros), title), true, won, "awarded")
		won.Message = &m
		out.Prize = won
		return err
	})
	if err != nil {
		return Award{}, prizeErr("award", err)
	}
	out.Licence = lic
	return out, nil
}
