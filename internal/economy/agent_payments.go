package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/workspace"
)

// agent_payments.go — B19.3: ONE COMPANY'S AGENTS PAY EACH OTHER, INSIDE THE CLOSED LOOP.
//
// A payment is ONE entry of two postings: agent:<payer> −amount, agent:<payee> +amount. The LXC never
// leaves the workspace — lxc_balances and lxc_ledger do not move, and neither do the 'workspace' and
// 'spend' accounts — so it cannot mint, redeem or reach another company. The payer's rules (B19.2) judge
// it as spending: its active hours, its limits per payment, day and month, and its approval amount.

// ErrSameAgent: an agent cannot pay itself.
var ErrSameAgent = errors.New("economy: an agent cannot pay itself")

// ErrWashTrade: a payment to another company's agent that the single-party detector refused.
var ErrWashTrade = errors.New("economy: refused as a wash trade")

// AgentPayment is one payment between two of a workspace's agents — or, through the marketplace, to
// another company's agent (B19.15): then EntryID is the use on the company's marketplace bill, ToWorkspaceID
// the company paid, and neither agent's balance moves (the company's bill carries it).
type AgentPayment struct {
	EntryID         string `json:"entry_id"`
	FromAgentID     string `json:"from_agent_id"`
	ToAgentID       string `json:"to_agent_id"`
	AmountULXC      int64  `json:"amount_ulxc"`
	FromBalanceULXC int64  `json:"from_balance_ulxc"`
	ToBalanceULXC   int64  `json:"to_balance_ulxc"`
	Memo            string `json:"memo,omitempty"`
	ToWorkspaceID   string `json:"to_workspace_id,omitempty"`
	Via             string `json:"via,omitempty"` // "marketplace": on the company's monthly marketplace bill
}

// CompanyPayments is what a payment to another company's agent needs of the marketplace (B19.15).
// *market.Store satisfies it.
type CompanyPayments interface {
	// CompanyPayeeRefusal says why payerWorkspaceID may not pay payeeWorkspaceID's agents, or "".
	CompanyPayeeRefusal(ctx context.Context, payerWorkspaceID, payeeWorkspaceID string) (string, error)
	// ChargeAgentPayment records the payment, in tx, on the paying company's marketplace bill.
	ChargeAgentPayment(ctx context.Context, tx pgx.Tx, payerWorkspaceID, fromAgentID, payeeWorkspaceID, toAgentID string,
		amount int64, memo string, at time.Time) (useID string, err error)
}

// SetCompanyPayments lets agents pay other companies' agents through the marketplace. Unset, an agent of
// another company is one this workspace does not have.
func (s *DualTokenStore) SetCompanyPayments(c CompanyPayments) { s.companyPayments = c }

// AgentOfKey returns the agent a key is attached to, and its workspace; ErrAgentNotFound for a key
// attached to none.
func (s *DualTokenStore) AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT a.id, a.workspace_id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		WHERE k.scoped_key_id = $1`, scopedKeyID).Scan(&agentID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrAgentNotFound
	}
	return agentID, workspaceID, err
}

// PayAgent moves amount µLXC from one of workspaceID's agents to another.
func (s *DualTokenStore) PayAgent(ctx context.Context, workspaceID, fromAgentID, toAgentID string, amount int64, memo string) (AgentPayment, error) {
	pay := AgentPayment{FromAgentID: fromAgentID, ToAgentID: toAgentID, AmountULXC: amount, Memo: memo}
	if amount <= 0 {
		return pay, errors.New("economy: the amount must be positive")
	}
	if fromAgentID == toAgentID {
		return pay, ErrSameAgent
	}
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: paymentFingerprint(fromAgentID, toAgentID, amount, memo),
		Payee: Payee{Kind: "agent", ID: toAgentID}, Memo: memo})
	if s.companyPayments != nil {
		var payeeWorkspace string
		err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM agent_accounts WHERE id = $1`, toAgentID).Scan(&payeeWorkspace)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return pay, fmt.Errorf("economy: payee agent: %w", err)
		}
		if err == nil && payeeWorkspace != workspaceID {
			return s.payCompanyAgent(ctx, workspaceID, payeeWorkspace, pay)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return pay, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	entry := uuid.New()
	if key, _ := ctx.Value(moveKeyKey{}).(string); key != "" {
		// B50.1: with an Idempotency-Key the entry is the key's, so a retry finds what it first posted.
		entry = uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent_pay\x00"+workspaceID+"\x00"+fromAgentID+"\x00"+key))
	}
	if pay, err = payAgentTx(ctx, tx, workspaceID, pay, entry); err != nil {
		return pay, s.refusedMovement(ctx, tx, err)
	}
	return pay, tx.Commit(ctx)
}

