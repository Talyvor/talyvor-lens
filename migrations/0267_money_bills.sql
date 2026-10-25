-- B41.101 — bills a workspace receives: an uploaded or photographed invoice becomes a payable bill.
--
-- A bill is what reading an invoice found — supplier, amount, currency, due date, invoice number and the bank
-- details printed on it — and the saved payee (money_payees) it was matched to. payee_match is 'matched' (a saved
-- payee with these bank details, or this name when the invoice prints none), 'bank_details_differ' (a saved payee by
-- this name, whose saved details differ from the invoice's — the saved ones are what is paid), or 'new_payee'. The
-- invoice's own bank details are never paid on their own: they become a payee only through the payee check (B30.16).
-- One invoice is one bill: the same document, or the same supplier's same invoice number, finds the bill already
-- there.

CREATE TABLE IF NOT EXISTS money_bills (
    id              TEXT PRIMARY KEY,                                  -- bill_<uuid>
    workspace_id    TEXT NOT NULL,
    source          TEXT NOT NULL CHECK (source IN ('upload', 'photo', 'email')),
    document_sha256 TEXT NOT NULL,
    filename        TEXT NOT NULL DEFAULT '',
    supplier        TEXT NOT NULL DEFAULT '',
    invoice_number  TEXT NOT NULL DEFAULT '',
    amount_minor    BIGINT NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    currency        TEXT NOT NULL DEFAULT '',
    due_date        DATE,
    sort_code       TEXT NOT NULL DEFAULT '',
    account_number  TEXT NOT NULL DEFAULT '',
    iban            TEXT NOT NULL DEFAULT '',
    bic             TEXT NOT NULL DEFAULT '',
    routing_number  TEXT NOT NULL DEFAULT '',
    payee_id        TEXT NOT NULL DEFAULT '',                          -- the saved payee; '' for a new payee
    payee_match     TEXT NOT NULL CHECK (payee_match IN ('matched', 'bank_details_differ', 'new_payee')),
    status          TEXT NOT NULL DEFAULT 'due' CHECK (status IN ('due', 'paid')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, document_sha256)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_money_bills_invoice ON money_bills (workspace_id, lower(supplier), invoice_number)
    WHERE invoice_number <> '';
CREATE INDEX IF NOT EXISTS idx_money_bills_due ON money_bills (workspace_id, status, due_date);
