package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/smartaccount"
)

// chain_indexer.go — B48.6: EVERY MOVEMENT OF AN AGENT'S CRYPTO WALLET SEEN, CONFIRMED AND RECONCILED.
//
// A transfer of USDC into or out of an agent's smart account (B48.2) is seen when the provider's signed webhook names
// its transaction (Observe), or when the node's logs show it (Poll, the fallback for a webhook that never came). Either
// way Lens reads the transaction from the node itself, so a notification brings in nothing that is not on the chain,
// and the same transfer told of twice, or by both, is one chain_movements row (migration 0286).
//
// It moves the ledger only once it has its network's confirmations, more for a large amount (Confirm): then it is
// posted between the agent's account for the wallet on that network — purpose chain, which nothing in Lens spends, as
// the money is the person's on the chain and not Talyvor's to move — and the partner account it came in or went out
// through. Until it is deep enough not to move it is watched: a reorg that takes it off the chain posts the reversal,
// and it posts again if it comes back.
//
// Each night every wallet's balance on the chain is compared with its account and the movements seen but not yet
// confirmed (Reconcile); any difference — money that moved behind Talyvor's back — is kept and raised to the operator.

// ChainNetwork is one EVM network the indexer reads, and how deep a movement there must be before it posts.
type ChainNetwork struct {
	Name               string             `json:"name"` // base, ethereum, arbitrum, optimism or polygon
	Chain              smartaccount.Chain `json:"-"`
	USDC               string             `json:"usdc"`                // USDC's contract on the network
	Confirmations      int64              `json:"confirmations"`       // before a movement posts
	LargeConfirmations int64              `json:"large_confirmations"` // before a large one posts; 0 = Confirmations
	LargeFromMinor     int64              `json:"large_from_minor"`    // a movement of this many micro-USDC or more is large; 0 = none is
	Funding            string             `json:"funding"`             // test on a devnet or testnet, live on a real network
}

// threshold is how many confirmations a movement of amount needs on n.
func (n ChainNetwork) threshold(amount int64) int64 {
	if n.LargeFromMinor > 0 && amount >= n.LargeFromMinor && n.LargeConfirmations > n.Confirmations {
		return n.LargeConfirmations
	}
	return n.Confirmations
}

// ChainIndexer mirrors agents' crypto wallets in the ledger.
type ChainIndexer struct {
	s    *DualTokenStore
	nets map[string]ChainNetwork
}

// ErrChainNetwork: a network the indexer does not read.
var ErrChainNetwork = errors.New("economy: the chain indexer does not read that network")

// NewChainIndexer reads nets into s's ledger. Every network names its USDC contract, its confirmations and its funding.
func (s *DualTokenStore) NewChainIndexer(nets []ChainNetwork) (*ChainIndexer, error) {
	x := &ChainIndexer{s: s, nets: map[string]ChainNetwork{}}
	for _, n := range nets {
		n.USDC = smartaccount.Checksum(n.USDC)
		switch {
		case n.Name == "" || x.nets[n.Name].Name != "":
			return nil, fmt.Errorf("economy: chain indexer: each network once, by name, not %q", n.Name)
		case n.USDC == "":
			return nil, fmt.Errorf("economy: chain indexer: %s: USDC's contract address", n.Name)
		case n.Confirmations < 1 || n.LargeConfirmations < 0 || n.LargeFromMinor < 0:
			return nil, fmt.Errorf("economy: chain indexer: %s: confirmations are set, and at least 1", n.Name)
		case n.Funding != FundingTest && n.Funding != FundingLive:
			return nil, fmt.Errorf("economy: chain indexer: %s: funding is test or live, not %q", n.Name, n.Funding)
		case n.Chain.RPCURL == "":
			return nil, fmt.Errorf("economy: chain indexer: %s: a node", n.Name)
		}
		x.nets[n.Name] = n
	}
	return x, nil
}

// Networks is the networks x reads.
func (x *ChainIndexer) Networks() []string {
	out := make([]string, 0, len(x.nets))
	for name := range x.nets {
		out = append(out, name)
	}
	return out
}

