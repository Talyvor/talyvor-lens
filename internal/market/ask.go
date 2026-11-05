package market

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ask.go — B41.171: ASK THE MARKETPLACE. A job described in words ("translate 500 product descriptions into German";
// spoken, it is first turned into words by POST /v1/audio/transcriptions) answers with up to three listings, the most
// relevant first as Discover ranks them, each with what that volume of uses would cost and why it fits.

// The most picks an answer has, and the most uses an estimate prices.
const (
	MaxAskPicks  = 3
	MaxAskVolume = 1_000_000
)

// Where an answer's volume came from.
const (
	VolumeAsked   = "asked"   // the volume the buyer gave
	VolumeInWords = "words"   // the first number in the job's words
	VolumeDefault = "default" // neither: one use
)

// AskEstimate is what a listing would cost for the volume, on its cheapest commercial offer.
type AskEstimate struct {
	Volume         int      `json:"volume"`
	OfferID        string   `json:"offer_id,omitempty"` // "" when the listing is free
	OfferKind      string   `json:"offer_kind"`         // per_use, buy, rent, subscribe, or free
	Licences       int      `json:"licences,omitempty"` // how many rentals or subscriptions the volume takes
	TotalUSDMicros int64    `json:"total_usd_micros"`
	Display        *Display `json:"display,omitempty"` // the total in the buyer's currency (B32.51)
	Basis          string   `json:"basis"`             // how the total is worked out, in words
}

// AskPick is one listing an answer offers.
type AskPick struct {
	DiscoverHit
	Estimate *AskEstimate `json:"estimate"` // nil: only an enterprise offer sells it, priced by seat
	Why      []string     `json:"why"`
}

// AskAnswer is the answer to a job described in words.
type AskAnswer struct {
	Q          string    `json:"q"`
	Volume     int       `json:"volume"`
	VolumeFrom string    `json:"volume_from"` // VolumeAsked, VolumeInWords or VolumeDefault
	Picks      []AskPick `json:"picks"`
	Note       string    `json:"note"`
}

// askNote is what every estimate leaves out.
const askNote = "Each estimate is the marketplace price for the volume; the AI calls each use makes are charged as your own, beside it."

// firstNumber is the first whole number in a job's words, thousands separated by commas or not ("10,000").
var firstNumber = regexp.MustCompile(`\d{1,3}(?:,\d{3})+|\d+`)

// Ask answers the job q, for volume uses (0: the first number in q, or one), with offer prices and totals shown as
// buyer sees them in currency (as ShowPrices reads them).
func (s *Store) Ask(ctx context.Context, buyer, currency, q string, volume int) (AskAnswer, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return AskAnswer{}, invalid("q must describe the job, such as: translate 500 product descriptions into German")
	}
	from := VolumeAsked
	if volume == 0 {
		from, volume = VolumeDefault, 1
		if m := firstNumber.FindString(q); m != "" {
			n, err := strconv.Atoi(strings.ReplaceAll(m, ",", ""))
			if err != nil || n > MaxAskVolume {
				return AskAnswer{}, invalid("the job asks for %s uses; an estimate covers 1 to %d: give volume", m, MaxAskVolume)
			}
			if n > 0 {
				from, volume = VolumeInWords, n
			}
		}
	}
	if volume < 1 || volume > MaxAskVolume {
		return AskAnswer{}, invalid("volume must be a whole number from 1 to %d", MaxAskVolume)
	}
	page, err := s.Discover(ctx, DiscoverQuery{Q: q, Sort: SortRelevance})
	if err != nil {
		return AskAnswer{}, err
	}
	hits := page.Listings[:min(len(page.Listings), MaxAskPicks)]
	out := AskAnswer{Q: q, Volume: volume, VolumeFrom: from, Picks: make([]AskPick, len(hits)), Note: askNote}
	// shown holds each pick's listing, then a stand-in listing per estimate whose one offer is the total, so
	// ShowPrices puts both the offers and the totals in the buyer's currency.
	shown := []*Listing{}
	totals := []*Listing{}
	for i := range hits {
		p := &out.Picks[i]
		p.DiscoverHit = hits[i]
		p.Estimate = estimate(p.Offers, volume)
		shown = append(shown, &p.Listing)
		if p.Estimate != nil && p.Estimate.TotalUSDMicros > 0 {
			totals = append(totals, &Listing{Offers: []Offer{{PriceUSDMicros: p.Estimate.TotalUSDMicros}}})
		}
		if p.Why, err = s.why(ctx, q, &p.DiscoverHit); err != nil {
			return AskAnswer{}, err
		}
	}
	if err := s.ShowPrices(ctx, buyer, currency, append(shown, totals...)...); err != nil {
		return AskAnswer{}, err
	}
	t := 0
	for i := range out.Picks {
		if e := out.Picks[i].Estimate; e != nil && e.TotalUSDMicros > 0 {
			e.Display = totals[t].Offers[0].Display
			t++
		}
	}
	return out, nil
}

