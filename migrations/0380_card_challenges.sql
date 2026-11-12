-- 0380: B59.22 — 3-D Secure. A merchant online may ask the card's issuer to have the cardholder confirm a payment; for
-- an agent's card that is its owner. The issuer's challenge is kept here, once under its own id, and filed as an
-- approval of its own kind (agent_approvals.kind = 'card_challenge'), decided only by the owner's signed answer. The
-- answer goes back to the issuer; no answer by the issuer's deadline is a no (timed_out). answered is whether the
-- issuer was told, so an answer it did not take is sent again.
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '' CHECK (kind IN ('', 'card_challenge'));

CREATE TABLE IF NOT EXISTS money_card_challenges (
    partner      TEXT        NOT NULL,                              -- the issuer's adapter name
    id           TEXT        NOT NULL CHECK (id <> ''),             -- the issuer's
    workspace_id TEXT        NOT NULL,
    agent_id     TEXT        NOT NULL,
    card_id      TEXT        NOT NULL,                              -- the one-time (money_cards) or standing (money_reusable_cards) card
    merchant     TEXT        NOT NULL CHECK (merchant <> ''),
    amount_minor BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency     TEXT        NOT NULL,
    approval_id  TEXT        NOT NULL UNIQUE,
    status       TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'declined', 'timed_out')),
    answered     BOOLEAN     NOT NULL DEFAULT false,
    expires_at   TIMESTAMPTZ NOT NULL,                              -- the issuer's deadline
    decided_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (partner, id)
);
CREATE INDEX IF NOT EXISTS idx_money_card_challenges_due ON money_card_challenges (expires_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_money_card_challenges_unanswered ON money_card_challenges (decided_at) WHERE status <> 'pending' AND NOT answered;
