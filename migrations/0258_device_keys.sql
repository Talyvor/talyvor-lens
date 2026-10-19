-- B40.4 — device keys: the phone app's hardware key (a P-256 key in the Secure Enclave, StrongBox or a TEE) that
-- signs approvals and mandates as a passkey does. `attestation` is the App Attest object or Android certificate chain
-- it was registered with, kept as evidence. A revoked key's signatures are refused from `revoked_at` on; the row is
-- kept, so a workspace that ever had a signer never falls back to approving without one.
CREATE TABLE IF NOT EXISTS workspace_device_keys (
    id           TEXT PRIMARY KEY,                    -- dev_<uuid>
    workspace_id TEXT NOT NULL,
    person       TEXT NOT NULL DEFAULT '',            -- who registered it
    platform     TEXT NOT NULL CHECK (platform IN ('ios', 'android', 'test')),
    name         TEXT NOT NULL DEFAULT '',
    public_key   BYTEA NOT NULL,                      -- SubjectPublicKeyInfo DER, P-256
    attestation  BYTEA NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    revoked_by   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_workspace_device_keys_workspace ON workspace_device_keys (workspace_id);

-- A mandate signed by a device key names it here; credential_id and the WebAuthn fields are then ''.
ALTER TABLE permission_mandates ADD COLUMN IF NOT EXISTS device_id TEXT NOT NULL DEFAULT '';
