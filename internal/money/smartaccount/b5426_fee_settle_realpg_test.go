package smartaccount_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/migrations"
)

// b5426_fee_settle_realpg_test.go — B54.26: an agent's payment whose network fee an ERC-20 paymaster takes in the token
// settles from its operation's receipt, and what is left approved is counted per paymaster and chain.

var settleMigrate sync.Once

// settleDB is a migrated database of its own; without LENS_TEST_DATABASE_URL the test skips.
func settleDB(t *testing.T) *pgxpool.Pool {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping the real-PG settle test")
	}
	const schema = "smartaccount_settle_realpg"
	ctx := context.Background()
	settleMigrate.Do(func() {
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		cfg.RuntimeParams["search_path"] = schema + ",public"
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatal(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// settleAgent gives agentID in ws a smart account at address, and answers a way to keep a use of its as the gate does
// and to read one back.
func settleAgent(t *testing.T, pool *pgxpool.Pool, ws, agentID, address string) (
	keep func(chainID int64, token, to string, amount, fee int64, paymaster string) int64,
	read func(id int64) (amount int64, charged, left *int64, outcome *string)) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id,
		owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x', '0x', 'cred', $4, '0x', 'cdp', '')`,
		agentID, ws, address, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	keep = func(chainID int64, token, to string, amount, fee int64, paymaster string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO agent_spend_permission_uses (agent_id, workspace_id, permission_number, token, counterparty,
			amount, fee, chain_id, paymaster) VALUES ($1, $2, 1, $3, $4, $5, $6, $7, $8) RETURNING id`,
			agentID, ws, token, to, amount+fee, fee, chainID, paymaster).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	read = func(id int64) (amount int64, charged, left *int64, outcome *string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT amount, fee_charged, fee_left, outcome FROM agent_spend_permission_uses WHERE id = $1`, id).
			Scan(&amount, &charged, &left, &outcome); err != nil {
			t.Fatal(err)
		}
		return
	}
	return keep, read
}

func n(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// A paymaster rotation keeps the old approval counted: what one paymaster still holds approved is never zeroed by
// another paymaster's approval, nor by the same paymaster's on another chain; only that paymaster's own next approval
// on that chain replaces it.
func TestB5426_APaymasterRotationKeepsTheOldApprovalCounted(t *testing.T) {
	pool := settleDB(t)
	ctx := context.Background()
	const ws, agent = "ws-b5426-rotation", "agt_b5426_rotation"
	const usdc, payee = "0x036CbD53842c5426634e7929541eC2318f3dCF7e", "0x000000000000000000000000000000000000b0b0"
	const oldPM, newPM = "0x000000000000000000000000000000000000FEE0", "0x000000000000000000000000000000000000FEE1"
	store := economy.NewDualTokenStore(nil, pool, nil)
	keep, read := settleAgent(t, pool, ws, agent, "0x00000000000000000000000000000000000B5426")
	settle := func(use, charged, left int64) {
		t.Helper()
		if err := store.SettleAgentCryptoFee(ctx, ws, agent, use, big.NewInt(charged), big.NewInt(left)); err != nil {
			t.Fatal(err)
		}
	}

	old := keep(8453, usdc, payee, 1_000_000, 1_000, oldPM)
	settle(old, 300, 700)
	rotated := keep(8453, usdc, payee, 1_000_000, 500, newPM)
	settle(rotated, 100, 400)
	otherChain := keep(84532, usdc, payee, 1_000_000, 500, oldPM)
	settle(otherChain, 100, 400)
	if a, _, l, _ := read(old); a != 1_001_000 || n(l) != 700 {
		t.Fatalf("the old paymaster's use counts %d with %d left after a rotation, want 1001000 and 700: it can still take it", a, n(l))
	}
	if a, _, l, _ := read(rotated); a != 1_000_500 || n(l) != 400 {
		t.Fatalf("the new paymaster's use counts %d with %d left, want 1000500 and 400", a, n(l))
	}
	// The old paymaster's own next approval on the chain replaces what it held.
	again := keep(8453, usdc, payee, 1_000_000, 800, oldPM)
	settle(again, 200, 600)
	if a, _, l, _ := read(old); a != 1_000_300 || n(l) != 0 {
		t.Fatalf("the old paymaster's first use counts %d with %d left once its approval was replaced, want 1000300 and 0", a, n(l))
	}
	if a, _, l, _ := read(otherChain); a != 1_000_500 || n(l) != 400 {
		t.Fatalf("the use on the other chain counts %d with %d left, want 1000500 and 400: another chain's approval is its own", a, n(l))
	}
}

// Receipt → settle → allowance on a local devnet: two payments whose network fees the ERC-20 paymaster takes in USDC
// are sent and settled from their receipts by the leader's job. Once the second's approval replaced the first's, the
// first counts exactly what the paymaster took for it, not the quote; the newest counts what it took and what is still
// approved, read from the chain. An operation handed to a bundler that refused it stays counted until its account uses
// its nonce for another operation, and then counts nothing.
func TestAgentSmartAccountOnDevnetTokenFeeSettlesFromTheReceipt(t *testing.T) {
	d := smartaccount.NewTokenFeeDevnet(t)
	pool := settleDB(t)
	ctx := context.Background()
	const ws, agent, payee = "ws-b5426-devnet", "agt_b5426_devnet", "0x000000000000000000000000000000000000b0b0"
	store := economy.NewDualTokenStore(nil, pool, nil)
	keep, read := settleAgent(t, pool, ws, agent, d.Account.Address)
	c := d.Chain
	c.TokenPaymasterLayout = smartaccount.LayoutSignedPrice
	c.CoinPrice = func(context.Context, string) (*big.Rat, error) { return big.NewRat(3_000_000_000, 1), nil }

	// pay prepares, quotes, keeps and signs a payment of amount to payee with its fee in USDC.
	pay := func(amount int64) (int64, smartaccount.UserOp, int64) {
		t.Helper()
		data, err := smartaccount.Execute(d.USDC, nil, transferCall(t, payee, amount))
		if err != nil {
			t.Fatal(err)
		}
		op, err := c.Prepare(ctx, d.Account, data)
		if err != nil {
			t.Fatal(err)
		}
		quote, err := c.SponsorInToken(ctx, &op, d.USDC)
		if err != nil {
			t.Fatal(err)
		}
		use := keep(c.ID, d.USDC, smartaccount.Checksum(payee), amount, quote.Int64(), d.Paymaster)
		if err := c.SignPaymentAsAgent(ctx, d.Signers, d.Account, d.AgentKey, &op, func(smartaccount.Payment) error { return nil }); err != nil {
			t.Fatal(err)
		}
		return use, op, quote.Int64()
	}
	// settled runs the leader's job until use has an outcome: only finalized blocks count, 64 behind anvil's head.
	settled := func(use int64) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if err := rpc(c.RPCURL, "anvil_mine", "0x41"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SettleAgentCryptoPayments(ctx, c); err != nil {
				t.Fatal(err)
			}
			if _, _, _, o := read(use); o != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("payment %d was not settled from the chain", use)
	}

	// A bundler that refuses the operation: it is kept counted, as it could still reach the chain.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32500, "message": "refused"}})
	}))
	t.Cleanup(refusing.Close)
	refused, refusedOp, refusedQuote := pay(100_000)
	other := refusedOp
	other.Sender = payee
	if _, err := store.SendAgentCryptoPayment(ctx, c, ws, agent, refused, other); err == nil {
		t.Fatal("an operation out of another account was kept as the agent's payment")
	}
	down := c
	down.BundlerURL = refusing.URL
	if _, err := store.SendAgentCryptoPayment(ctx, down, ws, agent, refused, refusedOp); err == nil {
		t.Fatal("a payment the bundler refused was answered sent")
	}
	if _, err := store.SettleAgentCryptoPayments(ctx, c); err != nil {
		t.Fatal(err)
	}
	if a, _, _, o := read(refused); a != 100_000+refusedQuote || o != nil {
		t.Fatalf("a refused operation whose nonce is unused counts %d (%v), want %d: it could still land", a, o, 100_000+refusedQuote)
	}

	first, firstOp, firstQuote := pay(250_000) // the refused operation's nonce
	if _, err := store.SendAgentCryptoPayment(ctx, c, ws, agent, first, firstOp); err != nil {
		t.Fatal(err)
	}
	settled(first)
	tookFirst := d.Tokens(t, d.Paymaster)
	if a, ch, l, o := read(first); o == nil || *o != "paid" || n(ch) != tookFirst || n(l) != firstQuote-tookFirst || a != 250_000+firstQuote {
		t.Fatalf("the first payment settled counts %d (%d charged, %d left, %v); want paid, %d charged, the rest of the quote still approved",
			a, n(ch), n(l), o, tookFirst)
	}
	settled(refused)
	if a, _, _, o := read(refused); a != 0 || o == nil || *o != "dropped" {
		t.Fatalf("the refused operation, its nonce used by another, counts %d (%v); want 0, dropped", a, o)
	}

	second, secondOp, _ := pay(300_000)
	if _, err := store.SendAgentCryptoPayment(ctx, c, ws, agent, second, secondOp); err != nil {
		t.Fatal(err)
	}
	settled(second)
	tookSecond := d.Tokens(t, d.Paymaster) - tookFirst
	left, err := c.FeeApprovalLeft(ctx, secondOp)
	if err != nil {
		t.Fatal(err)
	}
	if a, _, l, _ := read(first); a != 250_000+tookFirst || n(l) != 0 {
		t.Fatalf("the first payment counts %d with %d left once its approval was replaced; want %d: what the paymaster took, not the quote",
			a, n(l), 250_000+tookFirst)
	}
	if a, ch, l, _ := read(second); n(ch) != tookSecond || n(l) != left.Int64() || a != 300_000+tookSecond+left.Int64() {
		t.Fatalf("the second payment counts %d (%d charged, %d left); want %d charged and the %s approved now", a, n(ch), n(l), tookSecond, left)
	}
	// The allowance the gate reads is the receipts' figures.
	var counted int64
	if err := pool.QueryRow(ctx, `SELECT sum(amount) FROM agent_spend_permission_uses WHERE agent_id = $1`, agent).Scan(&counted); err != nil {
		t.Fatal(err)
	}
	if want := 550_000 + tookFirst + tookSecond + left.Int64(); counted != want || d.Tokens(t, payee) != 550_000 {
		t.Fatalf("the allowance counts %d, want %d; the payee holds %d", counted, want, d.Tokens(t, payee))
	}
}

// rpc calls method on the node at url.
func rpc(url, method string, params ...any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func transferCall(t *testing.T, to string, amount int64) []byte {
	t.Helper()
	b, err := smartaccount.TransferCall(to, big.NewInt(amount))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
