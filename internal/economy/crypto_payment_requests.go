package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/smartaccount"
)

// crypto_payment_requests.go — B48.10: ASKING TO BE PAID INTO AN AGENT'S CRYPTO WALLET.
//
// An agent, or its owner, asks for an amount of USDC on one network the chain indexer reads, with a memo and an expiry.
// The request is a link to share, an EIP-681 URI for a wallet or a QR code, and an x402 payment requirement for an
// agent. Nothing here moves money: when the indexer posts a deposit into the wallet (ChainIndexer.move), the deposit
// is matched to the wallet's oldest open request it fits — one naming the sender first, then one it pays exactly — and
// a reorg that takes the deposit back takes back its match. A request is then paid, partly paid (what is left is
// shown), or overpaid (the owner says refund or keep); a payment seen after the expiry does not count and is shown as
// late. A request naming a payer that is another Talyvor agent lands in that agent's owner's approvals, with the
// trust Talyvor has in the requester.

var (
	// ErrCryptoRequest: arguments a request cannot have.
	ErrCryptoRequest = errors.New("economy: invalid request to be paid")
	// ErrCryptoRequestNotFound: no such request in this workspace.
	ErrCryptoRequestNotFound = errors.New("economy: no such request to be paid")
	// ErrCryptoRequestConflict: the Idempotency-Key was used before for another request.
	ErrCryptoRequestConflict = errors.New("economy: this Idempotency-Key was already used for a different request")
	// ErrCryptoRequestWaiting: this agent's earlier request to the same Talyvor agent still waits in its owner's
	// approvals — one at a time, so no agent can fill another owner's approvals.
	ErrCryptoRequestWaiting = errors.New("economy: this agent's earlier request to that agent still waits for its owner")
)

// CryptoRequestLinkBase is the app's page for a request: the link that is shared.
const CryptoRequestLinkBase = "https://app.talyvor.com/request/"

// CryptoRequestIn is a request as asked for.
type CryptoRequestIn struct {
	WorkspaceID    string    `json:"-"`
	AgentID        string    `json:"-"`
	Token          string    `json:"token"` // USDC, the one token the indexer reads
	Network        string    `json:"network"`
	AmountMinor    int64     `json:"amount_minor"` // micro-USDC
	Memo           string    `json:"memo"`
	ExpiresAt      time.Time `json:"expires_at"`
	PayerAddress   string    `json:"payer_address,omitempty"` // only this address's payments count; a Talyvor agent's is asked in its owner's approvals
	IdempotencyKey string    `json:"-"`
	CreatedBy      string    `json:"-"`
	Trust          string    `json:"-"` // the requester's trust level as its approval card shows it (B45.24)
}

// CryptoRequestPayment is one deposit matched to a request.
type CryptoRequestPayment struct {
	TxHash       string    `json:"tx_hash"`
	Counterparty string    `json:"from"`
	AmountMinor  int64     `json:"amount_minor"`
	Late         bool      `json:"late"` // seen after the request expired: it does not count
	SeenAt       time.Time `json:"seen_at"`
}

