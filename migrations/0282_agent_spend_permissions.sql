-- B48.3 — the owner's limits on what a crypto agent's key may pay, signed by them, and an instant freeze
-- (internal/economy/agent_spend_permissions.go).
--
-- Each permission the person signs is kept with its number (one after the one it replaces) and the assertion by the
-- passkey or device key that owns the agent's smart account, which the gate checks again before every payment. Each
-- payment the gate allows is kept, so the period's allowance counts it. A frozen agent's key signs nothing.
CREATE TABLE IF NOT EXISTS agent_spend_permissions (
    agent_id           TEXT NOT NULL,
    number             BIGINT NOT NULL CHECK (number > 0),
    workspace_id       TEXT NOT NULL,
    canonical          TEXT NOT NULL,
    credential_id      TEXT NOT NULL DEFAULT '',
    device_id          TEXT NOT NULL DEFAULT '',
    client_data_json   TEXT NOT NULL DEFAULT '',
    authenticator_data TEXT NOT NULL DEFAULT '',
    signature          TEXT NOT NULL,
    signed_by          TEXT NOT NULL,
    signed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, number),
    CHECK ((credential_id = '') <> (device_id = ''))
);
CREATE INDEX IF NOT EXISTS idx_agent_spend_permissions_workspace ON agent_spend_permissions (workspace_id);

CREATE TABLE IF NOT EXISTS agent_spend_permission_uses (
    id                BIGSERIAL PRIMARY KEY,
    agent_id          TEXT NOT NULL,
    workspace_id      TEXT NOT NULL,
    permission_number BIGINT NOT NULL,
    token             TEXT NOT NULL,              -- EIP-55; the chain's coin is 0xEeee…EEeE
    counterparty      TEXT NOT NULL,
    amount            BIGINT NOT NULL CHECK (amount > 0), -- the token's base units
    used_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_spend_permission_uses_period ON agent_spend_permission_uses (agent_id, token, used_at);
CREATE INDEX IF NOT EXISTS idx_agent_spend_permission_uses_workspace ON agent_spend_permission_uses (workspace_id);

ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS frozen_at TIMESTAMPTZ;
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS frozen_by TEXT NOT NULL DEFAULT '';
