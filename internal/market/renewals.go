package market

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// renewals.go — B32.20: SUBSCRIPTIONS RENEW AND CANCEL AT PERIOD END, AND RENTALS ADD UP TO OWNERSHIP.
//
// On the schedules' tick (economy.RunAgentSchedules) each active licence that auto-renews and whose ends_at has come is
// renewed: one market_uses row (use_kind 'renewal') at its offer's price, stamped at the period's start, on the buyer's
// monthly marketplace bill — judged by its agent's rules when an agent holds it, and asked of the capability its
// charge is recorded under, as the licence's first charge was (licences.go). A refused renewal ends the licence at its
// ends_at as unpaid; so does a bill Stripe gives up on (FailInvoice). CancelLicence turns auto_renew off: the licence
// runs to its ends_at and nothing is charged for a period after it.
//
// Rent-to-own: each rent, or renewal of a rent, that clears (ClearInvoice) adds its price to its licence's
// rent_paid_usd_micros. Once a buyer's rents of a listing under one licence type have paid that listing's buy offer for
// the same licence type, a perpetual licence (kind buy, source rent_to_own) is issued at no charge and their renewals
// stop.

// Where a licence came from.
const (
	SourceOffer     = "offer"       // bought, rented or subscribed through one of the listing's offers
	SourceRentToOwn = "rent_to_own" // the rents its buyer paid reached the listing's buy offer
)

// maxRenewalsPerRun bounds one tick's renewing; the rest waits for the next tick.
const maxRenewalsPerRun = 1000

var _ economy.LicenceRenewer = (*Store)(nil)

// errNotDue: the licence was renewed, cancelled or ended while its renewal was being judged.
var errNotDue = errors.New("market: the licence is no longer due to renew")

// dueLicence is a licence whose period has ended and that renews, with its offer's terms.
type dueLicence struct {
	id, listingID, buyer, agentID, person string
	kind, licence                         string
	pinned                                *int
	endsAt                                time.Time
	priceUSDMicros                        int64
	periodDays                            *int
}

// RenewLicences renews every licence due at now, one renewal per period, and ends as unpaid each whose renewal is
// refused. It answers how many it renewed and how many it ended; a licence whose renewal fails is passed over until the
// next tick, and its failure returned once the others are done.
func (s *Store) RenewLicences(ctx context.Context, now time.Time, judge economy.LicenceJudge) (renewed, unpaid int, err error) {
	passed := []string{} // licences this run tried and did not renew or end
	var failed []error
	for range maxRenewalsPerRun {
		var d dueLicence
		err := s.pool.QueryRow(ctx, `SELECT c.id, c.listing_id, c.buyer_workspace_id, c.agent_id, c.person_id, c.kind, c.licence, c.pinned_version, c.ends_at,
				COALESCE(o.price_usd_micros, 0), o.period_days
			FROM market_licences c LEFT JOIN market_offers o ON o.id = c.offer_id
			WHERE c.status = 'active' AND c.auto_renew AND c.ends_at <= $1 AND c.id <> ALL($2)
			ORDER BY c.ends_at, c.id LIMIT 1`, now, passed).
			Scan(&d.id, &d.listingID, &d.buyer, &d.agentID, &d.person, &d.kind, &d.licence, &d.pinned, &d.endsAt, &d.priceUSDMicros, &d.periodDays)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("market: licences due: %w", err))
			break
		}
		refused, err := s.renew(ctx, d, judge)
		switch {
		case errors.Is(err, errNotDue):
			passed = append(passed, d.id)
		case err != nil:
			passed = append(passed, d.id)
			failed = append(failed, fmt.Errorf("market: renew licence %s: %w", d.id, err))
		case refused:
			tag, err := s.pool.Exec(ctx, `UPDATE market_licences SET status = 'unpaid', auto_renew = false
				WHERE id = $1 AND status = 'active' AND auto_renew AND ends_at = $2`, d.id, d.endsAt)
			if err != nil {
				return renewed, unpaid, fmt.Errorf("market: end licence %s unpaid: %w", d.id, err)
			}
			unpaid += int(tag.RowsAffected())
		default:
			renewed++
		}
	}
	return renewed, unpaid, errors.Join(failed...)
}

