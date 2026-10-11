-- B30.21 — bulk payouts: up to 1,000 payments out uploaded at once.
--
-- A payout batch is a CSV or JSON upload of rows — payee, amount, currency, reference — validated row by row when it
-- arrives and held, moving nothing, for one approval. On approval each valid row becomes a payment out
-- (money_payments) under its own idempotency key, so approving twice pays nothing twice. rows keeps every row as
-- uploaded, with why it was refused or the payment it became and where that stood when it was sent.

CREATE TABLE IF NOT EXISTS money_payout_batches (
    id              TEXT PRIMARY KEY,                                 -- mpob_<uuid>
    workspace_id    TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    rows_sha256     TEXT NOT NULL,                                    -- of the rows as uploaded: the same key with other rows is refused
    agent_id        TEXT NOT NULL DEFAULT '',                         -- the agent whose accounts pay; '' for the company's
    funding         TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    status          TEXT NOT NULL CHECK (status IN ('awaiting_approval', 'sending', 'sent')),
    rows            JSONB NOT NULL,
    rows_total      INTEGER NOT NULL CHECK (rows_total BETWEEN 1 AND 1000),
    rows_valid      INTEGER NOT NULL CHECK (rows_valid BETWEEN 0 AND rows_total),
    totals_minor    JSONB NOT NULL DEFAULT '{}',                      -- of the valid rows, by currency
    approved_by     TEXT NOT NULL DEFAULT '',
    approved_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_money_payout_batches_workspace ON money_payout_batches (workspace_id, created_at);