// CryptoRequest is a request as kept, with where it stands.
type CryptoRequest struct {
	ID             string                 `json:"id"`
	AgentID        string                 `json:"agent_id,omitempty"`
	Token          string                 `json:"token"`
	Network        string                 `json:"network"`
	ChainID        int64                  `json:"chain_id"`
	Asset          string                 `json:"asset"` // the token's contract
	PayTo          string                 `json:"pay_to"`
	AmountMinor    int64                  `json:"amount_minor"`
	Memo           string                 `json:"memo"`
	ExpiresAt      time.Time              `json:"expires_at"`
	PayerAddress   string                 `json:"payer_address,omitempty"`
	ApprovalID     string                 `json:"-"`      // in the payer's workspace: never shown to the requester
	Status         string                 `json:"status"` // open | partial | paid | overpaid | expired
	ReceivedMinor  int64                  `json:"received_minor"`
	RemainingMinor int64                  `json:"remaining_minor"`
	OverpaidMinor  int64                  `json:"overpaid_minor"`
	LateMinor      int64                  `json:"late_minor"`
	Overpayment    string                 `json:"overpayment,omitempty"` // the owner's decision: refund | keep
	Payments       []CryptoRequestPayment `json:"payments,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
	Link           string                 `json:"link"`
	URI            string                 `json:"uri"` // EIP-681: what the QR code encodes
	X402           map[string]any         `json:"x402,omitempty"`
}

// fill works out where r stands at now, and its link, URI and — while something is due — its x402 requirement.
func (r *CryptoRequest) fill(now time.Time) {
	r.RemainingMinor, r.OverpaidMinor = max(0, r.AmountMinor-r.ReceivedMinor), max(0, r.ReceivedMinor-r.AmountMinor)
	switch {
	case r.OverpaidMinor > 0:
		r.Status = "overpaid"
	case r.RemainingMinor == 0:
		r.Status = "paid"
	case !now.Before(r.ExpiresAt):
		r.Status = "expired"
	case r.ReceivedMinor > 0:
		r.Status = "partial"
	default:
		r.Status = "open"
	}
	r.Link = CryptoRequestLinkBase + r.ID
	r.URI = fmt.Sprintf("ethereum:%s@%d/transfer?address=%s&uint256=%d", r.Asset, r.ChainID, r.PayTo, r.RemainingMinor)
	r.X402 = nil
	if r.Status == "open" || r.Status == "partial" {
		r.X402 = map[string]any{"x402Version": 1, "accepts": []map[string]any{{
			"scheme": "exact", "network": r.Network, "maxAmountRequired": strconv.FormatInt(r.RemainingMinor, 10),
			"resource": r.Link, "description": r.Memo, "mimeType": "application/json", "payTo": r.PayTo,
			"maxTimeoutSeconds": int64(r.ExpiresAt.Sub(now).Seconds()), "asset": r.Asset,
			"extra": map[string]any{"name": "USD Coin", "version": "2"}}}}
	}
}

// usdcString is minor micro-USDC as a decimal: 25.5 for 25500000.
func usdcString(minor int64) string {
	s := fmt.Sprintf("%d.%06d", minor/1_000000, minor%1_000000)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// CreatePaymentRequest keeps a request to be paid into in.AgentID's wallet. The same Idempotency-Key with the same
// arguments answers the same request.
func (x *ChainIndexer) CreatePaymentRequest(ctx context.Context, in CryptoRequestIn) (CryptoRequest, error) {
	in.Token, in.Memo, in.PayerAddress = strings.ToUpper(strings.TrimSpace(in.Token)), strings.TrimSpace(in.Memo), strings.TrimSpace(in.PayerAddress)
	n, ok := x.nets[in.Network]
	switch {
	case !ok:
		return CryptoRequest{}, fmt.Errorf("%w: network is one of %s", ErrCryptoRequest, strings.Join(x.Networks(), ", "))
	case in.Token != "" && in.Token != money.USDC:
		return CryptoRequest{}, fmt.Errorf("%w: token is USDC", ErrCryptoRequest)
	case in.AmountMinor <= 0:
		return CryptoRequest{}, fmt.Errorf("%w: amount_minor is micro-USDC above 0", ErrCryptoRequest)
	case len(in.Memo) > 140:
		return CryptoRequest{}, fmt.Errorf("%w: a memo is at most 140 characters", ErrCryptoRequest)
	case !in.ExpiresAt.After(time.Now()):
		return CryptoRequest{}, fmt.Errorf("%w: expires_at is in the future", ErrCryptoRequest)
	case in.IdempotencyKey == "":
		return CryptoRequest{}, fmt.Errorf("%w: an Idempotency-Key is required", ErrCryptoRequest)
	}
	if in.PayerAddress != "" {
		if in.PayerAddress = smartaccount.Checksum(in.PayerAddress); in.PayerAddress == "" {
			return CryptoRequest{}, fmt.Errorf("%w: payer_address is an 0x address", ErrCryptoRequest)
		}
	}
	var payTo string
	err := x.s.pool.QueryRow(ctx, `SELECT address FROM agent_smart_accounts WHERE agent_id = $1 AND workspace_id = $2`,
		in.AgentID, in.WorkspaceID).Scan(&payTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return CryptoRequest{}, ErrSmartAccountNotFound
	}
	if err != nil {
		return CryptoRequest{}, fmt.Errorf("economy: request to be paid: %w", err)
	}
	// A payer that is another Talyvor agent is asked in its owner's approvals.
	var payerWS, payerAgent string
	if in.PayerAddress != "" {
		err := x.s.pool.QueryRow(ctx, `SELECT workspace_id, agent_id FROM agent_smart_accounts WHERE lower(address) = lower($1)`,
			in.PayerAddress).Scan(&payerWS, &payerAgent)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return CryptoRequest{}, fmt.Errorf("economy: request to be paid: %w", err)
		}
		if payerAgent == in.AgentID {
			return CryptoRequest{}, fmt.Errorf("%w: an agent does not ask itself to pay", ErrCryptoRequest)
		}
		if payerAgent != "" {
			if _, err := x.s.expireApprovals(ctx, payerAgent, "crypto_request:"+in.AgentID); err != nil {
				return CryptoRequest{}, err
			}
		}
	}

	id := "cpr_" + uuid.NewString()
	tx, err := x.s.pool.Begin(ctx)
	if err != nil {
		return CryptoRequest{}, fmt.Errorf("economy: request to be paid: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO crypto_payment_requests (id, workspace_id, agent_id, network, chain_id, token, asset, pay_to,
		amount_minor, memo, expires_at, payer_address, created_by, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) ON CONFLICT (workspace_id, idempotency_key) DO NOTHING`,
		id, in.WorkspaceID, in.AgentID, in.Network, n.Chain.ID, money.USDC, n.USDC, payTo, in.AmountMinor, in.Memo, in.ExpiresAt,
		in.PayerAddress, in.CreatedBy, in.IdempotencyKey)
	if err != nil {
		return CryptoRequest{}, fmt.Errorf("economy: request to be paid: %w", err)
	}
	if tag.RowsAffected() == 0 {
		_ = tx.Rollback(ctx)
		var was CryptoRequest
		err := x.s.pool.QueryRow(ctx, `SELECT id FROM crypto_payment_requests WHERE workspace_id = $1 AND idempotency_key = $2`,
			in.WorkspaceID, in.IdempotencyKey).Scan(&was.ID)
		if err != nil {
			return CryptoRequest{}, fmt.Errorf("economy: request to be paid: %w", err)
		}
		if was, err = x.s.CryptoPaymentRequest(ctx, in.WorkspaceID, was.ID); err != nil {
			return CryptoRequest{}, err
		}
		if was.AgentID != in.AgentID || was.Network != in.Network || was.AmountMinor != in.AmountMinor || was.Memo != in.Memo ||
			!was.ExpiresAt.Equal(in.ExpiresAt.Truncate(time.Microsecond)) || was.PayerAddress != in.PayerAddress {
			return CryptoRequest{}, ErrCryptoRequestConflict
		}
		return was, nil
	}
	approval := ""
	if payerAgent != "" {
		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM agent_accounts WHERE id = $1`, in.AgentID).Scan(&name); err != nil {
			return CryptoRequest{}, fmt.Errorf("economy: request to be paid: %w", err)
		}
		approval = "apr_" + uuid.NewString()
		// No payee: the requester is another workspace's agent, never one this workspace's rules can allow for good. One
		// open approval per requester and payer (idx_agent_approvals_open on the requester in the fingerprint).
		tag, err := tx.Exec(ctx, `INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, reason, memo,
			expires_at, payment_request_id, currency, amount_minor, trust)
			VALUES ($1, $2, $3, $4, 0, $5, $6, LEAST($7, now() + $8 * interval '1 microsecond'), $9, $10, $11, $12)
			ON CONFLICT (agent_id, fingerprint) WHERE status IN ('pending', 'approved') DO NOTHING`,
			approval, payerWS, payerAgent, "crypto_request:"+in.AgentID,
			fmt.Sprintf("%s asks to be paid %s USDC on %s", name, usdcString(in.AmountMinor), in.Network), in.Memo,
			in.ExpiresAt, x.s.approvalLife().Microseconds(), id, money.USDC, in.AmountMinor, in.Trust)
		if err != nil {
			return CryptoRequest{}, fmt.Errorf("economy: request to be paid: the payer's approval: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return CryptoRequest{}, ErrCryptoRequestWaiting
		}
		if _, err := tx.Exec(ctx, `UPDATE crypto_payment_requests SET approval_id = $2 WHERE id = $1`, id, approval); err != nil {
			return CryptoRequest{}, fmt.Errorf("economy: request to be paid: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CryptoRequest{}, fmt.Errorf("economy: request to be paid: %w", err)
	}
	if approval != "" {
		x.s.notifyApproval(payerWS, approval)
	}
	return x.s.CryptoPaymentRequest(ctx, in.WorkspaceID, id)
}

const cryptoRequestColumns = `id, workspace_id, agent_id, network, chain_id, token, asset, pay_to, amount_minor, memo, expires_at,
	payer_address, approval_id, overpayment, created_at`

// cryptoRequests reads the requests of workspaceID, of agentID and with id — each only when given — newest first,
// with their payments.
func (s *DualTokenStore) cryptoRequests(ctx context.Context, workspaceID, agentID, id string) ([]CryptoRequest, error) {
	if workspaceID == "" && id == "" {
		return []CryptoRequest{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cryptoRequestColumns+` FROM crypto_payment_requests
		WHERE ($1 = '' OR workspace_id = $1) AND ($2 = '' OR agent_id = $2) AND ($3 = '' OR id = $3)
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID, agentID, id)
	if err != nil {
		return nil, fmt.Errorf("economy: requests to be paid: %w", err)
	}
	out := []CryptoRequest{}
	byID := map[string]int{}
	var ids []string
	for rows.Next() {
		var r CryptoRequest
		var ws string
		if err := rows.Scan(&r.ID, &ws, &r.AgentID, &r.Network, &r.ChainID, &r.Token, &r.Asset, &r.PayTo, &r.AmountMinor, &r.Memo,
			&r.ExpiresAt, &r.PayerAddress, &r.ApprovalID, &r.Overpayment, &r.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("economy: requests to be paid: %w", err)
		}
		r.Payments = []CryptoRequestPayment{}
		byID[r.ID] = len(out)
		ids = append(ids, r.ID)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: requests to be paid: %w", err)
	}
	if len(ids) == 0 {
		return out, nil
	}
	prows, err := s.pool.Query(ctx, `SELECT p.request_id, m.tx_hash, m.counterparty, p.amount_minor, p.late, m.seen_at
		FROM crypto_payment_request_payments p JOIN chain_movements m ON m.id = p.movement_id
		WHERE p.request_id = ANY($1) ORDER BY m.seen_at, m.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("economy: requests to be paid: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var id string
		var p CryptoRequestPayment
		if err := prows.Scan(&id, &p.TxHash, &p.Counterparty, &p.AmountMinor, &p.Late, &p.SeenAt); err != nil {
			return nil, fmt.Errorf("economy: requests to be paid: %w", err)
		}
		r := &out[byID[id]]
		r.Payments = append(r.Payments, p)
		if p.Late {
			r.LateMinor += p.AmountMinor
		} else {
			r.ReceivedMinor += p.AmountMinor
		}
	}
	if err := prows.Err(); err != nil {
		return nil, fmt.Errorf("economy: requests to be paid: %w", err)
	}
	now := time.Now()
	for i := range out {
		out[i].fill(now)
	}
	return out, nil
}

// CryptoPaymentRequest is one of workspaceID's requests.
func (s *DualTokenStore) CryptoPaymentRequest(ctx context.Context, workspaceID, id string) (CryptoRequest, error) {
	rs, err := s.cryptoRequests(ctx, workspaceID, "", id)
	if err != nil {
		return CryptoRequest{}, err
	}
	if len(rs) == 0 {
		return CryptoRequest{}, ErrCryptoRequestNotFound
	}
	return rs[0], nil
}

// AgentCryptoPaymentRequests is agentID's requests, newest first.
func (s *DualTokenStore) AgentCryptoPaymentRequests(ctx context.Context, workspaceID, agentID string) ([]CryptoRequest, error) {
	return s.cryptoRequests(ctx, workspaceID, agentID, "")
}

// PublicCryptoPaymentRequest is a request as whoever holds its link sees it: what to pay, where, and whether it is
// still due — never whose workspace or agent it is, nor who was asked.
func (s *DualTokenStore) PublicCryptoPaymentRequest(ctx context.Context, id string) (CryptoRequest, error) {
	rs, err := s.cryptoRequests(ctx, "", "", id)
	if err != nil {
		return CryptoRequest{}, err
	}
	if len(rs) == 0 {
		return CryptoRequest{}, ErrCryptoRequestNotFound
	}
	r := rs[0]
	r.AgentID, r.PayerAddress, r.Overpayment, r.Payments = "", "", "", nil
	return r, nil
}

// DecideCryptoOverpayment records the owner's decision on an overpaid request: refund what was paid over, or keep it.
func (s *DualTokenStore) DecideCryptoOverpayment(ctx context.Context, workspaceID, id, decision, by string) (CryptoRequest, error) {
	if decision != "refund" && decision != "keep" {
		return CryptoRequest{}, fmt.Errorf("%w: decision is refund or keep", ErrCryptoRequest)
	}
	r, err := s.CryptoPaymentRequest(ctx, workspaceID, id)
	if err != nil {
		return r, err
	}
	if r.Status != "overpaid" {
		return r, fmt.Errorf("%w: only an overpaid request has an overpayment to decide", ErrCryptoRequest)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE crypto_payment_requests SET overpayment = $3 WHERE workspace_id = $1 AND id = $2`,
		workspaceID, id, decision); err != nil {
		return r, fmt.Errorf("economy: overpayment: %w", err)
	}
	if err := s.AppendAudit(ctx, workspaceID, by, "crypto.request.overpayment", map[string]any{"request_id": id, "decision": decision,
		"overpaid_minor": r.OverpaidMinor}); err != nil {
		return r, err
	}
	return s.CryptoPaymentRequest(ctx, workspaceID, id)
}

