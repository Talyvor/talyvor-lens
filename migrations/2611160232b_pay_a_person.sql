-- 2611160232b_pay_a_person.sql — B61.32: pay a person on Talyvor by their name, phone number or email (economy/people_found.go,
-- economy/person_payments.go).
--
-- people_found:    one row a person who has set how they are found. name is their Talyvor name, kept lower-case;
--                  name_key is it with look-alikes folded (0 and o, 1 and l, rn and m, vv and w, no dots or
--                  underscores), unique, so no two names read alike. email is the one the BFF confirmed, lower-case,
--                  one person an email. Each way of being found is its own switch, off until the person says yes.
--                  first_workspace_id and person_key are the person's first account and the key person_accounts holds
--                  for them, so their accounts are found from here. receive_workspace_id is the account they named to
--                  receive into, or ''.
-- person_lookups:  one row a lookup an account made, by find or by a payment, for the limit an hour.
-- person_payments: one row a payment to a person, append-only, written in the transaction of its money entry (id).
--                  It holds both sides' shown names, which each side's statement shows for the other, and the hash of
--                  what the sender typed to find them, so a retry under the same key is the same payment.
CREATE TABLE IF NOT EXISTS people_found (
    person_id            TEXT PRIMARY KEY REFERENCES people (id) ON DELETE CASCADE,
    person_key           TEXT NOT NULL CHECK (person_key <> ''),
    first_workspace_id   TEXT NOT NULL CHECK (first_workspace_id <> ''),
    name                 TEXT CHECK (name ~ '^[a-z0-9._]{3,20}$'),
    name_key             TEXT UNIQUE CHECK (name_key <> ''),
    email                TEXT UNIQUE CHECK (email <> ''),
    by_name              BOOLEAN NOT NULL DEFAULT false,
    by_phone             BOOLEAN NOT NULL DEFAULT false,
    by_email             BOOLEAN NOT NULL DEFAULT false,
    receive_workspace_id TEXT NOT NULL DEFAULT '',
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((name IS NULL) = (name_key IS NULL))
);

CREATE TABLE IF NOT EXISTS person_lookups (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS person_lookups_workspace_created ON person_lookups (workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS person_payments (
    id                TEXT PRIMARY KEY CHECK (id <> ''),
    entry_key         TEXT NOT NULL CHECK (entry_key <> ''),
    from_workspace_id TEXT NOT NULL CHECK (from_workspace_id <> ''),
    from_agent_id     TEXT NOT NULL DEFAULT '',
    from_account_id   TEXT NOT NULL CHECK (from_account_id <> ''),
    from_shown_name   TEXT NOT NULL CHECK (from_shown_name <> ''),
    to_person_id      TEXT NOT NULL CHECK (to_person_id <> ''),
    to_workspace_id   TEXT NOT NULL CHECK (to_workspace_id <> ''),
    to_account_id     TEXT NOT NULL CHECK (to_account_id <> ''),
    to_shown_name     TEXT NOT NULL CHECK (to_shown_name <> ''),
    to_hash           TEXT NOT NULL CHECK (to_hash <> ''),
    amount_minor      BIGINT NOT NULL CHECK (amount_minor > 0),
    currency          TEXT NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'USDC')),
    funding           TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    note              TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (from_workspace_id, entry_key)
);
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON person_payments
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON person_payments
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
CREATE INDEX IF NOT EXISTS person_payments_to_workspace ON person_payments (to_workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS person_payments_paid_before ON person_payments (from_workspace_id, from_agent_id, to_person_id);

-- An agent's first payment to a person files an approval naming them as a payee of their own kind.
ALTER TABLE agent_approvals DROP CONSTRAINT IF EXISTS agent_approvals_payee_kind_check;
ALTER TABLE agent_approvals ADD CONSTRAINT agent_approvals_payee_kind_check
    CHECK (payee_kind IN ('', 'agent', 'listing', 'company', 'merchant', 'outside', 'person'));
