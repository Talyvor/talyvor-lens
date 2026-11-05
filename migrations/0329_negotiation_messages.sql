-- 0329_negotiation_messages.sql — B52.12: Counter reaching the other side (internal/economy/negotiation_outreach.go).
--
-- negotiation_messages: the deal's timeline, per counterparty. direction 'out' is a message Counter sent, the first to
--                       each counterparty opening with the AI disclosure; 'in' is the counterparty's reply, threaded
--                       into the deal of the message it answers. address is where it went: a verified Talyvor agent's
--                       id or a web form's URL. to_workspace_id is the agent's workspace, which reads and answers the
--                       messages sent to it and nothing else of the deal; '' for a web form.
CREATE TABLE IF NOT EXISTS negotiation_messages (
    id              BIGSERIAL PRIMARY KEY,
    mandate_id      TEXT NOT NULL REFERENCES negotiation_mandates (id),
    workspace_id    TEXT NOT NULL,
    counterparty    TEXT NOT NULL,
    channel         TEXT NOT NULL,
    direction       TEXT NOT NULL CHECK (direction IN ('out', 'in')),
    address         TEXT NOT NULL,
    to_workspace_id TEXT NOT NULL DEFAULT '',
    body            TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_negotiation_messages_mandate ON negotiation_messages (mandate_id, id);
CREATE INDEX IF NOT EXISTS idx_negotiation_messages_workspace ON negotiation_messages (workspace_id);
CREATE INDEX IF NOT EXISTS idx_negotiation_messages_to ON negotiation_messages (to_workspace_id, id) WHERE to_workspace_id <> '';
