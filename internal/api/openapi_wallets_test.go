package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B28.12: GET /openapi.json describes Agent Wallets and lists the wallet routes, read from the served
// document a client generates against, not from the Go map.
func TestServedOpenAPIDescribesAgentWallets(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeOpenAPI(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	var doc struct {
		Info struct {
			Description string `json:"description"`
		} `json:"info"`
		Tags       []struct{ Name string } `json:"tags"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			Tags []string `json:"tags"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served document is not JSON: %v", err)
	}

	if !strings.HasPrefix(doc.Info.Description, "Agent Wallets and the gateway that enforces them") {
		t.Errorf("info.description = %q, want it to open on Agent Wallets", doc.Info.Description)
	}
	if len(doc.Tags) == 0 || doc.Tags[0].Name != walletTag {
		t.Errorf("tags = %+v, want %q first", doc.Tags, walletTag)
	}

	for _, want := range []struct{ method, path string }{
		{"get", "/v1/workspaces/{wsID}/agents"},
		{"get", "/v1/workspaces/{wsID}/agents/{agentID}/summary"},
		{"post", "/v1/workspaces/{wsID}/agents"},
		{"post", "/v1/workspaces/{wsID}/agents/{agentID}/keys"},
		{"post", "/v1/workspaces/{wsID}/agents/{agentID}/fund"},
		{"get", "/v1/workspaces/{wsID}/agents/{agentID}/rules"},
		{"put", "/v1/workspaces/{wsID}/agents/{agentID}/rules"},
		{"get", "/v1/workspaces/{wsID}/agents/rule-templates"},
		{"post", "/v1/workspaces/{wsID}/agents/{agentID}/rules/template"},
		{"post", "/v1/workspaces/{wsID}/agents/{agentID}/rules/simulate"},
		{"get", "/v1/workspaces/{wsID}/agents/{agentID}/rules/history"},
		{"post", "/v1/workspaces/{wsID}/agents/{agentID}/rules/rollback"},
		{"get", "/v1/workspaces/{wsID}/agents/{agentID}/rules/boosts"},
		{"post", "/v1/workspaces/{wsID}/agents/{agentID}/rules/boosts"},
		{"delete", "/v1/workspaces/{wsID}/agents/{agentID}/rules/boosts/{rule}"},
		{"get", "/v1/workspaces/{wsID}/agents/approvals"},
		{"post", "/v1/workspaces/{wsID}/agents/approvals/{approvalID}/approve"},
		{"post", "/v1/workspaces/{wsID}/agents/approvals/{approvalID}/deny"},
		{"get", "/v1/workspaces/{wsID}/agents/{agentID}/statement"},
		{"get", "/v1/workspaces/{wsID}/agents/statement"},
		{"post", "/v1/workspaces/{wsID}/agents/{agentID}/send"},
		{"post", "/v1/workspaces/{wsID}/agents/{agentID}/pay"},
		{"get", "/v1/workspaces/{wsID}/agents/{agentID}/transfers"},
		{"get", "/v1/workspaces/{wsID}/agents/{agentID}/card"},
	} {
		op, ok := doc.Paths[want.path][want.method]
		if !ok {
			t.Errorf("%s %s is not in the served document", strings.ToUpper(want.method), want.path)
			continue
		}
		if len(op.Tags) == 0 || op.Tags[0] != walletTag {
			t.Errorf("%s %s is tagged %v, want %q", strings.ToUpper(want.method), want.path, op.Tags, walletTag)
		}
	}

	// Every schema a wallet operation points at is in the document — a dangling $ref breaks generation.
	body := rec.Body.String()
	for _, name := range []string{"Agent", "AgentBook", "AgentRules", "AgentApproval", "AgentStatementLine", "AgentTransfer", "RuleTemplate"} {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("components.schemas.%s is missing", name)
		}
		if !strings.Contains(body, `"#/components/schemas/`+name+`"`) {
			t.Errorf("no operation references components.schemas.%s", name)
		}
	}
}
