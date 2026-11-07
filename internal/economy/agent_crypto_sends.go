package economy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/screening"
)

// agent_crypto_sends.go — B48.9: AN AGENT SENDS USDC FROM ITS SMART ACCOUNT, TO ANOTHER TALYVOR AGENT OR TO ANY ADDRESS.
//
// The agent (its own key) or its owner asks to send; Lens quotes the token, the amount, the most the network fee can
// be and who pays it, and when it should arrive. The destination is checksummed and, when it is another Talyvor
// agent's account, named by its trust level ("Verified agent of <business>" or "Unverified agent"); every one is
// screened (B48.7) before anything is signed. A first send to an address the workspace never paid waits for the owner
// when the agent asked for it; the owner approves it, or sends a small test amount there first, or declines it. The
// send then goes through the owner's signed spend permission (AuthoriseAgentCryptoPayment, via
// smartaccount.Chain.SignPaymentAsAgent) and is kept, signed, before the bundler is handed it, so a retry under the same
// idempotency key sends that same operation again and pays once. The receipt gives the transaction's hash and an
// explorer link; the chain indexer (B48.6) posts the movement to both agents' ledgers. A payment the bundler refuses or
// the chain reverts gives back its use of the allowance. Class: stablecoins, RED — sandbox networks only until Nick
// clears it.

var (
	// ErrCryptoSendsOff: no network is set up for agents' sends here.
	ErrCryptoSendsOff = errors.New("economy: agents' crypto sends are not switched on")
	// ErrCryptoSend wraps a send that cannot be asked for as it is.
	ErrCryptoSend = errors.New("economy: invalid crypto send")
	// ErrCryptoSendNotFound: no such send of the agent's.
	ErrCryptoSendNotFound = errors.New("economy: no such crypto send")
)

// The ways a send's network fee is paid.
const (
	FeePaidByGasBudget = "workspace_gas_budget" // the chain's paymaster, out of the workspace's budget (B48.5)
	FeePaidByAccount   = "agent_account"        // the agent's account, in the chain's coin
)

// CryptoSendNetwork is one network agents send on.
type CryptoSendNetwork struct {
	Name           string
	Chain          smartaccount.Chain // its node and bundler, and its paymaster when one pays agents' fees
	USDC           string
	Funding        string // test or live
	Explorer       string // such as https://basescan.org; "" for none
	ArrivalSeconds int64  // how long until a send is final there; 0 when not known
}

// SetCryptoSendNetworks is where agents may send. Set at start, before serving.
func (s *DualTokenStore) SetCryptoSendNetworks(nets []CryptoSendNetwork) {
	s.cryptoSendNets = map[string]CryptoSendNetwork{}
	for _, n := range nets {
		n.USDC = smartaccount.Checksum(n.USDC)
		s.cryptoSendNets[n.Name] = n
	}
}

// CryptoSendRequest is what is asked to be sent.
type CryptoSendRequest struct {
	To          string `json:"to"`      // a 0x address
	Network     string `json:"network"` // one of the networks set
	Token       string `json:"token"`   // USDC
	AmountMinor int64  `json:"amount_minor"`
}

// CryptoSendQuote is a send before it is made.
type CryptoSendQuote struct {
	Network        string `json:"network"`
	Token          string `json:"token"`
	TokenAddress   string `json:"token_address"`
	AmountMinor    int64  `json:"amount_minor"`
	To             string `json:"to"`
	ToKind         string `json:"to_kind"`            // agent or address
	ToLabel        string `json:"to_label,omitempty"` // an agent's trust level
	NewAddress     bool   `json:"new_address"`        // the workspace never paid it: the owner is asked first
	MaxFeeWei      string `json:"max_fee_wei"`
	FeePaidBy      string `json:"fee_paid_by"`
	ArrivalSeconds int64  `json:"arrival_seconds,omitempty"`
}

