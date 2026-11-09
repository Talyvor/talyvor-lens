-- B57.10 — each agent's colour and face, which Lens chooses and keeps.
--
-- Seven colours by four shapes, the Pocket faces the apps draw (B57.6). An agent's pick comes from its id:
-- h = FNV-1a 32-bit of the id's UTF-8 bytes, colour = colours[h mod 7], face = faces[(h div 7) mod 4] — the same
-- pick the apps fall back on, so no face changes when these columns arrive. B57.6's test vectors:
--
--   agt_research → 3529705301 → mint hexagon
--   agt_sales    → 2871286680 → mint flower
--   agt_ops      → 3402803948 → rose squircle
--
-- Every existing agent is backfilled with exactly its pick. A new agent gets its pick unless a live agent of its
-- workspace (not archived, not a room's wallet, which draws the room's badge) already wears that pair; then the next
-- free pair in list order, colours first: pair n is colours[n mod 7], faces[n div 7], and the pick is pair h mod 28.
-- When all 28 are worn it keeps its pick. An insert that names a colour or a face keeps what it names.
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS colour TEXT;
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS face TEXT;

CREATE OR REPLACE FUNCTION agent_face_hash(id TEXT) RETURNS BIGINT
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    b BYTEA := convert_to(id, 'UTF8');
    h BIGINT := 2166136261;
BEGIN
    FOR i IN 0 .. length(b) - 1 LOOP
        h := ((h # get_byte(b, i)) * 16777619) % 4294967296;  -- < 2^56, so bigint never overflows
    END LOOP;
    RETURN h;
END;
$$;

UPDATE agent_accounts SET
    colour = (ARRAY['cobalt', 'orchid', 'sky', 'mint', 'lemon', 'rose', 'lime'])[agent_face_hash(id) % 7 + 1],
    face   = (ARRAY['circle', 'squircle', 'hexagon', 'flower'])[(agent_face_hash(id) / 7) % 4 + 1]
  WHERE colour IS NULL OR face IS NULL;

CREATE OR REPLACE FUNCTION agent_accounts_pick_face() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    colours TEXT[] := ARRAY['cobalt', 'orchid', 'sky', 'mint', 'lemon', 'rose', 'lime'];
    faces   TEXT[] := ARRAY['circle', 'squircle', 'hexagon', 'flower'];
    pick    INT := agent_face_hash(NEW.id) % 28;
    n       INT;
BEGIN
    IF NEW.colour IS NOT NULL AND NEW.face IS NOT NULL THEN
        RETURN NEW;
    END IF;
    FOR k IN 0 .. 27 LOOP
        n := (pick + k) % 28;
        IF NOT EXISTS (SELECT 1 FROM agent_accounts WHERE workspace_id = NEW.workspace_id AND archived_at IS NULL
                         AND kind = 'agent' AND colour = colours[n % 7 + 1] AND face = faces[n / 7 + 1]) THEN
            pick := n;
            EXIT;
        END IF;
    END LOOP;
    NEW.colour := COALESCE(NEW.colour, colours[pick % 7 + 1]);
    NEW.face := COALESCE(NEW.face, faces[pick / 7 + 1]);
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS agent_accounts_pick_face ON agent_accounts;
CREATE TRIGGER agent_accounts_pick_face BEFORE INSERT ON agent_accounts
    FOR EACH ROW EXECUTE FUNCTION agent_accounts_pick_face();

ALTER TABLE agent_accounts ALTER COLUMN colour SET NOT NULL;
ALTER TABLE agent_accounts ALTER COLUMN face SET NOT NULL;
ALTER TABLE agent_accounts DROP CONSTRAINT IF EXISTS agent_accounts_colour_check;
ALTER TABLE agent_accounts ADD CONSTRAINT agent_accounts_colour_check
    CHECK (colour IN ('cobalt', 'orchid', 'sky', 'mint', 'lemon', 'rose', 'lime'));
ALTER TABLE agent_accounts DROP CONSTRAINT IF EXISTS agent_accounts_face_check;
ALTER TABLE agent_accounts ADD CONSTRAINT agent_accounts_face_check
    CHECK (face IN ('circle', 'squircle', 'hexagon', 'flower'));
