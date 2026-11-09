package market

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// offers.go — B32.18: A LISTING IS SOLD THROUGH ITS OFFERS.
//
// An offer is one way to buy a listing — pay per use, buy it, rent it or subscribe to it — under one licence, at a
// price in µUSD (migration 0201). A listing has at most one active offer per kind and licence; replacing its offers
// ends the old ones and adds the new, so the next charge uses the new price and every past use keeps its own. A use
// is billed at the listing's per_use commercial offer; a listing with no offers is free. Subscriptions, volume tiers
// and coupons, when they come, are offers too — never a second price table.

// Offer kinds.
const (
	OfferPerUse    = "per_use"
	OfferBuy       = "buy"
	OfferRent      = "rent"
	OfferSubscribe = "subscribe"
)

// offerVerbs is how an approval for a licence names taking it (B55.31).
var offerVerbs = map[string]string{OfferBuy: "Buy", OfferRent: "Rent", OfferSubscribe: "Subscribe to"}

// Licences. What each allows is LicenceTerms; it is defined here and nowhere else.
const (
	LicencePersonal   = "personal"
	LicenceCommercial = "commercial"
	LicenceEnterprise = "enterprise"
)

// LicenceTerms says what each licence allows.
var LicenceTerms = map[string]string{
	LicencePersonal:   "One person. No agent keys, and not inside a product sold to others.",
	LicenceCommercial: "The buying workspace's people and agents, inside its own products.",
	LicenceEnterprise: "Commercial, for up to the offer's seats of people and all of the workspace's agents.",
}

// MaxRentDays is the longest rent; a subscription runs for 30 or 365 days.
const MaxRentDays = 365

// ErrNotSoldPerUse: the listing is sold only by licence (buy, rent or subscribe), and no per_use commercial offer
// prices a use of it.
var ErrNotSoldPerUse = errors.New("market: this listing is not sold per use under a commercial licence")

// Offer is one way to buy a listing. PeriodDays, IncludedUses and Seats are present only where they apply.
type Offer struct {
	ID             string    `json:"id,omitempty"`
	Kind           string    `json:"kind"`
	Licence        string    `json:"licence"`
	PriceUSDMicros int64     `json:"price_usd_micros"`
	PeriodDays     *int      `json:"period_days,omitempty"`   // rent: 1 to 365; subscribe: 30 or 365
	IncludedUses   *int      `json:"included_uses,omitempty"` // rent and subscribe: 0 is unlimited
	Seats          *int      `json:"seats,omitempty"`         // enterprise: at least 1
	TrialUses      int       `json:"trial_uses,omitempty"`    // per_use (B32.21)
	Terms          string    `json:"terms,omitempty"`         // what the licence allows, as LicenceTerms says
	Display        *Display  `json:"display,omitempty"`       // the price in the buyer's currency, on a read (B32.51, display.go)
	CreatedAt      time.Time `json:"created_at,omitzero"`
}

// checkOffers validates a listing's whole set of offers: each well formed, one per kind and licence, and none giving
// more than trialMax trial uses.
func checkOffers(offers []Offer, trialMax int) error {
	seen := map[[2]string]bool{}
	for i := range offers {
		o := &offers[i]
		where := fmt.Sprintf("offer %d", i+1)
		switch o.Kind {
		case OfferPerUse, OfferBuy, OfferRent, OfferSubscribe:
		default:
			return invalid("%s: kind must be per_use, buy, rent or subscribe", where)
		}
		if _, ok := LicenceTerms[o.Licence]; !ok {
			return invalid("%s: licence must be personal, commercial or enterprise", where)
		}
		where = fmt.Sprintf("the %s %s offer", o.Licence, o.Kind)
		if seen[[2]string{o.Kind, o.Licence}] {
			return invalid("a listing has one active %s %s offer at a time", o.Licence, o.Kind)
		}
		seen[[2]string{o.Kind, o.Licence}] = true
		if o.PriceUSDMicros < 0 || o.PriceUSDMicros > math.MaxInt64/ulxcPerUSDMicro {
			return invalid("%s: the price must be from 0 (free) to %d µUSD", where, math.MaxInt64/ulxcPerUSDMicro)
		}
		switch o.Kind {
		case OfferRent:
			if o.PeriodDays == nil || *o.PeriodDays < 1 || *o.PeriodDays > MaxRentDays {
				return invalid("%s: a rent lasts 1 to %d days (period_days)", where, MaxRentDays)
			}
		case OfferSubscribe:
			if o.PeriodDays == nil || (*o.PeriodDays != 30 && *o.PeriodDays != 365) {
				return invalid("%s: a subscription renews every 30 or 365 days (period_days)", where)
			}
		default:
			if o.PeriodDays != nil {
				return invalid("%s: only a rent or a subscription has period_days", where)
			}
		}
		if o.Kind == OfferRent || o.Kind == OfferSubscribe {
			if o.IncludedUses == nil {
				o.IncludedUses = new(int) // unlimited
			} else if *o.IncludedUses < 0 {
				return invalid("%s: included_uses cannot be negative (0 is unlimited)", where)
			}
		} else if o.IncludedUses != nil {
			return invalid("%s: only a rent or a subscription has included_uses", where)
		}
		if o.Licence == LicenceEnterprise {
			if o.Seats == nil || *o.Seats < 1 {
				return invalid("%s: an enterprise licence names its seats, at least 1", where)
			}
		} else if o.Seats != nil {
			return invalid("%s: only an enterprise licence has seats", where)
		}
		if o.TrialUses < 0 || (o.TrialUses > 0 && o.Kind != OfferPerUse) {
			return invalid("%s: only a per_use offer gives trial uses, and never fewer than 0", where)
		}
		if o.TrialUses > trialMax {
			return invalid("%s: an offer gives at most %d trial uses", where, trialMax)
		}
	}
	return nil
}