// CryptoSend is one send and how it ended.
type CryptoSend struct {
	ID string `json:"id"`
	CryptoSendQuote
	AgentID     string    `json:"agent_id"`
	Direction   string    `json:"direction"` // out, or in for one another agent sent this one
	Status      string    `json:"status"`    // awaiting_owner, sending, sent, landed, failed or refused
	Reason      string    `json:"reason,omitempty"`
	RequestedBy string    `json:"requested_by"`
	ApprovedBy  string    `json:"approved_by,omitempty"`
	UserOpHash  string    `json:"user_op_hash,omitempty"`
	TxHash      string    `json:"tx_hash,omitempty"`
	ExplorerURL string    `json:"explorer_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	workspaceID, key, toAgentID, signedOp string
	useID                                 *int64
}

func (s *DualTokenStore) cryptoSendNetwork(name string) (CryptoSendNetwork, error) {
	if len(s.cryptoSendNets) == 0 || s.smartAccounts == nil {
		return CryptoSendNetwork{}, ErrCryptoSendsOff
	}
	n, ok := s.cryptoSendNets[name]
	if !ok {
		return n, fmt.Errorf("%w: agents do not send on %q here", ErrCryptoSend, name)
	}
	return n, nil
}

// madeAccount is agentID's smart account as the gate and the chain need it: its owners now, and as made, for its
// first operation on a chain, which deploys it.
func (s *DualTokenStore) madeAccount(ctx context.Context, workspaceID, agentID string) (smartaccount.Account, error) {
	var a smartaccount.Account
	var pub, madePub []byte
	var agentKey, recovery string
	var madeAgent, madeRecovery *string
	err := s.pool.QueryRow(ctx, `SELECT address, factory, implementation, owner_public_key, owner_index, agent_signer, agent_index,
		made_with_public_key, made_with_agent_signer, made_with_recovery_address, recovery_address
		FROM agent_smart_accounts WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID).
		Scan(&a.Address, &a.Factory.Address, &a.Factory.Implementation, &pub, &a.PasskeyIndex, &agentKey, &a.AgentIndex,
			&madePub, &madeAgent, &madeRecovery, &recovery)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrSmartAccountNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: crypto send: %w", err)
	}
	a.Passkey, a.AgentKey = pub, agentKey
	if madePub != nil {
		pub = madePub
	}
	if madeAgent != nil {
		agentKey = *madeAgent
	}
	if madeRecovery != nil {
		recovery = *madeRecovery
	}
	agentOwner, err := smartaccount.AddressOwner(agentKey)
	if err != nil {
		return a, err
	}
	a.Owners = [][]byte{pub, agentOwner}
	if recovery != "" {
		r, err := smartaccount.AddressOwner(recovery)
		if err != nil {
			return a, err
		}
		a.Owners = append(a.Owners, r)
	}
	return a, nil
}

// QuoteAgentCryptoSend answers what sending in from agentID would be, before anything is asked.
func (s *DualTokenStore) QuoteAgentCryptoSend(ctx context.Context, workspaceID, agentID string, in CryptoSendRequest) (CryptoSendQuote, error) {
	n, err := s.cryptoSendNetwork(in.Network)
	if err != nil {
		return CryptoSendQuote{}, err
	}
	q, _, err := s.cryptoDestination(ctx, workspaceID, agentID, n, in)
	if err != nil {
		return q, err
	}
	a, err := s.madeAccount(ctx, workspaceID, agentID)
	if err != nil {
		return q, err
	}
	fee, err := s.cryptoSendFee(ctx, n, a)
	if err != nil {
		return q, err
	}
	q.MaxFeeWei = fee.String()
	return q, nil
}

