-- B40.15 — each phone's push token, kept with its device key: APNs for an iPhone, FCM for an Android phone. '' is none.
-- A held payment or a prepared request is pushed to every device of the workspace with a token and no revoked_at.
ALTER TABLE workspace_device_keys ADD COLUMN IF NOT EXISTS push_token TEXT NOT NULL DEFAULT '';
