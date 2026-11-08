-- 0366_treasury_sweeps.sql — B30.59: a company account's sweep into a money-market fund.
--
-- One per company currency account: above target_minor of its funding (test or live), idle money moves each day into
-- the fund, opened at the broker partner (the Test broker until treasury_sweep is cleared) under the broker's
-- reference for it. What the fund holds is kept in the ledger in a company pot of its own (fund_account_id), so the
-- money stays the company's and is counted once; a withdrawal moves it back the same day. swept_on is the last day
-- the sweep ran, so it runs once a day.
CREATE TABLE IF NOT EXISTS treasury_sweeps (
    id                  TEXT PRIMARY KEY,
    workspace_id        TEXT NOT NULL,
    account_id          TEXT NOT NULL UNIQUE REFERENCES money_accounts (id),
    funding             TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    target_minor        BIGINT NOT NULL CHECK (target_minor >= 0),
    partner             TEXT NOT NULL,
    partner_account_ref TEXT NOT NULL,
    fund_account_id     TEXT NOT NULL REFERENCES money_accounts (id),
    swept_on            DATE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
