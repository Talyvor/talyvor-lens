package economy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/workspace"
)

// agent_transfers.go — B22.3: SEND AND REQUEST MONEY BETWEEN ANY AGENTS ON TALYVOR.
//
// Every wallet has an address: its wallet ID (the agent's id) and a handle its owner picks. Any agent can send
// credits to any other agent, ask one for credits (the asked side accepts or declines), give a transfer back,
// and pay another agent on a schedule (agent_schedules.go). Both owners must be verified (B19.11); the
// sender's rules (B19.2) judge each transfer as spending; between one owner's agents it is GREEN, between
// different owners AMBER (B22.1) — test-funded credits only until the operator clears it.
//
// A transfer is ONE agent_postings entry of TWO postings that sum to zero: agent:<sender> −amount in the
// sender's workspace and agent:<receiver> +amount in the receiver's. Between workspaces the LXC moves with it
// — an lxc_ledger row of type LXCTypeAgentTransfer on each side — and its test-funded part arrives
// test-funded. Credits stay credits: nothing here turns them into money.

// LXCTypeAgentTransfer marks credits leaving or reaching a workspace in a transfer between agents.
const LXCTypeAgentTransfer = "agent_transfer"

// ErrOwnerUnverified: an agent whose owner is not verified cannot send or receive a transfer.
var ErrOwnerUnverified = errors.New("economy: both agents' owners must be verified to move money between them")

// ErrRequestNotFound: no such request, or not one this workspace can act on.
var ErrRequestNotFound = errors.New("economy: no such money request")

// ErrTransferNotFound: no such transfer received by this workspace.
var ErrTransferNotFound = errors.New("economy: no such transfer")

// ErrAlreadyRefunded: the transfer has already been given back.
var ErrAlreadyRefunded = errors.New("economy: this transfer has already been given back")

// ErrHandle: a handle that is malformed or taken.
var ErrHandle = errors.New("economy: handle")

// AgentTransfer is one transfer of credits between two agents.
type AgentTransfer struct {
	ID              string    `json:"id"`
	EntryID         string    `json:"entry_id"`
	FromWorkspaceID string    `json:"from_workspace_id"`
	FromAgentID     string    `json:"from_agent_id"`
	ToWorkspaceID   string    `json:"to_workspace_id"`
	ToAgentID       string    `json:"to_agent_id"`
	AmountULXC      int64     `json:"amount_ulxc"`
	Memo            string    `json:"memo,omitempty"`
	Class           string    `json:"class"`
	TestFundedULXC  int64     `json:"test_funded_ulxc"`
	RequestID       string    `json:"request_id,omitempty"`
	ScheduleID      string    `json:"schedule_id,omitempty"`
	RefundOf        string    `json:"refund_of,omitempty"`
	LoanID          string    `json:"loan_id,omitempty"` // B22.5: the loan it paid out or repaid
	CreatedAt       time.Time `json:"created_at"`
	// B28.299: on a list, the transfer that gave this one back, and whether the listed agent may still give it back.
	RefundedBy string `json:"refunded_by,omitempty"`
	Refundable bool   `json:"refundable,omitempty"`

	capability string // the wallet capability it moves as, when not a plain transfer's (B22.5: a loan's)
}

