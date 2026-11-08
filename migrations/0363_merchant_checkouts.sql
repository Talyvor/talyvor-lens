-- B30.38 — Talyvor Pay: a company accepts payments from any Talyvor agent.
--
-- A merchant company opens a checkout for an order: an amount in one of its currency accounts' currencies, its own order
-- id and where the payer returns to. An agent pays it from its own account in that currency, judged by its rules, and the
-- money settles into the merchant's company account less Talyvor's fee, which goes to revenue: one money entry (0221)
-- that both statements show. The merchant may refund a paid checkout, which reverses that entry in full.

CREATE TABLE IF NOT EXISTS merchant_checkouts (
    id                 TEXT PRIMARY KEY,                                  -- chk_<uuid>, which the merchant hands the paying agent
    workspace_id       TEXT NOT NULL,                                     -- the merchant's
    account_id         TEXT NOT NULL,                                     -- the merchant's company account it settles into
    currency           TEXT NOT NULL,
    amount_minor       BIGINT NOT NULL CHECK (amount_minor > 0),
    order_id           TEXT NOT NULL,                                     -- the merchant's own
    return_url         TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'paid', 'refunded')),
    fee_minor          BIGINT NOT NULL DEFAULT 0 CHECK (fee_minor >= 0 AND fee_minor <= amount_minor),
    funding            TEXT NOT NULL DEFAULT '',                          -- the payment's: test or live
    payer_workspace_id TEXT NOT NULL DEFAULT '',
    payer_agent_id     TEXT NOT NULL DEFAULT '',
    payment_entry_id   TEXT REFERENCES money_entries (id),
    refund_entry_id    TEXT REFERENCES money_entries (id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at            TIMESTAMPTZ,
    refunded_at        TIMESTAMPTZ,
    FOREIGN KEY (account_id, currency) REFERENCES money_accounts (id, currency),
    UNIQUE (workspace_id, order_id)
);
CREATE INDEX IF NOT EXISTS idx_merchant_checkouts_workspace ON merchant_checkouts (workspace_id, created_at);
