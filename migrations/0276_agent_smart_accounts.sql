-- B48.2 — each crypto agent's wallet is its own smart account (internal/money/smartaccount), owned by the person.
--
-- Its owners are the person's passkey (one of the workspace's passkeys or device keys; its P-256 public key, x‖y, is
-- kept here because the account's address is worked out from it), the agent's key, which the wallet provider made and
-- holds in its enclave (only its address is here), and the recovery signer the person may add (an address). On Solana
-- the agent's wallet is a Solana account the same provider made and holds in its enclave (only its address is here).
-- Nothing in this table can sign: there is no private key, seed or key share in it, nor anywhere else in Lens.
CREATE TABLE IF NOT EXISTS agent_smart_accounts (
    agent_id            TEXT PRIMARY KEY,
    workspace_id        TEXT NOT NULL,
    address             TEXT NOT NULL,                -- EIP-55; the same on every chain the factory is on
    factory             TEXT NOT NULL,
    implementation      TEXT NOT NULL,
    owner_credential_id TEXT NOT NULL DEFAULT '',     -- the person's passkey (workspace_passkeys), or
    owner_device_id     TEXT NOT NULL DEFAULT '',     -- their phone's device key (workspace_device_keys)
    owner_public_key    BYTEA NOT NULL CHECK (length(owner_public_key) = 64),
    agent_signer        TEXT NOT NULL,                -- the address of the agent's key in the provider's enclave
    provider            TEXT NOT NULL,
    recovery_address    TEXT NOT NULL DEFAULT '',
    solana_address      TEXT NOT NULL,                -- the agent's Solana wallet at the provider
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((owner_credential_id = '') <> (owner_device_id = ''))
);
CREATE INDEX IF NOT EXISTS idx_agent_smart_accounts_workspace ON agent_smart_accounts (workspace_id);