func (x *ChainIndexer) network(name string) (ChainNetwork, error) {
	n, ok := x.nets[name]
	if !ok {
		return ChainNetwork{}, fmt.Errorf("%w: %q", ErrChainNetwork, name)
	}
	return n, nil
}

// chainWallet is an agent's smart account.
type chainWallet struct{ agentID, workspaceID, address string }

// wallets is the agents' smart accounts, by lower-case address; only those among addresses unless it is nil.
func (x *ChainIndexer) wallets(ctx context.Context, addresses []string) (map[string]chainWallet, error) {
	q := `SELECT agent_id, workspace_id, address FROM agent_smart_accounts`
	var args []any
	if addresses != nil {
		q += ` WHERE lower(address) = ANY($1)`
		args = append(args, addresses)
	}
	rows, err := x.s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("economy: chain wallets: %w", err)
	}
	defer rows.Close()
	out := map[string]chainWallet{}
	for rows.Next() {
		var w chainWallet
		if err := rows.Scan(&w.agentID, &w.workspaceID, &w.address); err != nil {
			return nil, fmt.Errorf("economy: chain wallets: %w", err)
		}
		out[strings.ToLower(w.address)] = w
	}
	return out, rows.Err()
}

// Observe reads transaction txHash on network from the node and keeps every USDC transfer in it into or out of an
// agent's wallet, once however often it is told: source says who told (webhook or poll). It answers how many it had
// not seen. A transaction the node does not have yet is money.ErrNotFound, so the provider sends its webhook again.
func (x *ChainIndexer) Observe(ctx context.Context, network, txHash, source string) (int, error) {
	n, err := x.network(network)
	if err != nil {
		return 0, err
	}
	ts, mined, err := n.Chain.TxTransfers(ctx, strings.ToLower(strings.TrimSpace(txHash)))
	if err != nil {
		return 0, err
	}
	if !mined {
		return 0, fmt.Errorf("%w: %s has no mined transaction %s", money.ErrNotFound, network, txHash)
	}
	var addrs []string
	for _, t := range ts {
		addrs = append(addrs, strings.ToLower(t.From), strings.ToLower(t.To))
	}
	ours, err := x.wallets(ctx, addrs)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, t := range ts {
		if t.Token != n.USDC {
			continue
		}
		for _, side := range []struct {
			wallet, counterparty, direction string
		}{{t.To, t.From, "in"}, {t.From, t.To, "out"}} {
			w, ok := ours[strings.ToLower(side.wallet)]
			if !ok {
				continue
			}
			if !t.Value.IsInt64() || t.Value.Sign() <= 0 {
				slog.Warn("economy: a chain transfer's amount is out of range; it is not mirrored", "network", network,
					"tx", t.TxHash, "position", t.Position)
				continue
			}
			tag, err := x.s.pool.Exec(ctx, `INSERT INTO chain_movements (id, workspace_id, agent_id, network, tx_hash, log_position,
				direction, wallet, counterparty, token, asset, amount_minor, funding, source, block_number, block_hash)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
				ON CONFLICT (network, tx_hash, log_position, direction) DO NOTHING`,
				"cmv_"+uuid.NewString(), w.workspaceID, w.agentID, network, t.TxHash, t.Position, side.direction, w.address,
				side.counterparty, t.Token, money.USDC, t.Value.Int64(), n.Funding, source, t.BlockNumber, t.BlockHash)
			if err != nil {
				return added, fmt.Errorf("economy: observe chain movement: %w", err)
			}
			added += int(tag.RowsAffected())
		}
	}
	return added, nil
}

// pollChunk is how many blocks one eth_getLogs asks for.
const pollChunk = 2000

