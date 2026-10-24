-- B46.3 — the money layer's partner webhooks already taken, per partner: a notification whose id is here is refused as
-- a replay. Written only once its money is posted, so one whose posting failed is taken when the partner sends it again.
CREATE TABLE IF NOT EXISTS money_webhook_events (
    partner     TEXT NOT NULL,
    event_id    TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (partner, event_id)
);
