-- 0331_counter_playbooks.sql — B52.16: Counter playbooks on the marketplace (internal/market/playbooks.go,
-- internal/economy/negotiation_playbooks.go).
--
-- market_listings.kind:      adds counter_playbook, a negotiation playbook for one deal type.
-- market_playbooks:          the gym result of each listed playbook's latest listed version, for the ranking; a
--                            version that fails the gym is never kept, so it has no row.
-- negotiation_playbook_runs: the playbook a negotiation runs, at most one, claimed before its one billed use; playbook
--                            is that version's artifact, which the engine plans and words the negotiation by.
ALTER TABLE market_listings DROP CONSTRAINT IF EXISTS market_listings_kind_check;
ALTER TABLE market_listings ADD CONSTRAINT market_listings_kind_check
    CHECK (kind IN ('agent', 'prompt', 'skill', 'evaluation', 'pipeline', 'counter_playbook'));

CREATE TABLE IF NOT EXISTS market_playbooks (
    listing_id   TEXT PRIMARY KEY REFERENCES market_listings (id) ON DELETE CASCADE,
    version      INTEGER NOT NULL,
    deal_type    TEXT NOT NULL CHECK (deal_type IN ('renewal', 'purchase')),
    score_bps    BIGINT NOT NULL,
    negotiations INTEGER NOT NULL,
    deals        INTEGER NOT NULL,
    scored_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_market_playbooks_rank ON market_playbooks (deal_type, score_bps DESC);

CREATE TABLE IF NOT EXISTS negotiation_playbook_runs (
    mandate_id   TEXT PRIMARY KEY REFERENCES negotiation_mandates (id),
    workspace_id TEXT NOT NULL,
    listing_id   TEXT NOT NULL,
    version      INTEGER,
    use_id       TEXT,
    playbook     JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((use_id IS NULL) = (playbook IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_negotiation_playbook_runs_workspace ON negotiation_playbook_runs (workspace_id);
