-- 0297_market_content_labels.sql — B41.185: every listing carries an age rating and content labels; a workspace
-- filters what it is shown by them and blocks the listings it never wants to see again (Apple guideline 4.7).
--
-- A listing published without a rating is rated 18+, and so is every listing from before this migration: a listing is
-- never shown to a family filter until its seller says it may be (internal/market/content.go).

ALTER TABLE market_listings
    ADD COLUMN IF NOT EXISTS age_rating     TEXT NOT NULL DEFAULT '18+' CHECK (age_rating IN ('all', '13+', '16+', '18+')),
    ADD COLUMN IF NOT EXISTS content_labels TEXT[] NOT NULL DEFAULT '{}';

-- A workspace's content filter: the highest age rating it is shown, and the labels it is never shown. No row: everything.
CREATE TABLE IF NOT EXISTS market_content_filters (
    workspace_id   TEXT PRIMARY KEY,
    max_age_rating TEXT NOT NULL DEFAULT '18+' CHECK (max_age_rating IN ('all', '13+', '16+', '18+')),
    blocked_labels TEXT[] NOT NULL DEFAULT '{}',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The listings a workspace blocked: never in its catalog, search or collections again.
CREATE TABLE IF NOT EXISTS market_listing_blocks (
    workspace_id TEXT NOT NULL,
    listing_id   TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, listing_id)
);
