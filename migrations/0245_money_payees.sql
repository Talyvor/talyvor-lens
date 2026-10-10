-- B30.16 — saved payees outside Talyvor, with confirmation of payee.
--
-- A payee is a person or company outside Talyvor a workspace pays: a name, one set of account details and a country.
-- check_result is what the payee's bank said of the name when the payee was saved (checked_by: the partner that
-- answered); a close match or no match is paid only once the owner has confirmed it with a passkey (confirmed_at,
-- confirmed_by: the passkey's credential id).

CREATE TABLE IF NOT EXISTS money_payees (
    id             TEXT PRIMARY KEY,                                  -- payee_<uuid>
    workspace_id   TEXT NOT NULL,
    name           TEXT NOT NULL CHECK (name <> ''),
    country        TEXT NOT NULL CHECK (country ~ '^[A-Z]{2}$'),
    sort_code      TEXT NOT NULL DEFAULT '',
    account_number TEXT NOT NULL DEFAULT '',
    iban           TEXT NOT NULL DEFAULT '',
    bic            TEXT NOT NULL DEFAULT '',
    routing_number TEXT NOT NULL DEFAULT '',
    check_result   TEXT NOT NULL CHECK (check_result IN ('exact_match', 'close_match', 'no_match')),
    suggested_name TEXT NOT NULL DEFAULT '',
    checked_by     TEXT NOT NULL,
    checked_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at   TIMESTAMPTZ,
    confirmed_by   TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_money_payees_workspace ON money_payees (workspace_id, created_at);
