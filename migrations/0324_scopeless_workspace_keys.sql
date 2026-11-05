-- 0324: B50.33 — a key with no scopes passes no scope gate any more (auth.RequireScope).
--
-- Workspace keys minted before scopes existed carry an empty set, and the only gate that set ever
-- passed was the proxy one (RequireScope(proxy) is the sole production RequireScope). Give them
-- exactly that, so a key that worked yesterday keeps working and none keeps a wildcard.
UPDATE workspace_api_keys SET scopes = ARRAY['proxy']::TEXT[] WHERE cardinality(scopes) = 0;
