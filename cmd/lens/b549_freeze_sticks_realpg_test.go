package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// B54.9 — a freeze sticks: an agent whose on-chain revoke calls cannot be built is still frozen and refused at the gate
// (the build failure is reported on the freeze); an audit failure rolls the freeze back, so the retry is a fresh freeze
// and writes agent.frozen; and a lockdown whose client hung up still freezes every agent.
func TestB549_AFreezeSticksEvenWhenItsRevokeCallsCannotBeBuilt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b549", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetSmartAccounts(cdpfake.New(t).Signers(t), "coinbase", smartaccount.CoinbaseV11)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path string, body any, out any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		return w.Code
	}
	sign := b5018Passkey(t, store, ws, rpID, origin)
	keys, err := store.ListPasskeys(ctx, ws)
	if err != nil || len(keys) != 1 {
		t.Fatalf("passkeys = %v %v", keys, err)
	}
	var ids []string
	for _, name := range []string{"Trader", "Buyer", "Seller"} {
		var agent economy.Agent
		if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents", map[string]string{"name": name, "wallet_kind": "crypto"}, &agent); code != http.StatusCreated {
			t.Fatalf("create agent = %d", code)
		}
		base := "/v1/workspaces/" + ws + "/agents/" + agent.ID
		var ch struct{ Challenge string }
		if code := call(http.MethodPost, base+"/smart-account/challenge", map[string]string{"credential_id": keys[0].CredentialID}, &ch); code != http.StatusOK {
			t.Fatalf("smart account challenge = %d", code)
		}
		if code := call(http.MethodPost, base+"/smart-account", map[string]any{"credential_id": keys[0].CredentialID, "assertion": sign(ch.Challenge)}, nil); code != http.StatusOK {
			t.Fatalf("smart account = %d", code)
		}
		ids = append(ids, agent.ID)
	}
	slices.Sort(ids)
	broken, audited, hungUp := ids[0], ids[1], ids[2]
	gate := func(agent string) error {
		return store.AuthoriseAgentCryptoPayment(ctx, ws, agent, smartaccount.Payment{Token: "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
			To: "0x000000000000000000000000000000000000b0b0", Amount: big.NewInt(1)})
	}
	frozenRecords := func(agent string) (n int) {
		rec, err := store.AuditRecord(ctx, ws, "agent:"+agent)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range rec {
			if e.Kind == "agent.frozen" {
				n++
			}
		}
		return n
	}

	// 1. The revoke calls cannot be built (a key it rotated from has no index): the freeze still holds, says why the
	// calls are missing, and the gate refuses.
	if _, err := pool.Exec(ctx, `UPDATE agent_smart_accounts SET rotated_from_signers = ARRAY['0x000000000000000000000000000000000000dead'],
		rotated_from_indexes = '{}' WHERE agent_id = $1`, broken); err != nil {
		t.Fatal(err)
	}
	var f economy.AgentFreeze
	if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+broken+"/freeze", nil, &f); code != http.StatusOK {
		t.Fatalf("freeze with unbuildable revoke calls = %d, want 200: the freeze holds", code)
	}
	if f.FrozenAt.IsZero() || f.RevokeCallsError == "" {
		t.Fatalf("freeze = %+v, want frozen with the revoke calls' failure named", f)
	}
	if err := gate(broken); !errors.Is(err, smartaccount.ErrFrozen) {
		t.Fatalf("agent whose revoke calls cannot be built: gate = %v, want frozen", err)
	}
	if n := frozenRecords(broken); n != 1 {
		t.Fatalf("agent.frozen entries = %d, want 1", n)
	}

	// 2. The audit entry cannot be written: the freeze rolls back with it (reported failed and truly open), and the
	// retry is a fresh freeze that writes agent.frozen.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION b549_refuse_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.kind = 'agent.frozen' THEN RAISE EXCEPTION 'b549: audit down'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER b549_refuse_audit BEFORE INSERT ON audit_chain FOR EACH ROW EXECUTE FUNCTION b549_refuse_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FreezeAgent(ctx, ws, audited, "owner"); err == nil {
		t.Fatal("freeze with the audit down answered no error")
	}
	if err := gate(audited); errors.Is(err, smartaccount.ErrFrozen) {
		t.Fatal("a freeze reported failed reads frozen: its retry would never write agent.frozen")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER b549_refuse_audit ON audit_chain`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FreezeAgent(ctx, ws, audited, "owner"); err != nil {
		t.Fatalf("retried freeze: %v", err)
	}
	if n := frozenRecords(audited); n != 1 {
		t.Fatalf("agent.frozen entries after the retry = %d, want 1", n)
	}

	// 3. The client hangs up before the lockdown runs: every agent is frozen anyway.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	l, err := store.LockdownAgents(gone, ws, "owner")
	if err != nil || len(l.Failed) != 0 || len(l.Frozen) != 3 {
		t.Fatalf("lockdown on a cancelled request = %+v %v, want all three frozen", l, err)
	}
	if err := gate(hungUp); !errors.Is(err, smartaccount.ErrFrozen) {
		t.Fatalf("agent after a lockdown whose client hung up: gate = %v, want frozen", err)
	}
}