// Poll reads network's logs since it last did, and Observes every transaction with a USDC transfer into or out of an
// agent's wallet. It reads again the blocks a reorg could still change, so a transaction a reorg brought in is seen.
func (x *ChainIndexer) Poll(ctx context.Context, network string) (int, error) {
	n, err := x.network(network)
	if err != nil {
		return 0, err
	}
	head, err := n.Chain.Head(ctx)
	if err != nil {
		return 0, err
	}
	depth := 2 * max(n.Confirmations, n.LargeConfirmations)
	var next int64
	err = x.s.pool.QueryRow(ctx, `SELECT next_block FROM chain_index_cursors WHERE network = $1`, network).Scan(&next)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		next = head - depth // a network read for the first time is read from as deep as a reorg reaches
	case err != nil:
		return 0, fmt.Errorf("economy: chain poll: %w", err)
	}
	from := max(0, min(next, head+1)-depth)
	ws, err := x.wallets(ctx, nil)
	if err != nil {
		return 0, err
	}
	addrs := make([]string, 0, len(ws))
	for _, w := range ws {
		addrs = append(addrs, w.address)
	}
	added := 0
	for lo := from; lo <= head; lo += pollChunk {
		txs, err := n.Chain.TransferTxs(ctx, n.USDC, addrs, lo, min(lo+pollChunk-1, head))
		if err != nil {
			return added, err
		}
		for _, tx := range txs {
			k, err := x.Observe(ctx, network, tx, "poll")
			if err != nil && !errors.Is(err, money.ErrNotFound) { // reorged out since the logs were read
				return added, err
			}
			added += k
		}
	}
	if _, err := x.s.pool.Exec(ctx, `INSERT INTO chain_index_cursors (network, next_block) VALUES ($1, $2)
		ON CONFLICT (network) DO UPDATE SET next_block = GREATEST(chain_index_cursors.next_block, EXCLUDED.next_block)`, network, head+1); err != nil {
		return added, fmt.Errorf("economy: chain poll: %w", err)
	}
	return added, nil
}

// ChainConfirmed is what one Confirm did.
type ChainConfirmed struct {
	Posted   int `json:"posted"`
	Reversed int `json:"reversed"`
	Final    int `json:"final"`
}

type chainMovement struct {
	id, network, workspaceID, agentID, txHash, direction, wallet, counterparty, token, funding, status string
	position                                                                                           int
	amount                                                                                             int64
	posted                                                                                             int
}