// MoneyRequest is one agent asking another for credits.
type MoneyRequest struct {
	ID              string     `json:"id"`
	FromWorkspaceID string     `json:"from_workspace_id"` // the asking agent's: it is paid
	FromAgentID     string     `json:"from_agent_id"`
	ToWorkspaceID   string     `json:"to_workspace_id"` // the asked agent's: it pays
	ToAgentID       string     `json:"to_agent_id"`
	AmountULXC      int64      `json:"amount_ulxc"`
	Memo            string     `json:"memo,omitempty"`
	Status          string     `json:"status"` // pending | accepted | declined
	TransferID      string     `json:"transfer_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
}

// WalletAddress is what an address resolves to.
type WalletAddress struct {
	WalletID    string `json:"wallet_id"`
	Handle      string `json:"handle,omitempty"`
	Name        string `json:"name"`
	WorkspaceID string `json:"-"`
}

var handleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)

// SetAgentHandle gives an agent a handle: 3–32 of a–z, 0–9, '.', '_' and '-', starting with a letter or
// digit, unique across Talyvor ("" removes it). A leading '@' is dropped.
func (s *DualTokenStore) SetAgentHandle(ctx context.Context, workspaceID, agentID, handle string) (WalletAddress, error) {
	handle = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(handle), "@"))
	if handle != "" && !handleRE.MatchString(handle) {
		return WalletAddress{}, fmt.Errorf("%w: 3 to 32 of a–z, 0–9, '.', '_' and '-', starting with a letter or a digit", ErrHandle)
	}
	var a WalletAddress
	var h *string
	err := s.pool.QueryRow(ctx, `UPDATE agent_accounts SET handle = NULLIF($3, '') WHERE id = $1 AND workspace_id = $2
		RETURNING id, handle, name, workspace_id`, agentID, workspaceID, handle).Scan(&a.WalletID, &h, &a.Name, &a.WorkspaceID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return a, ErrAgentNotFound
	case err != nil && strings.Contains(err.Error(), "idx_agent_accounts_handle"):
		return a, fmt.Errorf("%w: @%s is taken", ErrHandle, handle)
	case err != nil:
		return a, fmt.Errorf("economy: set handle: %w", err)
	}
	if h != nil {
		a.Handle = *h
	}
	return a, nil
}

// ResolveWallet finds the agent an address names: its wallet ID, or its handle with or without '@'.
func (s *DualTokenStore) ResolveWallet(ctx context.Context, address string) (WalletAddress, error) {
	return resolveWallet(ctx, s.pool, address)
}

func resolveWallet(ctx context.Context, q pgxDB, address string) (WalletAddress, error) {
	address = strings.TrimSpace(address)
	var a WalletAddress
	var h *string
	err := q.QueryRow(ctx, `SELECT id, handle, name, workspace_id FROM agent_accounts
		WHERE id = $1 OR (handle IS NOT NULL AND lower(handle) = lower($2)) LIMIT 1`, address, strings.TrimPrefix(address, "@")).
		Scan(&a.WalletID, &h, &a.Name, &a.WorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrAgentNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: resolve wallet: %w", err)
	}
	if h != nil {
		a.Handle = *h
	}
	return a, nil
}

// SendCredits sends amount µLXC from one of workspaceID's agents to the agent at address.
func (s *DualTokenStore) SendCredits(ctx context.Context, workspaceID, fromAgentID, address string, amount int64, memo string) (AgentTransfer, error) {
	to, err := s.ResolveWallet(ctx, address)
	if err != nil {
		return AgentTransfer{}, err
	}
	t := AgentTransfer{FromWorkspaceID: workspaceID, FromAgentID: fromAgentID, ToWorkspaceID: to.WorkspaceID, ToAgentID: to.WalletID,
		AmountULXC: amount, Memo: memo}
	if key, _ := ctx.Value(moveKeyKey{}).(string); key != "" {
		// B50.1: with an Idempotency-Key the transfer's id is the key's, so a retry finds what it first sent.
		t.ID = "xfer_" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent_transfer\x00"+workspaceID+"\x00"+fromAgentID+"\x00"+key)).String()
	}
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: paymentFingerprint(fromAgentID, to.WalletID, amount, memo),
		Payee: Payee{Kind: "agent", ID: to.WalletID, Name: to.Name}, Memo: memo})
	return s.runTransfer(ctx, t, true)
}

// runTransfer makes t in its own transaction; judged by the sender's rules when judged. A refusal that needs
// a person's approval files it.
func (s *DualTokenStore) runTransfer(ctx context.Context, t AgentTransfer, judged bool) (AgentTransfer, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return t, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if t, err = s.transferTx(ctx, tx, t, judged); err != nil {
		return t, s.refusedMovement(ctx, tx, err)
	}
	return t, tx.Commit(ctx)
}

// transferTx moves t.AmountULXC from t.FromAgentID to t.ToAgentID inside tx and records it.
func (s *DualTokenStore) transferTx(ctx context.Context, tx pgx.Tx, t AgentTransfer, judged bool) (AgentTransfer, error) {
	switch {
	case t.AmountULXC <= 0:
		return t, errors.New("economy: the amount must be positive")
	case t.FromAgentID == t.ToAgentID:
		return t, ErrSameAgent
	}
	if t.ToWorkspaceID == "" {
		to, err := resolveWallet(ctx, tx, t.ToAgentID)
		if err != nil {
			return t, err
		}
		t.ToWorkspaceID = to.WorkspaceID
	}
	// B25.1: test money moves only between test workspaces, real money only between real ones.
	if err := workspace.CheckMoneyWall(ctx, tx, t.FromWorkspaceID, t.ToWorkspaceID); err != nil {
		return t, err
	}
	// Both agents' rows, in id order, so two opposite transfers cannot deadlock.
	type side struct{ ws, agent string }
	sides := []side{{t.FromWorkspaceID, t.FromAgentID}, {t.ToWorkspaceID, t.ToAgentID}}
	if sides[1].agent < sides[0].agent {
		sides[0], sides[1] = sides[1], sides[0]
	}
	for _, x := range sides {
		if err := lockAgent(ctx, tx, x.ws, x.agent); err != nil {
			return t, err
		}
	}
	// B50.1: under both locks, a transfer already made under this id is a retry: the same one answers as made and
	// moves nothing, another is refused.
	if t.ID != "" {
		var was AgentTransfer
		err := tx.QueryRow(ctx, `SELECT from_workspace_id, from_agent_id, to_agent_id, amount_ulxc, memo, entry_id::text, class,
			test_funded_ulxc, created_at FROM agent_transfers WHERE id = $1`, t.ID).Scan(&was.FromWorkspaceID, &was.FromAgentID,
			&was.ToAgentID, &was.AmountULXC, &was.Memo, &was.EntryID, &was.Class, &was.TestFundedULXC, &was.CreatedAt)
		switch {
		case err == nil && (was.FromWorkspaceID != t.FromWorkspaceID || was.FromAgentID != t.FromAgentID || was.ToAgentID != t.ToAgentID ||
			was.AmountULXC != t.AmountULXC || was.Memo != t.Memo):
			return t, ErrIdempotencyKeyReused
		case err == nil:
			t.EntryID, t.Class, t.TestFundedULXC, t.CreatedAt = was.EntryID, was.Class, was.TestFundedULXC, was.CreatedAt
			return t, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return t, fmt.Errorf("economy: earlier transfer: %w", err)
		}
	}
	// B19.11: both owners, verified.
	owners := map[string]string{}
	for _, x := range sides {
		var owner string
		if err := tx.QueryRow(ctx, `SELECT owner_user_id FROM agent_accounts WHERE id = $1`, x.agent).Scan(&owner); err != nil {
			return t, fmt.Errorf("economy: agent owner: %w", err)
		}
		verified := false
		if owner != "" && s.ownerVerifier != nil {
			var err error
			if verified, err = s.ownerVerifier.MayEarn(ctx, tx, x.ws); err != nil {
				return t, fmt.Errorf("economy: owner verification: %w", err)
			}
		}
		if !verified {
			return t, ErrOwnerUnverified
		}
		owners[x.agent] = owner
	}
	capability := "move_between_own_agents"
	t.Class = string(ClassGreen)
	if owners[t.FromAgentID] != owners[t.ToAgentID] {
		capability, t.Class = CapabilityPayAnotherOwner, string(ClassAmber)
	}
	if c, ok := CapabilityByKey(t.capability); ok {
		capability, t.Class = c.Key, string(c.Class)
	}
	bal, err := accountBalance(ctx, tx, t.FromWorkspaceID, agentAccount(t.FromAgentID))
	if err != nil {
		return t, err
	}
	if bal < t.AmountULXC {
		return t, fmt.Errorf("%w: the agent holds %d µLXC", ErrAgentFunds, bal)
	}
	if t.ID == "" {
		t.ID = "xfer_" + uuid.NewString()
	}
	if judged {
		if err := enforceAgentRules(ctx, tx, t.FromWorkspaceID, t.FromAgentID, t.AmountULXC, t.ID); err != nil {
			return t, err
		}
	}
	// The workspaces' balance rows, in id order, after the agents (the lock order everywhere).
	wss := []string{t.FromWorkspaceID}
	if t.ToWorkspaceID != t.FromWorkspaceID {
		wss = append(wss, t.ToWorkspaceID)
		if wss[1] < wss[0] {
			wss[0], wss[1] = wss[1], wss[0]
		}
	}
	for _, ws := range wss {
		if _, _, _, err := readLXCBalance(ctx, tx, ws); err != nil {
			return t, err
		}
	}
	testBefore, err := testFundedULXC(ctx, tx, t.FromWorkspaceID)
	if err != nil {
		return t, err
	}
	// B22.1: an uncleared AMBER transfer takes test-funded credits only, or is refused naming the class.
	if _, err := spendForCapability(ctx, tx, t.FromWorkspaceID, capability, t.AmountULXC); err != nil {
		return t, err
	}
	if t.ToWorkspaceID != t.FromWorkspaceID {
		// B30.8: credits leave or enter a frozen workspace for no other one, whatever the class — the same owner's
		// included, or a freeze would be stepped round through a second workspace.
		if c, ok := CapabilityByKey(capability); ok {
			for _, ws := range []string{t.FromWorkspaceID, t.ToWorkspaceID} {
				if err := refuseFrozenAnyClass(ctx, tx, ws, c); err != nil {
					return t, err
				}
			}
		}
		if err := moveLXC(ctx, tx, t.FromWorkspaceID, -t.AmountULXC, t); err != nil {
			return t, err
		}
	}
	testAfter, err := testFundedULXC(ctx, tx, t.FromWorkspaceID)
	if err != nil {
		return t, err
	}
	t.TestFundedULXC = max(testBefore-testAfter, 0)
	if t.ToWorkspaceID != t.FromWorkspaceID {
		if err := moveLXC(ctx, tx, t.ToWorkspaceID, t.AmountULXC, t); err != nil {
			return t, err
		}
	}
	if err := addTestFunded(ctx, tx, t.ToWorkspaceID, t.TestFundedULXC); err != nil { // test money arrives test money
		return t, err
	}
	entry := uuid.New()
	t.EntryID = entry.String()
	for _, p := range []struct {
		ws, account string
		amount      int64
	}{{t.FromWorkspaceID, agentAccount(t.FromAgentID), -t.AmountULXC}, {t.ToWorkspaceID, agentAccount(t.ToAgentID), t.AmountULXC}} {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref) VALUES ($1, $2, $3, $4, 'transfer', $5)`,
			entry, p.ws, p.account, p.amount, t.ID); err != nil {
			return t, fmt.Errorf("economy: post transfer: %w", err)
		}
	}
	if err := tx.QueryRow(ctx, `INSERT INTO agent_transfers (id, entry_id, from_workspace_id, from_agent_id, to_workspace_id, to_agent_id,
		amount_ulxc, memo, class, test_funded_ulxc, request_id, schedule_id, refund_of, loan_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING created_at`,
		t.ID, entry, t.FromWorkspaceID, t.FromAgentID, t.ToWorkspaceID, t.ToAgentID, t.AmountULXC, t.Memo, t.Class, t.TestFundedULXC,
		t.RequestID, t.ScheduleID, t.RefundOf, t.LoanID).Scan(&t.CreatedAt); err != nil {
		if strings.Contains(err.Error(), "idx_agent_transfers_refund") {
			return t, ErrAlreadyRefunded
		}
		return t, fmt.Errorf("economy: record transfer: %w", err)
	}
	return t, nil
}

