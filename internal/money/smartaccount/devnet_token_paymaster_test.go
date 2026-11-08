package smartaccount

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
)

// devnet_token_paymaster_test.go — B48.25 on the local devnet: a local ERC-20 paymaster (scripts/devnet-contracts.sh's
// TokenPaymaster) deployed and funded at the EntryPoint, and a local ERC-7677 paymaster service in front of it that, for
// a context naming USDC, signs what it sponsors at a fixed price of USDC per ETH. Its name extends
// TestAgentSmartAccountOnDevnet's, so CI's devnet job runs it too.

// usdcPerETH is the local service's price: 3,000 USDC (6 decimals) per 1e18 wei.
var usdcPerETH = big.NewInt(3_000_000_000)

// tokenSponsorer is the local ERC-20 paymaster service: pm_getPaymasterStubData answers its price with a placeholder
// signature, pm_getPaymasterData the same price signed over the operation.
func tokenSponsorer(t *testing.T, c Chain, paymaster, usdc string, key *secp256k1.PrivateKey) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		var op UserOp
		var pmContext struct {
			Token string `json:"token"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || !strings.HasPrefix(req.Method, "pm_getPaymaster") || len(req.Params) != 4 ||
			json.Unmarshal(req.Params[0], &op) != nil || json.Unmarshal(req.Params[3], &pmContext) != nil || !strings.EqualFold(pmContext.Token, usdc) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "not offered"}})
			return
		}
		pw, _ := addressW(paymaster)
		tw, _ := addressW(usdc)
		sig := make([]byte, 65)
		if req.Method == "pm_getPaymasterData" {
			h, err := ethCall(c, paymaster, call("getHash("+opType+",uint48,uint48,address,uint256)", opTuple(op), uintW(0), uintW(0), tw, bigW(usdcPerETH)))
			if err != nil || len(h) != 32 {
				t.Errorf("getHash: %v", err)
				return
			}
			sig = keySign(key, keccak([]byte("\x19Ethereum Signed Message:\n32"), h))
		}
		data := append(append(append([]byte{}, pw[12:]...), tuple{uintW(0), uintW(0), tw, bigW(usdcPerETH)}.enc()...), sig...)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]string{"paymasterAndData": hexBytes(data)}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// B48.25 — on a network where no one sponsors its fees, an agent whose account holds no ETH sends USDC and pays the
// network fee in USDC to an ERC-20 paymaster: the fee is quoted in USDC before the agent's key signs and counted with the
// payment against the owner's permission, the paymaster takes no more than quoted, and the fee read back from the chain
// (what the agent's history shows) is exactly what the paymaster took.
func TestAgentSmartAccountOnDevnetTokenPaymaster(t *testing.T) {
	dir := contracts(t)
	ctx := context.Background()
	c := bundle(t, anvil(t, 31337))
	factory := deploy(t, c, dir)

	usdcCode := append(creationCode(t, dir, "ERC20"), tuple{bytesV("USD Coin"), bytesV("USDC")}.enc()...)
	usdc := Checksum(transact(t, c, "", usdcCode)["contractAddress"].(string))
	pmKey, _ := secp256k1.GeneratePrivateKey()
	ep, _ := addressW(EntryPoint)
	signer, _ := addressW(keyAddress(pmKey))
	paymaster := Checksum(transact(t, c, "", append(creationCode(t, dir, "TokenPaymaster"), tuple{ep, signer}.enc()...))["contractAddress"].(string))
	transactValue(t, c, paymaster, big.NewInt(1e18), call("deposit()"))
	c.PaymasterURL, c.TokenPaymaster = tokenSponsorer(t, c, paymaster, usdc, pmKey), paymaster

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
	transact(t, c, usdc, call("mint(address,uint256)", aw, uintW(100_000_000)))
	tokens := func(addr string) int64 {
		w, _ := addressW(addr)
		out, err := ethCall(c, usdc, call("balanceOf(address)", w))
		if err != nil {
			t.Fatal(err)
		}
		return new(big.Int).SetBytes(out).Int64()
	}

	pw, _ := addressW(payee)
	data, err := Execute(usdc, nil, call("transfer(address,uint256)", pw, uintW(250_000)))
	if err != nil {
		t.Fatal(err)
	}
	op, err := c.Prepare(ctx, acct, data)
	if err != nil {
		t.Fatal(err)
	}
	// B54.12: the paymaster's price is taken only within the chain's spread of an independent price of its coin.
	c.TokenPaymasterLayout = LayoutSignedPrice
	c.CoinPrice = func(context.Context, string) (*big.Rat, error) { return big.NewRat(2_900_000_000, 1), nil }
	c.TokenPriceSpreadBps = 300
	if _, err := c.SponsorInToken(ctx, &op, usdc); err == nil || !strings.Contains(err.Error(), "spread") {
		t.Fatalf("a paymaster's price beyond the spread: %v, want refused", err)
	}
	c.TokenPriceSpreadBps = 400 // 3,000 is 3.4% over 2,900
	op.CallData = data
	quote, err := c.SponsorInToken(ctx, &op, usdc)
	if err != nil {
		t.Fatal(err)
	}
	if want := new(big.Int).Div(new(big.Int).Add(new(big.Int).Mul(MaxFee(op), usdcPerETH), big.NewInt(1e18-1)), big.NewInt(1e18)); quote.Sign() <= 0 || quote.Cmp(want) != 0 {
		t.Fatalf("quoted %s USDC units, the operation's most at the paymaster's price is %s", quote, want)
	}

	// The owner's permission counts the fee with the payment: an allowance that covers the payment but not its most
	// possible fee refuses it before anything is signed; one that covers both lets the agent's key sign.
	now := time.Now()
	permit := func(allowance int64) func(Payment) error {
		p := Permission{Tokens: []TokenLimit{{Token: usdc, Allowance: allowance, Period: PeriodDay, PerPayment: allowance}}, Expires: now.Add(time.Hour)}
		return func(pay Payment) error {
			if pay.Fee == nil || pay.Fee.Cmp(quote) != 0 || pay.Paymaster != paymaster {
				t.Errorf("the gate was asked about %+v, want the fee %s to %s", pay, quote, paymaster)
			}
			return p.Check(pay, 0, now)
		}
	}
	if err := c.SignPaymentAsAgent(ctx, signers, acct, agentKey, &op, permit(250_000+quote.Int64()-1)); !errors.Is(err, ErrPermission) {
		t.Fatalf("a payment whose fee goes over the allowance: %v, want ErrPermission", err)
	}
	if err := c.SignPaymentAsAgent(ctx, signers, acct, agentKey, &op, permit(250_000+quote.Int64())); err != nil {
		t.Fatal(err)
	}
	hash, err := c.Send(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	var fee *big.Int
	for i := 0; fee == nil && i < 100; i++ {
		if fee, err = c.TokenFeeCharged(ctx, op, hash); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fee == nil {
		t.Fatal("the operation did not reach the chain")
	}
	if got := balance(t, c, acct.Address); got.Sign() != 0 {
		t.Fatalf("the agent's account holds %s wei: it was meant to need none", got)
	}
	took := tokens(paymaster)
	if tokens(payee) != 250_000 || fee.Int64() != took || took <= 0 || fee.Cmp(quote) > 0 || tokens(acct.Address) != 100_000_000-250_000-took {
		t.Fatalf("the payee holds %d, the paymaster took %d, the agent holds %d; the fee read back is %s, the quote %s",
			tokens(payee), took, tokens(acct.Address), fee, quote)
	}
	// B54.12: what the paymaster did not take stays approved to it, and the allowance counts it until it is replaced.
	if left, err := c.FeeApprovalLeft(ctx, op); err != nil || left.Int64() != quote.Int64()-took {
		t.Fatalf("still approved to the paymaster: %v, %v; want the quote %s less the %d taken", left, err, quote, took)
	}
}
