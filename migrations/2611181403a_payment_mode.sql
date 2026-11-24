-- B62.7 — each payment's record says whether it was Sandbox or Live.
--
-- mode is kept when the row is written, from the label (B56.7) of the capability and rail that moved the money at that
-- moment, and from the money's own funding where the payment names it: a payment of test money is Sandbox whatever the
-- labels say. Labels are worked out at read time and change when a rail goes live, so a record that did not keep its
-- mode could not say afterwards which it was.
--
-- Rows written before this were read as Sandbox by the partner's view (B62.5), which refused to answer while anything
-- was Live; they keep that reading. Nothing but Lens's own writes sets mode, and the CHECK holds it to the two words.
-- payment_proofs stays append-only: adding a column with a default rewrites no row through the triggers.

ALTER TABLE payment_proofs ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'sandbox'
    CHECK (mode IN ('sandbox', 'live'));
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'sandbox'
    CHECK (mode IN ('sandbox', 'live'));
