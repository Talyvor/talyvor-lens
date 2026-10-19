-- B40.2 — permission mandates: what a person allowed an assistant or an agent to prepare and pay for them, signed by
-- them. The mandate itself is `canonical`, the exact JSON the person's passkey signed (its SHA-256 is the WebAuthn
-- challenge); the assertion is kept so the signature is checked again on every evaluation. Only the revocation is
-- outside what was signed.
CREATE TABLE IF NOT EXISTS permission_mandates (
    id                 TEXT PRIMARY KEY,
    workspace_id       TEXT NOT NULL,
    agent_id           TEXT NOT NULL DEFAULT '',
    canonical          TEXT NOT NULL,
    credential_id      TEXT NOT NULL,
    client_data_json   TEXT NOT NULL,
    authenticator_data TEXT NOT NULL,
    signature          TEXT NOT NULL,
    created_by         TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at         TIMESTAMPTZ,
    revoked_by         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_permission_mandates_workspace ON permission_mandates (workspace_id, created_at);