// renew records d's renewal for the period starting at its ends_at, and moves its ends_at on a period — or reports
// that the renewal was refused, recording nothing.
func (s *Store) renew(ctx context.Context, d dueLicence, judge economy.LicenceJudge) (refused bool, err error) {
	l, err := visibleListing(ctx, s.pool, d.buyer, d.listingID, "")
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && l.ReviewStatus == ReviewTakenDown):
		return true, nil
	case err != nil:
		return false, err
	case d.periodDays == nil: // a licence that renews was sold under a rent or subscribe offer, which has a period
		return true, nil
	}
	charge, ulxc := ChargeBilled, d.priceUSDMicros*ulxcPerUSDMicro
	if ulxc == 0 {
		charge = ChargeFree
	} else {
		if err := workspace.CheckMoneyWall(ctx, s.pool, d.buyer, l.WorkspaceID); err != nil {
			return refusal(err), ignoreRefusal(err)
		}
		linked, err := s.linked(ctx, l.WorkspaceID, d.buyer)
		if err != nil {
			return false, err
		}
		if linked {
			charge, ulxc = ChargeLinked, 0
		}
	}
	if charge == ChargeBilled {
		if err := judge.RequireBilledCapability(ctx, d.buyer, economy.CapabilityRentAndSubscribe); err != nil {
			return refusal(err), ignoreRefusal(err)
		}
		if err := s.sellable(ctx, d.buyer); err != nil { // B32.39
			return refusal(err), ignoreRefusal(err)
		}
	}
	version := l.LatestVersion
	if d.pinned != nil {
		version = *d.pinned
	}
	record := func(tx pgx.Tx) error {
		var still bool
		if err := tx.QueryRow(ctx, `SELECT status = 'active' AND auto_renew AND ends_at = $2 FROM market_licences WHERE id = $1 FOR UPDATE`,
			d.id, d.endsAt).Scan(&still); err != nil {
			return err
		}
		if !still {
			return errNotDue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, person_id,
				price_ulxc, charge, use_kind, licence_id, used_at, ran_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'renewal', $10, $11, $11)`,
			"use_"+uuid.NewString(), l.ID, version, l.WorkspaceID, d.buyer, d.agentID, d.person, ulxc, charge, d.id, d.endsAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE market_licences SET ends_at = ends_at + make_interval(days => $2) WHERE id = $1`, d.id, *d.periodDays)
		return err
	}
	if d.agentID != "" {
		what := fmt.Sprintf("renewal:%s:%s:%d", d.id, d.endsAt.UTC().Format(time.RFC3339), ulxc)
		// B32.22: a renewal commits the agent to another period, judged as the licence was: it renews itself, so it
		// needs may_subscribe still, and an owner who turns that off stops the agent's licences renewing.
		c := economy.Commitment{Kind: d.kind, Licence: d.licence, Renews: true, ULXC: ulxc}
		memo := fmt.Sprintf("%s renews: %s on your monthly marketplace bill", l.Title, economy.LXCMoney(ulxc))
		err = judge.JudgeAgentPurchase(ctx, d.buyer, d.agentID, l.ID, ulxc, c, what, memo, record)
	} else {
		err = pgx.BeginFunc(ctx, s.pool, record)
	}
	return refusal(err), ignoreRefusal(err)
}

// refusal reports whether err is a charge being refused — by an agent's rules, the capability or the money wall —
// rather than something failing.
func refusal(err error) bool {
	var need *economy.ApprovalNeededError
	return errors.As(err, &need) || errors.Is(err, economy.ErrAgentRule) || errors.Is(err, economy.ErrAgentFunds) ||
		errors.Is(err, economy.ErrApprovalRequired) || errors.Is(err, economy.ErrAgentNotFound) || errors.Is(err, economy.ErrAgentOwnerless) ||
		errors.Is(err, economy.ErrOwnerUnverified) || errors.Is(err, economy.ErrCapabilityNotCleared) || errors.Is(err, workspace.ErrMoneyWall) ||
		errors.Is(err, ErrNotSoldHere)
}

// ignoreRefusal is err, unless it is a refusal.
func ignoreRefusal(err error) error {
	if refusal(err) {
		return nil
	}
	return err
}

