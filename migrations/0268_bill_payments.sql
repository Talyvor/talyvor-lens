-- B41.102 — pay all due: one batch of bills, one approval.
-- A payment request's item can name a bill (money_bills): it is paid to the bill's saved payee, its amount and
-- currency, the invoice number its reference. A bill is in at most one live request at a time, so no bill is paid
-- twice; once its item is paid the bill is 'paid'.
ALTER TABLE payment_request_items ADD COLUMN IF NOT EXISTS bill_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_payment_request_items_bill ON payment_request_items (bill_id) WHERE bill_id <> '';
