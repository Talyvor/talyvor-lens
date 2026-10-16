package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// agent_self_service.go — B19.9: AGENTS USE THEIR WALLETS THEMSELVES.
//
// What an agent's own key may do with its own account, for the MCP tools (internal/mcp/agent_tools.go):
// ask for a payment's approval with a reason, before making it; read the receipt of an entry it is party
// to; and have each call it makes logged (migration 0145). Paying goes through PayAgent, so its rules
// judge it exactly as they judge the owner's payment from it.

// ErrApprovalNotNeeded: the payment is within the agent's approval amount, so there is nothing to approve.
var ErrApprovalNotNeeded = errors.New("economy: this payment does not need approval — make it")

// ErrReceiptNotFound: no entry of that id moved this agent's account.
var ErrReceiptNotFound = errors.New("economy: no such entry on this agent's account")

// RequestPaymentApproval files, with a reason, the approval the payment from agentID to toAgentID of
// amount µLXC with memo needs, or gives the one already open for it this reason. Once the workspace's owner
// approves it, that exact payment goes through once.
func (s *DualTokenStore) RequestPaymentApproval(ctx context.Context, workspaceID, agentID, toAgentID string, amount int64, memo, reason string) (AgentApproval, error) {
	rules, err := s.GetAgentRules(ctx, workspaceID, agentID)
	if err != nil {
		return AgentApproval{}, err
	}
	if rules.ApprovalAboveULXC == 0 || amount <= rules.ApprovalAboveULXC {
		return AgentApproval{}, ErrApprovalNotNeeded
	}
	fp := paymentFingerprint(agentID, toAgentID, amount, memo)
	payee, err := namePayee(ctx, s.pool, Payee{Kind: "agent", ID: toAgentID})
	if err != nil {
		return AgentApproval{}, err
	}
	var filed string
	var inserted bool
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, reason, payee_kind, payee_id, payee_name, memo)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (agent_id, fingerprint) WHERE status IN ('pending', 'approved') DO UPDATE SET reason = EXCLUDED.reason
		RETURNING id, xmax = 0`,
		"apr_"+uuid.NewString(), workspaceID, agentID, fp, amount, reason, payee.Kind, payee.ID, payee.Name, memo).Scan(&filed, &inserted); err != nil {
		return AgentApproval{}, fmt.Errorf("economy: request approval: %w", err)
	}
	if inserted {
		s.notifyApproval(workspaceID, filed) // B19.16
	}
	a, err := scanAgentApproval(s.pool.QueryRow(ctx, `SELECT `+agentApprovalColumns+` FROM agent_approvals
		WHERE agent_id = $1 AND fingerprint = $2 AND status IN ('pending', 'approved')`, agentID, fp))
	if err != nil {
		return a, fmt.Errorf("economy: read approval: %w", err)
	}
	return a, nil
}

// ReceiptPosting is one posting of an entry.
type ReceiptPosting struct {
	PostingID  int64  `json:"posting_id"`
	Account    string `json:"account"` // workspace | spend | agent:<id>
	AmountULXC int64  `json:"amount_ulxc"`
}

// AgentReceipt is one entry an agent is party to: every posting of it, which sum to zero.
type AgentReceipt struct {
	EntryID  string           `json:"entry_id"`
	Kind     string           `json:"kind"`
	Ref      string           `json:"ref,omitempty"`
	At       time.Time        `json:"at"`
	Postings []ReceiptPosting `json:"postings"`
}

// AgentReceipt reads entryID, if it moved agentID's account.
func (s *DualTokenStore) AgentReceipt(ctx context.Context, workspaceID, agentID, entryID string) (AgentReceipt, error) {
	r := AgentReceipt{EntryID: entryID, Postings: []ReceiptPosting{}}
	if _, err := uuid.Parse(entryID); err != nil {
		return r, ErrReceiptNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT id, account, amount_ulxc, kind, ref, created_at FROM agent_postings
		WHERE entry_id = $1::uuid AND workspace_id = $2
		  AND EXISTS (SELECT 1 FROM agent_postings o WHERE o.entry_id = $1::uuid AND o.account = $3)
		ORDER BY id`, entryID, workspaceID, agentAccount(agentID))
	if err != nil {
		return r, fmt.Errorf("economy: receipt: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p ReceiptPosting
		if err := rows.Scan(&p.PostingID, &p.Account, &p.AmountULXC, &r.Kind, &r.Ref, &r.At); err != nil {
			return r, err
		}
		r.Postings = append(r.Postings, p)
	}
	if err := rows.Err(); err != nil {
		return r, fmt.Errorf("economy: receipt: %w", err)
	}
	if len(r.Postings) == 0 {
		return r, ErrReceiptNotFound
	}
	return r, nil
}

// RecordAgentToolCall logs one call to the wallet tools: outcome is ok, refused or error.
func (s *DualTokenStore) RecordAgentToolCall(ctx context.Context, workspaceID, agentID, scopedKeyID, tool string, args json.RawMessage, outcome, detail string) error {
	if len(args) == 0 || !json.Valid(args) {
		args = json.RawMessage(`{}`)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_tool_calls (workspace_id, agent_id, scoped_key_id, tool, arguments, outcome, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, workspaceID, agentID, scopedKeyID, tool, []byte(args), outcome, detail); err != nil {
		return fmt.Errorf("economy: log agent tool call: %w", err)
	}
	return nil
}
