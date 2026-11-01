package smartaccount

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
)

// devnet_paymaster_test.go — B48.5 on the local devnet: ERC-4337's VerifyingPaymaster deployed and funded at the
// EntryPoint, a local ERC-7677 paymaster service in front of it signing what it sponsors (as CDP's paymaster does on
// Base), and a local USDC. Its name extends TestAgentSmartAccountOnDevnet's, so CI's devnet job runs it too.

// sponsorer is the local paymaster service: it answers pm_getPaymasterData with the paymaster's signature over the
// operation, valid with no time limit.
func sponsorer(t *testing.T, c Chain, paymaster string, key *secp256k1.PrivateKey) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		var op UserOp
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Method != "pm_getPaymasterData" || len(req.Params) != 4 ||
			json.Unmarshal(req.Params[0], &op) != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "not offered"}})
			return
		}
		// getHash covers the operation's ABI head, which places the signature after paymasterAndData: it is asked with
		// a paymasterAndData as long as the one it answers.
		op.PaymasterAndData = make([]byte, 20+64+65)
		h, err := ethCall(c, paymaster, call("getHash("+opType+",uint48,uint48)", opTuple(op), uintW(0), uintW(0)))
		if err != nil || len(h) != 32 {
			t.Errorf("getHash: %v", err)
			return
		}
		signed := keccak([]byte("\x19Ethereum Signed Message:\n32"), h)
		pw, _ := addressW(paymaster)
		data := append(append(append([]byte{}, pw[12:]...), tuple{uintW(0), uintW(0)}.enc()...), keySign(key, signed)...)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]string{"paymasterAndData": hexBytes(data)}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// budget is a workspace's budget in memory: Lens's own is economy's, in Postgres.
type budget struct {
	mu      sync.Mutex
	limit   *big.Int
	held    map[string]*big.Int
	charged map[string]*big.Int
}

func (b *budget) used() *big.Int {
	n := new(big.Int)
	for _, m := range []map[string]*big.Int{b.held, b.charged} {
		for _, v := range m {
			n.Add(n, v)
		}
	}
	return n
}

func (b *budget) Hold(_ context.Context, hash string, fee *big.Int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held[hash] != nil || b.charged[hash] != nil {
		return nil
	}
	if left := new(big.Int).Sub(b.limit, b.used()); left.Cmp(fee) < 0 {
		return fmt.Errorf("%w: %s wei left, the fee may be %s", ErrGasBudgetSpent, left, fee)
	}
	b.held[hash] = fee
	return nil
}

func (b *budget) Release(_ context.Context, hash string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.held, hash)
	return nil
}

func (b *budget) Charge(_ context.Context, hash string, charged *big.Int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.charged[hash] == nil {
		delete(b.held, hash)
		b.charged[hash] = charged
	}
	return nil
}

