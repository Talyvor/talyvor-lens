-- 0295_market_licence_idle_alerts.sql — B41.176: a renewing licence nobody uses is flagged to its buyer once.
--
-- A rent or subscription that renews and has had no use of its listing by the buyer's workspace for 30 days is idle
-- (market/idle.go). The schedules' tick pushes its buyer one alert per idle stretch, recorded here: idle_since is the
-- later of the licence's start and its listing's last use, so a use and a new 30 days idle alert once more.
CREATE TABLE IF NOT EXISTS market_licence_idle_alerts (
    licence_id         TEXT NOT NULL REFERENCES market_licences (id),
    buyer_workspace_id TEXT NOT NULL,
    idle_since         TIMESTAMPTZ NOT NULL,
    alerted_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (licence_id, idle_since)
);
CREATE INDEX IF NOT EXISTS idx_market_licence_idle_alerts_buyer ON market_licence_idle_alerts (buyer_workspace_id);

-- A licence's last use: its buyer's latest use of its listing.
CREATE INDEX IF NOT EXISTS idx_market_uses_last_use ON market_uses (buyer_workspace_id, listing_id, used_at DESC) WHERE use_kind = 'use';
