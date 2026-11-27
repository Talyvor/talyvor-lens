-- B17.564 — a payout batch states the fee of the payments it approves.
--
-- Each valid row is priced when the batch arrives as its payment out will be charged: free while the plan's local
-- payments this month last, then the plan's fee. rows keeps each row's fee_minor; fees_minor is their sum by currency,
-- beside totals_minor (the rows' amounts). The funds check counts them, an approval is signed over them, and a row is
-- never paid at a fee above the one it was approved at.

ALTER TABLE money_payout_batches ADD COLUMN IF NOT EXISTS fees_minor JSONB NOT NULL DEFAULT '{}';
