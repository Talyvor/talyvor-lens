-- 0321_crypto_payment_requests.sql — B48.10: asking to be paid into an agent's crypto wallet.
--
-- A request is an amount of a token on one network, paid into the agent's smart account (pay_to), with a memo and an
-- expiry; its link, EIP-681 URI and x402 requirement are worked out from these columns. Where it stands — paid, partly
-- paid, overpaid, expired — is worked out from the deposits matched to it, one row each: a chain movement pays at
-- most one request, and a reorg that takes the deposit back deletes its row. A late one (seen after the expiry) is
-- kept to show and does not count.
CREATE TABLE IF NOT EXISTS crypto_payment_requests (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    agent_id        TEXT NOT NULL,
    network         TEXT NOT NULL,
    chain_id        BIGINT NOT NULL,
    token           TEXT NOT NULL,
    asset           TEXT NOT NULL,                -- the token's contract, EIP-55
    pay_to          TEXT NOT NULL,                -- the agent's smart account, EIP-55
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    memo            TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ NOT NULL,
    payer_address   TEXT NOT NULL DEFAULT '',     -- when set, only its payments count
    approval_id     TEXT NOT NULL DEFAULT '',     -- the approval filed in the payer's workspace, when the payer is a Talyvor agent
    overpayment     TEXT NOT NULL DEFAULT '' CHECK (overpayment IN ('', 'refund', 'keep')),
    created_by      TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_crypto_payment_requests_agent ON crypto_payment_requests (workspace_id, agent_id, network);

CREATE TABLE IF NOT EXISTS crypto_payment_request_payments (
    movement_id  TEXT PRIMARY KEY,               -- chain_movements.id
    request_id   TEXT NOT NULL REFERENCES crypto_payment_requests (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    late         BOOLEAN NOT NULL,
    matched_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_crypto_payment_request_payments_request ON crypto_payment_request_payments (request_id);

-- A request to be paid lands in the payer agent's owner's approvals: what is asked, in its own currency, and the trust
-- Talyvor has in the agent asking.
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS payment_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS amount_minor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS trust TEXT NOT NULL DEFAULT '';
