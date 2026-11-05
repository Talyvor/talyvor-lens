-- 0306_negotiation_mandates.sql — B52.10: Counter's negotiation mandate the owner signs, and its term sheets
-- (internal/economy/negotiation_mandates.go).
--
-- negotiation_mandates: each mandate as the signed canonical JSON with the passkey or device key assertion, which every
--                       use checks again. deal_ref is the RFQ, auction or quote the mandate negotiates in (empty: none
--                       named); at most one live mandate holds a deal, across every workspace, so Counter never acts
--                       for both sides of one deal or for two bidders in one RFQ. ended_at is set on revocation and
--                       when a mandate past its deadline gives up its deal.
-- negotiation_terms:    the term sheet per counterparty: each offer and counter-offer as structured terms, ours with
--                       the outbound message, theirs as received.
CREATE TABLE IF NOT EXISTS negotiation_mandates (
    id                 TEXT PRIMARY KEY,
    workspace_id       TEXT NOT NULL,
    deal_ref           TEXT NOT NULL DEFAULT '',
    deadline           TIMESTAMPTZ NOT NULL,
    canonical          TEXT NOT NULL,
    credential_id      TEXT NOT NULL DEFAULT '',
    device_id          TEXT NOT NULL DEFAULT '',
    client_data_json   TEXT NOT NULL DEFAULT '',
    authenticator_data TEXT NOT NULL DEFAULT '',
    signature          TEXT NOT NULL,
    created_by         TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at           TIMESTAMPTZ,
    ended_by           TEXT NOT NULL DEFAULT '',
    CHECK ((credential_id = '') <> (device_id = ''))
);
CREATE INDEX IF NOT EXISTS idx_negotiation_mandates_workspace ON negotiation_mandates (workspace_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_negotiation_mandates_live_deal ON negotiation_mandates (deal_ref)
    WHERE deal_ref <> '' AND ended_at IS NULL;

CREATE TABLE IF NOT EXISTS negotiation_terms (
    id           BIGSERIAL PRIMARY KEY,
    mandate_id   TEXT NOT NULL REFERENCES negotiation_mandates (id),
    workspace_id TEXT NOT NULL,
    counterparty TEXT NOT NULL,
    party        TEXT NOT NULL CHECK (party IN ('ours', 'theirs')),
    channel      TEXT NOT NULL,
    terms        JSONB NOT NULL,
    message      TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_negotiation_terms_mandate ON negotiation_terms (mandate_id, id);
CREATE INDEX IF NOT EXISTS idx_negotiation_terms_workspace ON negotiation_terms (workspace_id);
