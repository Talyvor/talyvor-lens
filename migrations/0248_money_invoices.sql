-- B30.20 — invoices and payment links.
--
-- An agent issues an invoice from one of its currency accounts: lines, a due date, optional VAT, and a pay link
-- (pay_token) that takes a card, a transfer to the company's account details quoting the invoice's reference, or a
-- Talyvor agent's payment. Each payment is one money entry (0221) into the issuing account, kept here; the invoice is
-- paid once its payments cover the total. Overdue is a sent invoice past its due date, read, not stored. A reminder
-- is sent once of each kind: before the due date when the issuer asked for one, and once overdue.

CREATE TABLE IF NOT EXISTS money_invoices (
    id                  TEXT PRIMARY KEY,                                 -- inv_<uuid>
    workspace_id        TEXT NOT NULL,                                    -- the issuer's
    agent_id            TEXT NOT NULL REFERENCES agent_accounts (id),
    account_id          TEXT NOT NULL,                                    -- the agent's account it is paid into
    currency            TEXT NOT NULL,
    number              INT NOT NULL CHECK (number > 0),                  -- the workspace's, from 1
    reference           TEXT NOT NULL UNIQUE,                             -- INV and twelve hex digits, quoted with a transfer
    pay_token           TEXT NOT NULL UNIQUE,                             -- the pay link's
    customer_name       TEXT NOT NULL,
    customer_email      TEXT NOT NULL DEFAULT '',
    customer_address    TEXT NOT NULL DEFAULT '',
    customer_vat_number TEXT NOT NULL DEFAULT '',
    seller_vat_number   TEXT NOT NULL DEFAULT '',
    lines               JSONB NOT NULL,                                   -- [{description, quantity, unit_amount_minor, vat_rate_bps, ...}]
    subtotal_minor      BIGINT NOT NULL CHECK (subtotal_minor > 0),
    vat_minor           BIGINT NOT NULL CHECK (vat_minor >= 0),
    total_minor         BIGINT NOT NULL CHECK (total_minor = subtotal_minor + vat_minor),
    due_date            DATE NOT NULL,
    remind_days_before  INT NOT NULL DEFAULT 0 CHECK (remind_days_before >= 0),
    memo                TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'sent', 'paid', 'void')),
    sent_at             TIMESTAMPTZ,
    paid_at             TIMESTAMPTZ,
    voided_at           TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (account_id, currency) REFERENCES money_accounts (id, currency),
    UNIQUE (workspace_id, number)
);
CREATE INDEX IF NOT EXISTS idx_money_invoices_workspace ON money_invoices (workspace_id, agent_id);
CREATE INDEX IF NOT EXISTS idx_money_invoices_open ON money_invoices (due_date) WHERE status = 'sent';

CREATE TABLE IF NOT EXISTS money_invoice_payments (
    invoice_id   TEXT NOT NULL REFERENCES money_invoices (id),
    entry_id     TEXT NOT NULL UNIQUE REFERENCES money_entries (id),       -- one payment pays one invoice
    method       TEXT NOT NULL CHECK (method IN ('card', 'transfer', 'agent')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    paid_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (invoice_id, entry_id)
);

CREATE TABLE IF NOT EXISTS money_invoice_reminders (
    invoice_id TEXT NOT NULL REFERENCES money_invoices (id),
    kind       TEXT NOT NULL CHECK (kind IN ('before_due', 'overdue')),
    sent_to    TEXT NOT NULL DEFAULT '',
    message    TEXT NOT NULL,
    sent_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (invoice_id, kind)
);

-- Card payments come in through Stripe: one partner account per workspace and currency holds what came that way.
CREATE UNIQUE INDEX IF NOT EXISTS uq_money_accounts_card_partner
    ON money_accounts (workspace_id, currency) WHERE purpose = 'partner' AND partner_account_ref = 'card:stripe';
