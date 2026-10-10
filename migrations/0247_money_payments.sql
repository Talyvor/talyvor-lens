-- B30.17 — payments out to people and companies outside Talyvor.
--
-- A payment out pays a saved payee (B30.16) from a company's or an agent's currency account through the account
-- partner. Its money is held first (a hold entry); then, as the partner answers, it goes out (completed), comes back
-- (failed), or goes out and comes back (returned). value_ulxc is what the paying agent's rules counted it as — the
-- amount at the conversion partner's rate, in µLXC — and 0 for a payment from the company's own account.

CREATE TABLE IF NOT EXISTS money_payments (
    id              TEXT PRIMARY KEY,                                 -- mpay_<uuid>
    workspace_id    TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    account_id      TEXT NOT NULL REFERENCES money_accounts (id),     -- the account it is paid from
    hold_account_id TEXT NOT NULL REFERENCES money_accounts (id),     -- where its money waits for the partner
    agent_id        TEXT NOT NULL DEFAULT '',                         -- the agent whose account pays; '' for the company's
    payee_id        TEXT NOT NULL,                                    -- the payee (money_payees), kept if it is deleted
    payee_name      TEXT NOT NULL,
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    currency        TEXT NOT NULL,
    funding         TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    reference       TEXT NOT NULL DEFAULT '',                         -- what the payee sees
    value_ulxc      BIGINT NOT NULL DEFAULT 0 CHECK (value_ulxc >= 0),
    status          TEXT NOT NULL CHECK (status IN ('pending', 'sent', 'completed', 'failed', 'returned')),
    partner         TEXT NOT NULL DEFAULT '',
    partner_ref     TEXT NOT NULL DEFAULT '',
    detail          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_money_payments_workspace ON money_payments (workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_money_payments_agent ON money_payments (agent_id, created_at) WHERE agent_id <> '';

-- The approval an agent's payment out files names its payee as outside Talyvor.
ALTER TABLE agent_approvals DROP CONSTRAINT IF EXISTS agent_approvals_payee_kind_check;
ALTER TABLE agent_approvals ADD CONSTRAINT agent_approvals_payee_kind_check
    CHECK (payee_kind IN ('', 'agent', 'listing', 'company', 'merchant', 'outside'));
