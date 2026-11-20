-- B63.1: the payments pool, measured only (internal/paypool). It reads payments and writes its own tables; it moves
-- no money, changes no payment, balance, fee or decision, and none of it is shown to a customer.
--
-- payments_pool_assumed_costs: what a first payment to a payee is ASSUMED to cost, for the test only, per currency in
--   its minor units: a name check, a screening and an outside transfer. None of it is a fee anyone is charged.
--   Nicolai fills it; a currency with no row assumes 0 for each.
CREATE TABLE IF NOT EXISTS payments_pool_assumed_costs (
    currency               TEXT PRIMARY KEY CHECK (currency IN ('GBP', 'EUR', 'USD', 'USDC')),
    name_check_minor       BIGINT NOT NULL DEFAULT 0 CHECK (name_check_minor >= 0),
    screening_minor        BIGINT NOT NULL DEFAULT 0 CHECK (screening_minor >= 0),
    outside_transfer_minor BIGINT NOT NULL DEFAULT 0 CHECK (outside_transfer_minor >= 0),
    assumed                BOOLEAN NOT NULL DEFAULT true CHECK (assumed),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- payments_pool_settings: the share of a cost avoided that a rule would give the first payer, in basis points.
--   One row; none means 0.
CREATE TABLE IF NOT EXISTS payments_pool_settings (
    id                BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    royalty_share_bps INTEGER NOT NULL DEFAULT 0 CHECK (royalty_share_bps BETWEEN 0 AND 10000),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- payments_pool_measures: one row a settled Sandbox payment out or payment to a person, append-only, written after it
--   settles and never in its transaction.
--   payment_kind, payment_id:  payment_out (money_payments.id) or person_payment (its money entry's id).
--   payee_hash:                the payee's own details hashed (a person: their person id), never its name.
--   kind:                      first (no account had paid this payee), own_repeat (this owner had), others_repeat
--                              (another owner paid it first and 3 or more owners have), others_below_floor (another
--                              owner paid it first but fewer than 3 have), inside (the payee holds a Talyvor account).
--   first_payer_workspace_id:  the account that paid this payee first, '' for a first; who a royalty would go to.
--   owners:                    how many separate owners have paid this payee, this payment's owner included.
--   ruled_out:                 the identity graph found the first payer is this payment's owner under another account.
--   cost_avoided_minor:        the assumed cost a first would have paid and this did not, in currency.
--   would_royalty_minor:       what a rule would have given the first payer; nothing is owed and nothing moves.
CREATE TABLE IF NOT EXISTS payments_pool_measures (
    id                       TEXT PRIMARY KEY,
    payment_kind             TEXT NOT NULL CHECK (payment_kind IN ('payment_out', 'person_payment')),
    payment_id               TEXT NOT NULL,
    workspace_id             TEXT NOT NULL,
    payee_hash               TEXT NOT NULL,
    kind                     TEXT NOT NULL CHECK (kind IN ('first', 'own_repeat', 'others_repeat', 'others_below_floor', 'inside')),
    first_payer_workspace_id TEXT NOT NULL DEFAULT '',
    owners                   INTEGER NOT NULL CHECK (owners > 0),
    ruled_out                BOOLEAN NOT NULL DEFAULT false,
    currency                 TEXT NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'USDC')),
    cost_avoided_minor       BIGINT NOT NULL DEFAULT 0 CHECK (cost_avoided_minor >= 0),
    would_royalty_minor      BIGINT NOT NULL DEFAULT 0 CHECK (would_royalty_minor >= 0 AND would_royalty_minor <= cost_avoided_minor),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (payment_kind, payment_id)
);
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON payments_pool_measures
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON payments_pool_measures
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
CREATE INDEX IF NOT EXISTS payments_pool_measures_payee ON payments_pool_measures (payee_hash, created_at);
CREATE INDEX IF NOT EXISTS payments_pool_measures_week ON payments_pool_measures (created_at);
