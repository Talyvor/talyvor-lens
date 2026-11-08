-- B28.452 — a chat shared from Chat as a link (talyvor-suite B28.127, apps/bff/chat_shares.go).
--
-- A private chat lives in its person's browser; sharing it hands Lens this copy: the title and each question and
-- answer's text, as they were when shared. The token is the whole credential a signed-out reader holds. Turning the
-- link off deletes the row, so the copy is gone and not only hidden.
CREATE TABLE IF NOT EXISTS chat_shares (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    token           TEXT NOT NULL UNIQUE,
    conversation_id TEXT NOT NULL,
    title           TEXT NOT NULL,
    messages        JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_chat_shares_workspace ON chat_shares (workspace_id, created_at DESC);
