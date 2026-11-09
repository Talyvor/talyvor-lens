-- B30.39 — a credit score for companies, with the reasons shown.
--
-- The score itself is never stored: it is worked out from the company's own history on Talyvor each time it is
-- read (economy/credit_score.go). What is stored is the owner asking a person to look at it: one row per
-- request, carrying the score, band and reasons the owner saw when they asked, and their note. An open row is a
-- task on the operator's list (`lens credit-scores`); the operator closes it with a note the owner then sees.
-- A score never decides live credit alone: a person approves every live limit.
CREATE TABLE IF NOT EXISTS credit_score_reviews (
    id            TEXT PRIMARY KEY,                                     -- csr_<uuid>
    workspace_id  TEXT NOT NULL,
    score         INT NOT NULL CHECK (score BETWEEN 0 AND 1000),
    band          TEXT NOT NULL CHECK (band IN ('A', 'B', 'C', 'D', 'E')),
    reasons       JSONB NOT NULL,
    note          TEXT NOT NULL DEFAULT '',                             -- the owner's
    requested_by  TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    outcome       TEXT NOT NULL DEFAULT '',                             -- the operator's note on closing it
    closed_by     TEXT NOT NULL DEFAULT '',
    requested_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at     TIMESTAMPTZ
);
-- One open review per company: asking again while one is open is the same request.
CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_score_reviews_open ON credit_score_reviews (workspace_id) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_credit_score_reviews_ws ON credit_score_reviews (workspace_id, requested_at DESC);
