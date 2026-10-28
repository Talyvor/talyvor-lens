-- B45.29 — an outside agent's payment request keeps who Talyvor took the asker to be (agentsig.Result, B45.24): its
-- trust level, the words the approval card shows, the signing domain and the checked business. NULL for a request
-- made inside the workspace.
ALTER TABLE payment_requests ADD COLUMN IF NOT EXISTS agent_trust JSONB;
