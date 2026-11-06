package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// B52.4: a Connect token authenticates only on its own agent's pay route, so no handler that forgets to check the
// method — /v1/auth/refresh, which re-mints any JWT without its audience, or a route gated on a workspace alone —
// can be reached with it.
func TestConnectToken_AuthenticatesOnlyOnItsAgentsPayRoute(t *testing.T) {
	key, err := GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager("", key, nil, nil)
	tok, err := GenerateConnectToken("ws1", "u1", "https://claude.ai", "agt_1", []string{ScopePaymentsSend}, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	try := func(method, path string) (*AuthContext, error) {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		return m.Authenticate(req)
	}
	actx, err := try(http.MethodPost, "/v1/workspaces/ws1/agents/agt_1/pay")
	if err != nil || actx.AuthMethod != MethodConnect || actx.ConnectAgentID != "agt_1" || actx.ConnectClientID != "https://claude.ai" ||
		actx.WorkspaceID != "ws1" || !actx.HasScope(ScopePaymentsSend) || actx.IsAdmin {
		t.Fatalf("the token on its own pay route = %+v, %v", actx, err)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/auth/refresh"},
		{http.MethodGet, "/v1/auth/me"},
		{http.MethodPost, "/v1/workspaces/ws1/api-keys"},
		{http.MethodGet, "/v1/workspaces/ws1/agents/agt_1/pay"},
		{http.MethodPost, "/v1/workspaces/ws1/agents/agt_2/pay"},
		{http.MethodPost, "/v1/workspaces/ws2/agents/agt_1/pay"},
		{http.MethodPost, "/v1/workspaces/ws1/agents/agt_1%2Fpay"},
	} {
		if _, err := try(c.method, c.path); !errors.Is(err, ErrConnectRoute) {
			t.Errorf("%s %s with a Connect token: err = %v, want ErrConnectRoute", c.method, c.path, err)
		}
	}
}
