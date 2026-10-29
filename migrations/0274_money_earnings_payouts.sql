-- B46.11 — earnings paid out on the money layer: a marketplace sale to its seller, a Room's agreed split to each
-- member, a royalty to its creator.
--
-- money_payout_details is where a workspace is paid: an IBAN or a USDC address, one per asset, and the partner's
-- verification of the person it belongs to. A payout is made only to details whose verification passed.
CREATE TABLE IF NOT EXISTS money_payout_details (
    workspace_id TEXT NOT NULL,
    asset        TEXT NOT NULL CHECK (asset IN ('GBP', 'EUR', 'USD', 'USDC')),
    holder       TEXT NOT NULL,
    iban         TEXT NOT NULL DEFAULT '',
    bic          TEXT NOT NULL DEFAULT '',
    network      TEXT NOT NULL DEFAULT '',
    address      TEXT NOT NULL DEFAULT '',
    partner      TEXT NOT NULL,                           -- who verified the holder
    kyc_ref      TEXT NOT NULL,                           -- the partner's reference for the verification
    status       TEXT NOT NULL CHECK (status IN ('pending', 'verified', 'failed')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, asset),
    CHECK ((iban <> '') <> (address <> ''))               -- bank details or a chain address, never both
);

-- money_earnings_payouts is one payout of earnings: who pays (workspace_id, from agent_id's account or the company's),
-- why (kind and source: the sale, the Room and its terms, the royalty), under what agreement, to whom and how much.
-- One payout per source and recipient: asked again, it pays nothing twice. Its money is held from when it is made;
-- a payout to a recipient without verified details stays held until they have them.
CREATE TABLE IF NOT EXISTS money_earnings_payouts (
    id                     TEXT PRIMARY KEY,               -- mpo_…
    workspace_id           TEXT NOT NULL,
    agent_id               TEXT NOT NULL DEFAULT '',
    kind                   TEXT NOT NULL CHECK (kind IN ('sale', 'room_split', 'royalty')),
    source                 TEXT NOT NULL,
    agreement              TEXT NOT NULL DEFAULT '',
    recipient_workspace_id TEXT NOT NULL,
    share_bps              INTEGER NOT NULL DEFAULT 0 CHECK (share_bps BETWEEN 0 AND 10000),
    amount_minor           BIGINT NOT NULL CHECK (amount_minor > 0),
    currency               TEXT NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'USDC')),
    funding                TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    account_id             TEXT NOT NULL,
    hold_account_id        TEXT NOT NULL,
    status                 TEXT NOT NULL CHECK (status IN ('held', 'pending', 'completed', 'failed', 'returned')),
    partner                TEXT NOT NULL DEFAULT '',
    partner_ref            TEXT NOT NULL DEFAULT '',
    detail                 TEXT NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, kind, source, recipient_workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_money_earnings_payouts_recipient ON money_earnings_payouts (recipient_workspace_id, status);
CREATE INDEX IF NOT EXISTS idx_money_earnings_payouts_partner_ref ON money_earnings_payouts (partner, partner_ref) WHERE partner_ref <> '';
