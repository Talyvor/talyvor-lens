-- B48.42: the ramp's reference for the execution of a top-up or cash-out, as the ramp reported it — the transaction a
-- top-up's delivery is matched by. '' until the ramp has executed it, or when it reported none.
ALTER TABLE wallet_moves ADD COLUMN IF NOT EXISTS ramp_ref TEXT NOT NULL DEFAULT '';