// B48.5 — an agent whose account holds no ETH sends USDC: the paymaster pays its network fee, the fee it was quoted
// before it signed covers what the chain took, its workspace's budget is charged exactly what the paymaster paid, and
// a spent budget stops the next sponsored send before anything is signed or sent.
func TestAgentSmartAccountOnDevnetSponsored(t *testing.T) {
	dir := contracts(t)
	ctx := context.Background()
	c := bundle(t, anvil(t, 31337))
	factory := deploy(t, c, dir)

	usdcCode := append(creationCode(t, dir, "ERC20"), tuple{bytesV("USD Coin"), bytesV("USDC")}.enc()...)
	usdc := transact(t, c, "", usdcCode)["contractAddress"].(string)
	pmKey, _ := secp256k1.GeneratePrivateKey()
	ep, _ := addressW(EntryPoint)
	signer, _ := addressW(keyAddress(pmKey))
	paymaster := transact(t, c, "", append(creationCode(t, dir, "VerifyingPaymaster"), tuple{ep, signer}.enc()...))["contractAddress"].(string)
	transactValue(t, c, paymaster, big.NewInt(1e18), call("deposit()"))
	c.PaymasterURL = sponsorer(t, c, paymaster, pmKey)

	cdp := cdpfake.New(t)
	signers := cdp.Signers(t)
	agentKey, err := signers.NewSigner(ctx, "agent_signer:agt_1", "agt_1")
	if err != nil {
		t.Fatal(err)
	}
	passkey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	acct, err := New(factory, &passkey.PublicKey, agentKey, "")
	if err != nil {
		t.Fatal(err)
	}
	aw, _ := addressW(acct.Address)
	transact(t, c, usdc, call("mint(address,uint256)", aw, uintW(1_000_000)))

	payee := "0x000000000000000000000000000000000000b0b0"
	tokens := func(addr string) int64 {
		w, _ := addressW(addr)
		out, err := ethCall(c, usdc, call("balanceOf(address)", w))
		if err != nil {
			t.Fatal(err)
		}
		return new(big.Int).SetBytes(out).Int64()
	}
	deposit := func() *big.Int {
		out, err := ethCall(c, paymaster, call("getDeposit()"))
		if err != nil {
			t.Fatal(err)
		}
		return new(big.Int).SetBytes(out)
	}
	sponsored := func(amount uint64) (UserOp, *big.Int) {
		pw, _ := addressW(payee)
		data, err := Execute(usdc, nil, call("transfer(address,uint256)", pw, uintW(amount)))
		if err != nil {
			t.Fatal(err)
		}
		op, err := c.Prepare(ctx, acct, data)
		if err != nil {
			t.Fatal(err)
		}
		quote, err := c.Sponsor(ctx, &op)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SignAsAgent(ctx, signers, acct, agentKey, &op); err != nil {
			t.Fatal(err)
		}
		return op, quote
	}

	b := &budget{limit: big.NewInt(1e17), held: map[string]*big.Int{}, charged: map[string]*big.Int{}}
	op, quote := sponsored(250_000)
	if quote.Sign() <= 0 || quote.Cmp(MaxFee(op)) != 0 {
		t.Fatalf("quoted %s wei, the operation's most is %s", quote, MaxFee(op))
	}
	before := deposit()
	hash, err := c.SendSponsored(ctx, b, op)
	if err != nil {
		t.Fatal(err)
	}
	var r *Receipt
	for i := 0; r == nil && i < 100; i++ {
		if r, err = c.Settle(ctx, b, hash); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r == nil || !r.Success {
		t.Fatalf("the sponsored send: %+v", r)
	}
	if got := balance(t, c, acct.Address); got.Sign() != 0 {
		t.Fatalf("the agent's account holds %s wei: it was meant to need none", got)
	}
	if tokens(payee) != 250_000 || tokens(acct.Address) != 750_000 {
		t.Fatalf("the payee holds %d, the agent %d", tokens(payee), tokens(acct.Address))
	}
	paid := new(big.Int).Sub(before, deposit())
	if charged := b.charged[hash]; charged == nil || charged.Cmp(paid) != 0 || paid.Sign() <= 0 || paid.Cmp(quote) > 0 || len(b.held) != 0 {
		t.Fatalf("the paymaster paid %s wei, the budget was charged %v and still holds %v; the quote was %s", paid, charged, b.held, quote)
	}

	// The budget has less left than the next send's fee may be: it is refused before it is sent.
	b.limit = new(big.Int).Add(paid, big.NewInt(1))
	op, _ = sponsored(100_000)
	before = deposit()
	if _, err := c.SendSponsored(ctx, b, op); !errors.Is(err, ErrGasBudgetSpent) {
		t.Fatalf("a send over the budget: %v, want ErrGasBudgetSpent", err)
	}
	if tokens(payee) != 250_000 || deposit().Cmp(before) != 0 || len(b.held) != 0 {
		t.Fatalf("a send over the budget moved something: payee %d, deposit %s → %s, held %v", tokens(payee), before, deposit(), b.held)
	}
}
