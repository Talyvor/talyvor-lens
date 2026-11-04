-- 0302_spend_notices.sql — B53.7: notices that arrive at the right time.
--
-- spend_notice_settings: the owner's quiet hours (local HH:MM, in their time zone), the kinds of notice they switched
--                        off, and how far the sweep has read the workspace's payments and alerts.
-- spend_notices:         one row per notice — a payment, a budget alert, an unusual payment, a Coach tip or the Monday
--                        digest — with what it says, and whether it was sent, is waiting (held, and why) or was dropped.
--                        The unique key makes each payment, alert, finding, tip and digest a notice once.

CREATE TABLE IF NOT EXISTS spend_notice_settings (
    workspace_id TEXT PRIMARY KEY,
    timezone     TEXT NOT NULL DEFAULT 'UTC',
    quiet_from   TEXT NOT NULL DEFAULT '' CHECK (quiet_from ~ '^([0-9]{2}:[0-9]{2})?$'),  -- '': no quiet hours
    quiet_until  TEXT NOT NULL DEFAULT '' CHECK (quiet_until ~ '^([0-9]{2}:[0-9]{2})?$'),
    off          TEXT[] NOT NULL DEFAULT '{}',
    seen_until   TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS spend_notices (
    id           TEXT PRIMARY KEY,                                   -- snt_<uuid>
    workspace_id TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('payment', 'budget', 'unusual', 'tip', 'digest')),
    subject      TEXT NOT NULL,                                      -- the entry, alert, finding, budget-day or Monday
    title        TEXT NOT NULL,
    body         TEXT NOT NULL,
    urgent       BOOLEAN NOT NULL DEFAULT false,                     -- ignores quiet hours
    data         JSONB NOT NULL DEFAULT '{}',
    status       TEXT NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'sent', 'dropped')),
    held         TEXT NOT NULL DEFAULT '',                           -- why a waiting notice waits: quiet_hours, daily_tip
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at      TIMESTAMPTZ,
    UNIQUE (workspace_id, kind, subject)
);
CREATE INDEX IF NOT EXISTS idx_spend_notices_workspace ON spend_notices (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_spend_notices_waiting ON spend_notices (workspace_id) WHERE status = 'waiting';
