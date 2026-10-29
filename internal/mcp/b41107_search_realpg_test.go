package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B41.107 — the owner's assistant searches the workspace with search_workspace and finds the payee; an agent's own
// key may not search its owner's workspace.
func TestSearchWorkspace_TheOwnersAssistantFindsAPayee(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws = "ws-mcp-search"
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, check_result, checked_by)
		VALUES ('payee_acme_mcp', $1, 'Acme Supplies Ltd', 'GB', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetSearcher(store)
	call := func(actx *auth.AuthContext) (text string, isError bool) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_workspace","arguments":{"query":"acme"}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), actx))
		w := httptest.NewRecorder()
		srv.HandleRPC(w, req)
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Result.Content) == 0 {
			t.Fatalf("search_workspace = %s", w.Body.String())
		}
		return resp.Result.Content[0].Text, resp.Result.IsError
	}

	text, isErr := call(&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}})
	var got economy.SearchResults
	if err := json.Unmarshal([]byte(text), &got); err != nil || isErr {
		t.Fatalf("search_workspace = %s", text)
	}
	if len(got.Groups) != 1 || got.Groups[0].Kind != economy.SearchPayee || got.Groups[0].Results[0].ID != "payee_acme_mcp" {
		t.Errorf("search_workspace = %+v, want the payee Acme Supplies Ltd", got)
	}
	if _, isErr := call(&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-agent"}); !isErr {
		t.Error("a key without the keys scope searched the workspace")
	}
}