// moveLXC moves delta µLXC in or out of workspaceID's balance for transfer t, with its ledger row. The
// caller holds the balance row.
func moveLXC(ctx context.Context, tx pgx.Tx, workspaceID string, delta int64, t AgentTransfer) error {
	bal, minted, spent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	desc, other, otherWS := "credits sent to another agent", t.ToAgentID, t.ToWorkspaceID
	if delta > 0 {
		desc, other, otherWS = "credits received from another agent", t.FromAgentID, t.FromWorkspaceID
	}
	if err := insertLXCLedger(ctx, tx, workspaceID, delta, bal+delta, LXCTypeAgentTransfer, desc, map[string]interface{}{
		"transfer_id": t.ID, "counterparty_agent_id": other, "counterparty_workspace_id": otherWS, "class": t.Class}); err != nil {
		return err
	}
	return writeLXCBalance(ctx, tx, workspaceID, bal+delta, minted, spent)
}

// RequestCredits has one of workspaceID's agents ask the agent at address for amount µLXC.
func (s *DualTokenStore) RequestCredits(ctx context.Context, workspaceID, agentID, address string, amount int64, memo string) (MoneyRequest, error) {
	if amount <= 0 {
		return MoneyRequest{}, errors.New("economy: the amount must be positive")
	}
	payer, err := s.ResolveWallet(ctx, address)
	if err != nil {
		return MoneyRequest{}, err
	}
	if payer.WalletID == agentID {
		return MoneyRequest{}, ErrSameAgent
	}
	if err := workspace.CheckMoneyWall(ctx, s.pool, workspaceID, payer.WorkspaceID); err != nil {
		return MoneyRequest{}, err
	}
	r, err := scanMoneyRequest(s.pool.QueryRow(ctx, `INSERT INTO agent_money_requests (id, from_workspace_id, from_agent_id, to_workspace_id,
		to_agent_id, amount_ulxc, memo)
		SELECT $1, a.workspace_id, a.id, $4, $5, $6, $7 FROM agent_accounts a WHERE a.id = $3 AND a.workspace_id = $2
		RETURNING `+moneyRequestColumns, "mreq_"+uuid.NewString(), workspaceID, agentID, payer.WorkspaceID, payer.WalletID, amount, memo))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrAgentNotFound
	}
	return r, err
}