// cryptoDestination checks in and resolves its destination: q without its fee, and the receiving agent's ID when it
// is another Talyvor agent's account.
func (s *DualTokenStore) cryptoDestination(ctx context.Context, workspaceID, agentID string, n CryptoSendNetwork, in CryptoSendRequest) (CryptoSendQuote, string, error) {
	q := CryptoSendQuote{Network: n.Name, Token: money.USDC, TokenAddress: n.USDC, AmountMinor: in.AmountMinor, FeePaidBy: FeePaidByAccount,
		ArrivalSeconds: n.ArrivalSeconds}
	if n.Chain.PaymasterURL != "" {
		q.FeePaidBy = FeePaidByGasBudget
	}
	switch {
	case !strings.EqualFold(strings.TrimSpace(in.Token), money.USDC):
		return q, "", fmt.Errorf("%w: agents send USDC", ErrCryptoSend)
	case in.AmountMinor <= 0:
		return q, "", fmt.Errorf("%w: a send is a positive amount in USDC's base units", ErrCryptoSend)
	}
	q.To = smartaccount.Checksum(strings.TrimSpace(in.To))
	if q.To == "" {
		return q, "", fmt.Errorf("%w: send to a 0x address", ErrCryptoSend)
	}
	var mine string
	if err := s.pool.QueryRow(ctx, `SELECT address FROM agent_smart_accounts WHERE agent_id = $1 AND workspace_id = $2`,
		agentID, workspaceID).Scan(&mine); errors.Is(err, pgx.ErrNoRows) {
		return q, "", ErrSmartAccountNotFound
	} else if err != nil {
		return q, "", fmt.Errorf("economy: crypto send: %w", err)
	}
	if strings.EqualFold(mine, q.To) {
		return q, "", fmt.Errorf("%w: that is the agent's own account", ErrCryptoSend)
	}
	var toAgent, toWorkspace string
	err := s.pool.QueryRow(ctx, `SELECT agent_id, workspace_id FROM agent_smart_accounts WHERE lower(address) = lower($1)`, q.To).
		Scan(&toAgent, &toWorkspace)
	switch {
	case err == nil:
		// Another Talyvor agent: named only by the trust its card carries, never by its name or workspace.
		f, err := s.AgentCardFacts(ctx, toAgent)
		if err != nil {
			return q, "", err
		}
		q.ToKind, q.ToLabel = "agent", "Unverified agent"
		if f.Owner.Name != "" && f.Owner.Level >= LevelIdentity {
			q.ToLabel = "Verified agent of " + f.Owner.Name
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return q, "", fmt.Errorf("economy: crypto send: %w", err)
	default:
		q.ToKind = "address"
	}
	if toWorkspace == workspaceID {
		return q, toAgent, nil // one of the workspace's own agents
	}
	// Anyone can make a Talyvor agent: another workspace's is new, as any address is, until the workspace has paid it.
	var paid bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_crypto_sends WHERE workspace_id = $1 AND lower(to_address) = lower($2)
		AND status = 'landed')`, workspaceID, q.To).Scan(&paid); err != nil {
		return q, "", fmt.Errorf("economy: crypto send: %w", err)
	}
	q.NewAddress = !paid
	return q, toAgent, nil
}

// cryptoSendFee is the most a's send on n can be charged for its network fee, in wei of the chain's coin.
func (s *DualTokenStore) cryptoSendFee(ctx context.Context, n CryptoSendNetwork, a smartaccount.Account) (*big.Int, error) {
	if n.Chain.PaymasterURL != "" {
		return n.Chain.QuoteSponsored(ctx)
	}
	op, err := n.Chain.Prepare(ctx, a, nil)
	if err != nil {
		return nil, err
	}
	return smartaccount.MaxFee(op), nil
}

const cryptoSendColumns = `id, workspace_id, agent_id, idempotency_key, network, token, to_address, to_kind, to_label, to_agent_id, amount_minor,
	max_fee_wei, fee_paid_by, status, reason, requested_by, approved_by, use_id, signed_op, user_op_hash, tx_hash, created_at, updated_at`

func (s *DualTokenStore) scanCryptoSend(row pgx.Row) (CryptoSend, error) {
	var c CryptoSend
	err := row.Scan(&c.ID, &c.workspaceID, &c.AgentID, &c.key, &c.Network, &c.TokenAddress, &c.To, &c.ToKind, &c.ToLabel, &c.toAgentID, &c.AmountMinor,
		&c.MaxFeeWei, &c.FeePaidBy, &c.Status, &c.Reason, &c.RequestedBy, &c.ApprovedBy, &c.useID, &c.signedOp, &c.UserOpHash, &c.TxHash,
		&c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrCryptoSendNotFound
	}
	if err != nil {
		return c, fmt.Errorf("economy: crypto send: %w", err)
	}
	c.Token, c.Direction = money.USDC, "out"
	c.NewAddress = c.Status == "awaiting_owner"
	if n, ok := s.cryptoSendNets[c.Network]; ok {
		c.ArrivalSeconds = n.ArrivalSeconds
		if n.Explorer != "" && c.TxHash != "" {
			c.ExplorerURL = strings.TrimRight(n.Explorer, "/") + "/tx/" + c.TxHash
		}
	}
	return c, nil
}

func (s *DualTokenStore) cryptoSend(ctx context.Context, workspaceID, agentID, id string) (CryptoSend, error) {
	return s.scanCryptoSend(s.pool.QueryRow(ctx, `SELECT `+cryptoSendColumns+` FROM agent_crypto_sends
		WHERE id = $1 AND workspace_id = $2 AND agent_id = $3`, id, workspaceID, agentID))
}

func (c CryptoSend) request() CryptoSendRequest {
	return CryptoSendRequest{To: c.To, Network: c.Network, Token: c.Token, AmountMinor: c.AmountMinor}
}

// SendAgentCrypto sends in from agentID's smart account, once per idempotency key: asking again with the same key
// answers the same send, never a second one, and a key used for another send is refused. byOwner is whether the owner
// asks (their command is their say on a new address); otherwise the agent's own key asks, and a first send to a new
// address waits for the owner. A send ends landed, failed or refused, or waits; its Reason says why.
func (s *DualTokenStore) SendAgentCrypto(ctx context.Context, workspaceID, agentID, by string, byOwner bool, key string, in CryptoSendRequest) (CryptoSend, error) {
	if key == "" || len(key) > 128 {
		return CryptoSend{}, fmt.Errorf("%w: an Idempotency-Key of at most 128 characters", ErrCryptoSend)
	}
	n, err := s.cryptoSendNetwork(in.Network)
	if err != nil {
		return CryptoSend{}, err
	}
	if had, err := s.scanCryptoSend(s.pool.QueryRow(ctx, `SELECT `+cryptoSendColumns+` FROM agent_crypto_sends
		WHERE workspace_id = $1 AND agent_id = $2 AND idempotency_key = $3`, workspaceID, agentID, key)); err == nil {
		if r := had.request(); r.Network != in.Network || r.AmountMinor != in.AmountMinor || !strings.EqualFold(r.To, strings.TrimSpace(in.To)) ||
			!strings.EqualFold(in.Token, money.USDC) {
			return CryptoSend{}, ErrIdempotencyKeyReused
		}
		return s.carryOn(ctx, workspaceID, n, had)
	} else if !errors.Is(err, ErrCryptoSendNotFound) {
		return CryptoSend{}, err
	}
	q, toAgent, err := s.cryptoDestination(ctx, workspaceID, agentID, n, in)
	if err != nil {
		return CryptoSend{}, err
	}
	var toWorkspace string
	if toAgent != "" {
		if err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM agent_smart_accounts WHERE agent_id = $1`, toAgent).Scan(&toWorkspace); err != nil {
			return CryptoSend{}, fmt.Errorf("economy: crypto send: %w", err)
		}
	}
	status := "sending"
	if q.NewAddress && !byOwner {
		status = "awaiting_owner"
	}
	id := "csend_" + uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_crypto_sends (id, workspace_id, agent_id, idempotency_key, network, token, to_address,
		to_kind, to_label, to_agent_id, to_workspace_id, amount_minor, fee_paid_by, status, requested_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (workspace_id, agent_id, idempotency_key) DO NOTHING`, id, workspaceID, agentID, key, n.Name, n.USDC, q.To, q.ToKind,
		q.ToLabel, toAgent, toWorkspace, q.AmountMinor, q.FeePaidBy, status, by); err != nil {
		return CryptoSend{}, fmt.Errorf("economy: crypto send: %w", err)
	}
	// One asked at the same time under the same key is the send.
	c, err := s.scanCryptoSend(s.pool.QueryRow(ctx, `SELECT `+cryptoSendColumns+` FROM agent_crypto_sends
		WHERE workspace_id = $1 AND agent_id = $2 AND idempotency_key = $3`, workspaceID, agentID, key))
	if err != nil || c.ID != id {
		return c, err
	}
	if err := s.AppendAudit(ctx, workspaceID, "agent:"+agentID, "agent.crypto_send", map[string]any{"send_id": id, "network": n.Name,
		"to": q.To, "to_kind": q.ToKind, "amount_minor": q.AmountMinor, "status": status, "by": by}); err != nil {
		return c, err
	}
	if status == "awaiting_owner" {
		if c, err = s.screenCryptoSend(ctx, workspaceID, n, c); err != nil || c.Status != "awaiting_owner" {
			return c, err
		}
		s.askOwnerAboutSend(workspaceID, c)
		return c, nil
	}
	return s.carryOn(ctx, workspaceID, n, c)
}

// askOwnerAboutSend tells the owner's phones that an agent asks to pay an address the workspace never paid.
func (s *DualTokenStore) askOwnerAboutSend(workspaceID string, c CryptoSend) {
	if s.nativePusher == nil {
		return
	}
	msg := nativepush.Message{Title: "New address", Body: "An agent asks to send " + formatMinor(money.USDC, c.AmountMinor) + " to " + c.To +
		", an address you haven't paid before. Approve it, send a small test amount first, or decline it.",
		Data: map[string]string{"type": "crypto_send_new_address", "agent_id": c.AgentID, "send_id": c.ID, "workspace_id": workspaceID}}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.pushDevices(ctx, workspaceID, msg)
	}()
}

// screenCryptoSend screens c's destination (B48.7): a hit refuses c, before anything is signed.
func (s *DualTokenStore) screenCryptoSend(ctx context.Context, workspaceID string, n CryptoSendNetwork, c CryptoSend) (CryptoSend, error) {
	if s.screener == nil {
		return c, errors.New("economy: crypto send: no screener is set, so nothing is sent")
	}
	err := s.screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: workspaceID, ID: "crypto_send:" + c.ID, Capability: CapabilityStablecoins,
		Direction: "out", Counterparty: c.To, AmountMinor: c.AmountMinor, Currency: money.USDC, Funding: n.Funding})
	var r *screening.Refusal
	if errors.As(err, &r) {
		return s.endCryptoSend(ctx, c, "refused", err.Error())
	}
	return c, err
}

// endCryptoSend marks c refused or failed for reason and gives back its use of the allowance, if the gate kept one.
func (s *DualTokenStore) endCryptoSend(ctx context.Context, c CryptoSend, status, reason string) (CryptoSend, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if c.useID != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM agent_spend_permission_uses WHERE id = $1 AND agent_id = $2`, *c.useID, c.AgentID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE agent_crypto_sends SET status = $2, reason = $3, use_id = NULL, updated_at = now() WHERE id = $1`,
			c.ID, status, reason)
		return err
	})
	if err != nil {
		return c, fmt.Errorf("economy: crypto send: %w", err)
	}
	return s.cryptoSend(ctx, c.workspaceID, c.AgentID, c.ID)
}

// carryOn takes c as far as it can go now, one caller at a time: it signs and sends a send not yet sent, sends a
// signed one again, and reads the receipt of one sent.
func (s *DualTokenStore) carryOn(ctx context.Context, workspaceID string, n CryptoSendNetwork, c CryptoSend) (CryptoSend, error) {
	if c.Status != "sending" && c.Status != "sent" {
		return c, nil
	}
	// A lease, not a lock held open across the chain's calls: what is kept along the way commits as it goes. Only one
	// signed operation is ever kept for a send (signAndSend), so the lease is for doing the work once, not for safety.
	lease := uuid.NewString()
	tag, err := s.pool.Exec(ctx, `UPDATE agent_crypto_sends SET carried_until = now() + interval '2 minutes', carried_by = $2
		WHERE id = $1 AND (carried_until IS NULL OR carried_until < now())`, c.ID, lease)
	if err != nil {
		return c, fmt.Errorf("economy: crypto send: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return c, nil // another caller is carrying it on
	}
	defer func() {
		_, _ = s.pool.Exec(context.WithoutCancel(ctx), `UPDATE agent_crypto_sends SET carried_until = NULL, carried_by = ''
			WHERE id = $1 AND carried_by = $2`, c.ID, lease)
	}()
	if c, err = s.cryptoSend(ctx, workspaceID, c.AgentID, c.ID); err != nil {
		return c, err
	}
	switch {
	case c.Status == "sent":
		return s.readCryptoReceipt(ctx, workspaceID, n, c)
	case c.Status != "sending":
		return c, nil
	case c.signedOp != "":
		var op smartaccount.UserOp
		if err := json.Unmarshal([]byte(c.signedOp), &op); err != nil {
			return c, fmt.Errorf("economy: crypto send: %w", err)
		}
		if r, err := n.Chain.Receipt(ctx, c.UserOpHash); err == nil && r != nil {
			return s.landCryptoSend(ctx, workspaceID, c, r)
		}
		// Not sent again once the owner froze the agent, or the workspace is frozen: it may have landed, so it still
		// counts, and its receipt is read whenever it is asked about.
		var frozen bool
		if err := s.pool.QueryRow(ctx, `SELECT frozen_at IS NOT NULL FROM agent_smart_accounts WHERE agent_id = $1 AND workspace_id = $2`,
			c.AgentID, workspaceID).Scan(&frozen); err != nil {
			return c, fmt.Errorf("economy: crypto send: %w", err)
		}
		stablecoins, _ := CapabilityByKey(CapabilityStablecoins)
		if frozen || refuseFrozen(ctx, s.pool, workspaceID, stablecoins) != nil {
			return c, nil
		}
		// Sent again as signed: its nonce lets it land once. A refusal now may be its landing: it stays to be read.
		if _, err := s.handToBundler(ctx, workspaceID, n, op); err != nil {
			return c, nil
		}
		return s.markCryptoSent(ctx, workspaceID, n, c)
	}
	return s.signAndSend(ctx, workspaceID, n, c)
}

// signAndSend screens c, has the agent's key sign it under the owner's permission, keeps it signed, and hands it to
// the bundler.
func (s *DualTokenStore) signAndSend(ctx context.Context, workspaceID string, n CryptoSendNetwork, c CryptoSend) (CryptoSend, error) {
	stablecoins, _ := CapabilityByKey(CapabilityStablecoins)
	if err := refuseFrozen(ctx, s.pool, workspaceID, stablecoins); err != nil {
		return s.endCryptoSend(ctx, c, "refused", err.Error())
	}
	if n.Funding != FundingTest {
		if err := refusalErr(capabilityLive(ctx, s.pool, workspaceID, stablecoins)); err != nil {
			return s.endCryptoSend(ctx, c, "refused", err.Error())
		}
	}
	c, err := s.screenCryptoSend(ctx, workspaceID, n, c)
	if err != nil || c.Status != "sending" {
		return c, err
	}
	a, err := s.madeAccount(ctx, workspaceID, c.AgentID)
	if err != nil {
		return c, err
	}
	call, err := smartaccount.Execute(n.USDC, new(big.Int), transferCall(c.To, c.AmountMinor))
	if err != nil {
		return c, err
	}
	op, err := n.Chain.Prepare(ctx, a, call)
	if err != nil {
		return c, err
	}
	if n.Chain.PaymasterURL != "" {
		if _, err := n.Chain.Sponsor(ctx, &op); err != nil {
			return c, err
		}
	}
	err = n.Chain.SignPaymentAsAgent(ctx, s.smartAccounts.signers, a, a.AgentKey, &op, func(pay smartaccount.Payment) error {
		use, err := s.AuthoriseAgentCryptoPayment(ctx, workspaceID, c.AgentID, pay)
		if err == nil {
			c.useID = &use // kept on the send with its signed operation, below
		}
		return err
	})
	if errors.Is(err, smartaccount.ErrPermission) || errors.Is(err, smartaccount.ErrFrozen) {
		return s.endCryptoSend(ctx, c, "refused", err.Error())
	}
	if err != nil {
		return s.endCryptoSend(ctx, c, "failed", err.Error())
	}
	h, err := op.Hash(n.Chain.ID)
	if err != nil {
		return s.endCryptoSend(ctx, c, "failed", err.Error())
	}
	signed, err := json.Marshal(op)
	if err != nil {
		return s.endCryptoSend(ctx, c, "failed", err.Error())
	}
	fee := smartaccount.MaxFee(op)
	// Kept before the bundler sees it, and only if no other operation was kept for the send: from here a retry sends
	// this very operation, never a new one.
	tag, err := s.pool.Exec(ctx, `UPDATE agent_crypto_sends SET signed_op = $2, user_op_hash = $3, max_fee_wei = $4, use_id = $5, updated_at = now()
		WHERE id = $1 AND status = 'sending' AND signed_op = ''`, c.ID, string(signed), fmt.Sprintf("0x%x", h), fee.String(), c.useID)
	if err != nil {
		return c, fmt.Errorf("economy: crypto send: %w", err)
	}
	if tag.RowsAffected() == 0 { // another caller's operation is the send: this one is never sent, and counts nothing
		if c.useID != nil {
			if _, err := s.pool.Exec(ctx, `DELETE FROM agent_spend_permission_uses WHERE id = $1 AND agent_id = $2`, *c.useID, c.AgentID); err != nil {
				return c, fmt.Errorf("economy: crypto send: %w", err)
			}
		}
		return s.cryptoSend(ctx, workspaceID, c.AgentID, c.ID)
	}
	c.UserOpHash = fmt.Sprintf("0x%x", h)
	if _, err := s.handToBundler(ctx, workspaceID, n, op); err != nil {
		// Only a bundler's own refusal, or a budget that cannot cover the fee, is known not to reach the chain. Any other
		// error may follow its taking the operation: the send stays to be sent again, as signed, and still counts.
		if errors.Is(err, smartaccount.ErrRefused) || errors.Is(err, smartaccount.ErrGasBudgetSpent) {
			return s.endCryptoSend(ctx, c, "failed", err.Error())
		}
		return c, nil
	}
	return s.markCryptoSent(ctx, workspaceID, n, c)
}

// handToBundler sends op, sponsored out of the workspace's gas budget when the chain's paymaster pays it.
func (s *DualTokenStore) handToBundler(ctx context.Context, workspaceID string, n CryptoSendNetwork, op smartaccount.UserOp) (string, error) {
	if n.Chain.PaymasterURL != "" {
		return n.Chain.SendSponsored(ctx, s.SponsoredGas(workspaceID, n.Chain.ID), op)
	}
	return n.Chain.Send(ctx, op)
}

func (s *DualTokenStore) markCryptoSent(ctx context.Context, workspaceID string, n CryptoSendNetwork, c CryptoSend) (CryptoSend, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE agent_crypto_sends SET status = 'sent', updated_at = now() WHERE id = $1 AND status = 'sending'`,
		c.ID); err != nil {
		return c, fmt.Errorf("economy: crypto send: %w", err)
	}
	c, err := s.cryptoSend(ctx, workspaceID, c.AgentID, c.ID)
	if err != nil {
		return c, err
	}
	return s.readCryptoReceipt(ctx, workspaceID, n, c)
}