// sameTerms says whether two offers sell the same thing at the same price.
func sameTerms(a, b Offer) bool {
	eq := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Kind == b.Kind && a.Licence == b.Licence && a.PriceUSDMicros == b.PriceUSDMicros && a.TrialUses == b.TrialUses &&
		eq(a.PeriodDays, b.PeriodDays) && eq(a.IncludedUses, b.IncludedUses) && eq(a.Seats, b.Seats)
}

// perUseULXC is what the per_use commercial offer among offers charges a use, in µLXC; ok false when there is none.
func perUseULXC(offers []Offer) (int64, bool) {
	for _, o := range offers {
		if o.Kind == OfferPerUse && o.Licence == LicenceCommercial {
			return o.PriceUSDMicros * ulxcPerUSDMicro, true
		}
	}
	return 0, false
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const offerColumns = `id, listing_id, kind, licence, price_usd_micros, period_days, included_uses, seats, trial_uses, created_at`

// activeOffers reads the active offers of each of listingIDs, keyed by listing, in a stable order.
func activeOffers(ctx context.Context, q querier, listingIDs ...string) (map[string][]Offer, error) {
	rows, err := q.Query(ctx, `SELECT `+offerColumns+` FROM market_offers WHERE listing_id = ANY($1) AND active
		ORDER BY listing_id, array_position(ARRAY['per_use', 'rent', 'subscribe', 'buy'], kind),
		         array_position(ARRAY['personal', 'commercial', 'enterprise'], licence)`, listingIDs)
	if err != nil {
		return nil, fmt.Errorf("market: offers: %w", err)
	}
	defer rows.Close()
	out := map[string][]Offer{}
	for rows.Next() {
		var o Offer
		var listing string
		if err := rows.Scan(&o.ID, &listing, &o.Kind, &o.Licence, &o.PriceUSDMicros, &o.PeriodDays, &o.IncludedUses, &o.Seats,
			&o.TrialUses, &o.CreatedAt); err != nil {
			return nil, err
		}
		o.Terms = LicenceTerms[o.Licence]
		out[listing] = append(out[listing], o)
	}
	return out, rows.Err()
}

// perUsePrice is what one billed use of listingID costs, in µLXC, read from its active per_use commercial offer.
// ErrNotSoldPerUse when it has offers but none of them prices a use; free (0) when it has none at all.
func perUsePrice(ctx context.Context, q querier, listingID string) (int64, error) {
	offers, err := activeOffers(ctx, q, listingID)
	if err != nil {
		return 0, err
	}
	if price, ok := perUseULXC(offers[listingID]); ok {
		return price, nil
	}
	if len(offers[listingID]) > 0 {
		return 0, ErrNotSoldPerUse
	}
	return 0, nil
}

// writeOffers makes offers the active set of listingID, in tx: an active offer on the same terms stays as it is
// (its id with it), every other active offer ends, and the rest are added. The listing's price_per_use_ulxc follows
// its per_use commercial offer (0 without one). offers must have passed checkOffers.
func writeOffers(ctx context.Context, tx pgx.Tx, listingID string, offers []Offer) ([]Offer, error) {
	current, err := activeOffers(ctx, tx, listingID)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	out := make([]Offer, len(offers))
	for i, o := range offers {
		for _, c := range current[listingID] {
			if !kept[c.ID] && sameTerms(o, c) {
				kept[c.ID], out[i] = true, c
				break
			}
		}
	}
	for _, c := range current[listingID] {
		if !kept[c.ID] {
			if _, err := tx.Exec(ctx, `UPDATE market_offers SET active = false WHERE id = $1`, c.ID); err != nil {
				return nil, fmt.Errorf("market: end offer: %w", err)
			}
		}
	}
	for i, o := range offers {
		if out[i].ID != "" {
			continue
		}
		o.ID, o.Terms = "ofr_"+uuid.NewString(), LicenceTerms[o.Licence]
		if err := tx.QueryRow(ctx, `INSERT INTO market_offers (id, listing_id, kind, licence, price_usd_micros, period_days, included_uses, seats, trial_uses)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING created_at`,
			o.ID, listingID, o.Kind, o.Licence, o.PriceUSDMicros, o.PeriodDays, o.IncludedUses, o.Seats, o.TrialUses).Scan(&o.CreatedAt); err != nil {
			return nil, fmt.Errorf("market: add offer: %w", err)
		}
		out[i] = o
	}
	price, _ := perUseULXC(out)
	if _, err := tx.Exec(ctx, `UPDATE market_listings SET price_per_use_ulxc = $2, updated_at = now() WHERE id = $1`, listingID, price); err != nil {
		return nil, fmt.Errorf("market: offers: %w", err)
	}
	return out, nil
}

// ReplaceOffers makes offers the active set of workspaceID's listing listingID (B32.18): the next charge uses the new
// prices, and every past use keeps its own. An empty set makes the listing free.
func (s *Store) ReplaceOffers(ctx context.Context, workspaceID, listingID string, offers []Offer) ([]Offer, error) {
	if err := checkOffers(offers, s.maxTrials()); err != nil {
		return nil, err
	}
	var out []Offer
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var review string
		err := tx.QueryRow(ctx, `SELECT review_status FROM market_listings WHERE id = $1 AND workspace_id = $2 FOR UPDATE`,
			listingID, workspaceID).Scan(&review)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("market: offers: %w", err)
		}
		if review == ReviewTakenDown {
			return ErrTakenDown
		}
		out, err = writeOffers(ctx, tx, listingID, offers)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
