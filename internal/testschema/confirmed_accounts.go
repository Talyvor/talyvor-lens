package testschema

// ConfirmedAccountsSQL makes a test database's accounts confirm who they are when they first hold money: B61.52 refuses
// every movement of money, Sandbox money included, to an account below the level its type needs, and the tests of
// everything else that moves money are about accounts that have passed it — as the nightly testers' people pass it in
// a run's set-up (B61.51). The first agent, credit balance or money account a workspace gets brings the Test provider's
// passes at L1, L2 and L3, unless the test recorded checks of its own first. Test passes count for nothing live, so
// live money is judged as before. The gate itself is proven on databases without this: economy's
// identity_gate_realpg_test.go and cmd/lens's b6152 tests.
const ConfirmedAccountsSQL = `
CREATE OR REPLACE FUNCTION test_confirmed_on_first_money() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref)
	SELECT NEW.workspace_id, l.level, l.subject, 'test', 'completed', 'test-fixture-' || NEW.workspace_id || '-' || l.level
	FROM (VALUES (1, 'contact'), (2, 'person'), (3, 'company')) AS l(level, subject)
	WHERE NOT EXISTS (SELECT 1 FROM workspace_verifications v WHERE v.workspace_id = NEW.workspace_id);
	RETURN NEW;
END $$;
CREATE OR REPLACE TRIGGER test_confirmed_on_first_money AFTER INSERT ON agent_accounts
	FOR EACH ROW EXECUTE FUNCTION test_confirmed_on_first_money();
CREATE OR REPLACE TRIGGER test_confirmed_on_first_money AFTER INSERT ON lxc_balances
	FOR EACH ROW EXECUTE FUNCTION test_confirmed_on_first_money();
CREATE OR REPLACE TRIGGER test_confirmed_on_first_money AFTER INSERT ON money_accounts
	FOR EACH ROW EXECUTE FUNCTION test_confirmed_on_first_money();
`