// readCryptoReceipt reads sent c's receipt: landed with its transaction, failed (its use given back) when the chain
// reverted it, or still sent while it is not on the chain.
//
// shortcut: an operation the bundler took that never lands stays sent, and its use counts; upgrade by dropping it, and
// giving the use back, once its nonce is taken by another operation.
func (s *DualTokenStore) readCryptoReceipt(ctx context.Context, workspaceID string, n CryptoSendNetwork, c CryptoSend) (CryptoSend, error) {
	r, err := n.Chain.Receipt(ctx, c.UserOpHash)
	if err != nil || r == nil {
		return c, nil
	}
	return s.landCryptoSend(ctx, workspaceID, c, r)
}

func (s *DualTokenStore) landCryptoSend(ctx context.Context, workspaceID string, c CryptoSend, r *smartaccount.Receipt) (CryptoSend, error) {
	if !r.Success {
		if _, err := s.pool.Exec(ctx, `UPDATE agent_crypto_sends SET tx_hash = $2 WHERE id = $1`, c.ID, r.Receipt.TransactionHash); err != nil {
			return c, fmt.Errorf("economy: crypto send: %w", err)
		}
		return s.endCryptoSend(ctx, c, "failed", "the chain reverted the payment: nothing was sent")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE agent_crypto_sends SET status = 'landed', tx_hash = $2, updated_at = now() WHERE id = $1`,
		c.ID, r.Receipt.TransactionHash); err != nil {
		return c, fmt.Errorf("economy: crypto send: %w", err)
	}
	return s.cryptoSend(ctx, workspaceID, c.AgentID, c.ID)
}

// AgentCryptoSend is one of agentID's sends, its receipt read again when it is still on its way.
func (s *DualTokenStore) AgentCryptoSend(ctx context.Context, workspaceID, agentID, id string) (CryptoSend, error) {
	c, err := s.cryptoSend(ctx, workspaceID, agentID, id)
	if err != nil {
		return c, err
	}
	if n, ok := s.cryptoSendNets[c.Network]; ok && s.smartAccounts != nil {
		return s.carryOn(ctx, workspaceID, n, c)
	}
	return c, nil
}

// AnswerAgentCryptoSend is the owner's answer to a send waiting for them: approve sends it; decline refuses it; a
// positive testMinor sends that much to the same address first, as the owner's own send, and the send keeps waiting.
func (s *DualTokenStore) AnswerAgentCryptoSend(ctx context.Context, workspaceID, agentID, id, by string, approve bool, testMinor int64) (CryptoSend, error) {
	c, err := s.cryptoSend(ctx, workspaceID, agentID, id)
	if err != nil {
		return c, err
	}
	if c.Status != "awaiting_owner" {
		return c, fmt.Errorf("%w: this send is %s, not waiting for you", ErrCryptoSend, c.Status)
	}
	n, err := s.cryptoSendNetwork(c.Network)
	if err != nil {
		return c, err
	}
	switch {
	case testMinor > 0:
		if testMinor >= c.AmountMinor {
			return c, fmt.Errorf("%w: a test amount is less than the send", ErrCryptoSend)
		}
		in := c.request()
		in.AmountMinor = testMinor
		return s.SendAgentCrypto(ctx, workspaceID, agentID, by, true, c.ID+":test", in)
	case !approve:
		tag, err := s.pool.Exec(ctx, `UPDATE agent_crypto_sends SET status = 'refused', reason = 'the owner declined it', approved_by = $2,
			updated_at = now() WHERE id = $1 AND status = 'awaiting_owner'`, c.ID, by)
		if err != nil {
			return c, fmt.Errorf("economy: crypto send: %w", err)
		}
		if tag.RowsAffected() == 1 {
			if err := s.AppendAudit(ctx, workspaceID, "agent:"+agentID, "agent.crypto_send_declined", map[string]any{"send_id": c.ID, "by": by}); err != nil {
				return c, err
			}
		}
		return s.cryptoSend(ctx, workspaceID, agentID, c.ID)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE agent_crypto_sends SET status = 'sending', approved_by = $2, updated_at = now()
		WHERE id = $1 AND status = 'awaiting_owner'`, c.ID, by)
	if err != nil {
		return c, fmt.Errorf("economy: crypto send: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if err := s.AppendAudit(ctx, workspaceID, "agent:"+agentID, "agent.crypto_send_approved", map[string]any{"send_id": c.ID, "by": by}); err != nil {
			return c, err
		}
	}
	if c, err = s.cryptoSend(ctx, workspaceID, agentID, c.ID); err != nil {
		return c, err
	}
	return s.carryOn(ctx, workspaceID, n, c)
}

