-- B61.35 — what an agent does, and the picture the apps draw it with.
--
-- role is one of the six kinds Talyvor offers (research, sales, bookkeeping, ads, travel, support), or other with the
-- owner's own word in role_word (at most 40 characters); '' for an agent made before kinds, which has none yet.
--
-- picture is an id from the list Lens holds (economy.AgentPictures). NULL is the picture of the agent's colour, so
-- every agent that exists keeps the face it has, and an agent recoloured without a picture takes its colour's. The
-- list grows with B61.36, so it is kept in Lens rather than in a CHECK here; colour keeps its own CHECK (0360) and
-- follows the picture. Neither column changes a rule, a limit or what the agent may do.
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS role_word TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS picture TEXT;

ALTER TABLE agent_accounts DROP CONSTRAINT IF EXISTS agent_accounts_role_check;
ALTER TABLE agent_accounts ADD CONSTRAINT agent_accounts_role_check CHECK (
    (role IN ('', 'research', 'sales', 'bookkeeping', 'ads', 'travel', 'support') AND role_word = '')
    OR (role = 'other' AND role_word <> '' AND char_length(role_word) <= 40));
