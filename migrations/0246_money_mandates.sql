-- B30.19 — mandates: a payee may pull money within limits the owner set.
--
-- An agent's account grants a mandate to one payee — another Talyvor company, or an outside business by its id — with a
-- maximum per pull, a maximum per calendar month and an expiry; it is revoked once and stays revoked. An outside
-- business pulls with the mandate's key, of which only the SHA-256 is kept, and what it pulls waits in the mandate's
-- hold account to be paid out to it. Each pull is one money entry (0221), kept here under the payee's idempotency key
-- so a retried pull is the same pull.

CREATE TABLE IF NOT EXISTS money_mandates (
    id                  TEXT PRIMARY KEY,                                 -- mdt_<uuid>
    workspace_id        TEXT NOT NULL,                                    -- the payer's
    agent_id            TEXT NOT NULL REFERENCES agent_accounts (id),
    account_id          TEXT NOT NULL,                                    -- the agent's account the payee pulls from
    currency            TEXT NOT NULL,
    payee_workspace_id  TEXT,                                             -- another Talyvor company
    payee_business_id   TEXT,                                             -- or an outside business, by its id
    payee_name          TEXT NOT NULL DEFAULT '',
    pull_key_sha256     TEXT NOT NULL DEFAULT '',                         -- an outside business's key
    hold_account_id     TEXT REFERENCES money_accounts (id),              -- where an outside business's pulls wait
    max_per_pull_minor  BIGINT NOT NULL CHECK (max_per_pull_minor > 0),
    max_per_month_minor BIGINT NOT NULL CHECK (max_per_month_minor >= max_per_pull_minor),
    expires_at          TIMESTAMPTZ NOT NULL,
    revoked_at          TIMESTAMPTZ,
    revoked_by          TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (account_id, currency) REFERENCES money_accounts (id, currency),
    CHECK ((payee_workspace_id IS NULL) <> (payee_business_id IS NULL)),
    CHECK (payee_business_id IS NULL OR (pull_key_sha256 <> '' AND hold_account_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_money_mandates_workspace ON money_mandates (workspace_id, agent_id);
CREATE INDEX IF NOT EXISTS idx_money_mandates_payee ON money_mandates (payee_workspace_id) WHERE payee_workspace_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS money_mandate_pulls (
    mandate_id      TEXT NOT NULL REFERENCES money_mandates (id),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    entry_id        TEXT NOT NULL REFERENCES money_entries (id),
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    pulled_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (mandate_id, idempotency_key)
);
