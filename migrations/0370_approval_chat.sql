-- B56.16 — Slack and Teams approvals (internal/approvalchat).
--
-- A workspace on a plan with slack_teams_approvals keeps one incoming webhook, Slack's or Teams', and Lens posts each
-- approval filed there with a button that opens it in Talyvor. The webhook's URL is its credential: it is sealed like a
-- provider key (internal/envelope, the workspace and kind as aad) and never shown back; host is all an answer names.
CREATE TABLE IF NOT EXISTS workspace_approval_chat (
    workspace_id TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('slack', 'teams')),
    host         TEXT NOT NULL,
    key_id       TEXT NOT NULL,
    wrapped_dek  BYTEA NOT NULL,
    dek_nonce    BYTEA NOT NULL,
    ciphertext   BYTEA NOT NULL,
    ct_nonce     BYTEA NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_workspace_approval_chat_key_id ON workspace_approval_chat (key_id);
