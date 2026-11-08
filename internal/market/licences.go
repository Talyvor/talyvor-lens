package market

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// licences.go — B32.19: BUYING, RENTING OR SUBSCRIBING COVERS THE BUYER'S USES, WITH VERSION PINNING.
//
// A buyer licenses a listing through one of its buy, rent or subscribe offers (offers.go): one billed market_uses row
// at the offer's price goes on the buyer's monthly marketplace bill — cleared, earned and refunded like any use — and
// the licence is recorded beside it (migration 0202). While the licence is active, each use it covers is charged
// 'licensed' at 0 and never metered; past a rent's or a subscription's included uses, the listing's per_use offer
// bills. Who a licence covers is LicenceTerms: personal, the one person who bought it and never an agent key;
// commercial, the workspace's people and agents; enterprise, the same with its people counted against its seats. A
// licence runs its pinned version (none: the latest) unless a use names one it may use — any up to the pinned.

// Charges a licence makes.
const (
	ChargeLicensed = "licensed" // a licence covered the use: nothing more to pay
	ChargeTrial    = "trial"    // one of a per_use offer's trial uses (B32.21)
)

// Licence statuses. Only a licence's status, ends_at and auto_renew ever change.
const (
	LicenceActive    = "active"
	LicenceExpired   = "expired"
	LicenceCancelled = "cancelled"
	LicenceRefunded  = "refunded"
	LicenceUnpaid    = "unpaid"
)

var (
	// ErrKeyReused: an Idempotency-Key already licensed something else.
	ErrKeyReused = errors.New("market: that Idempotency-Key already licensed a different offer")
	// errLicenceUsedUp: the licence a use counted on was used up, or ended, while the use was being recorded.
	errLicenceUsedUp = errors.New("market: the licence no longer covers this use")
)

// Capabilities asks a wallet capability whether money may be taken on a buyer's Stripe bill (economy, B22.1).
type Capabilities interface {
	RequireBilledCapability(ctx context.Context, workspaceID, key string) error
}

// LicenceDeps are what licensing needs besides the catalog.
type LicenceDeps struct {
	Meter        Meter        // nil: no licence that costs anything can be bought
	Agents       AgentJudge   // nil: no agent keys
	Capabilities Capabilities // asked where a billed licence's charge is recorded
}

// LicenceRequest is what a buyer asks to license: an offer, and the version to pin (0: follow the latest).
type LicenceRequest struct {
	OfferID   string `json:"offer_id"`
	Version   int    `json:"version"`
	AutoRenew *bool  `json:"auto_renew,omitempty"` // B32.20 — null: a subscription renews, a rent does not; a buy never ends
	Person    string `json:"-"`                    // who is buying: whom a personal licence covers
}

// Licence is one licence a buyer holds, and what bought it.
type Licence struct {
	ID                string     `json:"id"`
	ListingID         string     `json:"listing_id"`
	Title             string     `json:"title"`
	OfferID           string     `json:"offer_id,omitempty"`
	AgentID           string     `json:"agent_id,omitempty"`
	Licence           string     `json:"licence"`
	Terms             string     `json:"terms"`
	Kind              string     `json:"kind"`
	PinnedVersion     *int       `json:"pinned_version"` // null: it follows the latest
	StartsAt          time.Time  `json:"starts_at"`
	EndsAt            *time.Time `json:"ends_at"` // null: perpetual
	AutoRenew         bool       `json:"auto_renew"`
	Status            string     `json:"status"`
	Seats             *int       `json:"seats,omitempty"`
	IncludedUses      *int       `json:"included_uses,omitempty"` // a rent's or subscription's, from its offer: 0 is unlimited
	UsesCovered       int        `json:"uses_covered"`
	RentPaidUSDMicros int64      `json:"rent_paid_usd_micros"` // what its rents have cleared (B32.20)
	Source            string     `json:"source"`               // offer, or rent_to_own: its buyer's rents paid for it (B32.20)
	CreatedAt         time.Time  `json:"created_at"`
	LastUsedAt        *time.Time `json:"last_used_at"` // its buyer's last use of the listing; null: never (B41.176)
	Idle              bool       `json:"idle"`         // it renews and has gone unused for IdleFor: worth cancelling
	// The licence's purchase: one use on the buyer's bill.
	UseID      string `json:"use_id"`
	Charge     string `json:"charge"`
	PriceULXC  int64  `json:"price_ulxc"`
	MeterError string `json:"-"`
}

