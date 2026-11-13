-- 0379: B59.30 — a Business workspace's details, ready for the partner's identity flow (launch pack 6 (4) and 3.2): the
-- company's name here, its registration number and country on workspaces (0374, the sign-up profile), and the people
-- with control, each with a name, a role and a share. Read and changed by the workspace's owner alone, each change in
-- the audit trail (audit_chain, kind workspace.business_details, without the people's names). A Personal workspace has
-- no row: the owner's route refuses it, and a workspace's type never changes (B59.6). No identity document is
-- collected or stored; the company check (L3) is handed these when it is not given its own.
CREATE TABLE IF NOT EXISTS workspace_business_details (
    workspace_id TEXT        PRIMARY KEY,
    company_name TEXT        NOT NULL CHECK (char_length(company_name) BETWEEN 1 AND 200),
    updated_by   TEXT        NOT NULL CHECK (updated_by <> ''),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- share_bps is the person's share of the company in hundredths of a percent (2500 is 25%), 0 for a director who holds
-- none; the shares of one company come to 10000 at most.
CREATE TABLE IF NOT EXISTS workspace_business_people (
    workspace_id TEXT     NOT NULL REFERENCES workspace_business_details (workspace_id) ON DELETE CASCADE,
    position     SMALLINT NOT NULL CHECK (position BETWEEN 0 AND 49),
    name         TEXT     NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    role         TEXT     NOT NULL CHECK (role IN ('director', 'shareholder', 'other')),
    share_bps    INTEGER  NOT NULL DEFAULT 0 CHECK (share_bps BETWEEN 0 AND 10000),
    PRIMARY KEY (workspace_id, position)
);