// payCompanyAgent pays another company's agent through the marketplace (B19.15): the single-party detector
// first, then the paying agent's rules exactly as for a payment inside the company, then one billed use on
// the company's monthly marketplace bill — in the rules' transaction, so the next payment's limits count it.
func (s *DualTokenStore) payCompanyAgent(ctx context.Context, workspaceID, payeeWorkspace string, pay AgentPayment) (AgentPayment, error) {
	pay.ToWorkspaceID, pay.Via = payeeWorkspace, "marketplace"
	if err := workspace.CheckMoneyWall(ctx, s.pool, workspaceID, payeeWorkspace); err != nil {
		return pay, err
	}
	refusal, err := s.companyPayments.CompanyPayeeRefusal(ctx, workspaceID, payeeWorkspace)
	if err != nil {
		return pay, err
	}
	if refusal != "" {
		return pay, fmt.Errorf("%w: %s", ErrWashTrade, refusal)
	}
	// B22.1: paying another owner is AMBER — the company's marketplace bill is real money once Stripe is live.
	if err := s.requireBilledCapability(ctx, s.pool, workspaceID, CapabilityPayAnotherOwner); err != nil {
		return pay, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return pay, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, pay.FromAgentID); err != nil {
		return pay, err
	}
	// B30.8: neither a frozen payer nor a frozen payee moves money with another owner.
	c, _ := CapabilityByKey(CapabilityPayAnotherOwner)
	for _, ws := range []string{workspaceID, payeeWorkspace} {
		if err := refuseFrozen(ctx, tx, ws, c); err != nil {
			return pay, err
		}
	}
	if err := enforceAgentRules(ctx, tx, workspaceID, pay.FromAgentID, pay.AmountULXC, "company:"+payeeWorkspace); err != nil {
		return pay, s.refusedMovement(ctx, tx, err)
	}
	if pay.EntryID, err = s.companyPayments.ChargeAgentPayment(ctx, tx, workspaceID, pay.FromAgentID, payeeWorkspace, pay.ToAgentID,
		pay.AmountULXC, pay.Memo, agentRequestFrom(ctx).At); err != nil {
		return pay, err
	}
	if pay.FromBalanceULXC, err = accountBalance(ctx, tx, workspaceID, agentAccount(pay.FromAgentID)); err != nil {
		return pay, err
	}
	return pay, tx.Commit(ctx)
}

// paymentFingerprint is what an approval of a payment is for: the payer, the payee, the amount and the memo.
func paymentFingerprint(fromAgentID, toAgentID string, amount int64, memo string) string {
	fp := sha256.Sum256([]byte("payment\x00" + fromAgentID + "\x00" + toAgentID + "\x00" + strconv.FormatInt(amount, 10) + "\x00" + memo))
	return hex.EncodeToString(fp[:])
}