// CancelLicence turns buyerWorkspaceID's licence licenceID off renewing: it runs to its ends_at, and nothing is charged
// for a period after it. Cancelling a licence that does not renew changes nothing.
func (s *Store) CancelLicence(ctx context.Context, buyerWorkspaceID, licenceID string) (Licence, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE market_licences SET auto_renew = false WHERE id = $1 AND buyer_workspace_id = $2 AND auto_renew`,
		licenceID, buyerWorkspaceID); err != nil {
		return Licence{}, fmt.Errorf("market: cancel licence: %w", err)
	}
	if err := expireLicences(ctx, s.pool, buyerWorkspaceID, s.now()); err != nil {
		return Licence{}, err
	}
	lic, err := scanLicence(s.pool.QueryRow(ctx, `SELECT `+licenceColumns+` WHERE c.id = $1 AND c.buyer_workspace_id = $2`, licenceID, buyerWorkspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Licence{}, fmt.Errorf("%w: no licence %q", ErrNotFound, licenceID)
	}
	if err != nil {
		return Licence{}, fmt.Errorf("market: licence: %w", err)
	}
	return lic, nil
}

// FailInvoice ends as unpaid every licence the buyer's marketplace invoice that Stripe gave up on was to pay for — a
// purchase, rent, subscription, renewal or room prize used within [periodStart, periodEnd) and not cleared: each runs to its
// ends_at (a bought one, which has none, ends now) and renews no more. It answers how many it ended; a replay ends none.
func (s *Store) FailInvoice(ctx context.Context, buyerWorkspaceID, invoiceID string, periodStart, periodEnd time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE market_licences c SET status = 'unpaid', auto_renew = false
		WHERE c.buyer_workspace_id = $1 AND c.status IN ('active', 'expired')
		  AND EXISTS (SELECT 1 FROM market_uses u WHERE u.licence_id = c.id AND u.use_kind IN ('buy', 'rent', 'subscribe', 'renewal', 'prize')
		              AND u.charge = 'billed' AND u.cleared_at IS NULL AND u.used_at >= $2 AND u.used_at < $3)`,
		buyerWorkspaceID, periodStart, periodEnd)
	if err != nil {
		return 0, fmt.Errorf("market: fail invoice %s: %w", invoiceID, err)
	}
	return int(tag.RowsAffected()), nil
}

// rentCleared adds a cleared rent's price, gross µUSD, to its licence's rent paid, in ClearInvoice's tx. Once what the
// buyer's rents of the listing under that licence type have paid reaches the listing's buy offer for the same licence
// type, it issues the perpetual licence they add up to, at no charge, and stops their renewals. A use that is not a
// rent's changes nothing.
func rentCleared(ctx context.Context, tx pgx.Tx, licenceID string, gross int64, at time.Time) error {
	var buyer, listing, licence, person, agent string
	var pinned *int
	err := tx.QueryRow(ctx, `UPDATE market_licences SET rent_paid_usd_micros = rent_paid_usd_micros + $2 WHERE id = $1 AND kind = 'rent'
		RETURNING buyer_workspace_id, listing_id, licence, person_id, agent_id, pinned_version`, licenceID, gross).
		Scan(&buyer, &listing, &licence, &person, &agent, &pinned)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rent paid: %w", err)
	}
	var buyOffer string
	var price, paid int64
	var seats *int
	err = tx.QueryRow(ctx, `SELECT o.id, o.price_usd_micros, o.seats,
		       (SELECT COALESCE(sum(c.rent_paid_usd_micros), 0)::bigint FROM market_licences c
		        WHERE c.buyer_workspace_id = $3 AND c.listing_id = $1 AND c.licence = $2 AND c.person_id = $4 AND c.kind = 'rent')
		FROM market_offers o WHERE o.listing_id = $1 AND o.active AND o.kind = 'buy' AND o.licence = $2`, listing, licence, buyer, person).
		Scan(&buyOffer, &price, &seats, &paid)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && paid < price) {
		return nil // nothing to own: the listing is not sold outright under this licence, or not yet paid for
	}
	if err != nil {
		return fmt.Errorf("rent to own: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO market_licences (id, listing_id, offer_id, buyer_workspace_id, agent_id, person_id, licence, kind,
			pinned_version, starts_at, seats, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'buy', $8, $9, $10, 'rent_to_own')
		ON CONFLICT (buyer_workspace_id, listing_id, licence, person_id) WHERE source = 'rent_to_own' DO NOTHING`,
		"lic_"+uuid.NewString(), listing, buyOffer, buyer, agent, person, licence, pinned, at, seats); err != nil {
		return fmt.Errorf("rent to own: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE market_licences SET auto_renew = false
		WHERE buyer_workspace_id = $1 AND listing_id = $2 AND licence = $3 AND person_id = $4 AND kind IN ('rent', 'subscribe') AND auto_renew`,
		buyer, listing, licence, person); err != nil {
		return fmt.Errorf("rent to own: %w", err)
	}
	return nil
}