// AnswerMoneyRequest accepts (making the transfer, judged as the paying agent's spending) or declines a
// request made to one of workspaceID's agents.
func (s *DualTokenStore) AnswerMoneyRequest(ctx context.Context, workspaceID, requestID string, accept bool) (MoneyRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MoneyRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := scanMoneyRequest(tx.QueryRow(ctx, `SELECT `+moneyRequestColumns+` FROM agent_money_requests
		WHERE id = $1 AND to_workspace_id = $2 AND status = 'pending' FOR UPDATE`, requestID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrRequestNotFound
	}
	if err != nil {
		return r, err
	}
	status := "declined"
	if accept {
		status = "accepted"
		ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: paymentFingerprint(r.ToAgentID, r.FromAgentID, r.AmountULXC, r.Memo),
			Payee: Payee{Kind: "agent", ID: r.FromAgentID}, Memo: r.Memo})
		t, err := s.transferTx(ctx, tx, AgentTransfer{FromWorkspaceID: r.ToWorkspaceID, FromAgentID: r.ToAgentID,
			ToWorkspaceID: r.FromWorkspaceID, ToAgentID: r.FromAgentID, AmountULXC: r.AmountULXC, Memo: r.Memo, RequestID: r.ID}, true)
		if err != nil {
			return r, s.refusedMovement(ctx, tx, err)
		}
		r.TransferID = t.ID
	}
	if r, err = scanMoneyRequest(tx.QueryRow(ctx, `UPDATE agent_money_requests SET status = $2, transfer_id = $3, decided_at = now()
		WHERE id = $1 RETURNING `+moneyRequestColumns, r.ID, status, r.TransferID)); err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

