-- B17.206 — the first version of an approved listing the review holds. It and every later version wait for an admin; the
-- versions before it stay live, so a use pinned to one, or an unpinned use, still runs. NULL: no version is held.
ALTER TABLE market_listings ADD COLUMN IF NOT EXISTS held_version INTEGER;
