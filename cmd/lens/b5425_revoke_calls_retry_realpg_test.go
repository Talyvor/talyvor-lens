package main

import (
	"context"
	"encoding/json"
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

// B54.25 — a freeze's revoke calls that could not be built are stored, retried and raised: a lockdown with one
// unbuildable revoke answers 207, the owner sees on the agent what is wrong with its record, the retry leaves it
// outstanding while the record is wrong, and once the record is fixed the retry builds the calls and clears it.
func TestB5425_UnbuildableRevokeCallsAreStoredRetriedAndRaised(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b5425", "app.talyvor.com", "https://app.talyvor.com"
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
	for _, name := range []string{"Trader", "Buyer"} {
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
	broken := ids[0]
	onAgent := func() string {
		var smart economy.AgentSmartAccount
		if code := call(http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+broken+"/smart-account", nil, &smart); code != http.StatusOK {
			t.Fatalf("smart account = %d", code)
		}
		return smart.RevokeCallsError
	}
	lockdown := func() (int, economy.Lockdown) {
		var l economy.Lockdown
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/lockdown", nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
			t.Fatalf("lockdown: %v in %s", err, w.Body.String())
		}
		return w.Code, l
	}

	// 1. One agent's record of the keys it rotated from is wrong: the lockdown freezes both, answers 207, and names
	// what is wrong on that agent's freeze and on the agent itself.
	if _, err := pool.Exec(ctx, `UPDATE agent_smart_accounts SET rotated_from_signers = ARRAY['0x000000000000000000000000000000000000dead'],
		rotated_from_indexes = '{}' WHERE agent_id = $1`, broken); err != nil {
		t.Fatal(err)
	}
	code, l := lockdown()
	if code != http.StatusMultiStatus || len(l.Frozen) != 2 || len(l.Failed) != 0 {
		t.Fatalf("lockdown with one unbuildable revoke = %d %+v, want 207 with both frozen", code, l)
	}
	if l.Frozen[0].AgentID != broken || !strings.Contains(l.Frozen[0].RevokeCallsError, "record") || l.Frozen[1].RevokeCallsError != "" {
		t.Fatalf("lockdown's freezes = %+v, want only %s's revoke calls named as failed, by its record", l.Frozen, broken)
	}
	if strings.Contains(l.Frozen[0].RevokeCallsError, "freeze it again to retry") {
		t.Fatalf("revoke calls error = %q: freezing again cannot fix a wrong record", l.Frozen[0].RevokeCallsError)
	}
	if got := onAgent(); got != l.Frozen[0].RevokeCallsError {
		t.Fatalf("agent's revoke calls error = %q, want the stored %q", got, l.Frozen[0].RevokeCallsError)
	}

	// 2. The retry leaves it outstanding while the record is still wrong, and the operator is told once.
	sink := &b5425Sink{}
	told := retryRevokeCallsOnce(ctx, store, sink, "")
	if retryRevokeCallsOnce(ctx, store, sink, told); len(sink.alerts) != 1 || !strings.Contains(sink.alerts[0], ws+", agent "+broken) {
		t.Fatalf("operator alerts = %q, want one naming %s", sink.alerts, broken)
	}
	if got := onAgent(); got == "" {
		t.Fatal("the retry cleared a failure whose record is still wrong")
	}

	// 3. The record is fixed: the retry builds the calls and clears the failure, and the next lockdown answers 200.
	if _, err := pool.Exec(ctx, `UPDATE agent_smart_accounts SET rotated_from_indexes = '{7}' WHERE agent_id = $1`, broken); err != nil {
		t.Fatal(err)
	}
	if built, outstanding, err := store.RetryRevokeCalls(ctx); err != nil || built != 1 || len(outstanding) != 0 {
		t.Fatalf("retry on a fixed record = %d %+v %v, want it built", built, outstanding, err)
	}
	if got := onAgent(); got != "" {
		t.Fatalf("agent's revoke calls error after the retry = %q, want none", got)
	}
	if rec, err := store.AuditRecord(ctx, ws, "agent:"+broken); err != nil || rec[len(rec)-1].Kind != "agent.revoke_calls_built" {
		t.Fatalf("agent's record after the retry = %+v %v, want agent.revoke_calls_built last", rec, err)
	}
	if code, l := lockdown(); code != http.StatusOK || len(l.Frozen[0].PreviousKeyRevokeCalls) != 1 {
		t.Fatalf("lockdown after the fix = %d %+v, want 200 with the rotated-from key's call", code, l)
	}
}

type b5425Sink struct{ alerts []string }

func (s *b5425Sink) NotifyAs(_ context.Context, _, _, _, body string) error {
	s.alerts = append(s.alerts, body)
	return nil
}
