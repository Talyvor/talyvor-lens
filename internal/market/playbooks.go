package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
)

// playbooks.go — B52.16: COUNTER PLAYBOOKS ON THE MARKETPLACE.
//
// A creator publishes a negotiation playbook for one deal type as a listing of kind counter_playbook, its artifact the
// playbook (economy.NegotiationPlaybook: the engine's settings and its wording, nothing of any owner's). Every version
// plays Counter's gym before it is kept, and one that fails it is refused, so no failing playbook is ever listed; its
// result is on the version's scan, and CounterPlaybooks ranks the listed ones by gym score. A playbook runs only on a
// negotiation, once per negotiation: RunCounterPlaybook binds it to the owner's mandate through Use, so its price is
// billed and its creator earns under the marketplace's seller terms like any other per-use listing.

// KindCounterPlaybook is a Counter negotiation playbook's listing kind.
const KindCounterPlaybook = "counter_playbook"

// checkPlaybook reads a playbook's canonical artifact and plays it in the gym; a failure refuses the version.
func checkPlaybook(canonical []byte) (*economy.NegotiationGymResult, error) {
	pb, err := economy.ParseNegotiationPlaybook(canonical)
	if err != nil {
		return nil, invalid("%s", strings.TrimPrefix(err.Error(), economy.ErrNegotiationMandate.Error()+": "))
	}
	r := economy.PlayNegotiationGym(pb)
	if r.Failed != "" {
		return nil, invalid("the playbook failed Counter's gym, so it cannot be listed: %s", r.Failed)
	}
	return &r, nil
}