// License records buyerWorkspaceID's licence to listingID under req's offer (agentID when an agent's key asked), once
// per key: the same key again answers the licence it made, with again true. Its charge is one billed use at the
// offer's price — judged by the agent's rules, after the money wall and the single-party check — metered onto the
// buyer's bill.
func (s *Store) License(ctx context.Context, deps LicenceDeps, buyerWorkspaceID, agentID, listingID, key string, req LicenceRequest) (Licence, bool, error) {
	if key == "" || len(key) > 128 {
		return Licence{}, false, invalid("licensing takes an Idempotency-Key of 1 to 128 characters, so a retry never buys twice")
	}
	if lic, err := s.licensedWith(ctx, buyerWorkspaceID, key, req.OfferID); !errors.Is(err, ErrNotFound) {
		return lic, err == nil, err
	}
	l, _, version, err := s.resolve(ctx, buyerWorkspaceID, listingID, req.Version)
	if err != nil {
		return Licence{}, false, err
	}
	if l.WorkspaceID == buyerWorkspaceID {
		return Licence{}, false, invalid("your own listing needs no licence")
	}
	offers, err := activeOffers(ctx, s.pool, l.ID)
	if err != nil {
		return Licence{}, false, err
	}
	var o *Offer
	for i := range offers[l.ID] {
		if offers[l.ID][i].ID == req.OfferID {
			o = &offers[l.ID][i]
		}
	}
	switch {
	case o == nil:
		return Licence{}, false, fmt.Errorf("%w: the listing has no active offer %q", ErrNotFound, req.OfferID)
	case o.Kind == OfferPerUse:
		return Licence{}, false, invalid("a per_use offer is paid by each use: license a buy, rent or subscribe offer")
	case o.Licence == LicencePersonal && agentID != "":
		return Licence{}, false, invalid("a personal licence is for one person, never an agent key")
	case o.Licence == LicencePersonal && req.Person == "":
		return Licence{}, false, invalid("a personal licence is bought by the person it is for")
	case o.Kind == OfferBuy && req.AutoRenew != nil && *req.AutoRenew:
		return Licence{}, false, invalid("a bought licence never ends, so it has nothing to renew")
	}
	if o.Kind != OfferBuy {
		// B32.20: what the buyer owns needs no renting: its rents may have paid for it already.
		var owned bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM market_licences WHERE buyer_workspace_id = $1 AND listing_id = $2
				AND licence = $3 AND person_id = $4 AND kind = 'buy' AND status = 'active' AND ends_at IS NULL)`,
			buyerWorkspaceID, l.ID, o.Licence, personFor(o.Licence, agentID, req.Person)).Scan(&owned); err != nil {
			return Licence{}, false, fmt.Errorf("market: licences: %w", err)
		}
		if owned {
			return Licence{}, false, invalid("you already own this listing under a %s licence, so it needs no %s", o.Licence, o.Kind)
		}
	}

	lic := Licence{ID: "lic_" + uuid.NewString(), ListingID: l.ID, Title: l.Title, OfferID: o.ID, AgentID: agentID, Licence: o.Licence,
		Terms: LicenceTerms[o.Licence], Kind: o.Kind, Status: LicenceActive, AutoRenew: o.Kind == OfferSubscribe, Seats: o.Seats,
		IncludedUses: o.IncludedUses, Source: SourceOffer, UseID: "use_" + uuid.NewString(), Charge: ChargeBilled, PriceULXC: o.PriceUSDMicros * ulxcPerUSDMicro}
	if req.Version != 0 {
		lic.PinnedVersion = &version
	}
	if req.AutoRenew != nil {
		lic.AutoRenew = *req.AutoRenew
	}
	if lic.PriceULXC == 0 {
		lic.Charge = ChargeFree
	} else {
		if err := workspace.CheckMoneyWall(ctx, s.pool, buyerWorkspaceID, l.WorkspaceID); err != nil {
			return Licence{}, false, err
		}
		linked, err := s.linked(ctx, l.WorkspaceID, buyerWorkspaceID)
		if err != nil {
			return Licence{}, false, err
		}
		if linked {
			lic.Charge, lic.PriceULXC = ChargeLinked, 0
		}
	}
	if lic.Charge == ChargeBilled {
		if deps.Meter == nil {
			return Licence{}, false, ErrNoBill
		}
		if deps.Capabilities == nil {
			return Licence{}, false, errors.New("market: a licence's charge cannot be recorded here: no wallet capability answers for it")
		}
		capability := economy.CapabilityRentAndSubscribe
		if o.Kind == OfferBuy {
			capability = economy.CapabilityBuyListings
		}
		if err := deps.Capabilities.RequireBilledCapability(ctx, buyerWorkspaceID, capability); err != nil {
			return Licence{}, false, err
		}
		if err := s.sellable(ctx, buyerWorkspaceID); err != nil { // B32.39
			return Licence{}, false, err
		}
	}

	record := func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO market_licences (id, listing_id, offer_id, buyer_workspace_id, agent_id, person_id, licence, kind,
				pinned_version, ends_at, auto_renew, seats, rent_paid_usd_micros, idempotency_key,
				starts_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $15::timestamptz + make_interval(days => $10::int), $11, $12, $13, $14, $15)
			RETURNING starts_at, ends_at, created_at`,
			lic.ID, l.ID, o.ID, buyerWorkspaceID, agentID, personFor(o.Licence, agentID, req.Person), o.Licence, o.Kind, lic.PinnedVersion,
			o.PeriodDays, lic.AutoRenew, o.Seats, lic.RentPaidUSDMicros, key, s.now()).Scan(&lic.StartsAt, &lic.EndsAt, &lic.CreatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, person_id,
				price_ulxc, charge, use_kind, licence_id, used_at, ran_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
			lic.UseID, l.ID, version, l.WorkspaceID, buyerWorkspaceID, agentID, req.Person, lic.PriceULXC, lic.Charge, o.Kind, lic.ID, lic.StartsAt)
		return err
	}
	if agentID != "" && deps.Agents != nil {
		what := fmt.Sprintf("licence:%s:%s:%s:%d", agentID, l.ID, o.ID, lic.PriceULXC)
		// B32.22: what the licence commits the agent to is what it charges — nothing when it is free or linked.
		c := economy.Commitment{Kind: o.Kind, Licence: o.Licence, Renews: lic.AutoRenew, ULXC: lic.PriceULXC}
		err = deps.Agents.JudgeAgentPurchase(ctx, buyerWorkspaceID, agentID, l.ID, lic.PriceULXC, c, what, record)
	} else {
		err = pgx.BeginFunc(ctx, s.pool, record)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "idx_market_licences_key" {
		// The same key licensed concurrently and won the insert: answer its licence.
		lic, err := s.licensedWith(ctx, buyerWorkspaceID, key, req.OfferID)
		return lic, err == nil, err
	}
	if err != nil {
		return Licence{}, false, err
	}
	if lic.Charge == ChargeBilled {
		if err := s.meter(ctx, deps.Meter, lic.UseID, buyerWorkspaceID, lic.PriceULXC, lic.StartsAt); err != nil {
			lic.MeterError = err.Error() // the licence is the buyer's; MeterPending bills it on its next pass
		}
	}
	return lic, false, nil
}

