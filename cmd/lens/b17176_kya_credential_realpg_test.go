package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/tenant"
)

// B17.176 — the testers' kya-credential, through the routes it calls, wired as main.go wires them: the owner creates
// an agent, sets its daily limit with PUT …/rules and issues it a key; the owner's credential read and the agent's
// wallet_credential over /mcp, on that key through the real key check, hand over one credential, which verifies with
// only the published JWKS and states the limit, and verify says valid. The owner's PUT of a new limit lists it on
// revoked.json as "rules changed" before anything asks verify, and the next one states the new limit; the owner's
// POST …/pause lists that one "frozen", the credential route is 409 and wallet_credential hands the agent none. On
// the simple protocol CI leg this is the path whose tool-call log failed production with 22P02.
func TestB17176_AnAgentsKYACredentialThroughTheTestersRoutes(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b17176"
	store := economy.NewDualTokenStore(nil, pool, nil)
	key, _, err := kya.ParseKey("")
	if err != nil {
		t.Fatal(err)
	}
	svc := kya.New(store, key, time.Hour)
	mcpServer := mcp.New(pool, nil, nil, nil, nil, "test")
	mcpServer.SetAgentBank(store)
	mcpServer.SetCredentials(svc)
	keyStore := auth.New(pool)
	r := chi.NewRouter()
	mountKYAPublicRoutes(r, svc)
	mountKYAAgentRoutes(r, svc, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	r.With(auth.AuthMiddleware(keyStore, auth.NewManager("", nil, keyStore, tenant.NewStore(pool)))).Post("/mcp", mcpServer.HandleRPC)

	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(who *auth.AuthContext, bearer, method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && (w.Code == http.StatusOK || w.Code == http.StatusCreated) {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		if w.Code >= 300 {
			t.Logf("%s %s = %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Code
	}
	base := "/v1/workspaces/" + ws + "/agents"

	var agent struct {
		ID string `json:"id"`
	}
	if code := call(owner, "", http.MethodPost, base, `{"name":"KYA check"}`, &agent); code != http.StatusCreated {
		t.Fatalf("create the agent = %d", code)
	}
	setDaily := func(daily int64) {
		t.Helper()
		var rules map[string]any
		if code := call(owner, "", http.MethodGet, base+"/"+agent.ID+"/rules", "", &rules); code != http.StatusOK {
			t.Fatalf("read the rules = %d", code)
		}
		rules["daily_limit_ulxc"] = daily
		body, _ := json.Marshal(rules)
		var put struct {
			Daily int64 `json:"daily_limit_ulxc"`
		}
		if code := call(owner, "", http.MethodPut, base+"/"+agent.ID+"/rules", string(body), &put); code != http.StatusOK || put.Daily != daily {
			t.Fatalf("set the daily limit to %d = %d %+v", daily, code, put)
		}
	}
	setDaily(5_000_000)
	var issued struct {
		Key string `json:"key"`
	}
	if code := call(owner, "", http.MethodPost, base+"/"+agent.ID+"/keys", `{"name":"kya-credential"}`, &issued); code != http.StatusCreated {
		t.Fatalf("issue the agent a key = %d", code)
	}

	read := func() (int, kya.Credential) {
		t.Helper()
		var c kya.Credential
		return call(owner, "", http.MethodGet, base+"/"+agent.ID+"/credential", "", &c), c
	}
	shown := func() (kya.Credential, string) {
		t.Helper()
		var rpc struct {
			Result *struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		code := call(nil, issued.Key, http.MethodPost, "/mcp",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wallet_credential","arguments":{}}}`, &rpc)
		if code != http.StatusOK || rpc.Result == nil || len(rpc.Result.Content) == 0 {
			t.Fatalf("wallet_credential = %d %+v", code, rpc)
		}
		var c kya.Credential
		if rpc.Result.IsError || json.Unmarshal([]byte(rpc.Result.Content[0].Text), &c) != nil {
			return c, rpc.Result.Content[0].Text
		}
		return c, ""
	}
	published := func(path string) string {
		t.Helper()
		var raw json.RawMessage
		if code := call(nil, "", http.MethodGet, path, "", &raw); code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		return string(raw)
	}
	verify := func(c kya.Credential) kya.Verification {
		t.Helper()
		var v kya.Verification
		if code := call(nil, "", http.MethodPost, "/v1/kya/verify", `{"credential":"`+c.Token+`"}`, &v); code != http.StatusOK {
			t.Fatalf("verify = %d", code)
		}
		return v
	}
	statesDaily := func(c kya.Credential, daily float64) {
		t.Helper()
		claims, err := verifyWithPublishedKeys([]byte(published("/.well-known/talyvor-kya/jwks.json")), c.Token)
		if err != nil {
			t.Fatalf("a platform holding only the published JWKS could not verify %s: %v", c.ID, err)
		}
		limits, _ := claims["limits"].(map[string]any)
		if claims["sub"] != agent.ID || claims["jti"] != c.ID || limits["daily_limit_ulxc"] != daily {
			t.Fatalf("credential %s says sub %v, jti %v, limits %v; want %s, itself, %v a day", c.ID, claims["sub"], claims["jti"], limits, agent.ID, daily)
		}
	}

	code, first := read()
	if code != http.StatusOK {
		t.Fatalf("the owner's credential read = %d", code)
	}
	own, refused := shown()
	if refused != "" || own.ID != first.ID {
		t.Fatalf("wallet_credential on the agent's own key = %s %q; want the owner's %s", own.ID, refused, first.ID)
	}
	statesDaily(first, 5_000_000)
	if v := verify(first); !v.Valid {
		t.Fatalf("verify said the fresh credential is not valid: %s", v.Reason)
	}

	// The rule change itself revokes it: the list is read before verify, which revokes a stale credential as it answers.
	setDaily(9_000_000)
	if !strings.Contains(published("/.well-known/talyvor-kya/revoked.json"), `"jti":"`+first.ID+`","reason":"rules changed"`) {
		t.Fatal("after the owner's PUT of a new daily limit, revoked.json does not list the first credential as rules changed")
	}
	if v := verify(first); v.Valid || v.Reason != "revoked: rules changed" {
		t.Fatalf("the credential from before the rule change verified as %+v", v)
	}
	code, second := read()
	if code != http.StatusOK || second.ID == first.ID {
		t.Fatalf("after the rule change the owner's read = %d %s; want a new credential", code, second.ID)
	}
	statesDaily(second, 9_000_000)

	// The owner's pause revokes that one, the route is 409 and the agent is handed none.
	if code := call(owner, "", http.MethodPost, base+"/"+agent.ID+"/pause", `{"reason":"nightly KYA check"}`, nil); code != http.StatusOK {
		t.Fatalf("pause the agent = %d", code)
	}
	if !strings.Contains(published("/.well-known/talyvor-kya/revoked.json"), `"jti":"`+second.ID+`","reason":"frozen"`) {
		t.Fatal("after the owner paused the agent, revoked.json does not list its credential as frozen")
	}
	if v := verify(second); v.Valid || v.Reason != "revoked: frozen" {
		t.Fatalf("the paused agent's credential verified as %+v", v)
	}
	if code, _ := read(); code != http.StatusConflict {
		t.Fatalf("the paused agent's credential route = %d, want 409", code)
	}
	if c, refused := shown(); refused == "" {
		t.Fatalf("wallet_credential handed the paused agent %s", c.ID)
	}
	var logged int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_tool_calls WHERE agent_id = $1 AND tool = 'wallet_credential'`, agent.ID).Scan(&logged); err != nil || logged != 2 {
		t.Fatalf("agent_tool_calls holds %d wallet_credential calls (%v); want both logged", logged, err)
	}
}
