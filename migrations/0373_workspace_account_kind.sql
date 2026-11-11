-- B59.6 — a workspace is Business or Personal, and Fiat only, Stablecoin and crypto only, or Hybrid (launch pack 3.1).
--
-- account_type is who the account is for: business or personal. It is set when the workspace is made and never changes.
-- money_kind is the money it holds: fiat, stablecoin or hybrid. It is set when the workspace is made and changes only
-- as launch pack 3.5 says (internal/economy/workspace_money_kind.go): a single kind becomes hybrid at any time, hybrid
-- becomes a single kind only once the other side holds nothing.
--
-- The kind is the limit for its agents' wallet kinds (B48.1): fiat allows a fiat agent, stablecoin a crypto agent,
-- hybrid any of the three. Every workspace made before this becomes business and hybrid, so nothing it uses today goes.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS account_type TEXT NOT NULL DEFAULT 'business'
    CHECK (account_type IN ('business', 'personal'));
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS money_kind TEXT NOT NULL DEFAULT 'hybrid'
    CHECK (money_kind IN ('fiat', 'stablecoin', 'hybrid'));
