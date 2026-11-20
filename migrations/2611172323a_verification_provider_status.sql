-- B65.1 — a check's detail is in plain words; the provider's own word for where it stands (Persona's inquiry status:
-- created, pending, approved, declined, failed, expired, needs_review…) is kept beside it for support only, and never
-- shown to a person.
ALTER TABLE workspace_verifications ADD COLUMN IF NOT EXISTS provider_status TEXT NOT NULL DEFAULT '';