// estimate is the cheapest way offers' commercial licences cover volume uses; a listing with no offer is free.
func estimate(offers []Offer, volume int) *AskEstimate {
	if len(offers) == 0 {
		return &AskEstimate{Volume: volume, OfferKind: "free", Basis: "free to use"}
	}
	var best *AskEstimate
	for _, o := range offers {
		if o.Licence != LicenceCommercial {
			continue // a personal licence never covers an agent; an enterprise one is priced by seat
		}
		e := AskEstimate{Volume: volume, OfferID: o.ID, OfferKind: o.Kind}
		n := int64(1)
		switch o.Kind {
		case OfferPerUse:
			n = int64(volume)
			e.Basis = fmt.Sprintf("%d uses at its per-use price", volume)
		case OfferBuy:
			e.Basis = "one purchase covers every use"
		case OfferRent, OfferSubscribe:
			what := map[string]string{OfferRent: "rental", OfferSubscribe: "subscription"}[o.Kind]
			days := 0
			if o.PeriodDays != nil {
				days = *o.PeriodDays
			}
			if o.IncludedUses == nil || *o.IncludedUses == 0 {
				e.Basis = fmt.Sprintf("one %d-day %s covers every use", days, what)
			} else {
				n = int64((volume + *o.IncludedUses - 1) / *o.IncludedUses)
				e.Basis = fmt.Sprintf("%d %d-day %ss of %d uses each", n, days, what, *o.IncludedUses)
			}
			e.Licences = int(n)
		default:
			continue
		}
		if o.PriceUSDMicros > 0 && n > math.MaxInt64/o.PriceUSDMicros {
			continue
		}
		e.TotalUSDMicros = o.PriceUSDMicros * n
		if best == nil || e.TotalUSDMicros < best.TotalUSDMicros {
			best = &e
		}
	}
	return best
}

// why says, in words, why a listing fits the job q: the job's words its title or description has (as the search's
// English stemmer matches them, so "translations" has "translate"), the capabilities it declares that the job names,
// and how many buyers used it this week.
func (s *Store) why(ctx context.Context, q string, h *DiscoverHit) ([]string, error) {
	var words, caps []string
	err := s.pool.QueryRow(ctx, `SELECT
		  array(SELECT lower(w) FROM unnest($2::text[]) WITH ORDINALITY AS u(w, n)
			WHERE to_tsvector('english', l.title || ' ' || l.description) @@ plainto_tsquery('english', w) ORDER BY n),
		  array(SELECT c.label FROM listing_capabilities lc JOIN market_capabilities c ON c.slug = lc.capability
			WHERE lc.listing_id = l.id AND to_tsvector('english', $3) @@ plainto_tsquery('english', replace(c.slug, '-', ' '))
			ORDER BY c.position)
		FROM market_listings l WHERE l.id = $1`, h.ID, searchWords(q), q).Scan(&words, &caps)
	if err != nil {
		return nil, fmt.Errorf("market: why a listing fits: %w", err)
	}
	out, seen, said := []string{}, map[string]bool{}, []string{}
	for _, w := range words {
		if !seen[w] {
			seen[w], said = true, append(said, w)
		}
	}
	if len(caps) > 0 {
		out = append(out, "It declares it can "+inWords(caps)+".")
	}
	if len(said) > 0 {
		out = append(out, "Its title or description has "+inWords(said)+".")
	}
	if n := h.DistinctBuyers7d; n == 1 {
		out = append(out, "1 buyer used it in the last seven days.")
	} else if n > 1 {
		out = append(out, fmt.Sprintf("%d buyers used it in the last seven days.", n))
	}
	return out, nil
}

// inWords lists items as a sentence does: "a", "a and b", "a, b and c".
func inWords(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
