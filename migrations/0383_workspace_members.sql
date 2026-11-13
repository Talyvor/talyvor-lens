-- B52.35 — a person joins another workspace by invite.
--
-- A workspace's owner is the identity it is derived from (POST /v1/provision); a member is another identity the owner
-- let in through an invite link. identity is the opaque identity provision takes, never an email. A member reads the
-- workspace and does nothing else: what a member may do beyond reading is Nick's decision. Removing a member stamps
-- removed_at, so the same identity may be invited back as a new row.
CREATE TABLE IF NOT EXISTS workspace_members (
    id           TEXT PRIMARY KEY,                       -- wsm_…
    workspace_id TEXT NOT NULL,
    identity     TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'member' CHECK (role = 'member'),
    added_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_workspace_members_live ON workspace_members (workspace_id, identity) WHERE removed_at IS NULL;
-- "the workspaces I am in".
CREATE INDEX IF NOT EXISTS idx_workspace_members_identity ON workspace_members (identity) WHERE removed_at IS NULL;

-- An invite link: an unguessable token kept only as its SHA-256, with an expiry and a use count, as B32.29's room
-- invite links. It admits identities until it is revoked, expires or is used up, and then answers 404.
CREATE TABLE IF NOT EXISTS workspace_member_invites (
    id           TEXT PRIMARY KEY,                       -- wsinv_…
    workspace_id TEXT NOT NULL,
    token_sha256 TEXT NOT NULL UNIQUE,
    max_uses     INTEGER NOT NULL CHECK (max_uses > 0),
    uses         INTEGER NOT NULL DEFAULT 0 CHECK (uses >= 0 AND uses <= max_uses),
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workspace_member_invites_ws ON workspace_member_invites (workspace_id, created_at DESC);
