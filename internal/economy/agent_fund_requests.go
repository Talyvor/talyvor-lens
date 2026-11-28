package economy

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// agent_fund_requests.go — B28.73: THE OWNER'S SDK AND MCP TOOLS ASK TO FUND AN AGENT; A PERSON FUNDS IT.
//
// A key — the owner's SDK, or an assistant over MCP — files a request to move amount of the workspace's unallocated
// LXC to one of its agents, under an Idempotency-Key, and gets back an approval link. Nothing moves while it is
// pending. A person signed in to the workspace approves it from the link, and FundAgent then moves it: one posting,
// its ref the request's key, so the same key is one posting however often it is sent or approved. A declined request
// moves nothing. Class: rules_approvals_statements_pots, GREEN — the request moves nothing; the approval is FundAgent's.

var (
	// ErrFundRequest: arguments a fund request cannot have.
	ErrFundRequest = errors.New("economy: invalid fund request")
	// ErrFundRequestNotFound: no such fund request in this workspace.
	ErrFundRequestNotFound = errors.New("economy: no such fund request")
	// ErrFundRequestConflict: the Idempotency-Key was used before for another agent or amount.
	ErrFundRequestConflict = errors.New("economy: this Idempotency-Key was already used for a different fund request")
	// ErrFundRequestDecided: the request was already approved or declined.
	ErrFundRequestDecided = errors.New("economy: the fund request was already decided")
)

// FundRequestLinkBase is the app's page for an agent, where a person opens a fund request (?fund_request=).
const FundRequestLinkBase = "https://app.talyvor.com/agents/"

// The states of a fund request.
const (
	FundRequestPending  = "pending"
	FundRequestApproved = "approved"
	FundRequestDeclined = "declined"
)

// AgentFundRequest is a request to fund an agent, as kept.
type AgentFundRequest struct {
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	AmountULXC     int64      `json:"amount_ulxc"`
	IdempotencyKey string     `json:"idempotency_key"`
	Status         string     `json:"status"`
	RequestedBy    string     `json:"requested_by"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	// ApprovalURL is where a person approves or declines it.
	ApprovalURL string `json:"approval_url"`
	// BalanceULXC is the agent's balance once the approval funded it; null otherwise.
	BalanceULXC *int64 `json:"balance_ulxc"`
}

const fundRequestColumns = `id, agent_id, amount_ulxc, idempotency_key, status, requested_by, created_at, decided_at, decided_by`

func scanFundRequest(row pgx.Row) (AgentFundRequest, error) {
	var r AgentFundRequest
	err := row.Scan(&r.ID, &r.AgentID, &r.AmountULXC, &r.IdempotencyKey, &r.Status, &r.RequestedBy, &r.CreatedAt, &r.DecidedAt, &r.DecidedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrFundRequestNotFound
	}
	if err != nil {
		return r, fmt.Errorf("economy: fund request: %w", err)
	}
	r.ApprovalURL = FundRequestLinkBase + url.PathEscape(r.AgentID) + "?fund_request=" + url.QueryEscape(r.ID)
	return r, nil
}

// RequestAgentFunding files by's request to move amount of workspaceID's unallocated LXC to agentID, under
// idempotencyKey. It moves nothing. The same key for the same agent and amount returns the request kept the first time
// (again = true); for another agent or amount it is ErrFundRequestConflict.
func (s *DualTokenStore) RequestAgentFunding(ctx context.Context, workspaceID, agentID string, amount int64, idempotencyKey, by string) (r AgentFundRequest, again bool, err error) {
	if idempotencyKey = strings.TrimSpace(idempotencyKey); idempotencyKey == "" || len(idempotencyKey) > 128 {
		return r, false, fmt.Errorf("%w: an Idempotency-Key of at most 128 characters is required", ErrFundRequest)
	}
	if amount <= 0 {
		return r, false, fmt.Errorf("%w: the amount must be positive", ErrFundRequest)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return r, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return r, false, err
	}
	if err := requireNotArchived(ctx, tx, agentID); err != nil {
		return r, false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO agent_fund_requests (id, workspace_id, agent_id, amount_ulxc, idempotency_key, requested_by)
	  VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (workspace_id, idempotency_key) DO NOTHING`,
		"afr_"+uuid.NewString(), workspaceID, agentID, amount, idempotencyKey, by)
	if err != nil {
		return r, false, fmt.Errorf("economy: fund request: %w", err)
	}
	r, err = scanFundRequest(tx.QueryRow(ctx, `SELECT `+fundRequestColumns+` FROM agent_fund_requests
	  WHERE workspace_id = $1 AND idempotency_key = $2`, workspaceID, idempotencyKey))
	if err != nil {
		return r, false, err
	}
	if r.AgentID != agentID || r.AmountULXC != amount {
		return AgentFundRequest{}, false, ErrFundRequestConflict
	}
	return r, tag.RowsAffected() == 0, tx.Commit(ctx)
}

