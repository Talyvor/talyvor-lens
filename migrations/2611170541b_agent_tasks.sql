-- B62.2: the task an agent gives with a payment (economy/agent_task.go): one line in the agent's own words of what the
-- payment is for, at most 200 characters, key-shaped strings cut. It is what the agent says: no rule reads it, and it is
-- no part of an approval's fingerprint. '' for none.
--
-- agent_approvals.task: on the approval the payment filed, as the owner is shown it.
-- agent_postings.task:  on the paying agent's posting of the payment, never the payee's.
-- money_entries.task:   on the money entry of a payment from a currency account, in the paying workspace.
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS task TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_postings ADD COLUMN IF NOT EXISTS task TEXT NOT NULL DEFAULT '';
ALTER TABLE money_entries ADD COLUMN IF NOT EXISTS task TEXT NOT NULL DEFAULT '';
