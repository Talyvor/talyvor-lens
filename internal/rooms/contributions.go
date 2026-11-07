package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/market"
)

// contributions.go — B32.31: a room's members propose work, fork each other's with lineage, and vote (migration 0213).
//
// A contribution is a marketplace listing published with visibility room (market.PublishOnce, so the publish scan
// and the review apply as to any listing): its owner and the room's live members see it and open its artifact, anyone
// else gets 404, and the catalog never lists it. Its price is the room's default_price_usd_micros unless the
// contribution names one. Each contribution posts a contribution message to the room, which every member's event
// stream receives. A member contributes under the room's current terms: one that has not accepted a new terms version
// is asked to first.
//
// Forking a contribution publishes the forker's own contribution built on it — its artifact as it is, or as the
// forker edited it — and the family tree records a room_fork edge at the room's remix share (internal/market
// lineage.go). Each member that is not a viewer votes +1 or -1 on a contribution; its next vote replaces it, and the
// tally is the sum of the members' latest votes. The room's owner or an editor accepts or rejects a contribution.

// Contribution statuses, as migration 0213 checks them.
const (
	ContributionProposed = "proposed"
	ContributionAccepted = "accepted"
	ContributionRejected = "rejected"
)

// maxContributions bounds GET /v1/rooms/{id}/contributions.
const maxContributions = 200

// ContributionDraft is POST /v1/rooms/{id}/contributions, and what a fork changes of the contribution it forks.
type ContributionDraft struct {
	Kind           string             `json:"kind"`
	Title          string             `json:"title"`
	Description    string             `json:"description"`
	Artifact       json.RawMessage    `json:"artifact"`
	Changelog      string             `json:"changelog"`
	PriceUSDMicros *int64             `json:"price_usd_micros"` // per use; unset: the room's default_price_usd_micros
	Parents        []market.ParentRef `json:"parents"`
}

// Contribution is a listing version proposed to a room, with the members' votes on it.
type Contribution struct {
	ID                   string          `json:"id"`
	RoomID               string          `json:"room_id"`
	ListingID            string          `json:"listing_id"`
	Version              int             `json:"version"`
	Kind                 string          `json:"kind"`
	Title                string          `json:"title"`
	AuthorWorkspaceID    string          `json:"author_workspace_id"`
	AuthorWorkspaceName  string          `json:"author_workspace_name"`
	ForkedFrom           string          `json:"forked_from,omitempty"` // the contribution this one forked
	Status               string          `json:"status"`
	DecidedByWorkspaceID string          `json:"decided_by_workspace_id,omitempty"`
	DecidedAt            *time.Time      `json:"decided_at,omitempty"`
	MessageID            string          `json:"message_id"`
	Size                 string          `json:"size,omitempty"` // B41.142: the size claimed on the room's rate card
	Points               int64           `json:"points"`         // that size's points, fixed when it was claimed
	Tally                int             `json:"tally"`          // the sum of the members' latest votes
	Up                   int             `json:"up"`
	Down                 int             `json:"down"`
	MyVote               int             `json:"my_vote"` // the caller's: 1, -1, or 0 for none
	CreatedAt            time.Time       `json:"created_at"`
	Listing              *market.Listing `json:"listing,omitempty"` // one contribution read alone: its listing, artifacts open to members
}

// SetMarket gives the store the marketplace it publishes contributions through.
func (s *Store) SetMarket(m *market.Store) { s.market = m }

// contributor reads the room and ws's membership, under the room's row lock, as one that may contribute: a live member
// that is not a viewer, under the room's current terms, in a room that is not closed. It answers the current terms.
func contributor(ctx context.Context, tx pgx.Tx, ws, roomID string) (Room, Terms, error) {
	var t Terms
	r, err := lockRoom(ctx, tx, ws, roomID)
	if err != nil {
		return r, t, err
	}
	me, ok, err := member(ctx, tx, roomID, ws)
	switch {
	case err != nil:
		return r, t, err
	case !ok:
		return r, t, forbidden("join the room to contribute to it")
	case me.Role == RoleViewer:
		return r, t, forbidden("a viewer reads the room and does not contribute to it")
	case me.MutedAt != nil:
		return r, t, forbidden("the room's owner or an editor has muted you: you read the room and post nothing in it")
	case shut(r) != nil:
		return r, t, shut(r)
	case !me.TermsCurrent:
		return r, t, fmt.Errorf("%w: the room's terms are at version %d and you accepted version %d: read them and accept them with "+
			"POST /v1/rooms/%s/join {\"terms_version\": %d} before you contribute", ErrConflict, r.TermsVersion, me.TermsVersion, roomID, r.TermsVersion)
	}
	err = tx.QueryRow(ctx, `SELECT version, split_rule, remix_share_bps, default_price_usd_micros, spend_policy, created_at
		FROM room_terms WHERE room_id = $1 AND version = $2`, roomID, r.TermsVersion).Scan(&t.Version, &t.SplitRule, &t.RemixShareBPS,
		&t.DefaultPriceUSDMicros, &t.SpendPolicy, &t.CreatedAt)
	return r, t, err
}