// AgentFundRequestByID is workspaceID's fund request id.
func (s *DualTokenStore) AgentFundRequestByID(ctx context.Context, workspaceID, id string) (AgentFundRequest, error) {
	return scanFundRequest(s.pool.QueryRow(ctx, `SELECT `+fundRequestColumns+` FROM agent_fund_requests
	  WHERE workspace_id = $1 AND id = $2`, workspaceID, id))
}

// ApproveAgentFundRequest is by's approval of workspaceID's fund request id: FundAgent moves its amount to its agent
// under its key, and it is approved. An approved request approved again moves nothing more; a declined one is
// ErrFundRequestDecided. A refusal of FundAgent's — the workspace cannot cover it, the agent has no owner — leaves it
// pending, to be approved once that is put right.
func (s *DualTokenStore) ApproveAgentFundRequest(ctx context.Context, workspaceID, id, by string) (AgentFundRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentFundRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The request's row is held until it is marked: an approval and a decline at once are one, then the other refused.
	r, err := scanFundRequest(tx.QueryRow(ctx, `SELECT `+fundRequestColumns+` FROM agent_fund_requests
	  WHERE workspace_id = $1 AND id = $2 FOR UPDATE`, workspaceID, id))
	if err != nil {
		return r, err
	}
	if r.Status == FundRequestDeclined {
		return r, fmt.Errorf("%w: it was declined", ErrFundRequestDecided)
	}
	// Approved or not, the move is FundAgent's under the request's key: a key already posted moves nothing again, so a
	// second approval, or one after a first whose mark was lost, answers the balance and posts nothing.
	bal, err := s.FundAgent(WithMoveKey(ctx, r.IdempotencyKey), workspaceID, r.AgentID, r.AmountULXC)
	if err != nil {
		return r, err
	}
	r.BalanceULXC = &bal
	if r.Status == FundRequestApproved {
		return r, nil
	}
	if err := tx.QueryRow(ctx, `UPDATE agent_fund_requests SET status = 'approved', decided_at = now(), decided_by = $2
	  WHERE id = $1 RETURNING status, decided_at, decided_by`, id, by).Scan(&r.Status, &r.DecidedAt, &r.DecidedBy); err != nil {
		return r, fmt.Errorf("economy: approve fund request: %w", err)
	}
	return r, tx.Commit(ctx)
}

// DeclineAgentFundRequest is by's refusal of workspaceID's pending fund request id: nothing moves. One already decided
// is ErrFundRequestDecided — and so is one whose key already moved money, which is marked approved.
func (s *DualTokenStore) DeclineAgentFundRequest(ctx context.Context, workspaceID, id, by string) (AgentFundRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentFundRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := scanFundRequest(tx.QueryRow(ctx, `SELECT `+fundRequestColumns+` FROM agent_fund_requests
	  WHERE workspace_id = $1 AND id = $2 FOR UPDATE`, workspaceID, id))
	if err != nil {
		return r, err
	}
	if r.Status != FundRequestPending {
		return r, fmt.Errorf("%w: it is %s", ErrFundRequestDecided, r.Status)
	}
	var posted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_postings WHERE workspace_id = $1 AND account = $2 AND kind = 'fund' AND ref = $3)`,
		workspaceID, agentAccount(r.AgentID), r.IdempotencyKey).Scan(&posted); err != nil {
		return r, fmt.Errorf("economy: decline fund request: %w", err)
	}
	status := FundRequestDeclined
	if posted { // its approval moved the money and its mark was lost: it is approved, and stays so
		status = FundRequestApproved
	}
	if err := tx.QueryRow(ctx, `UPDATE agent_fund_requests SET status = $2, decided_at = now(), decided_by = $3
	  WHERE id = $1 RETURNING status, decided_at, decided_by`, id, status, by).Scan(&r.Status, &r.DecidedAt, &r.DecidedBy); err != nil {
		return r, fmt.Errorf("economy: decline fund request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return r, err
	}
	if posted {
		return r, fmt.Errorf("%w: its money already moved", ErrFundRequestDecided)
	}
	return r, nil
}
