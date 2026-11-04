-- 0301_bank_connections.sql — B53.13: payments made with other banks' cards, through open banking.
--
-- bank_connections:  one account at another bank the owner consented to Lens reading, through the open-banking
--                    partner (partners.AccountInfoPartner): its consent, when that runs out, its balance and when it
--                    was last read. Consent is per account.
-- bank_transactions: what was read from it. Spending (B53.3) counts the money out like a card charge on the owner's
--                    own wallet. Disconnecting deletes the connection, and with it every transaction read from it.

CREATE TABLE IF NOT EXISTS bank_connections (
    id                 TEXT PRIMARY KEY,                            -- bnk_<uuid>
    workspace_id       TEXT NOT NULL,
    provider           TEXT NOT NULL,                               -- the partner's name
    bank               TEXT NOT NULL,
    account            TEXT NOT NULL,
    currency           TEXT NOT NULL,
    consent_ref        TEXT NOT NULL,
    consent_expires_at TIMESTAMPTZ NOT NULL,
    balance_minor      BIGINT,
    balance_as_of      TIMESTAMPTZ,
    refreshed_at       TIMESTAMPTZ,                                 -- the last read; NULL before the first
    connected_by       TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, bank, account)
);

CREATE TABLE IF NOT EXISTS bank_transactions (
    id            TEXT PRIMARY KEY,                                 -- btx_<uuid>
    workspace_id  TEXT NOT NULL,
    connection_id TEXT NOT NULL REFERENCES bank_connections (id) ON DELETE CASCADE,
    external_id   TEXT NOT NULL,                                    -- the bank's
    amount_minor  BIGINT NOT NULL,                                  -- negative: money out
    currency      TEXT NOT NULL,
    merchant      TEXT NOT NULL DEFAULT '',
    mcc           TEXT NOT NULL DEFAULT '',
    booked_at     TIMESTAMPTZ NOT NULL,
    imported_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (connection_id, external_id)
);
CREATE INDEX IF NOT EXISTS idx_bank_transactions_workspace ON bank_transactions (workspace_id, booked_at);
