package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// agent_tools.go — B19.9: AGENTS USE THEIR WALLETS THEMSELVES.
//
// Four tools an agent calls with its OWN key (a key attached to it, B19.1): its balance and rules, a
// payment's approval asked for with a reason, a payment to another of its workspace's agents, and the
// receipt of an entry it is party to. The agent is the key's, never an argument, so a key can reach only
// its own account; paying goes through PayAgent, so the agent's rules judge it exactly as they judge the
// owner's payment from it. Every call is logged (agent_tool_calls, migration 0145), refused ones too.

// AgentBank is what the agent tools need of the economy store.
type AgentBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
	GetAgentRules(ctx context.Context, workspaceID, agentID string) (economy.AgentRules, error)
	AgentWallet(ctx context.Context, workspaceID, agentID string) (economy.AgentWallet, error)
	RequestPaymentApproval(ctx context.Context, workspaceID, agentID, toAgentID string, amount int64, memo, reason string) (economy.AgentApproval, error)
	PayAgent(ctx context.Context, workspaceID, fromAgentID, toAgentID string, amount int64, memo string) (economy.AgentPayment, error)
	AgentReceipt(ctx context.Context, workspaceID, agentID, entryID string) (economy.AgentReceipt, error)
	RecordAgentToolCall(ctx context.Context, workspaceID, agentID, scopedKeyID, tool string, args json.RawMessage, outcome, detail string) error
	AgentToolCallsAllowed(ctx context.Context, workspaceID, agentID string) error
}

// SetAgentBank enables the agent tools.
func (s *Server) SetAgentBank(b AgentBank) { s.agentBank = b }

// idempotencyKeyDescription is the idempotency_key argument of every agent tool that moves money (B50.15).
const idempotencyKeyDescription = "send the same key when you retry this exact payment: it is paid once. Without one, every call is its own payment."

// toolRefusal is a tool's answer that Lens refused what the agent asked: the agent reads why, as a
// tool result marked isError, rather than as a protocol failure.
type toolRefusal struct{ msg string }

func (e *toolRefusal) Error() string { return e.msg }

func agentToolDefinitions() []map[string]any {
	payment := map[string]any{
		"to_agent_id": map[string]any{"type": "string", "description": "the agent to pay, in the same workspace"},
		"amount_ulxc": map[string]any{"type": "integer", "description": "the amount in µLXC (1 LXC = 1,000,000 µLXC)"},
		"memo":        map[string]any{"type": "string", "description": "what the payment is for"},
	}
	withReason := map[string]any{"reason": map[string]any{"type": "string", "description": "why the payment is needed, for the person approving it"}}
	for k, v := range payment {
		withReason[k] = v
	}
	// B50.15: a payment retried with its key lands once, as through the HTTP route's Idempotency-Key.
	keyed := map[string]any{"idempotency_key": map[string]any{"type": "string", "description": idempotencyKeyDescription}}
	for k, v := range payment {
		keyed[k] = v
	}
	return []map[string]any{
		{
			"name":        "agent_balance",
			"description": "Your own wallet: balance, what you have spent, whether you are paused, your spending rules, and your USDC wallet with its balance and, once a partner serves it, the address that pays into it.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "agent_request_approval",
			"description": "Ask a person to approve a payment above your approval amount, with a reason. Once approved, make exactly that payment with agent_pay.",
			"inputSchema": map[string]any{"type": "object", "properties": withReason, "required": []string{"to_agent_id", "amount_ulxc", "reason"}},
		},
		{
			"name":        "agent_pay",
			"description": "Pay another agent of your workspace from your own balance, within your spending rules.",
			"inputSchema": map[string]any{"type": "object", "properties": keyed, "required": []string{"to_agent_id", "amount_ulxc"}},
		},
		{
			"name":        "agent_receipt",
			"description": "The receipt of a payment or other entry in your wallet: every posting of it.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"entry_id": map[string]any{"type": "string"}}, "required": []string{"entry_id"}},
		},
	}
}