// matchRequest matches deposit m, just posted, to the request it pays — or, reverse, takes back its match. The
// leader's Confirm is the one caller, network by network, so two deposits are never matched at once.
func (x *ChainIndexer) matchRequest(ctx context.Context, m chainMovement, reverse bool) {
	if m.direction != "in" {
		return
	}
	var err error
	if reverse {
		_, err = x.s.pool.Exec(ctx, `DELETE FROM crypto_payment_request_payments WHERE movement_id = $1`, m.id)
	} else {
		// Unexpired requests first; among them one naming this sender, then one this pays exactly, then the oldest. An
		// expired one takes the deposit only as late, and only if it names the sender or the deposit is what was due.
		_, err = x.s.pool.Exec(ctx, `
			WITH mv AS (SELECT seen_at FROM chain_movements WHERE id = $1),
			due AS (
				SELECT r.id, r.created_at, r.expires_at <= mv.seen_at AS late, lower(r.payer_address) = lower($6) AS named,
					r.amount_minor - COALESCE((SELECT sum(p.amount_minor) FROM crypto_payment_request_payments p
						WHERE p.request_id = r.id AND NOT p.late), 0) AS remaining
				FROM crypto_payment_requests r, mv
				WHERE r.workspace_id = $2 AND r.agent_id = $3 AND r.network = $4 AND lower(r.asset) = lower($5)
					AND (r.payer_address = '' OR lower(r.payer_address) = lower($6)))
			INSERT INTO crypto_payment_request_payments (movement_id, request_id, workspace_id, amount_minor, late)
			SELECT $1, id, $2, $7::bigint, late FROM due
			WHERE remaining > 0 AND (NOT late OR named OR remaining = $7::bigint)
			ORDER BY late, named DESC, remaining = $7::bigint DESC, created_at, id LIMIT 1
			ON CONFLICT (movement_id) DO NOTHING`,
			m.id, m.workspaceID, m.agentID, m.network, m.token, m.counterparty, m.amount)
	}
	if err != nil {
		// The deposit is posted either way; only its request does not show it.
		slog.Warn("chain indexer: a deposit was not matched to its request", "movement", m.id, "err", err)
	}
}
