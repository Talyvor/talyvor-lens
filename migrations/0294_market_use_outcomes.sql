-- 0294_market_use_outcomes.sql — B41.173: live performance on every listing.
--
-- Every run of a listing leaves one row here, whether it answered or failed, with how long it took and what it
-- was charged. A failed use's market_uses row is still removed (it was never billed and no limit counts it); this
-- table is what remembers that it failed. The trust read turns the last 30 days of rows into the listing's success
-- rate, median latency and cost per use. A listing's rows go with it.
CREATE TABLE IF NOT EXISTS market_use_outcomes (
    use_id      TEXT PRIMARY KEY,
    listing_id  TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    ok          BOOLEAN NOT NULL,
    latency_ms  INTEGER NOT NULL CHECK (latency_ms >= 0),
    charge      TEXT NOT NULL,
    price_ulxc  BIGINT NOT NULL CHECK (price_ulxc >= 0),
    ran_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS market_use_outcomes_listing_ran ON market_use_outcomes (listing_id, ran_at);