// AgentCryptoSends is what agentID sent, and what other Talyvor agents sent it, newest first.
//
// shortcut: the newest 200; upgrade with paging when an agent sends more.
func (s *DualTokenStore) AgentCryptoSends(ctx context.Context, workspaceID, agentID string) ([]CryptoSend, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cryptoSendColumns+`, workspace_id = $1 AND agent_id = $2 FROM agent_crypto_sends
		WHERE (workspace_id = $1 AND agent_id = $2) OR (to_workspace_id = $1 AND to_agent_id = $2 AND status = 'landed')
		ORDER BY created_at DESC LIMIT 200`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: crypto sends: %w", err)
	}
	defer rows.Close()
	out := []CryptoSend{}
	for rows.Next() {
		var mine bool
		c, err := s.scanCryptoSend(scanFunc(func(dest ...any) error { return rows.Scan(append(dest, &mine)...) }))
		if err != nil {
			return nil, err
		}
		if !mine {
			// Received: the sender is named by its account only, never by its agent or workspace.
			var from string
			if err := s.pool.QueryRow(ctx, `SELECT address FROM agent_smart_accounts WHERE agent_id = $1`, c.AgentID).Scan(&from); err != nil {
				return nil, fmt.Errorf("economy: crypto sends: %w", err)
			}
			c = CryptoSend{ID: c.ID, CryptoSendQuote: CryptoSendQuote{Network: c.Network, Token: c.Token, TokenAddress: c.TokenAddress,
				AmountMinor: c.AmountMinor, To: from, ToKind: "agent"}, AgentID: agentID, Direction: "in", Status: c.Status,
				TxHash: c.TxHash, ExplorerURL: c.ExplorerURL, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// transferCall is USDC's transfer(to, amount).
func transferCall(to string, amount int64) []byte {
	b := make([]byte, 4+64)
	copy(b, []byte{0xa9, 0x05, 0x9c, 0xbb})
	addr, _ := hex.DecodeString(strings.TrimPrefix(smartaccount.Checksum(to), "0x"))
	copy(b[4+12:4+32], addr)
	new(big.Int).SetInt64(amount).FillBytes(b[4+32:])
	return b
}

type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }
