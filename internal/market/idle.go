package market

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
)

// idle.go — B41.176: A RENEWING LICENCE NOBODY USES IS FLAGGED, AND ITS BUYER TOLD ONCE.
//
// A rent or subscription that renews is idle once its buyer's workspace has not used its listing for IdleFor: since
// the later of the licence's start and the listing's last use. The licences list marks it idle, so the buyer can cancel
// it there (CancelLicence); the schedules' tick pushes the buyer one alert for each idle stretch (migration 0295).

// IdleFor is how long a renewing licence goes unused before it is idle.
const IdleFor = 30 * 24 * time.Hour

// lastUse is when a licence's buyer last used its listing: null, never.
const lastUse = `(SELECT max(u.used_at) FROM market_uses u
	WHERE u.buyer_workspace_id = c.buyer_workspace_id AND u.listing_id = c.listing_id AND u.use_kind = 'use')`

// idle reports whether l is a renewing rent or subscription unused for IdleFor at now, as AlertIdleLicences judges it.
func idle(l Licence, now time.Time) bool {
	if l.Status != LicenceActive || !l.AutoRenew || (l.Kind != OfferRent && l.Kind != OfferSubscribe) {
		return false
	}
	since := l.StartsAt
	if l.LastUsedAt != nil && l.LastUsedAt.After(since) {
		since = *l.LastUsedAt
	}
	return !now.Before(since.Add(IdleFor))
}

// AlertIdleLicences records each licence idle at now that its buyer has not been told of for its current idle stretch,
// and answers them: the schedules' tick tells each buyer once.
func (s *Store) AlertIdleLicences(ctx context.Context, now time.Time) ([]economy.IdleLicence, error) {
	rows, err := s.pool.Query(ctx, `WITH idle AS (
			SELECT c.id, c.buyer_workspace_id, COALESCE(l.title, '') AS title, GREATEST(c.starts_at, `+lastUse+`) AS since
			FROM market_licences c LEFT JOIN market_listings l ON l.id = c.listing_id
			WHERE c.status = 'active' AND c.auto_renew AND c.kind IN ('rent', 'subscribe')
		), told AS (
			INSERT INTO market_licence_idle_alerts (licence_id, buyer_workspace_id, idle_since, alerted_at)
			SELECT id, buyer_workspace_id, since, $1 FROM idle WHERE since <= $2
			ON CONFLICT DO NOTHING RETURNING licence_id
		)
		SELECT i.buyer_workspace_id, i.id, i.title FROM told t JOIN idle i ON i.id = t.licence_id ORDER BY i.buyer_workspace_id, i.id`,
		now, now.Add(-IdleFor))
	if err != nil {
		return nil, fmt.Errorf("market: idle licences: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (economy.IdleLicence, error) {
		var x economy.IdleLicence
		return x, r.Scan(&x.WorkspaceID, &x.LicenceID, &x.Title)
	})
	if err != nil {
		return nil, fmt.Errorf("market: idle licences: %w", err)
	}
	return out, nil
}
