-- B41.103 — invoice risk checks on every bill and payment.
-- Each item of a payment request carries the flags it was judged with, for the approval card: the same invoice number
-- or amount already paid to this payee ('duplicate'), bank details on the bill that differ from the saved payee's
-- ('bank_details_changed'), the first payment to this payee ('first_payment'), and an amount far above this payee's
-- usual ('unusual_amount'). Each is {"code", "detail"}.
ALTER TABLE payment_request_items ADD COLUMN IF NOT EXISTS flags JSONB NOT NULL DEFAULT '[]'::jsonb;
