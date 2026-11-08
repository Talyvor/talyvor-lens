package smartaccount

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
)

// B48.24 — on a local devnet, the person's one passkey signature takes the agent's key out of the account's owners and
// approves its spend permissions in Coinbase's SpendPermissionManager; the agent pays inside them from its own account,
// through Talyvor's gate and the provider. Then, with the gate bypassed: a payment over the cap on one payment or to a
// blocked counterparty is refused by the provider's policy engine, and — with the provider's policy bypassed too, its
// enclave signing a bare hash — one over the period's allowance, in a token not allowed or after the expiry is
// reverted on chain, and nothing moves.
func TestAgentSmartAccountOnDevnetSpendPermission(t *testing.T) {
	dir := contracts(t)
	ctx := context.Background()
	c := bundle(t, anvil(t, 31341))
	factory := deploy(t, c, dir)
	vw, _ := addressW(create2(t, c, creationCode(t, dir, "PublicERC6492Validator")))
	c.SpendManager = create2(t, c, append(creationCode(t, dir, "SpendPermissionManager"), tuple{vw, word{}}.enc()...))
	usdc, other := mockToken(t, c, dir, "USD Coin", "USDC", 6), mockToken(t, c, dir, "Other", "OTH", 6)

	// The agent's own account's calls, as one user operation signed by its key through sign.
	spenderOp := func(owner string, calls []money.Call) UserOp {
		t.Helper()
		sa, err := SpenderAccount(factory, owner)
		if err != nil {
			t.Fatal(err)
		}
		items := make([]batchItem, len(calls))
		for i, cl := range calls {
			v, _ := new(big.Int).SetString(cl.Value, 10)
			d, err := hex.DecodeString(strings.TrimPrefix(cl.Data, "0x"))
			if err != nil || v == nil {
				t.Fatalf("call %d: %+v", i, cl)
			}
			items[i] = batchItem{cl.To, v, d}
		}
		data, err := executeBatch(items...)
		if err != nil {
			t.Fatal(err)
		}
		op, err := c.Prepare(ctx, sa, data)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	landed := func(op UserOp) (bool, error) {
		hash, err := c.Send(ctx, op)
		if err != nil {
			return false, err
		}
		r, err := c.Receipt(ctx, hash)
		if err != nil || r == nil {
			t.Fatalf("operation %s: %+v %v", hash, r, err)
		}
		return r.Success, nil
	}

	cdp := cdpfake.New(t)
	cdp.SmartAddress = func(owner string) string { a, _ := SpenderAccount(factory, owner); return a.Address }
	cdp.Land = func(account, owner string, calls []money.Call, sign func([32]byte) []byte) (string, error) {
		op := spenderOp(owner, calls)
		h, _ := op.Hash(c.ID)
		op.Signature, _ = KeySignature(0, sign(h))
		hash, err := c.Send(ctx, op)
		if err != nil {
			return "", err
		}
		if r, err := c.Receipt(ctx, hash); err != nil || r == nil || !r.Success {
			return "", fmt.Errorf("the chain reverted operation %s", hash)
		}
		return hash, nil
	}
	signers, spenders := cdp.Signers(t), cdp.Spenders(t)
	agentKey, err := signers.NewSigner(ctx, "agent_signer:agt_spend", "agt_spend")
	if err != nil {
		t.Fatal(err)
	}
	spender, err := spenders.NewSpender(ctx, "spender:agt_spend", agentKey)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := SpenderAccount(factory, agentKey); spender != want.Address {
		t.Fatalf("the provider made the agent's account at %s, the factory puts it at %s", spender, want.Address)
	}
	passkey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	acct, err := New(factory, &passkey.PublicKey, agentKey, "")
	if err != nil {
		t.Fatal(err)
	}
	// shortcut: both accounts pay their own gas; B48.5's paymaster pays it instead.
	for _, a := range []string{acct.Address, spender} {
		if err := c.rpc(ctx, c.RPCURL, "anvil_setBalance", []any{a, "0xde0b6b3a7640000"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	mint(t, c, usdc, acct.Address, big.NewInt(1_000_000))
	mint(t, c, other, acct.Address, big.NewInt(1_000_000))
	tokens := func(token, addr string) int64 {
		t.Helper()
		w, _ := addressW(addr)
		out, err := ethCall(c, token, call("balanceOf(address)", w))
		if err != nil {
			t.Fatal(err)
		}
		return new(big.Int).SetBytes(out).Int64()
	}

	const payee, banned = "0x000000000000000000000000000000000000b0b0", "0x000000000000000000000000000000000000BAD0"
	perm := Permission{Tokens: []TokenLimit{{Token: usdc, Allowance: 10_000, Period: PeriodDay, PerPayment: 3_000},
		{Token: Native, Allowance: 10_000, Period: PeriodDay, PerPayment: 3_000}},
		Blocked: []string{banned}, Expires: time.Now().Add(time.Hour)}
	perm.Normalise()
	if err := perm.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	sps, err := perm.OnChain(acct.Address, spender, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	spOf := map[string]SpendPermission{}
	for _, sp := range sps {
		spOf[sp.Token] = sp
	}

	// The person's one passkey signature: the manager in, the permissions approved, the agent's key out.
	grant, err := c.GrantCall(acct, true, nil, sps)
	if err != nil {
		t.Fatal(err)
	}
	op, err := c.Prepare(ctx, acct, grant)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := op.Hash(c.ID)
	if err := c.SignAsOwner(&op, webAuthn(t, passkey, h)); err != nil {
		t.Fatal(err)
	}
	if ok, err := landed(op); err != nil || !ok {
		t.Fatalf("the owner's grant: %v %v", ok, err)
	}
	if err := spenders.SetSpendPolicy(ctx, "policy:agt_spend:1", agentKey, c.ProviderPolicy(perm)); err != nil {
		t.Fatal(err)
	}

	// The agent's key is no longer an owner: a payment straight out of the person's account that it signs is refused.
	tw, _ := addressW(payee)
	direct, _ := Execute(usdc, nil, call("transfer(address,uint256)", tw, uintW(1)))
	op, err = c.Prepare(ctx, acct, direct)
	if err != nil {
		t.Fatal(err)
	}
	h, _ = op.Hash(c.ID)
	sig, err := signers.SignHash(ctx, agentKey, h)
	if err != nil {
		t.Fatal(err)
	}
	op.Signature, _ = KeySignature(OwnerAgent, sig)
	if _, err := c.Send(ctx, op); !errors.Is(err, ErrRefused) {
		t.Fatalf("the agent's key signing for the person's account after the grant: %v, want refused on chain", err)
	}

	pay := func(token, to string, amount int64) []money.Call {
		t.Helper()
		sp, ok := spOf[token]
		if !ok {
			sp = spOf[usdc]
			sp.Token = token
		}
		calls, err := c.SpendCalls(sp, to, big.NewInt(amount))
		if err != nil {
			t.Fatal(err)
		}
		return calls
	}
	n := 0
	key := func() string { n++; return fmt.Sprint("spend:agt_spend:", n) }

	// Inside the permission, through Talyvor's gate and the provider: it lands.
	var spent int64
	gate := func(p Payment) error {
		if err := perm.Check(p, spent, time.Now()); err != nil {
			return err
		}
		spent += p.Amount.Int64()
		return nil
	}
	if _, err := c.SendSpendAsAgent(ctx, spenders, key(), spender, pay(usdc, payee, 1000), gate); err != nil {
		t.Fatalf("a payment inside the permission: %v", err)
	}
	if got := tokens(usdc, payee); got != 1000 {
		t.Fatalf("the payee holds %d, not 1000", got)
	}
	if _, err := c.SendSpendAsAgent(ctx, spenders, key(), spender, []money.Call{{To: usdc, Value: "0", Data: hexBytes(call("transfer(address,uint256)", tw, uintW(1)))}}, gate); !errors.Is(err, ErrPermission) {
		t.Fatalf("calls that are not a spend: %v, want refused at the gate", err)
	}

	// The gate bypassed: the provider's policy engine refuses a payment over the cap or to a blocked counterparty.
	refused := cdp.Refused()
	for name, calls := range map[string][]money.Call{"over the cap on one payment": pay(usdc, payee, 3001), "to a blocked counterparty": pay(usdc, banned, 10),
		"of the coin over its cap": pay(Native, payee, 3001)} {
		if _, err := spenders.SendCalls(ctx, key(), spender, calls); !errors.Is(err, money.ErrRefused) {
			t.Fatalf("a payment %s: %v, want refused by the provider", name, err)
		}
		refused++
		if cdp.Refused() != refused {
			t.Fatalf("a payment %s was not refused by the provider's policy engine", name)
		}
	}

	// The gate and the provider's policy bypassed — its enclave signs a bare hash — the chain holds the rest.
	bare := func(calls []money.Call) bool {
		t.Helper()
		op := spenderOp(agentKey, calls)
		h, _ := op.Hash(c.ID)
		sig, err := signers.SignHash(ctx, agentKey, h)
		if err != nil {
			t.Fatal(err)
		}
		op.Signature, _ = KeySignature(0, sig)
		ok, err := landed(op)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for range 3 { // 1000 + 3 × 3000: the day's allowance spent
		if !bare(pay(usdc, payee, 3000)) {
			t.Fatal("a payment inside the day's allowance was reverted")
		}
	}
	if bare(pay(usdc, payee, 1)) {
		t.Fatal("a payment over the day's allowance landed on chain")
	}
	if bare(pay(other, payee, 1)) {
		t.Fatal("a payment in a token not allowed landed on chain")
	}
	if !bare(pay(Native, payee, 1000)) {
		t.Fatal("a coin payment inside its allowance was reverted")
	}
	if err := c.rpc(ctx, c.RPCURL, "evm_increaseTime", []any{2 * 3600}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.rpc(ctx, c.RPCURL, "evm_mine", nil, nil); err != nil {
		t.Fatal(err)
	}
	if bare(pay(Native, payee, 1000)) {
		t.Fatal("a payment after the permission's expiry landed on chain")
	}
	if got, coin := tokens(usdc, payee), balance(t, c, payee); got != 10_000 || coin.Int64() != 1000 || tokens(other, payee) != 0 {
		t.Fatalf("the payee holds %d USDC, %d of the other token and %s wei; want 10000, 0 and 1000", got, tokens(other, payee), coin)
	}
}
