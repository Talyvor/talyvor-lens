-- 0377: B59.27 — what a workspace asks Talyvor for: support, a complaint, closing the account, an export of its data,
-- or deleting it. Each is received, in progress or done, with when and by whom it was filed, and reaches the
-- operator's inbox (GET /v1/admin/requests). A complaint has its reference from the moment it is filed. A person at
-- Talyvor answers each one and moves its state (PUT /v1/admin/requests/{id}, recorded in operator_audit in the same
-- transaction): nothing else happens on its own.
CREATE TABLE IF NOT EXISTS workspace_requests (
    id           BIGSERIAL   PRIMARY KEY,
    workspace_id TEXT        NOT NULL,
    kind         TEXT        NOT NULL CHECK (kind IN ('support', 'complaint', 'account_closure', 'data_export', 'data_deletion')),
    state        TEXT        NOT NULL DEFAULT 'received' CHECK (state IN ('received', 'in_progress', 'done')),
    reference    TEXT        UNIQUE,
    filed_by     TEXT        NOT NULL CHECK (filed_by <> ''),
    message      TEXT        NOT NULL DEFAULT '' CHECK (char_length(message) <= 4000),
    answer       TEXT        NOT NULL DEFAULT '' CHECK (char_length(answer) <= 4000),
    filed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'complaint') = (reference IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_workspace_requests_workspace ON workspace_requests (workspace_id, filed_at DESC);
CREATE INDEX IF NOT EXISTS idx_workspace_requests_inbox ON workspace_requests (state, filed_at);