// Contribute publishes ws's contribution to the room, by user, under the Idempotency-Key key ("" for none): a contribution
// sent again with its key answers the one it made, with created false.
func (s *Store) Contribute(ctx context.Context, ws, user, roomID, key string, d ContributionDraft) (Contribution, bool, error) {
	return s.contribute(ctx, ws, user, roomID, key, d, "")
}

// Fork publishes ws's fork of one of the room's contributions as its own contribution: d's artifact, or the forked
// version's as it is, of the same kind, with d's title and description or the original's. The fork's lineage carries a
// room_fork edge to the version forked, at the room's remix share.
func (s *Store) Fork(ctx context.Context, ws, user, roomID, contributionID, key string, d ContributionDraft) (Contribution, bool, error) {
	src, err := s.ReadContribution(ctx, ws, roomID, contributionID)
	if err != nil {
		return Contribution{}, false, err
	}
	l := src.Listing
	if d.Kind != "" && d.Kind != l.Kind {
		return Contribution{}, false, invalid("a fork of a %s is a %s", l.Kind, l.Kind)
	}
	d.Kind = l.Kind
	if strings.TrimSpace(d.Title) == "" {
		d.Title = l.Title
	}
	if d.Description == "" {
		d.Description = l.Description
	}
	if len(d.Artifact) == 0 {
		for _, v := range l.Versions {
			if v.Version == src.Version {
				d.Artifact = v.Artifact
			}
		}
	}
	for _, p := range d.Parents {
		if p.ListingID == src.ListingID {
			return Contribution{}, false, invalid("a fork builds on the version it forks; do not declare it as a parent too")
		}
	}
	d.Parents = append([]market.ParentRef{{ListingID: src.ListingID, Version: src.Version}}, d.Parents...)
	return s.contribute(ctx, ws, user, roomID, key, d, src.ID)
}

func (s *Store) contribute(ctx context.Context, ws, user, roomID, key string, d ContributionDraft, forkedFrom string) (Contribution, bool, error) {
	if s.market == nil {
		return Contribution{}, false, errors.New("rooms: contributions are not wired to the marketplace")
	}
	if d.PriceUSDMicros != nil && *d.PriceUSDMicros < 0 {
		return Contribution{}, false, invalid("price_usd_micros cannot be negative (0 is free)")
	}
	// Asked before publishing, so a workspace that may not contribute publishes nothing; asked again, under the room's
	// lock, before the contribution is recorded.
	var terms Terms
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) (err error) {
		_, terms, err = contributor(ctx, tx, ws, roomID)
		return err
	})
	if err != nil {
		return Contribution{}, false, contributionErr("contribute", err)
	}
	price := terms.DefaultPriceUSDMicros
	if d.PriceUSDMicros != nil {
		price = *d.PriceUSDMicros
	}
	draft := market.Draft{Kind: d.Kind, Title: d.Title, Description: d.Description, Artifact: d.Artifact, Changelog: d.Changelog,
		Parents: d.Parents, RoomID: roomID}
	if price > 0 {
		draft.Offers = []market.Offer{{Kind: market.OfferPerUse, Licence: market.LicenceCommercial, PriceUSDMicros: price}}
	}
	l, again, err := s.market.PublishOnce(ctx, ws, key, draft)
	if err != nil {
		return Contribution{}, false, err
	}
	if l.RoomID != roomID {
		return Contribution{}, false, fmt.Errorf("%w: that Idempotency-Key already published listing %s, which is not a contribution to this room",
			ErrConflict, l.ID)
	}
	var out Contribution
	created := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, _, err := contributor(ctx, tx, ws, roomID)
		if err != nil {
			return err
		}
		if again {
			var id string
			err := tx.QueryRow(ctx, `SELECT id FROM room_contributions WHERE listing_id = $1 AND version = 1`, l.ID).Scan(&id)
			if err == nil {
				out, err = readContribution(ctx, tx, ws, roomID, id)
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			// The listing was published and its contribution never recorded: record it now.
		}
		id := "rcon_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		msgID := "rmsg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		name := fmt.Sprintf("%s “%s”", article(l.Kind), l.Title)
		if l.ReviewStatus != market.ReviewApproved {
			name = article(l.Kind) + ", held for review" // its title is its author's alone until an admin approves it
		}
		body := "proposed " + name
		refs := map[string]any{"contribution_id": id, "listing_id": l.ID, "version": 1}
		if forkedFrom != "" {
			body = "forked a contribution as " + name
			refs["forked_from"] = forkedFrom
		}
		refsJSON, _ := json.Marshal(refs)
		var scan []byte
		if r.Visibility == Public {
			if scan, err = scanPublic(body); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_messages (id, room_id, author_workspace_id, author_user_id, author_agent_id, kind, body, refs, scan)
			VALUES ($1, $2, $3, $4, $5, 'contribution', $6, $7, $8)`, msgID, roomID, ws, user, agentOf(ctx), body, string(refsJSON), jsonText(scan)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_contributions (id, room_id, listing_id, version, author_workspace_id, author_user_id, forked_from, message_id)
			VALUES ($1, $2, $3, 1, $4, $5, NULLIF($6, ''), $7)`, id, roomID, l.ID, ws, user, forkedFrom, msgID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, roomID, EventPosted, msgID); err != nil {
			return err
		}
		if err := touch(ctx, tx, roomID); err != nil {
			return err
		}
		created = true
		out, err = readContribution(ctx, tx, ws, roomID, id)
		return err
	})
	if err != nil {
		return Contribution{}, false, contributionErr("contribute", err)
	}
	return out, created, nil
}

