-- 0341_negotiation_deals.sql — B52.13: Counter's close (internal/economy/negotiation_deals.go).
--
-- negotiation_deals: each deal Counter closed or asked the owner to sign — the counterparty's exact terms as accepted,
--                    why it is beyond the mandate (outside, empty when inside), who pays whom, and the owner's signed
--                    statement when they approved it. request_id is the payment request (B40.3) that pays it, set only
--                    once the deal is accepted. offer_id names the term-sheet line accepted, with no foreign key: the
--                    terms are kept here, and the term sheet is deleted with an account while this is retained. A
--                    negotiation has at most one deal that is not declined.
CREATE TABLE IF NOT EXISTS negotiation_deals (
    id                 TEXT PRIMARY KEY,
    mandate_id         TEXT NOT NULL REFERENCES negotiation_mandates (id),
    workspace_id       TEXT NOT NULL,
    counterparty       TEXT NOT NULL,
    offer_id           BIGINT NOT NULL,
    terms              JSONB NOT NULL,
    outside            JSONB NOT NULL DEFAULT '[]',
    agent_id           TEXT NOT NULL DEFAULT '',
    payee_id           TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL CHECK (status IN ('accepted', 'awaiting_approval', 'declined')),
    statement          TEXT NOT NULL DEFAULT '',
    credential_id      TEXT NOT NULL DEFAULT '',
    device_id          TEXT NOT NULL DEFAULT '',
    client_data_json   TEXT NOT NULL DEFAULT '',
    authenticator_data TEXT NOT NULL DEFAULT '',
    signature          TEXT NOT NULL DEFAULT '',
    request_id         TEXT NOT NULL DEFAULT '',
    created_by         TEXT NOT NULL,
    decided_by         TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at         TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_negotiation_deals_live ON negotiation_deals (mandate_id) WHERE status <> 'declined';
CREATE INDEX IF NOT EXISTS idx_negotiation_deals_workspace ON negotiation_deals (workspace_id);
