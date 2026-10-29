-- B46.22 — the marketplace's weekly payout run pays a seller who saved payout details on the money layer
-- (money_payout_details, B46.11) through the money layer's payouts instead of a Stripe transfer.
--
-- A market_payouts row of method 'money' is that payout: asset is what it is paid in, royalty_usd_micros the part of it
-- the seller's lineage royalties earned (paid as a royalty, the rest as a sale). It has no Stripe account and no
-- Stripe fees; paid_at is set once the money layer has taken it (economy.PayEarnings), which pays it, or holds it
-- until the seller's payout details are verified. A seller is paid in money once a week, by Stripe or the money
-- layer, never both. Its journal entry moves the payout from the seller's available balance to money:clearing.
ALTER TABLE market_payouts ADD COLUMN IF NOT EXISTS asset TEXT NOT NULL DEFAULT '';
ALTER TABLE market_payouts ADD COLUMN IF NOT EXISTS royalty_usd_micros BIGINT NOT NULL DEFAULT 0 CHECK (royalty_usd_micros >= 0);

ALTER TABLE market_payouts DROP CONSTRAINT IF EXISTS market_payouts_method_check;
ALTER TABLE market_payouts ADD CONSTRAINT market_payouts_method_check CHECK (method IN ('stripe', 'credits', 'money'));
ALTER TABLE market_payouts DROP CONSTRAINT IF EXISTS market_payouts_check2;
ALTER TABLE market_payouts ADD CONSTRAINT market_payouts_check2 CHECK (method <> 'credits' OR (paid_at IS NOT NULL AND credits_ulxc > 0));
ALTER TABLE market_payouts DROP CONSTRAINT IF EXISTS market_payouts_money;
ALTER TABLE market_payouts ADD CONSTRAINT market_payouts_money CHECK (
    (method = 'money') = (asset IN ('GBP', 'EUR', 'USD', 'USDC'))
    AND (method = 'money' OR royalty_usd_micros = 0)
    AND royalty_usd_micros <= gross_usd_micros
    AND (method <> 'money' OR account_fee_usd_micros + payout_fee_usd_micros = 0));

DROP INDEX IF EXISTS idx_market_payouts_weekly;
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_payouts_weekly ON market_payouts (workspace_id, period)
    WHERE method IN ('stripe', 'money') AND period <> '';

ALTER TABLE market_journal_postings DROP CONSTRAINT IF EXISTS market_journal_postings_account_check;
ALTER TABLE market_journal_postings ADD CONSTRAINT market_journal_postings_account_check
    CHECK (account IN ('stripe:clearing', 'revenue:market_fee', 'rounding:stripe', 'stripe:connect_fees', 'credits:issued',
                       'trial:waived', 'trial:market_fee', 'money:clearing')
           OR account ~ '^seller:.+:(holdback|available)$'
           OR account ~ '^trial:seller:.+$'
           OR account ~ '^tax:[A-Z]{2}(:input)?$');