// jsonText sends JSON as text, or NULL for none: behind PgBouncer a []byte goes out as bytea, which jsonb refuses.
func jsonText(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// article is "a prompt", "an agent", "an evaluation".
func article(kind string) string {
	if strings.ContainsAny(kind[:1], "aeiou") {
		return "an " + kind
	}
	return "a " + kind
}

func contributionErr(op string, err error) error {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrConflict) {
		return err
	}
	var refusal *ScanRefusal
	if errors.As(err, &refusal) {
		return err
	}
	return fmt.Errorf("rooms: %s: %w", op, err)
}

const contributionCols = `c.id, c.room_id, c.listing_id, c.version, l.kind, l.title, c.author_workspace_id, coalesce(c.forked_from, ''),
	c.status, c.decided_by_workspace_id, c.decided_at, c.message_id, c.size, c.points, c.created_at,
	coalesce((SELECT sum(v.value) FROM room_votes v WHERE v.contribution_id = c.id), 0),
	(SELECT count(*) FROM room_votes v WHERE v.contribution_id = c.id AND v.value = 1),
	(SELECT count(*) FROM room_votes v WHERE v.contribution_id = c.id AND v.value = -1),
	coalesce((SELECT v.value FROM room_votes v WHERE v.contribution_id = c.id AND v.workspace_id = $1), 0), ` +
	workspaceNameOf + `c.author_workspace_id` + workspaceNameEnd

func scanContribution(row pgx.Row) (Contribution, error) {
	var c Contribution
	err := row.Scan(&c.ID, &c.RoomID, &c.ListingID, &c.Version, &c.Kind, &c.Title, &c.AuthorWorkspaceID, &c.ForkedFrom,
		&c.Status, &c.DecidedByWorkspaceID, &c.DecidedAt, &c.MessageID, &c.Size, &c.Points, &c.CreatedAt, &c.Tally, &c.Up, &c.Down, &c.MyVote, &c.AuthorWorkspaceName)
	return c, err
}

// shownTo keeps a contribution the review holds, or took down, to its author ($1 is the viewer), as its listing is.
const shownTo = `(l.review_status = 'approved' OR l.workspace_id = $1)`

// readContribution reads one of the room's contributions with viewer's vote on it.
func readContribution(ctx context.Context, tx pgx.Tx, viewer, roomID, id string) (Contribution, error) {
	c, err := scanContribution(tx.QueryRow(ctx, `SELECT `+contributionCols+` FROM room_contributions c
		JOIN market_listings l ON l.id = c.listing_id WHERE c.id = $2 AND c.room_id = $3 AND `+shownTo, viewer, id, roomID))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, fmt.Errorf("%w: no such contribution in this room", ErrNotFound)
	}
	return c, err
}

// seesContributions answers ErrNotFound unless the room is one viewer may read, and ErrForbidden unless viewer is its
// live member: a room's contributions are its members' alone, like the listings they are.
func seesContributions(ctx context.Context, tx pgx.Tx, viewer, roomID string) error {
	if err := readable(ctx, tx, viewer, roomID); err != nil {
		return err
	}
	if _, ok, err := member(ctx, tx, roomID, viewer); err != nil {
		return err
	} else if !ok {
		return forbidden("a room's contributions are shown to its members: join the room to see them")
	}
	return nil
}

