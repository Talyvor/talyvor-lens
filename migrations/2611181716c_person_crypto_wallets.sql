-- 2611181716c_person_crypto_wallets.sql — B48.32: an agent sends USDC to a person on Talyvor by their name or email, into the wallet they chose
-- (economy/person_crypto_wallets.go).
--
-- person_crypto_wallets: one row a person, network and token: the agent wallet of theirs that receives there.
--                        workspace_id is the agent's account, one of the person's own, checked when it is chosen and
--                        again at every send. A person with no row for a network is not found there.
-- agent_crypto_sends.to_hash: what the sender typed to find a person, kept only as a hash, so a retry under the same
--                        key is the same send; '' for a 0x address. to_kind 'person' is a send to a person found so.
CREATE TABLE IF NOT EXISTS person_crypto_wallets (
    person_id    TEXT NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    network      TEXT NOT NULL CHECK (network <> ''),
    token        TEXT NOT NULL CHECK (token = 'USDC'),
    agent_id     TEXT NOT NULL CHECK (agent_id <> ''),
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (person_id, network, token)
);
CREATE INDEX IF NOT EXISTS person_crypto_wallets_workspace ON person_crypto_wallets (workspace_id);

ALTER TABLE agent_crypto_sends ADD COLUMN IF NOT EXISTS to_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_crypto_sends DROP CONSTRAINT IF EXISTS agent_crypto_sends_to_kind_check;
ALTER TABLE agent_crypto_sends ADD CONSTRAINT agent_crypto_sends_to_kind_check CHECK (to_kind IN ('agent', 'address', 'person'));
