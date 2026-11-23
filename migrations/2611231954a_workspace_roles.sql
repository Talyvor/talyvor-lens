-- B28.342: workspace roles beyond the owner — Viewer, Finance, Approver and Admin — enforced on wallet actions.
--
-- workspace_roles: a role the workspace's owner gave a person, named by their email. person_key is the hash of the
--                  identity (workspace.PersonKey) that first signed in to the workspace with that email; once set, only
--                  that person holds the role. Removing the row ends every session the role gave at once.

CREATE TABLE IF NOT EXISTS workspace_roles (
    workspace_id TEXT NOT NULL,
    email        TEXT NOT NULL CHECK (email = lower(email) AND email LIKE '%_@_%'),
    role         TEXT NOT NULL CHECK (role IN ('viewer', 'finance', 'approver', 'admin')),
    person_key   TEXT,
    granted_by   TEXT NOT NULL DEFAULT '',
    granted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, email)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_workspace_roles_person ON workspace_roles (workspace_id, person_key) WHERE person_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_workspace_roles_email ON workspace_roles (email);