// ListMoneyRequests reads the requests workspaceID's agents made and were made, newest first.
func (s *DualTokenStore) ListMoneyRequests(ctx context.Context, workspaceID string) ([]MoneyRequest, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+moneyRequestColumns+` FROM agent_money_requests
		WHERE from_workspace_id = $1 OR to_workspace_id = $1 ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: money requests: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (MoneyRequest, error) { return scanMoneyRequest(row) })
}

// RefundTransfer gives a transfer one of workspaceID's agents received back to its sender, once. Giving back
// is not new spending, so the receiver's rules do not judge it; its class and funding are the transfer's own.
// A loan's payout or instalment is not given back this way (B22.5).
func (s *DualTokenStore) RefundTransfer(ctx context.Context, workspaceID, transferID string) (AgentTransfer, error) {
	var orig AgentTransfer
	var refunded bool
	err := s.pool.QueryRow(ctx, `SELECT from_workspace_id, from_agent_id, to_agent_id, amount_ulxc, memo,
		EXISTS (SELECT 1 FROM agent_transfers r WHERE r.refund_of = t.id) FROM agent_transfers t
		WHERE id = $1 AND to_workspace_id = $2 AND refund_of = '' AND loan_id = ''`, transferID, workspaceID).
		Scan(&orig.FromWorkspaceID, &orig.FromAgentID, &orig.ToAgentID, &orig.AmountULXC, &orig.Memo, &refunded)
	if errors.Is(err, pgx.ErrNoRows) {
		return orig, ErrTransferNotFound
	}
	if err != nil {
		return orig, fmt.Errorf("economy: transfer: %w", err)
	}
	if refunded { // the unique index on refund_of refuses a concurrent second one
		return orig, ErrAlreadyRefunded
	}
	return s.runTransfer(ctx, AgentTransfer{FromWorkspaceID: workspaceID, FromAgentID: orig.ToAgentID, ToWorkspaceID: orig.FromWorkspaceID,
		ToAgentID: orig.FromAgentID, AmountULXC: orig.AmountULXC, Memo: "refund: " + orig.Memo, RefundOf: transferID}, false)
}

// ListAgentTransfers reads the transfers an agent of workspaceID sent or received, newest first. Each says which
// transfer gave it back, and is refundable when the agent received it and RefundTransfer would still take it.
func (s *DualTokenStore) ListAgentTransfers(ctx context.Context, workspaceID, agentID string) ([]AgentTransfer, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, entry_id::text, from_workspace_id, from_agent_id, to_workspace_id, to_agent_id, amount_ulxc, memo,
		class, test_funded_ulxc, request_id, schedule_id, refund_of, loan_id, created_at,
		COALESCE((SELECT r.id FROM agent_transfers r WHERE r.refund_of = t.id), '') FROM agent_transfers t
		WHERE (from_workspace_id = $1 AND from_agent_id = $2) OR (to_workspace_id = $1 AND to_agent_id = $2)
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: transfers: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AgentTransfer, error) {
		var t AgentTransfer
		err := row.Scan(&t.ID, &t.EntryID, &t.FromWorkspaceID, &t.FromAgentID, &t.ToWorkspaceID, &t.ToAgentID, &t.AmountULXC, &t.Memo,
			&t.Class, &t.TestFundedULXC, &t.RequestID, &t.ScheduleID, &t.RefundOf, &t.LoanID, &t.CreatedAt, &t.RefundedBy)
		t.Refundable = t.ToWorkspaceID == workspaceID && t.ToAgentID == agentID && t.RefundOf == "" && t.LoanID == "" && t.RefundedBy == ""
		return t, err
	})
}

const moneyRequestColumns = `id, from_workspace_id, from_agent_id, to_workspace_id, to_agent_id, amount_ulxc, memo, status, transfer_id,
	created_at, decided_at`

func scanMoneyRequest(row pgx.Row) (MoneyRequest, error) {
	var r MoneyRequest
	err := row.Scan(&r.ID, &r.FromWorkspaceID, &r.FromAgentID, &r.ToWorkspaceID, &r.ToAgentID, &r.AmountULXC, &r.Memo, &r.Status,
		&r.TransferID, &r.CreatedAt, &r.DecidedAt)
	return r, err
}
