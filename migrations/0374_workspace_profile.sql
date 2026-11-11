-- B59.40 — Lens keeps the sign-up profile: a workspace's country, and a business's registration number (launch pack 3.2).
--
-- Both are set on the call that makes the workspace (/v1/provision), like account_type, and "" means not given. They are
-- profile, not identity checks: no provider is called and no verification level changes. country is an ISO 3166-1
-- alpha-2 code; registration_number is a business's alone, at most 64 characters.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS country TEXT NOT NULL DEFAULT ''
    CHECK (country = '' OR country ~ '^[A-Z]{2}$');
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS registration_number TEXT NOT NULL DEFAULT ''
    CHECK (char_length(registration_number) <= 64 AND (registration_number = '' OR account_type = 'business'));