// Confirm looks again at every movement on network not yet final: one with its confirmations posts; one a reorg took
// off the chain is reversed; one that came back posts again; one twice as deep as it needed is final.
func (x *ChainIndexer) Confirm(ctx context.Context, network string) (ChainConfirmed, error) {
	var res ChainConfirmed
	n, err := x.network(network)
	if err != nil {
		return res, err
	}
	head, err := n.Chain.Head(ctx)
	if err != nil {
		return res, err
	}
	rows, err := x.s.pool.Query(ctx, `SELECT id, workspace_id, agent_id, tx_hash, log_position, direction, wallet, counterparty, token,
		amount_minor, funding, status, posted FROM chain_movements WHERE network = $1 AND status <> 'final' ORDER BY seen_at, id`, network)
	if err != nil {
		return res, fmt.Errorf("economy: chain confirm: %w", err)
	}
	var ms []chainMovement
	for rows.Next() {
		m := chainMovement{network: network}
		if err := rows.Scan(&m.id, &m.workspaceID, &m.agentID, &m.txHash, &m.position, &m.direction, &m.wallet, &m.counterparty,
			&m.token, &m.amount, &m.funding, &m.status, &m.posted); err != nil {
			rows.Close()
			return res, fmt.Errorf("economy: chain confirm: %w", err)
		}
		ms = append(ms, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("economy: chain confirm: %w", err)
	}
	txs := map[string][]smartaccount.Transfer{}
	var errs []error
	for _, m := range ms {
		ts, ok := txs[m.txHash]
		if !ok {
			if ts, _, err = n.Chain.TxTransfers(ctx, m.txHash); err != nil {
				return res, err
			}
			txs[m.txHash] = ts
		}
		var on *smartaccount.Transfer
		for i, t := range ts {
			from, to := m.counterparty, m.wallet
			if m.direction == "out" {
				from, to = to, from
			}
			if t.Position == m.position && t.Token == m.token && t.From == from && t.To == to && t.Value.IsInt64() && t.Value.Int64() == m.amount {
				on = &ts[i]
			}
		}
		posted := m.status == "confirmed"
		var err error
		switch {
		case on == nil && posted:
			if err = x.move(ctx, m, true); err == nil {
				res.Reversed++
			}
		case on == nil:
			_, err = x.s.pool.Exec(ctx, `UPDATE chain_movements SET block_number = NULL, block_hash = '', confirmations = 0, updated_at = now()
				WHERE id = $1`, m.id)
		default:
			conf, need := head-on.BlockNumber+1, n.threshold(m.amount)
			status := m.status
			switch {
			case !posted && conf >= need:
				if err = x.move(ctx, m, false); err != nil {
					break
				}
				status = "confirmed"
				res.Posted++
			case posted && conf >= 2*need:
				// shortcut: final at twice the confirmations a movement needed; upgrade to the network's finalized
				// block (eth_getBlockByNumber "finalized") once every network's node serves it.
				status = "final"
				res.Final++
			case !posted:
				status = "seen"
			}
			if err == nil {
				_, err = x.s.pool.Exec(ctx, `UPDATE chain_movements SET status = $2, block_number = $3, block_hash = $4, confirmations = $5,
					updated_at = now() WHERE id = $1`, m.id, status, on.BlockNumber, on.BlockHash, conf)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("economy: chain movement %s: %w", m.id, err))
		}
	}
	return res, errors.Join(errs...)
}

// chainRef is the reference the ledger's accounts for wallet on network carry.
func chainRef(network, wallet string) string {
	return "chain:" + network + ":" + strings.ToLower(wallet)
}

// chainAccount is the ledger's account of purpose — chain, for agentID's, or partner — for wallet on network, opened
// the first time money moves there.
func (x *ChainIndexer) chainAccount(ctx context.Context, workspaceID, agentID, purpose, network, wallet string) (string, error) {
	ref := chainRef(network, wallet)
	var id string
	err := x.s.pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = $2 AND partner_account_ref = $3
		AND currency = 'USDC' ORDER BY created_at LIMIT 1`, workspaceID, purpose, ref).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if purpose != MoneyChain {
			agentID = ""
		}
		a, err := x.s.OpenMoneyAccount(ctx, MoneyAccount{WorkspaceID: workspaceID, AgentID: agentID, Currency: money.USDC, Purpose: purpose,
			Name: "chain:" + network, PartnerAccountRef: ref})
		return a.ID, err
	}
	if err != nil {
		return "", fmt.Errorf("economy: chain account: %w", err)
	}
	return id, nil
}

// move posts m to the ledger — or, reverse, takes back its last posting — and records it on m's row.
func (x *ChainIndexer) move(ctx context.Context, m chainMovement, reverse bool) error {
	wallet, err := x.chainAccount(ctx, m.workspaceID, m.agentID, MoneyChain, m.network, m.wallet)
	if err != nil {
		return err
	}
	mirror, err := x.chainAccount(ctx, m.workspaceID, m.agentID, MoneyPartner, m.network, m.wallet)
	if err != nil {
		return err
	}
	amount, kind, key, status, posted := m.amount, "chain_deposit", fmt.Sprintf("chain:%s:%d", m.id, m.posted+1), "confirmed", m.posted+1
	if m.direction == "out" {
		amount, kind = -amount, "chain_withdrawal"
	}
	if reverse {
		amount, kind, key, status, posted = -amount, "chain_reversal", fmt.Sprintf("chain_reversal:%s:%d", m.id, m.posted), "reversed", m.posted
	}
	e, err := x.s.PostMoney(ctx, MoneyEntry{WorkspaceID: m.workspaceID, Capability: CapabilityStablecoins, Kind: kind, IdempotencyKey: key,
		Funding: m.funding, Counterparty: m.counterparty, PartnerRef: m.txHash + ":" + strconv.Itoa(m.position),
		Postings: []MoneyPosting{{AccountID: wallet, AmountMinor: amount, Currency: money.USDC},
			{AccountID: mirror, AmountMinor: -amount, Currency: money.USDC}}})
	if err != nil {
		return err
	}
	block := "block_number"
	if reverse {
		block = "NULL"
	}
	_, err = x.s.pool.Exec(ctx, `UPDATE chain_movements SET status = $2, posted = $3, entry_id = $4, block_number = `+block+`,
		updated_at = now() WHERE id = $1`, m.id, status, posted, e.ID)
	return err
}

// ChainReconciliation is one wallet's reconciliation on one network.
type ChainReconciliation struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	Network         string `json:"network"`
	Wallet          string `json:"wallet"`
	Asset           string `json:"asset"`
	BlockNumber     int64  `json:"block_number"`
	ChainMinor      int64  `json:"chain_minor"`      // what the chain says the wallet holds at BlockNumber
	LedgerMinor     int64  `json:"ledger_minor"`     // what the ledger says the wallet holds
	PendingMinor    int64  `json:"pending_minor"`    // movements seen on the chain by BlockNumber, not yet confirmed
	DifferenceMinor int64  `json:"difference_minor"` // chain minus ledger and pending: 0 when they agree
}

// Reconcile compares every agent's wallet on network with the ledger and keeps each comparison: what the chain says
// it holds at the deepest block a confirmed movement can be in, against its account in the ledger and the movements
// seen by then but not yet confirmed. It answers those that differ.
func (x *ChainIndexer) Reconcile(ctx context.Context, network string) ([]ChainReconciliation, error) {
	n, err := x.network(network)
	if err != nil {
		return nil, err
	}
	head, err := n.Chain.Head(ctx)
	if err != nil {
		return nil, err
	}
	block := max(0, head-n.Confirmations+1)
	ws, err := x.wallets(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []ChainReconciliation
	for _, w := range ws {
		on, err := n.Chain.BalanceOf(ctx, n.USDC, w.address, block)
		if err != nil {
			return out, err
		}
		if !on.IsInt64() {
			return out, fmt.Errorf("economy: reconcile: %s holds more than Lens counts", w.address)
		}
		r := ChainReconciliation{ID: "crec_" + uuid.NewString(), WorkspaceID: w.workspaceID, AgentID: w.agentID, Network: network,
			Wallet: w.address, Asset: money.USDC, BlockNumber: block, ChainMinor: on.Int64()}
		if err := x.s.pool.QueryRow(ctx, `SELECT
			COALESCE((SELECT sum(b.balance_minor) FROM money_accounts a JOIN money_account_balances b ON b.account_id = a.id
				WHERE a.workspace_id = $1 AND a.purpose = 'chain' AND a.partner_account_ref = $2), 0)::bigint,
			COALESCE((SELECT sum(CASE direction WHEN 'in' THEN amount_minor ELSE -amount_minor END) FROM chain_movements
				WHERE workspace_id = $1 AND agent_id = $3 AND network = $4 AND status IN ('seen', 'reversed')
				AND block_number IS NOT NULL AND block_number <= $5), 0)::bigint`,
			w.workspaceID, chainRef(network, w.address), w.agentID, network, block).Scan(&r.LedgerMinor, &r.PendingMinor); err != nil {
			return out, fmt.Errorf("economy: reconcile: %w", err)
		}
		r.DifferenceMinor = r.ChainMinor - r.LedgerMinor - r.PendingMinor
		if _, err := x.s.pool.Exec(ctx, `INSERT INTO chain_reconciliations (id, workspace_id, agent_id, network, wallet, asset, block_number,
			chain_minor, ledger_minor, pending_minor, difference_minor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			r.ID, r.WorkspaceID, r.AgentID, r.Network, r.Wallet, r.Asset, r.BlockNumber, r.ChainMinor, r.LedgerMinor, r.PendingMinor,
			r.DifferenceMinor); err != nil {
			return out, fmt.Errorf("economy: reconcile: %w", err)
		}
		if r.DifferenceMinor != 0 {
			out = append(out, r)
		}
	}
	return out, nil
}
