-- B28.315: above a second, higher amount a Business's payment needs two approvers (economy/second_approver.go).
--
-- workspaces.second_approver_user_id:     the person the line names, by their user id; '' with no second approver.
-- workspaces.second_approval_above_minor: the amount above which a payment needs them, in second_approval_currency's
--                                         minor units; 0 with no second approver.
-- workspaces.second_approver_set_by/_at:  who last set or cleared it, and when.
-- agent_approvals.first_approved_by:      who gave the first of an approval's two approvals; '' until then. It stays
--                                         pending until the second.
-- agent_approvals.second_approved_by:     who gave the second.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS second_approver_user_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS second_approval_above_minor BIGINT NOT NULL DEFAULT 0 CHECK (second_approval_above_minor >= 0),
    ADD COLUMN IF NOT EXISTS second_approval_currency TEXT NOT NULL DEFAULT '' CHECK (second_approval_currency IN ('', 'GBP', 'EUR', 'USD', 'USDC')),
    ADD COLUMN IF NOT EXISTS second_approver_set_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS second_approver_set_at TIMESTAMPTZ;

ALTER TABLE agent_approvals
    ADD COLUMN IF NOT EXISTS first_approved_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS first_approved_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS second_approved_by TEXT NOT NULL DEFAULT '';
