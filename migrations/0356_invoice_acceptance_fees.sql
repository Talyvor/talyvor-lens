-- B56.21 — the fees for accepting payments.
--
-- Each payment on an invoice (0248) is charged Talyvor's fee for accepting it, as its own money entry from the issuing
-- account: fee_minor is what was charged. A card payment names its Stripe payment intent, which a refund names; what
-- has come back of it is returned_minor, and the same share of its fee was given back.
ALTER TABLE money_invoice_payments
    ADD COLUMN IF NOT EXISTS fee_minor      BIGINT NOT NULL DEFAULT 0 CHECK (fee_minor >= 0 AND fee_minor <= amount_minor),
    ADD COLUMN IF NOT EXISTS payment_intent TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS returned_minor BIGINT NOT NULL DEFAULT 0 CHECK (returned_minor >= 0 AND returned_minor <= amount_minor);

CREATE INDEX IF NOT EXISTS idx_money_invoice_payments_intent ON money_invoice_payments (payment_intent) WHERE payment_intent <> '';