// Contributions is the room's contributions as viewer, a live member, reads them, newest first.
func (s *Store) Contributions(ctx context.Context, viewer, roomID string) ([]Contribution, error) {
	out := []Contribution{}
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		if err := seesContributions(ctx, tx, viewer, roomID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+contributionCols+` FROM room_contributions c
			JOIN market_listings l ON l.id = c.listing_id WHERE c.room_id = $2 AND `+shownTo+` ORDER BY c.created_at DESC, c.id LIMIT $3`,
			viewer, roomID, maxContributions)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanContribution(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, contributionErr("contributions", err)
}

// ReadContribution is one of the room's contributions as viewer, a live member, reads it, with its listing and the
// artifact of each of its versions. A contribution the review holds is its author's alone until an admin approves it.
func (s *Store) ReadContribution(ctx context.Context, viewer, roomID, id string) (Contribution, error) {
	var c Contribution
	err := readTx(ctx, s, func(tx pgx.Tx) (err error) {
		if err = seesContributions(ctx, tx, viewer, roomID); err != nil {
			return err
		}
		c, err = readContribution(ctx, tx, viewer, roomID, id)
		return err
	})
	if err != nil {
		return Contribution{}, contributionErr("contribution", err)
	}
	if s.market == nil {
		return Contribution{}, errors.New("rooms: contributions are not wired to the marketplace")
	}
	l, err := s.market.Get(ctx, viewer, c.ListingID)
	if errors.Is(err, market.ErrNotFound) {
		return Contribution{}, fmt.Errorf("%w: no such contribution in this room", ErrNotFound)
	}
	if err != nil {
		return Contribution{}, err
	}
	c.Listing = &l
	return c, nil
}

// Vote records ws's vote on one of the room's contributions, +1 or -1, replacing any it gave before. ws must be a live
// member that is not a viewer, and the room not closed.
func (s *Store) Vote(ctx context.Context, ws, roomID, id string, value int) (Contribution, error) {
	if value != 1 && value != -1 {
		return Contribution{}, invalid("a vote is 1 or -1")
	}
	var out Contribution
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoom(ctx, tx, ws, roomID)
		if err != nil {
			return err
		}
		me, ok, err := member(ctx, tx, roomID, ws)
		switch {
		case err != nil:
			return err
		case !ok:
			return forbidden("join the room to vote in it")
		case me.Role == RoleViewer:
			return forbidden("a viewer reads the room and does not vote in it")
		case shut(r) != nil:
			return shut(r)
		}
		if _, err := readContribution(ctx, tx, ws, roomID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_votes (contribution_id, workspace_id, value) VALUES ($1, $2, $3)
			ON CONFLICT (contribution_id, workspace_id) DO UPDATE SET value = EXCLUDED.value, voted_at = now()`, id, ws, value); err != nil {
			return err
		}
		out, err = readContribution(ctx, tx, ws, roomID, id)
		return err
	})
	return out, contributionErr("vote", err)
}

// Decide accepts or rejects one of the room's contributions, by its owner or an editor, and tells the room.
func (s *Store) Decide(ctx context.Context, ws, user, roomID, id, status string) (Contribution, error) {
	if status != ContributionAccepted && status != ContributionRejected {
		return Contribution{}, invalid("status must be accepted or rejected")
	}
	var out Contribution
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoom(ctx, tx, ws, roomID)
		if err != nil {
			return err
		}
		me, ok, err := member(ctx, tx, roomID, ws)
		switch {
		case err != nil:
			return err
		case !ok || (me.Role != RoleOwner && me.Role != RoleEditor):
			return forbidden("only the room's owner or an editor accepts or rejects a contribution")
		case shut(r) != nil:
			return shut(r)
		}
		c, err := readContribution(ctx, tx, ws, roomID, id)
		if err != nil {
			return err
		}
		if c.Status == status {
			out = c
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE room_contributions SET status = $2, decided_by_workspace_id = $3, decided_at = now() WHERE id = $1`,
			id, status, ws); err != nil {
			return err
		}
		msgID := "rmsg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		body := fmt.Sprintf("%s %s “%s”", status, article(c.Kind), c.Title)
		refs, _ := json.Marshal(map[string]any{"contribution_id": id, "listing_id": c.ListingID, "status": status})
		var scan []byte
		if r.Visibility == Public {
			if scan, err = scanPublic(body); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_messages (id, room_id, author_workspace_id, author_user_id, kind, body, refs, scan)
			VALUES ($1, $2, $3, $4, 'system', $5, $6, $7)`, msgID, roomID, ws, user, body, string(refs), jsonText(scan)); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, roomID, EventPosted, msgID); err != nil {
			return err
		}
		if err := touch(ctx, tx, roomID); err != nil {
			return err
		}
		out, err = readContribution(ctx, tx, ws, roomID, id)
		return err
	})
	return out, contributionErr("decide", err)
}
