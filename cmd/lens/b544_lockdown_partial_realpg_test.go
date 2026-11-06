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

// B54.4 — a lockdown is the emergency button: when the second of three agents' freeze fails, the first and the third
// are still frozen, the answer is 207 naming the frozen and the failed by agent, and a retry freezes the one still open.
func TestB544_LockdownFreezesEveryAgentEvenWhenOneFails(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b544", "app.talyvor.com", "https://app.talyvor.com"
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
	slices.Sort(ids) // the lockdown freezes in agent order
	first, second, third := ids[0], ids[1], ids[2]
	// The second agent's freeze fails: a key it rotated from has no index.
	if _, err := pool.Exec(ctx, `UPDATE agent_smart_accounts SET rotated_from_signers = ARRAY['0x000000000000000000000000000000000000dead'],
		rotated_from_indexes = '{}' WHERE agent_id = $1`, second); err != nil {
		t.Fatal(err)
	}
	gate := func(agent string) error {
		_, err := store.AuthoriseAgentCryptoPayment(ctx, ws, agent, smartaccount.Payment{Token: "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
			To: "0x000000000000000000000000000000000000b0b0", Amount: big.NewInt(1)})
		return err
	}
	type lockdown struct {
		Frozen []economy.AgentFreeze
		Failed []economy.LockdownFailure
	}
	frozenIDs := func(l lockdown) (out []string) {
		for _, f := range l.Frozen {
			out = append(out, f.AgentID)
		}
		return out
	}

	var locked lockdown
	if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/lockdown", nil, &locked); code != http.StatusMultiStatus {
		t.Fatalf("lockdown with one freeze failing = %d %+v, want 207", code, locked)
	}
	if got := frozenIDs(locked); !slices.Equal(got, []string{first, third}) || len(locked.Failed) != 1 || locked.Failed[0].AgentID != second || locked.Failed[0].Error == "" {
		t.Fatalf("lockdown = frozen %v failed %+v, want frozen %v and %s failed", got, locked.Failed, []string{first, third}, second)
	}
	for _, id := range []string{first, third} {
		if err := gate(id); !errors.Is(err, smartaccount.ErrFrozen) {
			t.Fatalf("agent %s after the lockdown: %v, want frozen", id, err)
		}
	}
	if err := gate(second); errors.Is(err, smartaccount.ErrFrozen) {
		t.Fatalf("agent %s whose freeze failed reads frozen", second)
	}

	// Fixed, a retry freezes the one still open and answers the others' same freeze.
	if _, err := pool.Exec(ctx, `UPDATE agent_smart_accounts SET rotated_from_signers = '{}' WHERE agent_id = $1`, second); err != nil {
		t.Fatal(err)
	}
	var again lockdown
	if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/lockdown", nil, &again); code != http.StatusOK {
		t.Fatalf("retried lockdown = %d %+v, want 200", code, again)
	}
	if got := frozenIDs(again); !slices.Equal(got, ids) || len(again.Failed) != 0 || !again.Frozen[0].FrozenAt.Equal(locked.Frozen[0].FrozenAt) {
		t.Fatalf("retried lockdown = %+v, want all three frozen, the first at its first freeze", again)
	}
	if err := gate(second); !errors.Is(err, smartaccount.ErrFrozen) {
		t.Fatalf("agent %s after the retry: %v, want frozen", second, err)
	}
}