// personFor is the person a licence is recorded for: a personal licence's buyer. The others cover the workspace.
func personFor(licence, agentID, person string) string {
	if licence == LicencePersonal && agentID == "" {
		return person
	}
	return ""
}

// periodStart is when a licence's current period began: its last renewal's, or before any of its uses (B32.20). A
// rent's or a subscription's included uses are counted from it.
const periodStart = `COALESCE((SELECT max(r.used_at) FROM market_uses r WHERE r.licence_id = c.id AND r.use_kind = 'renewal'), '-infinity')`

const licenceColumns = `c.id, c.listing_id, COALESCE(l.title, ''), COALESCE(c.offer_id, ''), c.agent_id, c.licence, c.kind, c.pinned_version,
	c.starts_at, c.ends_at, c.auto_renew, c.status, c.seats, o.included_uses, c.rent_paid_usd_micros, c.source, c.created_at,
	(SELECT count(*) FROM market_uses u WHERE u.licence_id = c.id AND u.charge = 'licensed' AND u.used_at >= ` + periodStart + `),
	COALESCE(p.id, ''), COALESCE(p.charge, ''), COALESCE(p.price_ulxc, 0), ` + lastUse + `
	FROM market_licences c LEFT JOIN market_listings l ON l.id = c.listing_id LEFT JOIN market_offers o ON o.id = c.offer_id
	LEFT JOIN LATERAL (SELECT id, charge, price_ulxc FROM market_uses u WHERE u.licence_id = c.id AND u.use_kind <> 'use'
	                   ORDER BY u.used_at, u.id LIMIT 1) p ON true`

