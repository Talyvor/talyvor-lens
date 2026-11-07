package economy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/ap2"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// tokenNode is a node that answers eth_call of name() and version() as a USDC contract does. (Against the devnet's
// contract: TestB486_ in cmd/lens.)
func tokenNode(t *testing.T, name, version string) string {
	t.Helper()
	abi := func(s string) string { // an ABI string: offset, length, the bytes padded to 32
		return fmt.Sprintf("0x%064x%064x%s", 32, len(s), hex.EncodeToString([]byte(s))+strings.Repeat("00", 32-len(s)%32))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var call struct{ Data string }
		_ = json.Unmarshal(req.Params[0], &call)
		out := map[string]string{"0x06fdde03": abi(name), "0x54fd4d50": abi(version)}[call.Data]
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": out})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// B48.10 on the real schema: requests to be paid into an agent's wallet, and the deposits the indexer posts matched to
// them through its own posting path (ChainIndexer.move). A request naming no payer is paid only by exactly what it
// asks, before it expires; one naming its payer counts that payer's deposits — partial shows what is left, an
// overpayment is flagged for the owner to refund or keep, one after the expiry is late and does not count; the
// workspace's own wallets pay none; a reorg takes back its match; a request to another Talyvor agent lands in its
// owner's approvals with the requester's trust, is answered the same as one to an unknown address, and no approver
// agent may approve it; a workspace names at most PayerRequestsPerDay payers a day.
func TestB4810_RequestsToBePaidAreMatchedToTheirDeposits(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	s.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	const usdc = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	x, err := s.NewChainIndexer([]ChainNetwork{{Name: "base", Chain: smartaccount.Chain{ID: 8453, RPCURL: tokenNode(t, "USDC", "2")}, USDC: usdc,
		Confirmations: 1, Funding: FundingTest}})
	if err != nil {
		t.Fatal(err)
	}
	const wsA, wsB = "ws-b4810-seller", "ws-b4810-payer"
	wallets := map[string]string{}
	agent := func(ws, name, addr string) string {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1) ON CONFLICT DO NOTHING`, ws); err != nil {
			t.Fatal(err)
		}
		a, err := s.CreateAgent(ctx, ws, name, "owner")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation,
			owner_credential_id, owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x0', '0x0', 'cred', $4, '0x0', 'coinbase', '')`,
			a.ID, ws, addr, make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
		wallets[a.ID] = addr
		return a.ID
	}
	seller := agent(wsA, "Seller", "0x00000000000000000000000000000000000b4810")
	payer := agent(wsB, "Payer", "0x00000000000000000000000000000000000B481b")
	treasury := agent(wsA, "Treasury", "0x00000000000000000000000000000000000b481A") // the seller's owner's other wallet
	const stranger = "0x00000000000000000000000000000000000Ba5e5"

	// deposit posts amount from `from` into the seller's wallet as the indexer does once it has its confirmations.
	deposit := func(from string, amount int64) chainMovement {
		t.Helper()
		m := chainMovement{id: "cmv_" + uuid.NewString(), network: "base", workspaceID: wsA, agentID: seller, txHash: "0x" + uuid.NewString(),
			direction: "in", wallet: wallets[seller], counterparty: from, token: usdc, funding: FundingTest, amount: amount}
		if _, err := pool.Exec(ctx, `INSERT INTO chain_movements (id, workspace_id, agent_id, network, tx_hash, log_position, direction,
			wallet, counterparty, token, asset, amount_minor, funding, source) VALUES ($1, $2, $3, 'base', $4, 0, 'in', $5, $6, $7, 'USDC', $8, 'test', 'poll')`,
			m.id, wsA, seller, m.txHash, m.wallet, from, usdc, amount); err != nil {
			t.Fatal(err)
		}
		if err := x.move(ctx, m, false); err != nil {
			t.Fatal(err)
		}
		return m
	}
	ask := func(key string, amount int64, payerAddr string) CryptoRequest {
		t.Helper()
		r, err := x.CreatePaymentRequest(ctx, CryptoRequestIn{WorkspaceID: wsA, AgentID: seller, Token: "USDC", Network: "base", AmountMinor: amount,
			Memo: "invoice " + key, ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second), PayerAddress: payerAddr, IdempotencyKey: key,
			CreatedBy: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	read := func(id string) CryptoRequest {
		t.Helper()
		r, err := s.CryptoPaymentRequest(ctx, wsA, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Paid exactly: closed. The deposit itself is posted once, to the agent's chain account. Its x402 requirement
	// signs under the EIP-712 domain the token's contract says.
	exact := ask("exact", 25_000000, "")
	if exact.Status != "open" || exact.URI != "ethereum:"+usdc+"@8453/transfer?address="+wallets[seller]+"&uint256=25000000" ||
		exact.Link != CryptoRequestLinkBase+exact.ID || exact.X402 == nil {
		t.Fatalf("a new request: %+v", exact)
	}
	if extra := exact.X402["accepts"].([]map[string]any)[0]["extra"].(map[string]any); extra["name"] != "USDC" || extra["version"] != "2" {
		t.Fatalf("x402's EIP-712 domain: %v, want the contract's USDC / 2", extra)
	}
	if again := ask("exact", 25_000000, ""); again.ID != exact.ID {
		t.Fatalf("the same Idempotency-Key made %s, want %s", again.ID, exact.ID)
	}
	deposit(stranger, 3_000000) // any other deposit landing in the wallet pays no request naming no payer
	if r := read(exact.ID); r.Status != "open" || r.ReceivedMinor != 0 {
		t.Fatalf("a request naming no payer, after a deposit of another amount: %+v", r)
	}
	paid := deposit(stranger, 25_000000)
	if r := read(exact.ID); r.Status != "paid" || r.ReceivedMinor != 25_000000 || r.RemainingMinor != 0 || r.X402 != nil || len(r.Payments) != 1 {
		t.Fatalf("paid exactly: %+v", r)
	}
	var posted int64 // both deposits, the one that paid no request too
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0) FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
		WHERE a.workspace_id = $1 AND a.purpose = 'chain'`, wsA).Scan(&posted); err != nil || posted != 28_000000 {
		t.Fatalf("the wallet's chain account holds %d, %v; want 28000000", posted, err)
	}

	// The workspace's own wallets pay no request, not even exactly, and are not a payer one may name.
	own := ask("own", 8_000000, "")
	deposit(wallets[treasury], 8_000000)
	if r := read(own.ID); r.Status != "open" {
		t.Fatalf("a request after the owner's own wallet sent its amount: %+v", r)
	}
	if _, err := x.CreatePaymentRequest(ctx, CryptoRequestIn{WorkspaceID: wsA, AgentID: seller, Network: "base", AmountMinor: 1,
		ExpiresAt: time.Now().Add(time.Hour), PayerAddress: wallets[treasury], IdempotencyKey: "own-payer"}); !errors.Is(err, ErrCryptoRequest) {
		t.Fatalf("naming the workspace's own wallet as payer = %v, want ErrCryptoRequest", err)
	}

	// Partly paid by the payer it names: what is left is asked for, by the link, the URI and x402.
	part := ask("part", 10_000000, stranger)
	deposit(stranger, 4_000000)
	r := read(part.ID)
	if r.Status != "partial" || r.RemainingMinor != 6_000000 || r.URI != "ethereum:"+usdc+"@8453/transfer?address="+wallets[seller]+"&uint256=6000000" {
		t.Fatalf("partly paid: %+v", r)
	}
	if acc := r.X402["accepts"].([]map[string]any)[0]; acc["maxAmountRequired"] != "6000000" || acc["payTo"] != wallets[seller] || acc["asset"] != usdc {
		t.Fatalf("x402 of a partly paid request: %+v", acc)
	}

	// Overpaid: flagged, and the owner decides.
	deposit(stranger, 9_000000)
	if r := read(part.ID); r.Status != "overpaid" || r.OverpaidMinor != 3_000000 {
		t.Fatalf("overpaid: %+v", r)
	}
	if _, err := s.DecideCryptoOverpayment(ctx, wsA, exact.ID, "keep", "owner"); !errors.Is(err, ErrCryptoRequest) {
		t.Fatalf("deciding on a request paid exactly = %v, want ErrCryptoRequest", err)
	}
	if r, err := s.DecideCryptoOverpayment(ctx, wsA, part.ID, "refund", "owner"); err != nil || r.Overpayment != "refund" {
		t.Fatalf("refund the overpayment: %+v, %v", r, err)
	}

	// Expired: a payment after the expiry is not matched to it — it is shown as late.
	late := ask("late", 7_000000, stranger)
	if _, err := pool.Exec(ctx, `UPDATE crypto_payment_requests SET expires_at = now() - interval '1 minute' WHERE id = $1`, late.ID); err != nil {
		t.Fatal(err)
	}
	deposit(stranger, 7_000000)
	if r := read(late.ID); r.Status != "expired" || r.ReceivedMinor != 0 || r.LateMinor != 7_000000 || r.X402 != nil {
		t.Fatalf("paid after it expired: %+v", r)
	}

	// A reorg that takes a deposit back takes back its match.
	if err := x.move(ctx, paid, true); err != nil {
		t.Fatal(err)
	}
	if r := read(exact.ID); r.Status != "open" || r.ReceivedMinor != 0 {
		t.Fatalf("after the reorg took its payment back: %+v", r)
	}

	// Talyvor to Talyvor: the request lands in the payer's owner's approvals with the requester's trust; only the
	// named payer's payments count; no approver agent may approve it.
	toAgent := ask("to-agent", 12_500000, wallets[payer])
	deposit("0x00000000000000000000000000000000000Ba5e6", 12_500000) // never the request naming the payer
	approvals, err := s.ListAgentApprovals(ctx, wsB)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("the payer's approvals: %+v, %v", approvals, err)
	}
	a := approvals[0]
	if a.AgentID != payer || a.PaymentRequestID != toAgent.ID || a.Currency != "USDC" || a.AmountMinor != 12_500000 || a.Status != "pending" ||
		a.Trust != "Talyvor agent, owner not verified" || a.Reason != "Seller asks to be paid 12.5 USDC on base" || a.Payee != nil || toAgent.ApprovalID != a.ID {
		t.Fatalf("the payer's approval: %+v (request %+v)", a, toAgent)
	}
	// A second request to the same agent while the first waits files no second approval, and is answered as one to
	// an address that is no Talyvor agent's is.
	again, err := x.CreatePaymentRequest(ctx, CryptoRequestIn{WorkspaceID: wsA, AgentID: seller, Network: "base", AmountMinor: 1,
		ExpiresAt: time.Now().Add(time.Hour), PayerAddress: wallets[payer], IdempotencyKey: "to-agent-again"})
	unknown, err2 := x.CreatePaymentRequest(ctx, CryptoRequestIn{WorkspaceID: wsA, AgentID: seller, Network: "base", AmountMinor: 1,
		ExpiresAt: time.Now().Add(time.Hour), PayerAddress: "0x00000000000000000000000000000000000Fa4e1", IdempotencyKey: "to-nobody"})
	if err != nil || err2 != nil || again.Status != unknown.Status {
		t.Fatalf("to a waiting Talyvor agent: %+v, %v; to an unknown address: %+v, %v", again, err, unknown, err2)
	}
	if approvals, err := s.ListAgentApprovals(ctx, wsB); err != nil || len(approvals) != 1 {
		t.Fatalf("the payer's approvals after a second request: %d, %v; want 1", len(approvals), err)
	}

	// At most PayerRequestsPerDay requests naming a payer a day, Talyvor agent or not; a replay is still answered.
	x.PayerRequestsPerDay = 5 // part, late, to-agent, to-agent-again, to-nobody
	if _, err := x.CreatePaymentRequest(ctx, CryptoRequestIn{WorkspaceID: wsA, AgentID: seller, Network: "base", AmountMinor: 1,
		ExpiresAt: time.Now().Add(time.Hour), PayerAddress: stranger, IdempotencyKey: "one-too-many"}); !errors.Is(err, ErrCryptoRequestLimit) {
		t.Fatalf("a sixth request naming a payer today = %v, want ErrCryptoRequestLimit", err)
	}
	if r := ask("part", 10_000000, stranger); r.ID != part.ID {
		t.Fatalf("a replay at the cap made %s, want %s", r.ID, part.ID)
	}
	x.PayerRequestsPerDay = DefaultPayerRequestsPerDay
	if r := read(toAgent.ID); r.Status != "open" {
		t.Fatalf("a request naming its payer, after a stranger's deposit: %+v", r)
	}
	deposit(wallets[payer], 12_500000)
	if r := read(toAgent.ID); r.Status != "paid" {
		t.Fatalf("paid by the payer it named: %+v", r)
	}
	approver := agent(wsB, "Approver", "0x00000000000000000000000000000000000Ab481")
	if _, err := pool.Exec(ctx, `INSERT INTO agent_approvers (id, workspace_id, agent_id, approver_agent_id, limit_ulxc, mandate, signed_by)
		VALUES ('apv_b4810', $1, $2, $3, 1000000000, 'mandate', 'owner')`, wsB, payer, approver); err != nil {
		t.Fatal(err)
	}
	s.SetMandateSigner(ap2.NewSigner([]byte("b48.10 test seed, thirty-two by."), "https://lens.test"))
	if _, err := s.ApproveAsAgent(ctx, wsB, approver, a.ID); !errors.Is(err, ErrAboveApproverLimits) {
		t.Fatalf("an approver agent approving another agent's request to be paid = %v, want ErrAboveApproverLimits", err)
	}

	// The shared link shows what to pay, never whose it is.
	pub, err := s.PublicCryptoPaymentRequest(ctx, toAgent.ID)
	if err != nil || pub.AgentID != "" || pub.PayerAddress != "" || pub.Payments != nil || pub.PayTo != wallets[seller] {
		t.Fatalf("public view: %+v, %v", pub, err)
	}
}
