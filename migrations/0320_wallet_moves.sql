-- 0320_wallet_moves.sql — B48.8: topping up and cashing out an agent's wallet.
--
-- Each top-up or cash-out is quoted first — what it sells, the partner's fee, the rate and what it buys — and then
-- confirmed once: the confirmation posts the money_entries row named here, and a retry finds it posted. route is how
-- the money comes or goes: wallet (from the owner's wallet or another of their agents'), card or bank_transfer
-- (bought through the ramp), convert (from a fiat wallet through the ramp) or cash_out (sold through the ramp to the
-- owner's checked bank account, payee_id). from_ref and to_ref are the partner's references for the wallets it moves
-- between, and pay_in where a wallet pays the ramp, so the money layer lets through only this move. stage is how far a
-- quoted move got: '' nothing asked, 'approval' waiting for the owner with nothing moved (priced again if the quote ran
-- out), 'started' a partner step began — such a move is replayed to its end, whatever its quote's expiry says.
-- Once money has left, a refusal never fails a move: it is 'held' (held_at), where the owner and the operators see it,
-- until it is confirmed again to its end ('posted') or the ramp gives the money back ('returned', nothing posted).
-- gated_at is when the agent's rules allowed a cash-out's pay-in, so a replay of it is the same payment, not judged
-- again; paid_in_at when the money layer answered that the wallet's pay-in arrived at the ramp, before which nothing
-- is given back; executed_at when the ramp took the move, after which its money cannot be returned.
CREATE TABLE IF NOT EXISTS wallet_moves (
    id              TEXT PRIMARY KEY,                       -- wmove_<uuid>
    workspace_id    TEXT NOT NULL,
    agent_id        TEXT NOT NULL,                          -- the agent whose wallet is topped up or cashed out
    route           TEXT NOT NULL CHECK (route IN ('wallet', 'card', 'bank_transfer', 'convert', 'cash_out')),
    from_account_id TEXT NOT NULL DEFAULT '',               -- the money account money leaves; '' when it comes from outside
    to_account_id   TEXT NOT NULL DEFAULT '',               -- the money account money arrives in; '' when it leaves Talyvor
    from_ref        TEXT NOT NULL DEFAULT '',
    to_ref          TEXT NOT NULL DEFAULT '',
    payee_id        TEXT NOT NULL DEFAULT '',
    sell_minor      BIGINT NOT NULL CHECK (sell_minor > 0),
    sell_currency   TEXT NOT NULL,
    fee_minor       BIGINT NOT NULL CHECK (fee_minor >= 0),
    buy_minor       BIGINT NOT NULL CHECK (buy_minor > 0),
    buy_currency    TEXT NOT NULL,
    rate            TEXT NOT NULL,
    partner         TEXT NOT NULL DEFAULT '',
    quote_ref       TEXT NOT NULL DEFAULT '',
    pay_in_name     TEXT NOT NULL DEFAULT '',
    pay_in_address  TEXT NOT NULL DEFAULT '',
    partner_ref     TEXT NOT NULL DEFAULT '',
    funding         TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    status          TEXT NOT NULL DEFAULT 'quoted' CHECK (status IN ('quoted', 'posted', 'failed', 'held', 'returned')),
    stage           TEXT NOT NULL DEFAULT '' CHECK (stage IN ('', 'approval', 'started')),
    detail          TEXT NOT NULL DEFAULT '',
    entry_id        TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ,
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at       TIMESTAMPTZ,
    gated_at        TIMESTAMPTZ,
    paid_in_at      TIMESTAMPTZ,
    executed_at     TIMESTAMPTZ,
    held_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS wallet_moves_agent ON wallet_moves (workspace_id, agent_id, created_at);
CREATE INDEX IF NOT EXISTS wallet_moves_held ON wallet_moves (held_at) WHERE status = 'held';
