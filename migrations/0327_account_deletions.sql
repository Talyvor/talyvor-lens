-- 0327_account_deletions.sql — B52.37: a person deletes their account.
--
-- One row per workspace whose person deleted their account, and when. Kept when everything else of the
-- workspace goes: it is the record that the deletion was done, and the session tokens issued to that
-- workspace before deleted_at are refused against it (a token carries no row of its own to delete). A
-- person who signs up again later gets a NEW workspace id (the identity's next generation, cmd/lens
-- currentWorkspaceID), so nothing the deleted workspace kept for money's sake is ever theirs again.

CREATE TABLE IF NOT EXISTS account_deletions (
    workspace_id TEXT PRIMARY KEY,
    deleted_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
