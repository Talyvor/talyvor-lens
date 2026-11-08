-- B56.22 — the fee on a loan arranged (fees.LendingFeeBPS, LENS_LENDING_FEE_BPS).
--
-- An offer records the lending fee's rate when it is made and the fee it comes to on the principal, so the
-- borrower sees the fee before answering and accepting takes exactly that. Accepting pays the principal out and
-- takes the fee from what the borrower received, as its own lxc_ledger row and agent posting (kind
-- lending_fee); the payout event names it. Offers made before this carry no fee.
ALTER TABLE agent_loans ADD COLUMN IF NOT EXISTS fee_bps INT NOT NULL DEFAULT 0 CHECK (fee_bps BETWEEN 0 AND 10000);
ALTER TABLE agent_loans ADD COLUMN IF NOT EXISTS fee_ulxc BIGINT NOT NULL DEFAULT 0 CHECK (fee_ulxc >= 0);
ALTER TABLE agent_loan_events ADD COLUMN IF NOT EXISTS fee_ulxc BIGINT NOT NULL DEFAULT 0;
