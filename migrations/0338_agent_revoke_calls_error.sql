-- 0338_agent_revoke_calls_error.sql — B54.25: a freeze's revoke calls that could not be built are stored and retried.
--
-- revoke_calls_error: why the calls that take a frozen agent's key out of its account on chain could not be built, in
--                     words the owner reads on the agent; '' when they were. A job retries every frozen agent with one
--                     and alerts the operator while any remain (economy.RetryRevokeCalls).
ALTER TABLE agent_smart_accounts ADD COLUMN IF NOT EXISTS revoke_calls_error TEXT NOT NULL DEFAULT '';
