-- B30.15 — receive money from outside.
--
-- Money in that names no open account — details whose company account is no longer open, or a quoted agent payment
-- reference that names no open agent account — goes to the workspace's suspense account in its currency. Each
-- workspace has at most one open suspense account in each currency, opened the first time unmatched money arrives.

CREATE UNIQUE INDEX IF NOT EXISTS uq_money_accounts_suspense_currency
    ON money_accounts (workspace_id, currency) WHERE purpose = 'suspense' AND status <> 'closed';
