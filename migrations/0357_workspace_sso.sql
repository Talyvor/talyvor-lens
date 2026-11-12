-- B56.18 — single sign-on: a workspace's own identity provider (internal/sso).
--
--   workspace_sso_connections — one per workspace: the OIDC issuer, the client id, and the client secret sealed by
--                               internal/envelope under LENS_PROVIDER_SECRET_KEK. There is no plaintext secret column.
--   workspace_sso_domains     — the email domains the connection covers. A domain is proven when the TXT record
--                               _talyvor-sso.<domain> answers talyvor-sso=<token>. Any workspace may claim a domain;
--                               only one may prove it, so a claim nobody can prove blocks nobody.
CREATE TABLE IF NOT EXISTS workspace_sso_connections (
    workspace_id TEXT PRIMARY KEY,
    issuer       TEXT NOT NULL,
    client_id    TEXT NOT NULL,
    key_id       TEXT NOT NULL,
    wrapped_dek  BYTEA NOT NULL,
    dek_nonce    BYTEA NOT NULL,
    ciphertext   BYTEA NOT NULL,
    ct_nonce     BYTEA NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_workspace_sso_connections_key_id ON workspace_sso_connections (key_id);

CREATE TABLE IF NOT EXISTS workspace_sso_domains (
    workspace_id TEXT NOT NULL REFERENCES workspace_sso_connections (workspace_id) ON DELETE CASCADE,
    domain       TEXT NOT NULL,
    token        TEXT NOT NULL,
    proven_at    TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, domain)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_sso_domains_proven ON workspace_sso_domains (domain)
    WHERE proven_at IS NOT NULL;
