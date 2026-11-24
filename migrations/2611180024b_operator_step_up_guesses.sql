-- B28.461 — a step-up code cannot be guessed.
--
-- operator_step_up_guesses is the one wrong-code count for the whole step-up (the secret is one, so the count is one,
-- whatever operator is named): the wrong codes since window_start, and the wait, once five arrive inside ten minutes,
-- until locked_until. A single row, read FOR UPDATE, so the count holds on every replica.

CREATE TABLE IF NOT EXISTS operator_step_up_guesses (
    id           BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    wrong        INTEGER NOT NULL DEFAULT 0 CHECK (wrong >= 0),
    window_start TIMESTAMPTZ,
    locked_until TIMESTAMPTZ
);
