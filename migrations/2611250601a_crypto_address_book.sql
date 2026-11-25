-- B48.49: the crypto addresses a workspace's owner saved and named, which an agent's send may name instead of the 0x
-- address. A name and an address are each saved once per workspace, whatever their case.
CREATE TABLE IF NOT EXISTS workspace_crypto_addresses (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    name         TEXT NOT NULL CHECK (name <> ''),
    address      TEXT NOT NULL CHECK (address ~ '^0x[0-9a-fA-F]{40}$'),
    saved_by     TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_crypto_addresses_name ON workspace_crypto_addresses (workspace_id, lower(name));
CREATE UNIQUE INDEX IF NOT EXISTS workspace_crypto_addresses_address ON workspace_crypto_addresses (workspace_id, lower(address));
