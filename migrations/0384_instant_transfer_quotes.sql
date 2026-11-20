-- B59.14 — the Instant transfer quote, locked for 30 seconds (launch pack 4.1, 4.3).
--
-- A quote prices sending an amount from a hybrid workspace: the fee (rounded up), the rate and what the beneficiary
-- gets (rounded down), and when it arrives — in seconds, or the bank rail's own time. It keeps the route picked for the
-- owner and the money layer's quotes for each conversion, so a send within its 30 seconds pays exactly what it showed.
CREATE TABLE IF NOT EXISTS instant_transfer_quotes (
    id                     TEXT PRIMARY KEY,                       -- itq_<uuid>
    workspace_id           TEXT NOT NULL,
    recipient              TEXT NOT NULL CHECK (recipient IN ('talyvor_account', 'wallet', 'bank_account')),
    country                TEXT NOT NULL DEFAULT '',               -- a bank account's
    send_minor             BIGINT NOT NULL CHECK (send_minor > 0), -- what leaves the sender, the fee included
    send_currency          TEXT NOT NULL,
    fee_bps                BIGINT NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    fee_minor              BIGINT NOT NULL CHECK (fee_minor >= 0), -- Talyvor's, in the currency sent
    receive_minor          BIGINT NOT NULL CHECK (receive_minor > 0),
    receive_currency       TEXT NOT NULL,
    rate                   TEXT NOT NULL,
    arrives_within_seconds BIGINT NOT NULL DEFAULT 0,              -- a bank rail's longest; 0 is in seconds
    route                  TEXT NOT NULL CHECK (route IN ('instant', 'direct')),
    onchain_minor          BIGINT NOT NULL DEFAULT 0,              -- the USDC the on-chain leg moves
    sell_ref               TEXT NOT NULL DEFAULT '',               -- the money layer's quote into USDC
    buy_ref                TEXT NOT NULL DEFAULT '',               -- the money layer's quote out of USDC
    expires_at             TIMESTAMPTZ NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_instant_transfer_quotes_workspace ON instant_transfer_quotes (workspace_id, created_at);

-- Each leg of an Instant transfer as it ended, kept by the money layer's transfer hooks (B59.13) whether it moved money
-- or failed. A leg asked again under its key keeps one row.
CREATE TABLE IF NOT EXISTS instant_transfer_legs (
    workspace_id TEXT NOT NULL,
    transfer_id  TEXT NOT NULL,
    leg          TEXT NOT NULL,           -- convert, onchain or bank_payout
    method       TEXT NOT NULL,
    key          TEXT NOT NULL,
    partner      TEXT NOT NULL,
    partner_ref  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT '',
    outcome      TEXT NOT NULL,           -- ok, or the kind of its error
    amount_minor BIGINT NOT NULL DEFAULT 0,
    currency     TEXT NOT NULL DEFAULT '',
    recorded_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, transfer_id, leg, key, outcome)
);
