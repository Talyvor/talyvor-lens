-- B40.3 — prepared payment requests: what an assistant or an agent prepared for the person to approve, never paid here.
-- `canonical` is the frozen payload — each item's payee, listing, currency, amount and reference — and `payload_hash`
-- its SHA-256; an approval (B40.5) signs that hash. `args_hash` is the SHA-256 of the arguments the Idempotency-Key was
-- first used with: the same key with other arguments is refused. Each item keeps how it was judged and by which
-- mandate's rule; auto items count towards that mandate's limits until the request is cancelled.
CREATE TABLE IF NOT EXISTS payment_requests (
    id              TEXT PRIMARY KEY,                                 -- preq_<uuid>
    workspace_id    TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    args_hash       TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('payment', 'batch', 'marketplace_purchase')),
    client          TEXT NOT NULL,
    agent_id        TEXT NOT NULL DEFAULT '',
    canonical       TEXT NOT NULL,
    payload_hash    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('pending_approval', 'auto', 'blocked', 'cancelled')),
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    cancelled_at    TIMESTAMPTZ,
    cancelled_by    TEXT NOT NULL DEFAULT '',
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_payment_requests_workspace ON payment_requests (workspace_id, created_at);

CREATE TABLE IF NOT EXISTS payment_request_items (
    request_id   TEXT NOT NULL REFERENCES payment_requests (id),
    position     INTEGER NOT NULL,
    payee_id     TEXT NOT NULL,                                       -- a saved payee, or the seller's workspace
    payee_name   TEXT NOT NULL DEFAULT '',
    listing_id   TEXT NOT NULL DEFAULT '',
    currency     TEXT NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    reference    TEXT NOT NULL DEFAULT '',
    decision     TEXT NOT NULL CHECK (decision IN ('auto', 'needs_approval', 'blocked')),
    reason       TEXT NOT NULL DEFAULT '',
    rule         TEXT NOT NULL DEFAULT '',
    mandate_id   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id, position)
);
CREATE INDEX IF NOT EXISTS idx_payment_request_items_mandate ON payment_request_items (mandate_id, currency, created_at) WHERE decision = 'auto';
