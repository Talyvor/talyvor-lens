package mcp

import (
	"context"
	"encoding/json"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// payTools are the tools an agent pays with, or asks its owner to approve a payment with: each takes the agent's task
// (B62.2), kept with the payment and the approval it files.
var payTools = map[string]bool{"agent_pay": true, "wallet_send": true, "wallet_answer_request": true, "wallet_escrow_pay": true,
	"wallet_pay_external": true, "wallet_invoice_pay": true, "instant_transfer_send": true}

// taskDescription is the task argument, as an agent reads it.
const taskDescription = "what you are trying to do, in your own words, for your owner: one line, at most 200 characters. " +
	"Your owner sees it with the payment as what you say; it changes no rule and no decision."

// withTaskArgument gives every pay tool its task argument.
func withTaskArgument(tools []map[string]any) []map[string]any {
	for _, t := range tools {
		name, _ := t["name"].(string)
		if !payTools[name] {
			continue
		}
		schema, _ := t["inputSchema"].(map[string]any)
		if props, ok := schema["properties"].(map[string]any); ok {
			props["task"] = map[string]any{"type": "string", "description": taskDescription}
		}
	}
	return tools
}

// withToolTask is ctx carrying the task a pay tool's arguments give, cleaned, for the caller's workspace; a task the
// arguments cannot carry is a refusal in words. Any other tool's ctx is as it was.
func withToolTask(ctx context.Context, name string, raw json.RawMessage) (context.Context, error) {
	var in struct {
		Task *string `json:"task"`
	}
	if !payTools[name] || json.Unmarshal(raw, &in) != nil || in.Task == nil {
		return ctx, nil // a task that is not a string is refused by the schema's check
	}
	task, err := economy.CleanTask(*in.Task)
	if err != nil {
		return ctx, &toolRefusal{err.Error()}
	}
	ws, _ := auth.WorkspaceIdentity(ctx)
	return economy.WithAgentTask(ctx, ws, task), nil
}