// payAgentTx posts pay as entry inside tx, judged by the payer's rules. Its refusals are returned as
// they are; PayAgent files the approval one needs.
func payAgentTx(ctx context.Context, tx pgx.Tx, workspaceID string, pay AgentPayment, entry uuid.UUID) (AgentPayment, error) {
	fromAgentID, toAgentID, amount, memo := pay.FromAgentID, pay.ToAgentID, pay.AmountULXC, pay.Memo
	// Both agents' rows, in id order, so two opposite payments cannot deadlock.
	first, second := fromAgentID, toAgentID
	if second < first {
		first, second = second, first
	}
	for _, id := range []string{first, second} {
		if err := lockAgent(ctx, tx, workspaceID, id); err != nil {
			return pay, err
		}
	}
	// B50.1: under both locks, an entry already posted is a retry: the same payment answers as made and moves nothing,
	// another under the same key is refused.
	var paidTo, paidMemo string
	var paid int64
	err := tx.QueryRow(ctx, `SELECT account, amount_ulxc, ref FROM agent_postings WHERE entry_id = $1 AND amount_ulxc > 0`, entry).
		Scan(&paidTo, &paid, &paidMemo)
	switch {
	case err == nil && (paidTo != agentAccount(toAgentID) || paid != amount || paidMemo != memo):
		return pay, ErrIdempotencyKeyReused
	case err == nil:
		pay.EntryID = entry.String()
		if pay.FromBalanceULXC, err = accountBalance(ctx, tx, workspaceID, agentAccount(fromAgentID)); err != nil {
			return pay, err
		}
		pay.ToBalanceULXC, err = accountBalance(ctx, tx, workspaceID, agentAccount(toAgentID))
		return pay, err
	case !errors.Is(err, pgx.ErrNoRows):
		return pay, fmt.Errorf("economy: earlier payment: %w", err)
	}
	if err := requireOwner(ctx, tx, toAgentID); err != nil { // B19.11: a payment would give it a balance
		return pay, err
	}
	bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(fromAgentID))
	if err != nil {
		return pay, err
	}
	if bal < amount {
		return pay, fmt.Errorf("%w: the agent holds %d µLXC", ErrAgentFunds, bal)
	}
	pay.EntryID = entry.String()
	if err := enforceAgentRules(ctx, tx, workspaceID, fromAgentID, amount, pay.EntryID); err != nil {
		return pay, err
	}
	for _, l := range []leg{{agentAccount(fromAgentID), -amount}, {agentAccount(toAgentID), amount}} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref) VALUES ($1, $2, $3, $4, 'pay', $5)`,
			entry, workspaceID, l.account, l.amount, memo); err != nil {
			return pay, fmt.Errorf("economy: post payment: %w", err)
		}
	}
	if pay.ToBalanceULXC, err = accountBalance(ctx, tx, workspaceID, agentAccount(toAgentID)); err != nil {
		return pay, err
	}
	pay.FromBalanceULXC = bal - amount
	return pay, nil
}

// AgentStatementLine is one posting on an agent's account, with what it moved against and the balance
// it left.
type AgentStatementLine struct {
	EntryID          string    `json:"entry_id"`
	Kind             string    `json:"kind"` // fund | withdraw | spend | hold | settle | release | pay | platform_fee
	AmountULXC       int64     `json:"amount_ulxc"`
	Counterparty     string    `json:"counterparty"`  // workspace | spend | agent:<id>
	Ref              string    `json:"ref,omitempty"` // a payment's memo, or the request it paid for
	BalanceAfterULXC int64     `json:"balance_after_ulxc"`
	At               time.Time `json:"at"`
	// Label names a platform fee line, with its rate: "Platform fee 3%" (B32.11). Empty on every other kind.
	Label string `json:"label,omitempty"`
}

// AgentStatement reads an agent's account, newest first, at most limit lines.
func (s *DualTokenStore) AgentStatement(ctx context.Context, workspaceID, agentID string, limit int) ([]AgentStatementLine, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("economy: agent statement: %w", err)
	}
	if !exists {
		return nil, ErrAgentNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT entry_id::text, kind, amount_ulxc, counterparty, ref, balance_after, created_at, fee_bps FROM (
		  SELECT p.id, p.entry_id, p.kind, p.amount_ulxc, p.ref, p.created_at, p.fee_bps,
		         COALESCE((SELECT o.account FROM agent_postings o WHERE o.entry_id = p.entry_id AND o.id <> p.id LIMIT 1), '') AS counterparty,
		         sum(p.amount_ulxc) OVER (ORDER BY p.id)::bigint AS balance_after
		    FROM agent_postings p WHERE p.workspace_id = $1 AND p.account = $2) lines
		ORDER BY id DESC LIMIT $3`, workspaceID, agentAccount(agentID), limit)
	if err != nil {
		return nil, fmt.Errorf("economy: agent statement: %w", err)
	}
	defer rows.Close()
	out := []AgentStatementLine{}
	for rows.Next() {
		var l AgentStatementLine
		var feeBPS *int64
		if err := rows.Scan(&l.EntryID, &l.Kind, &l.AmountULXC, &l.Counterparty, &l.Ref, &l.BalanceAfterULXC, &l.At, &feeBPS); err != nil {
			return nil, err
		}
		if l.Kind == LXCTypePlatformFee && feeBPS != nil {
			l.Label = FeeLabel(*feeBPS)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
