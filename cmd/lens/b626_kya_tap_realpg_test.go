package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/kyatap"
)

// B62.6 — through the real routes on a migrated schema: an agent of a business Talyvor verified shows its credential
// with what the card networks' Know Your Agent checks link an agent to, and presents it in the form Visa's Trusted
// Agent Protocol publishes for one request to a merchant, which Talyvor's own verifier takes back: valid for that
// merchant, once, and revoked with the credential when the agent is frozen.
func TestB626_AnAgentsCredentialInTheTrustedAgentProtocolForm(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b626"
	store := economy.NewDualTokenStore(nil, pool, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type) VALUES ($1, $1, $1, 'business')`, ws); err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, ws, "Buyer bot", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref,
		verified_name, country, company_number) VALUES ($1, 1, 'contact', 'test', 'completed', 'ref-b626-1', '', '', ''),
		($1, 2, 'company', 'kyb-partner', 'completed', 'ref-b626-2', 'Acme Ltd', 'GB', '01234567')`,
		ws); err != nil {
		t.Fatal(err)
	}
	key, _, err := kya.ParseKey("")
	if err != nil {
		t.Fatal(err)
	}
	svc := kya.New(store, key, time.Hour)
	tap := kyatap.New(svc, key.Derive, agentsig.Store{Pool: pool}, time.Now)
	r := chi.NewRouter()
	mountKYAPublicRoutes(r, svc)
	mountKYAAgentRoutes(r, svc, store)
	mountKYATAPPublicRoutes(r, tap)
	mountKYATAPAgentRoutes(r, tap, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(who *auth.AuthContext, method, path string, body any, out any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		return w.Code
	}

	var c kya.Credential
	if code := call(owner, http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+agent.ID+"/credential", nil, &c); code != http.StatusOK {
		t.Fatalf("the credential = %d", code)
	}
	t.Run("kind", func(t *testing.T) {
		if c.Claims.Owner.Kind != economy.AccountTypeBusiness {
			t.Fatalf("owner kind = %q, want business", c.Claims.Owner.Kind)
		}
	})
	t.Run("country", func(t *testing.T) {
		if c.Claims.Owner.Country != "GB" {
			t.Fatalf("owner country = %q, want GB", c.Claims.Owner.Country)
		}
	})
	t.Run("registration number", func(t *testing.T) {
		if c.Claims.Owner.RegistrationNumber != "01234567" {
			t.Fatalf("owner registration number = %q, want 01234567", c.Claims.Owner.RegistrationNumber)
		}
	})

	tapPath := "/v1/workspaces/" + ws + "/agents/" + agent.ID + "/credential/tap"
	present := func() kyatap.Presentation {
		t.Helper()
		var p kyatap.Presentation
		if code := call(owner, http.MethodPost, tapPath, kyatap.Request{Authority: "shop.example", Path: "/checkout", Tag: agentsig.TAPTagPay}, &p); code != http.StatusOK {
			t.Fatalf("the Trusted Agent Protocol form = %d", code)
		}
		return p
	}
	check := func(p kyatap.Presentation, authority string) kyatap.Result {
		t.Helper()
		var res kyatap.Result
		if code := call(nil, http.MethodPost, kyatap.VerifyPath, kyatap.Check{Authority: authority, Path: "/checkout",
			SignatureInput: p.SignatureInput, Signature: p.Signature, AgenticConsumer: p.AgenticConsumer}, &res); code != http.StatusOK {
			t.Fatalf("verify = %d", code)
		}
		return res
	}

	p := present()
	if !strings.Contains(p.SignatureInput, `tag="agent-payer-auth"`) || p.AgenticConsumer.Kid == "" || p.ExpiresAt.Sub(time.Now()) > agentsig.TAPMaxLife {
		t.Fatalf("the form = %+v", p)
	}
	if res := check(p, "elsewhere.example"); res.Valid || !strings.Contains(res.Reason, "audience") {
		t.Fatalf("for another merchant = %+v, want refused for its audience", res)
	}
	res := check(p, "shop.example")
	if !res.Valid || res.Tag != agentsig.TAPTagPay || res.Claims == nil || res.Claims.Subject != agent.ID ||
		res.Claims.Owner.Name != "Acme Ltd" || res.Claims.Owner.RegistrationNumber != "01234567" {
		t.Fatalf("verify = %+v", res)
	}
	if res := check(p, "shop.example"); res.Valid || !strings.Contains(res.Reason, "already used") {
		t.Fatalf("the same one again = %+v, want its nonce refused", res)
	}

	held := present()
	if err := store.PauseAgent(ctx, ws, agent.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if res := check(held, "shop.example"); res.Valid || res.Reason != "revoked: "+economy.KYARevokedFrozen {
		t.Fatalf("after the freeze = %+v, want revoked: frozen", res)
	}
	if code := call(owner, http.MethodPost, tapPath, kyatap.Request{Authority: "shop.example", Path: "/checkout", Tag: agentsig.TAPTagPay}, nil); code != http.StatusConflict {
		t.Fatalf("a frozen agent's form = %d, want 409", code)
	}
}