// writePlaybook keeps the gym result of a listed playbook's latest version for the ranking. A version held for review
// is not listed yet, so it leaves the version before it ranked.
func writePlaybook(ctx context.Context, tx pgx.Tx, listingID string, version int, artifact []byte, scan Scan) error {
	if scan.Gym == nil || scan.Held != "" {
		return nil
	}
	pb, err := economy.ParseNegotiationPlaybook(artifact)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO market_playbooks (listing_id, version, deal_type, score_bps, negotiations, deals)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (listing_id) DO UPDATE SET version = $2, deal_type = $3, score_bps = $4, negotiations = $5, deals = $6, scored_at = now()`,
		listingID, version, pb.DealType, scan.Gym.ScoreBPS, scan.Gym.Negotiations, scan.Gym.Deals)
	if err != nil {
		return fmt.Errorf("market: playbook: %w", err)
	}
	return nil
}

// CounterPlaybook is a listed playbook with its gym result.
type CounterPlaybook struct {
	Listing
	DealType   string                       `json:"deal_type"`
	GymVersion int                          `json:"gym_version"`
	Gym        economy.NegotiationGymResult `json:"gym"`
	Wording    economy.NegotiationWording   `json:"wording"` // what it would send in the owner's name, read before running it
}

// CounterPlaybooks reads the public, approved playbooks, of dealType when it is set, best gym score first.
func (s *Store) CounterPlaybooks(ctx context.Context, dealType string) ([]CounterPlaybook, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+listingColumns+`, deal_type, version, score_bps, negotiations, deals,
			(SELECT artifact->'wording' FROM market_listing_versions v WHERE v.listing_id = id AND v.version = market_playbooks.version)::text
		FROM market_playbooks JOIN market_listings ON id = listing_id
		WHERE visibility = 'public' AND review_status = 'approved' AND ($1 = '' OR deal_type = $1)
		ORDER BY score_bps DESC, id LIMIT 100`, dealType)
	if err != nil {
		return nil, fmt.Errorf("market: playbooks: %w", err)
	}
	defer rows.Close()
	out, ids := []CounterPlaybook{}, []string{}
	for rows.Next() {
		var p CounterPlaybook
		var wording string
		if p.Listing, err = scanListing(rows, &p.DealType, &p.GymVersion, &p.Gym.ScoreBPS, &p.Gym.Negotiations, &p.Gym.Deals, &wording); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(wording), &p.Wording); err != nil {
			return nil, fmt.Errorf("market: playbook %s wording: %w", p.ID, err)
		}
		p.asSeenBy("")
		out, ids = append(out, p), append(ids, p.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, s.withOffersAndCapabilities(ctx, ids, func(i int) *Listing { return &out[i].Listing })
}

// CounterRun is a playbook's run on one negotiation.
type CounterRun struct {
	MandateID string `json:"mandate_id"`
	ListingID string `json:"listing_id"`
	Version   int    `json:"version"`
	UseID     string `json:"use_id"`
	Charge    string `json:"charge,omitempty"`
	PriceULXC int64  `json:"price_ulxc"`
	Again     bool   `json:"again"` // it already ran on this negotiation: nothing more was charged
}

// ErrPlaybookBound: the negotiation already runs another playbook.
var ErrPlaybookBound = errors.New("market: this negotiation already runs another playbook")

// RunCounterPlaybook runs playbook listingID on buyer's live negotiation mandateID: the first run is one use of the
// listing, billed and earned as any use is, never above maxPrice µUSD when it is set, and Counter plans and words that
// negotiation by it from then on; asked again, it answers that run and charges nothing.
func (s *Store) RunCounterPlaybook(ctx context.Context, deps UseDeps, buyer, mandateID, listingID string, maxPrice *int64) (CounterRun, error) {
	run := CounterRun{MandateID: mandateID, ListingID: listingID}
	var mandateKind, dealType string
	var beforeDeadline bool
	err := s.pool.QueryRow(ctx, `SELECT canonical::jsonb->'task'->>'kind', deadline > now() FROM negotiation_mandates
		WHERE id = $1 AND workspace_id = $2 AND ended_at IS NULL`, mandateID, buyer).Scan(&mandateKind, &beforeDeadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return run, fmt.Errorf("%w: no live negotiation %q in this workspace", ErrNotFound, mandateID)
	}
	if err != nil {
		return run, fmt.Errorf("market: playbook run: %w", err)
	}
	if !beforeDeadline {
		return run, invalid("the negotiation's deadline has passed, so no playbook can run on it")
	}
	// The version whose deal type is checked is the version run and bound.
	var version int
	if _, err := visibleListing(ctx, s.pool, buyer, listingID, ""); errors.Is(err, pgx.ErrNoRows) {
		return run, fmt.Errorf("%w: no listed playbook %q", ErrNotFound, listingID)
	} else if err != nil {
		return run, fmt.Errorf("market: playbook run: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT deal_type, version FROM market_playbooks WHERE listing_id = $1`, listingID).Scan(&dealType, &version); errors.Is(err, pgx.ErrNoRows) {
		return run, fmt.Errorf("%w: no listed playbook %q", ErrNotFound, listingID)
	} else if err != nil {
		return run, fmt.Errorf("market: playbook run: %w", err)
	}
	if dealType != mandateKind {
		return run, invalid("the playbook is for a %s and the negotiation is a %s", dealType, mandateKind)
	}
	if bound, err := s.boundRun(ctx, mandateID, listingID, &run); bound || err != nil {
		return run, err
	}
	// The run is bound in the transaction that marks its use ran, and only a use that ran is billed: a run that fails or
	// dies before then binds nothing and bills nothing, and asking again runs it again. Of two asks at once, one binds.
	bind := func(tx pgx.Tx, u Use) error {
		tag, err := tx.Exec(ctx, `INSERT INTO negotiation_playbook_runs (mandate_id, workspace_id, listing_id, version, use_id, playbook)
			SELECT $1, $2, $3, $4, $5, artifact FROM market_listing_versions WHERE listing_id = $3 AND version = $4
			ON CONFLICT (mandate_id) DO UPDATE SET listing_id = $3, version = $4, use_id = $5, playbook = EXCLUDED.playbook
			WHERE negotiation_playbook_runs.use_id IS NULL`, mandateID, buyer, listingID, u.Version, u.ID)
		if err == nil && tag.RowsAffected() == 0 {
			err = errRunBound
		}
		return err
	}
	u, err := s.Use(ctx, deps, buyer, "", listingID, UseRequest{bindRun: bind, Version: version, MaxPriceUSDMicros: maxPrice})
	if err != nil {
		// A use recorded but never marked ran was never billed: it goes, as a failed use does.
		if u.ID != "" {
			if _, derr := s.pool.Exec(context.WithoutCancel(ctx), `DELETE FROM market_uses WHERE id = $1 AND ran_at IS NULL AND metered_at IS NULL`, u.ID); derr != nil {
				return run, errors.Join(err, derr)
			}
		}
		if errors.Is(err, errRunBound) {
			if bound, berr := s.boundRun(ctx, mandateID, listingID, &run); bound || berr != nil {
				return run, berr
			}
		}
		return run, err
	}
	run.UseID, run.Version, run.Charge, run.PriceULXC = u.ID, u.Version, u.Charge, u.PriceULXC
	return run, nil
}

// errRunBound: another ask bound the negotiation's run first.
var errRunBound = errors.New("market: the negotiation's playbook run was bound by another ask")

// boundRun reads the run bound on mandateID into run: the same playbook's run, Again; another's, ErrPlaybookBound. A run
// never bound (claimed by a run before B54.30 that died) is no run.
func (s *Store) boundRun(ctx context.Context, mandateID, listingID string, run *CounterRun) (bool, error) {
	var bound string
	err := s.pool.QueryRow(ctx, `SELECT r.listing_id, r.use_id, r.version, coalesce(u.charge, ''), coalesce(u.price_ulxc, 0)
		FROM negotiation_playbook_runs r LEFT JOIN market_uses u ON u.id = r.use_id WHERE r.mandate_id = $1 AND r.use_id IS NOT NULL`, mandateID).
		Scan(&bound, &run.UseID, &run.Version, &run.Charge, &run.PriceULXC)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("market: playbook run: %w", err)
	}
	if bound != listingID {
		return true, ErrPlaybookBound
	}
	run.Again = true
	return true, nil
}