func scanLicence(r pgx.Row) (Licence, error) {
	var x Licence
	err := r.Scan(&x.ID, &x.ListingID, &x.Title, &x.OfferID, &x.AgentID, &x.Licence, &x.Kind, &x.PinnedVersion, &x.StartsAt, &x.EndsAt,
		&x.AutoRenew, &x.Status, &x.Seats, &x.IncludedUses, &x.RentPaidUSDMicros, &x.Source, &x.CreatedAt, &x.UsesCovered, &x.UseID, &x.Charge, &x.PriceULXC, &x.LastUsedAt)
	x.Terms = LicenceTerms[x.Licence]
	return x, err
}

// licensedWith reads the licence buyer bought with key, or ErrNotFound; ErrKeyReused when it was another offer's.
func (s *Store) licensedWith(ctx context.Context, buyer, key, offerID string) (Licence, error) {
	lic, err := scanLicence(s.pool.QueryRow(ctx, `SELECT `+licenceColumns+` WHERE c.buyer_workspace_id = $1 AND c.idempotency_key = $2`, buyer, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Licence{}, ErrNotFound
	}
	if err != nil {
		return Licence{}, fmt.Errorf("market: licence: %w", err)
	}
	if lic.OfferID != offerID {
		return Licence{}, ErrKeyReused
	}
	return lic, nil
}

// expireLicences marks buyer's licences past their end at now expired — but one that renews, which the schedules' tick
// renews or ends (B32.20).
func expireLicences(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, buyer string, now time.Time) error {
	if _, err := q.Exec(ctx, `UPDATE market_licences SET status = 'expired'
		WHERE buyer_workspace_id = $1 AND status = 'active' AND ends_at <= $2 AND NOT auto_renew`, buyer, now); err != nil {
		return fmt.Errorf("market: expire licences: %w", err)
	}
	return nil
}

// Licences lists the licences buyerWorkspaceID holds or held, newest first.
func (s *Store) Licences(ctx context.Context, buyerWorkspaceID string) ([]Licence, error) {
	if err := expireLicences(ctx, s.pool, buyerWorkspaceID, s.now()); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+licenceColumns+` WHERE c.buyer_workspace_id = $1 ORDER BY c.created_at DESC, c.id LIMIT 500`, buyerWorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("market: licences: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Licence, error) { return scanLicence(r) })
	if err != nil {
		return nil, fmt.Errorf("market: licences: %w", err)
	}
	for i := range out {
		out[i].Idle = idle(out[i], s.now())
	}
	return out, nil
}

// cover is the licence a use runs under: its pinned version, and whether it still covers the use's charge (false
// once a rent's or subscription's included uses are spent: the per_use offer bills).
type cover struct {
	id      string
	pinned  *int
	charged bool
}

// licenceFor finds the active licence of buyer to listingID that covers a use by agentID (or person) of version (0:
// whichever the licence runs), preferring one that still covers its charge, then the caller's own, then the newest.
// only, when set, asks about that one licence. nil: no licence covers it at now. An unpaid one runs to its ends_at
// (B32.20).
func licenceFor(ctx context.Context, tx pgx.Tx, now time.Time, buyer, agentID, person, listingID string, version int, only string) (*cover, error) {
	if err := expireLicences(ctx, tx, buyer, now); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id, c.agent_id, c.person_id, c.licence, c.pinned_version, COALESCE(c.seats, 0), COALESCE(o.included_uses, 0),
		       (SELECT count(*) FROM market_uses u WHERE u.licence_id = c.id AND u.charge = 'licensed' AND u.used_at >= `+periodStart+`),
		       (SELECT count(DISTINCT u.person_id) FROM market_uses u WHERE u.licence_id = c.id AND u.charge = 'licensed' AND u.agent_id = '' AND u.person_id <> ''),
		       EXISTS (SELECT 1 FROM market_uses u WHERE u.licence_id = c.id AND u.charge = 'licensed' AND u.agent_id = '' AND u.person_id = $3 AND $3 <> '')
		FROM market_licences c LEFT JOIN market_offers o ON o.id = c.offer_id
		WHERE c.buyer_workspace_id = $1 AND c.listing_id = $2 AND c.starts_at <= $5
		  AND (c.status = 'active' OR (c.status = 'unpaid' AND c.ends_at IS NOT NULL)) AND (c.ends_at IS NULL OR c.ends_at > $5)
		  AND ($4 = '' OR c.id = $4)
		ORDER BY c.created_at DESC, c.id`, buyer, listingID, person, only, now)
	if err != nil {
		return nil, fmt.Errorf("market: licences: %w", err)
	}
	defer rows.Close()
	var best *cover
	bestRank := -1
	for rows.Next() {
		var id, holderAgent, holderPerson, licence string
		var pinned *int
		var seats, included, used, people int
		var counted bool
		if err := rows.Scan(&id, &holderAgent, &holderPerson, &licence, &pinned, &seats, &included, &used, &people, &counted); err != nil {
			return nil, err
		}
		var own bool
		switch {
		case agentID != "": // an agent key: commercial or enterprise, never personal
			if licence == LicencePersonal {
				continue
			}
			own = holderAgent == agentID
		case licence == LicencePersonal:
			if person == "" || holderPerson != person {
				continue
			}
			own = true
		case licence == LicenceEnterprise:
			if person == "" || (!counted && people >= seats) {
				continue
			}
		}
		if version != 0 && pinned != nil && version > *pinned {
			continue // a version past the pinned one is not this licence's to run
		}
		c := &cover{id: id, pinned: pinned, charged: included == 0 || used < included}
		rank := 0
		if c.charged {
			rank += 2
		}
		if own {
			rank++
		}
		if rank > bestRank {
			best, bestRank = c, rank
		}
	}
	return best, rows.Err()
}

// coverUse finds the licence covering a use, outside any transaction.
func (s *Store) coverUse(ctx context.Context, buyer, agentID, person, listingID string, version int) (*cover, error) {
	var c *cover
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		c, err = licenceFor(ctx, tx, s.now(), buyer, agentID, person, listingID, version, "")
		return err
	})
	return c, err
}

// claimLicence makes sure, in tx and under the licence's lock, that licenceID still covers the use's charge: a use
// racing the last included one, or the licence's end, is told errLicenceUsedUp and billed instead.
func claimLicence(ctx context.Context, tx pgx.Tx, now time.Time, licenceID, buyer, agentID, person, listingID string, version int) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM market_licences WHERE id = $1 FOR UPDATE`, licenceID); err != nil {
		return err
	}
	c, err := licenceFor(ctx, tx, now, buyer, agentID, person, listingID, version, licenceID)
	if err != nil {
		return err
	}
	if c == nil || !c.charged {
		return errLicenceUsedUp
	}
	return nil
}
