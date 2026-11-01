-- 0296_connector_grants.sql — B41.178: add a listing to Claude, ChatGPT or Cursor through the Talyvor connector.
--
-- An assistant registers itself (OAuth dynamic client registration) as a connector_clients row, then sends its user to
-- /oauth/authorize, which opens a connector_grants row. The owner approves it on Talyvor's connect page for one of
-- their agents; the grant then holds the hash of a single-use code, which the assistant exchanges, with its PKCE
-- verifier, for that agent's own key. workspace_id and agent_id stay NULL until the owner decides.

CREATE TABLE IF NOT EXISTS connector_clients (
    client_id     TEXT PRIMARY KEY,
    client_name   TEXT NOT NULL,
    redirect_uris TEXT[] NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS connector_grants (
    id             TEXT PRIMARY KEY,
    client_id      TEXT NOT NULL REFERENCES connector_clients (client_id),
    redirect_uri   TEXT NOT NULL,
    code_challenge TEXT NOT NULL,
    state          TEXT NOT NULL DEFAULT '',
    resource       TEXT NOT NULL DEFAULT '',
    listing_id     TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'denied', 'used')),
    workspace_id   TEXT,
    agent_id       TEXT,
    code_hash      TEXT UNIQUE,
    key_id         TEXT,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS connector_grants_workspace ON connector_grants (workspace_id) WHERE workspace_id IS NOT NULL;
