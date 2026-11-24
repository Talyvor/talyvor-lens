-- B41.136 — the phone app's held-payment Live Activity, started and ended by Lens with the app closed.
--
-- workspace_device_keys.live_activity_token: the iPhone's ActivityKit push-to-start token ('' none). Each held payment
--   is pushed to it as a Live Activity's start.
-- workspace_live_activities: each Live Activity's own push token on a device, by the held payment it shows (key
--   pr:<request id> or ap:<approval id>, as the app keys them). Once the payment is approved or declined the activity is
--   sent its end and ended_at is set; an activity ended once is never sent another.
ALTER TABLE workspace_device_keys ADD COLUMN IF NOT EXISTS live_activity_token TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS workspace_live_activities (
    workspace_id TEXT NOT NULL,
    device_id    TEXT NOT NULL,
    held_key     TEXT NOT NULL,
    push_token   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at     TIMESTAMPTZ,
    PRIMARY KEY (device_id, held_key)
);
CREATE INDEX IF NOT EXISTS workspace_live_activities_open ON workspace_live_activities (workspace_id, held_key) WHERE ended_at IS NULL;
