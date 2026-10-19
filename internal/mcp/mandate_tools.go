package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// mandate_tools.go — B40.2: get_mandates reads the permission mandates of the caller's workspace — what the person
// allowed an assistant or an agent to prepare and pay, each with whether it is active. An agent's key sees the ones for
// it and the ones for no agent in particular. It reads only; a mandate is signed and revoked by the person.

// MandateReader reads a workspace's permission mandates (economy.DualTokenStore).
type MandateReader interface {
	PermissionMandates(ctx context.Context, workspaceID, agentID string) ([]economy.PermissionMandate, error)
}

// SetMandates offers get_mandates; nil leaves it out.
func (s *Server) SetMandates(m MandateReader) { s.mandates = m }

func mandateToolDefinitions() []map[string]any {
	return []map[string]any{{
		"name": "get_mandates",
		"description": "The permission mandates the person signed: which clients may prepare and pay, for which payees, sellers and rooms, " +
			"the limits, when approval is still needed, and whether each is active, expired or revoked.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}},
	}}
}

func (s *Server) toolGetMandates(ctx context.Context, _ json.RawMessage) (any, error) {
	ws := effectiveWorkspace(ctx, "")
	if ws == "" {
		return nil, &toolRefusal{"permission mandates are a workspace's: use a workspace's key"}
	}
	agentID := ""
	if actx := auth.GetAuthContext(ctx); actx != nil && actx.APIKeyID != "" && s.agentBank != nil {
		a, _, err := s.agentBank.AgentOfKey(ctx, actx.APIKeyID)
		if err != nil && !errors.Is(err, economy.ErrAgentNotFound) {
			return nil, err
		}
		agentID = a
	}
	list, err := s.mandates.PermissionMandates(ctx, ws, agentID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"mandates": list}, nil
}
