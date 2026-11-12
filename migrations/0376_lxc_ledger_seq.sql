-- B58.4 — the Ledger lists rows in the order their balances were written.
--
-- created_at is the writing transaction's start (NOW()), but balance_after is written under the lxc_balances
-- row lock, in commit order. Two overlapping writes therefore listed in the opposite order to the balances
-- they wrote, and a spend and its fee shared one timestamp. seq is a per-workspace counter taken from
-- lxc_balances.ledger_seq under that same row lock, on every insert, so it runs in balance order. Every reader
-- orders by it.
ALTER TABLE lxc_balances ADD COLUMN IF NOT EXISTS ledger_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE lxc_ledger ADD COLUMN IF NOT EXISTS seq BIGINT;

-- Rows written before this migration are numbered by (created_at, id), the best order they have. lxc_ledger is
-- append-only (0055): the trigger is lifted for this one backfill, inside the migration's transaction.
ALTER TABLE lxc_ledger DISABLE TRIGGER audit_no_mutation;
UPDATE lxc_ledger l SET seq = o.seq
FROM (SELECT id, workspace_id, row_number() OVER (PARTITION BY workspace_id ORDER BY created_at, id) AS seq
      FROM lxc_ledger) o
WHERE l.id = o.id AND l.workspace_id = o.workspace_id AND l.seq IS NULL;
ALTER TABLE lxc_ledger ENABLE TRIGGER audit_no_mutation;

UPDATE lxc_balances b SET ledger_seq = m.seq
FROM (SELECT workspace_id, max(seq) AS seq FROM lxc_ledger GROUP BY workspace_id) m
WHERE b.workspace_id = m.workspace_id;

ALTER TABLE lxc_ledger ALTER COLUMN seq SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS lxc_ledger_workspace_seq ON lxc_ledger (workspace_id, seq);

-- A row that arrives without a seq takes the next one from its balance row, locking it; the writers already
-- hold that lock while they compute balance_after, so the two run in the same order. A workspace with no
-- balance row yet gets one, numbered after any rows it already has. The one writer that supplies seq itself
-- is the bulk grant (economy.GrantLXCUpTo), which moves the balance row in the same statement.
CREATE OR REPLACE FUNCTION lxc_ledger_next_seq() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.seq IS NOT NULL THEN
        RETURN NEW;
    END IF;
    UPDATE lxc_balances SET ledger_seq = ledger_seq + 1
    WHERE workspace_id = NEW.workspace_id
    RETURNING ledger_seq INTO NEW.seq;
    IF NOT FOUND THEN
        INSERT INTO lxc_balances AS b (workspace_id, balance, lifetime_minted, lifetime_spent, ledger_seq)
        VALUES (NEW.workspace_id, 0, 0, 0,
                1 + COALESCE((SELECT max(seq) FROM lxc_ledger WHERE workspace_id = NEW.workspace_id), 0))
        ON CONFLICT (workspace_id) DO UPDATE SET ledger_seq = b.ledger_seq + 1
        RETURNING b.ledger_seq INTO NEW.seq;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE TRIGGER lxc_ledger_seq BEFORE INSERT ON lxc_ledger
    FOR EACH ROW EXECUTE FUNCTION lxc_ledger_next_seq();
