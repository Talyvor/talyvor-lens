-- 0343_approval_paid_from.sql — B57.11: an approval says what pays it and the budget left after
-- (internal/economy/agent_rules.go, fileApproval).
--
-- paid_from_kind:         lxc (the agent's LXC balance), account (one of its currency accounts), marketplace_bill (the
--                         company's monthly marketplace bill) or card (its card); NULL on a row filed before this.
-- paid_from_account_id:   the agent, the money account or the card that pays.
-- paid_from_currency:     the currency the payer is charged in, and paid_from_amount_minor that charge in its minor
--                         units, so an invoice of $340.00 from a dollar account keeps $340.00 while amount_ulxc keeps the
--                         3,400 LXC its rules judged.
-- paid_from_funding:      test or live, for an account: whose balance the sheet shows.
-- asks_above_ulxc:        the approval amount the rules asked above when it was filed.
-- budget_left_after_ulxc: the monthly limit less the month's spend and this amount when it was filed; NULL with no
--                         monthly limit.
ALTER TABLE agent_approvals
    ADD COLUMN IF NOT EXISTS paid_from_kind TEXT CHECK (paid_from_kind IN ('lxc', 'account', 'marketplace_bill', 'card')),
    ADD COLUMN IF NOT EXISTS paid_from_account_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS paid_from_currency TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS paid_from_amount_minor BIGINT,
    ADD COLUMN IF NOT EXISTS paid_from_funding TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS asks_above_ulxc BIGINT,
    ADD COLUMN IF NOT EXISTS budget_left_after_ulxc BIGINT;
