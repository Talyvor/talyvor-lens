-- B28.140 / B17.189 — the marketplace bill by Stripe invoice.
--
-- Each paid marketplace invoice of a buyer's bill, recorded when it is paid (invoice.paid, or a test bill Lens paid
-- itself, B25.7): the billing period it charged for, its number and Stripe's PDF, so the bill is read by invoice rather
-- than by calendar month. What it charged and refunded is not kept here: it is read off the uses it cleared
-- (market_uses.cleared_invoice_id) and their refunds, so it cannot disagree with them.
CREATE TABLE IF NOT EXISTS market_invoices (
    id                 TEXT PRIMARY KEY,
    buyer_workspace_id TEXT NOT NULL,
    number             TEXT NOT NULL DEFAULT '',
    period_start       TIMESTAMPTZ NOT NULL,
    period_end         TIMESTAMPTZ NOT NULL,
    invoice_pdf        TEXT NOT NULL DEFAULT '',
    livemode           BOOLEAN NOT NULL,
    paid_at            TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_market_invoices_buyer ON market_invoices (buyer_workspace_id, period_end DESC);
