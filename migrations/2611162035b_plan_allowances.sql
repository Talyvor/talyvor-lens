-- B61.58: fees follow the plan: what is free each month, then the fee (economy/plan_allowances.go).
--
-- plan_allowance_uses: one row per move that used an account's monthly allowance, written in the transaction that
--   moves the money under a lock on the account's allowance.
--   allowance:   fx (a conversion, or an Instant transfer's), local (a payment out in its own currency) or international.
--   ref:         the conversion, Instant transfer or payment that used it.
--   currency:    the currency moved; fx is counted per currency sold.
--   amount:      fx the minor units converted free; a payment 1.
--   month:       the first day of the calendar month, in UTC, the move was made in.
--   released_at: when it was given back: the payment failed or came back, or the transfer was reversed.
CREATE TABLE IF NOT EXISTS plan_allowance_uses (
    workspace_id TEXT NOT NULL,
    allowance    TEXT NOT NULL CHECK (allowance IN ('fx', 'local', 'international')),
    ref          TEXT NOT NULL,
    currency     TEXT NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'USDC')),
    amount       BIGINT NOT NULL CHECK (amount > 0),
    month        DATE NOT NULL CHECK (month = date_trunc('month', month)::date),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at  TIMESTAMPTZ,
    PRIMARY KEY (allowance, ref)
);
CREATE INDEX IF NOT EXISTS plan_allowance_uses_month ON plan_allowance_uses (workspace_id, allowance, month) WHERE released_at IS NULL;

-- The part of a quote's amount sold that converts free; the margin is on the rest. A conversion made at it takes
-- the allowance only if it is still there.
ALTER TABLE money_conversions ADD COLUMN IF NOT EXISTS free_minor BIGINT NOT NULL DEFAULT 0 CHECK (free_minor >= 0);
ALTER TABLE instant_transfer_quotes ADD COLUMN IF NOT EXISTS free_minor BIGINT NOT NULL DEFAULT 0 CHECK (free_minor >= 0);
