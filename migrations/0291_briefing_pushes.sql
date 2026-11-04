-- B41.205 — the daily briefing as a morning web push.
--
-- One row per workspace per UTC day its briefing was pushed: the run claims the day before it sends, so a workspace is
-- pushed at most once a day however many Lens processes tick.
CREATE TABLE IF NOT EXISTS workspace_briefing_pushes (
    workspace_id TEXT NOT NULL,
    day          DATE NOT NULL,
    sent_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, day)
);