// callAgentTool runs one agent tool for the calling key and logs the call.
func (s *Server) callAgentTool(ctx context.Context, name string, args json.RawMessage) (any, error) {
	keyID := ""
	if actx := auth.GetAuthContext(ctx); actx != nil {
		keyID = actx.APIKeyID
	}
	if s.agentBank == nil {
		return nil, fmt.Errorf("agent wallets are not configured")
	}
	agentID, workspaceID := "", ""
	var result any
	var err error = &toolRefusal{"these tools are for an agent's own key; this credential is not attached to an agent"}
	if keyID != "" {
		if a, w, aerr := s.agentBank.AgentOfKey(ctx, keyID); aerr == nil {
			agentID, workspaceID = a, w
			result, err = s.runAgentTool(ctx, name, workspaceID, agentID, args)
		} else if !errors.Is(aerr, economy.ErrAgentNotFound) {
			return nil, aerr
		}
	}
	if workspaceID == "" {
		workspaceID, _ = auth.WorkspaceIdentity(ctx)
	}
	outcome, detail := "ok", ""
	var refusal *toolRefusal
	switch {
	case errors.As(err, &refusal):
		outcome, detail = "refused", refusal.msg
	case err != nil:
		outcome, detail = "error", err.Error()
	}
	if lerr := s.agentBank.RecordAgentToolCall(ctx, workspaceID, agentID, keyID, name, args, outcome, detail); lerr != nil {
		return nil, lerr // an unlogged call is not answered
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Server) runAgentTool(ctx context.Context, name, workspaceID, agentID string, raw json.RawMessage) (any, error) {
	if err := s.checkArguments(name, raw); err != nil { // B50.4
		return nil, err
	}
	// B50.4: the agent's requests_per_minute stops a runaway loop of tool calls as it stops one of model calls.
	if err := s.agentBank.AgentToolCallsAllowed(ctx, workspaceID, agentID); err != nil {
		if errors.Is(err, economy.ErrAgentRequestRate) {
			return nil, &toolRefusal{err.Error()}
		}
		return nil, err
	}
	if isWalletTool(name) { // B22.11
		return s.runWalletTool(ctx, name, workspaceID, agentID, raw)
	}
	if isMarketTool(name) { // B32.23
		return s.runMarketTool(ctx, name, workspaceID, agentID, raw)
	}
	if isRoomTool(name) { // B32.36
		return s.runRoomTool(ctx, name, workspaceID, agentID, raw)
	}
	var args struct {
		ToAgentID  string `json:"to_agent_id"`
		AmountULXC int64  `json:"amount_ulxc"`
		Memo       string `json:"memo"`
		Reason     string `json:"reason"`
		EntryID    string `json:"entry_id"`
		Key        string `json:"idempotency_key"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, &toolRefusal{"invalid arguments: " + err.Error()}
		}
	}
	refused := func(err error) error {
		for _, e := range []error{economy.ErrAgentRule, economy.ErrApprovalRequired, economy.ErrAgentFunds, economy.ErrAgentNotFound,
			economy.ErrSameAgent, economy.ErrApprovalNotNeeded, economy.ErrReceiptNotFound, economy.ErrAgentOwnerless,
			economy.ErrCapabilityNotCleared, economy.ErrIdempotencyKeyReused, workspace.ErrMoneyWall} {
			if errors.Is(err, e) {
				return &toolRefusal{err.Error()}
			}
		}
		return err
	}
	switch name {
	case "agent_balance":
		book, err := s.agentBank.AgentBook(ctx, workspaceID)
		if err != nil {
			return nil, err
		}
		rules, err := s.agentBank.GetAgentRules(ctx, workspaceID, agentID)
		if err != nil {
			return nil, err
		}
		for _, a := range book.Agents {
			if a.ID != agentID {
				continue
			}
			out := map[string]any{"agent": a, "rules": rules, "workspace_paused": book.AllPausedAt != nil}
			// B46.4: the agent's wallet on the money layer — the same one, and the same balance, its owner sees.
			wallet, err := s.agentBank.AgentWallet(ctx, workspaceID, agentID)
			switch {
			case err == nil:
				out["wallet"] = wallet
			case !errors.Is(err, economy.ErrAgentWalletsOff):
				return nil, err
			}
			return out, nil
		}
		return nil, &toolRefusal{economy.ErrAgentNotFound.Error()}
	case "agent_request_approval":
		if args.ToAgentID == "" || args.AmountULXC <= 0 || args.Reason == "" {
			return nil, &toolRefusal{"to_agent_id, a positive amount_ulxc and a reason are required"}
		}
		a, err := s.agentBank.RequestPaymentApproval(ctx, workspaceID, agentID, args.ToAgentID, args.AmountULXC, args.Memo, args.Reason)
		if err != nil {
			return nil, refused(err)
		}
		return a, nil
	case "agent_pay":
		if args.ToAgentID == "" || args.AmountULXC <= 0 {
			return nil, &toolRefusal{"to_agent_id and a positive amount_ulxc are required"}
		}
		if len(args.Key) > 128 {
			return nil, &toolRefusal{"idempotency_key must be at most 128 characters"}
		}
		pay, err := s.agentBank.PayAgent(economy.WithMoveKey(ctx, args.Key), workspaceID, agentID, args.ToAgentID, args.AmountULXC, args.Memo)
		if err != nil {
			return nil, refused(err)
		}
		return pay, nil
	default: // agent_receipt
		r, err := s.agentBank.AgentReceipt(ctx, workspaceID, agentID, args.EntryID)
		if err != nil {
			return nil, refused(err)
		}
		return r, nil
	}
}
