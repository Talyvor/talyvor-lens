-- 0391_person_accounts.sql — B61.7: one person holds several accounts.
--
-- A person is the identity the BFF presents to POST /v1/provision, kept here only as person_key, a hash of it
-- (workspace.PersonKey). The person's FIRST account is the workspace whose id provision derives from the identity, as
-- it always was, and has no row here; every account they open after it has one. Each is a workspace of its own, with
-- its own ledger, keys, rules and verification: this table is the only thing the accounts share.
CREATE TABLE IF NOT EXISTS person_accounts (
    workspace_id TEXT PRIMARY KEY CHECK (workspace_id <> ''),
    person_key   TEXT NOT NULL CHECK (person_key <> ''),
    opened_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_person_accounts_person ON person_accounts (person_key, opened_at);
