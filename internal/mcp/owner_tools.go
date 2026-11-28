package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// owner_tools.go — B28.73: THE OWNER'S SIDE OF AGENT WALLETS OVER MCP.
//
// The owner_* tools are the workspace owner's, or an admin's — never an agent's key: list the agents and read one,
// create one, ask to fund one, and file its rules. Funding moves nothing here: owner_fund_agent files a request under
// the caller's idempotency_key and answers its approval link, and the money moves only when a person signed in
// approves it there, one posting whose ref is that key (economy.RequestAgentFunding). Rules that loosen any rule in
// force wait for the owner's passkey (economy.ProposeAgentRules); rules that only tighten apply at once.

// OwnerBank is what the owner tools need of the economy store.
type OwnerBank interface {
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
	AgentSummary(ctx context.Context, workspaceID, agentID string) (economy.AgentSummary, error)
	GetAgentRules(ctx context.Context, workspaceID, agentID string) (economy.AgentRules, error)
	CreateAgentOfKind(ctx context.Context, workspaceID, name, ownerUserID, kind, fiat string, look economy.AgentLook) (economy.Agent, error)
	RequestAgentFunding(ctx context.Context, workspaceID, agentID string, amount int64, idempotencyKey, by string) (economy.AgentFundRequest, bool, error)
	AgentFundRequestByID(ctx context.Context, workspaceID, id string) (economy.AgentFundRequest, error)
	ProposeAgentRules(ctx context.Context, workspaceID, agentID string, rules economy.AgentRules) (economy.FiledRules, error)
}

// SetOwnerBank offers the owner tools; nil leaves them out.
func (s *Server) SetOwnerBank(b OwnerBank) { s.ownerBank = b }

func ownerToolDefinitions() []map[string]any {
	agent := map[string]any{"type": "string", "description": "the agent's id"}
	return []map[string]any{
		{
			"name":        "owner_agents",
			"description": "Every agent of your workspace with its balance, and what of the workspace's credit is with an agent and what is not.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "owner_agent",
			"description": "One agent's page: its balance, what is safe to spend, this month's spending, its approvals waiting, its latest statement lines and its spending rules.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"agent_id": agent}, "required": []string{"agent_id"}},
		},
		{
			"name":        "owner_create_agent",
			"description": "Create an agent in your workspace, owned by you. It starts with an empty wallet and no key. Signed in as its owner only: a workspace key cannot create one.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"name": map[string]any{"type": "string", "maxLength": 80, "description": "the agent's name"},
			}, "required": []string{"name"}},
		},
		{
			"name": "owner_fund_agent",
			"description": "Ask to move credit from your workspace to an agent. Nothing moves yet: it answers an approval link, and the agent is funded " +
				"once a person signed in to the workspace approves it there. Send the same idempotency_key again and it is the same request.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"agent_id":        agent,
				"amount_ulxc":     map[string]any{"type": "integer", "minimum": 1, "description": "the amount in µLXC (1 LXC = 1,000,000 µLXC)"},
				"idempotency_key": map[string]any{"type": "string", "maxLength": 128, "description": "names this request: the same key is one request and, once approved, one movement"},
			}, "required": []string{"agent_id", "amount_ulxc", "idempotency_key"}},
		},
		{
			"name":        "owner_fund_request",
			"description": "A request to fund an agent: whether it waits, was approved or was declined, and its approval link.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"request_id": map[string]any{"type": "string", "description": "the request's id, from owner_fund_agent"},
			}, "required": []string{"request_id"}},
		},
		{
			"name": "owner_propose_rules",
			"description": "Set an agent's spending rules. Rules that only tighten what is in force apply at once; rules that loosen any of it " +
				"wait for the owner to approve them with their passkey. Answers whether they applied or wait, and what changed.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"agent_id": agent,
				"rules":    map[string]any{"type": "object", "description": "the agent's rules, as GET /v1/workspaces/{ws}/agents/{id}/rules shows them"},
			}, "required": []string{"agent_id", "rules"}},
		},
	}
}

// ownerAgentPage is owner_agent's answer: the agent's page, and the spending rules in force.
type ownerAgentPage struct {
	economy.AgentSummary
	SpendingRules economy.AgentRules `json:"spending_rules"`
}

// callOwnerTool runs an owner_* tool for the caller's workspace: the owner or an admin only.
func (s *Server) callOwnerTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	ws := effectiveWorkspace(ctx, "")
	by, owner := storedanswers.OwnerOrAdmin(ctx)
	if ws == "" || !owner {
		return nil, &toolRefusal{"the owner tools are the workspace owner's or an admin's: use the owner's key, not an agent's"}
	}
	var a struct {
		AgentID        string          `json:"agent_id"`
		Name           string          `json:"name"`
		AmountULXC     int64           `json:"amount_ulxc"`
		IdempotencyKey string          `json:"idempotency_key"`
		RequestID      string          `json:"request_id"`
		Rules          json.RawMessage `json:"rules"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &toolRefusal{"invalid arguments: " + err.Error()}
		}
	}
	refused := func(v any, err error) (any, error) {
		for _, known := range []error{economy.ErrAgentNotFound, economy.ErrAgentArchived, economy.ErrAgentRule, economy.ErrFundRequest,
			economy.ErrFundRequestNotFound, economy.ErrFundRequestConflict} {
			if errors.Is(err, known) {
				return nil, &toolRefusal{err.Error()}
			}
		}
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	switch name {
	case "owner_agents":
		return refused(s.ownerBank.AgentBook(ctx, ws))
	case "owner_agent":
		page, err := s.ownerBank.AgentSummary(ctx, ws, a.AgentID)
		if err != nil {
			return refused(nil, err)
		}
		rules, err := s.ownerBank.GetAgentRules(ctx, ws, a.AgentID)
		return refused(ownerAgentPage{page, rules}, err)
	case "owner_create_agent":
		if strings.TrimSpace(a.Name) == "" {
			return nil, &toolRefusal{"name the agent"}
		}
		user := ""
		if actx := auth.GetAuthContext(ctx); actx != nil {
			user = actx.UserID
		}
		created, err := s.ownerBank.CreateAgentOfKind(ctx, ws, strings.TrimSpace(a.Name), user, "", "", economy.AgentLook{})
		if errors.Is(err, economy.ErrAgentOwnerless) { // B19.11: every agent is a person's
			return nil, &toolRefusal{"an agent needs an owner: create it signed in as the person who will own it, not with a workspace key"}
		}
		return refused(created, err)
	case "owner_fund_agent":
		r, _, err := s.ownerBank.RequestAgentFunding(ctx, ws, a.AgentID, a.AmountULXC, a.IdempotencyKey, by)
		return refused(r, err)
	case "owner_fund_request":
		return refused(s.ownerBank.AgentFundRequestByID(ctx, ws, a.RequestID))
	case "owner_propose_rules":
		var rules economy.AgentRules
		dec := json.NewDecoder(bytes.NewReader(a.Rules))
		dec.DisallowUnknownFields() // a misspelt rule must not be silently no rule
		if err := dec.Decode(&rules); err != nil {
			return nil, &toolRefusal{"invalid arguments: rules: " + err.Error()}
		}
		return refused(s.ownerBank.ProposeAgentRules(economy.WithRulesChange(ctx, by, "set"), ws, a.AgentID, rules))
	}
	return nil, &toolRefusal{"unknown tool: " + name}
}
