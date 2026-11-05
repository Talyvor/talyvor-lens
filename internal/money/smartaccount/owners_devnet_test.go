package smartaccount

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
)

// B48.15 — on a local devnet: the person adds a recovery signer with their passkey, and when the passkey is lost the
// recovery signer puts a new one in its place, after which the lost one is refused and the new one pays; the agent's
// key hands its place to the provider's next key, after which the old key is refused even with Talyvor's gate
// bypassed; a lockdown has every agent of the person's take its own key out, one account deployed and one not yet;
// and the person's one tap sweeps everything an account holds, a token and the coin, into their own wallet.
func TestAgentSmartAccountOnDevnetRecoveryRotationLockdownSweep(t *testing.T) {
	dir := contracts(t)
	ctx := context.Background()
	a := bundle(t, anvil(t, 31340))
	factory := deploy(t, a, dir)
	cdp := cdpfake.New(t)
	signers := cdp.Signers(t)
	newKey := func(name string) string {
		t.Helper()
		k, err := signers.NewSigner(ctx, "agent_signer:"+name, "agt_b4815")
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	fund := func(addr string) {
		t.Helper()
		if err := a.rpc(ctx, a.RPCURL, "anvil_setBalance", []any{addr, "0xde0b6b3a7640000"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	must := func(data []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	prepare := func(acct Account, data []byte) UserOp {
		t.Helper()
		op, err := a.Prepare(ctx, acct, data)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	land := func(op UserOp) {
		t.Helper()
		hash, err := a.Send(ctx, op)
		if err != nil {
			t.Fatal(err)
		}
		if r, err := a.Receipt(ctx, hash); err != nil || r == nil || !r.Success {
			t.Fatalf("operation %s: %+v %v", hash, r, err)
		}
	}
	refused := func(what string, op UserOp) {
		t.Helper()
		if _, err := a.Send(ctx, op); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: %v, want refused on chain", what, err)
		}
	}
	byPasskey := func(op *UserOp, key *ecdsa.PrivateKey, index uint64) {
		t.Helper()
		h, _ := op.Hash(a.ID)
		var err error
		if op.Signature, err = PasskeySignature(index, webAuthn(t, key, h)); err != nil {
			t.Fatal(err)
		}
	}
	byProvider := func(op *UserOp, key string, index uint64) {
		t.Helper()
		h, _ := op.Hash(a.ID)
		sig, err := signers.SignHash(ctx, key, h)
		if err != nil {
			t.Fatal(err)
		}
		op.Signature, _ = KeySignature(index, sig)
	}
	const payee = "0x000000000000000000000000000000000000b0b0"
	pay := func(wei int64) ([]byte, error) { return Execute(payee, big.NewInt(wei), nil) }

	k1 := newKey("agt_b4815")
	lost, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	acct, err := New(factory, &lost.PublicKey, k1, "")
	if err != nil {
		t.Fatal(err)
	}
	fund(acct.Address)

	// The person adds a recovery signer with their passkey: the account puts it at its next index, 2.
	recovery, _ := secp256k1.GeneratePrivateKey()
	op := prepare(acct, must(AddRecoveryCall(acct, keyAddress(recovery))))
	byPasskey(&op, lost, OwnerPasskey)
	land(op)
	owner, _ := AddressOwner(keyAddress(recovery))
	if got, err := ethCall(a, acct.Address, call("ownerAtIndex(uint256)", uintW(2))); err != nil || !bytes.Equal(got, tuple{bytesV(owner)}.enc()) {
		t.Fatalf("owner 2 is %x (%v), not the recovery signer", got, err)
	}

	// The phone is lost: the recovery signer puts the new phone's passkey in its place, at index 3.
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	op = prepare(acct, must(RecoverCall(acct, &phone.PublicKey)))
	h, _ := op.Hash(a.ID)
	op.Signature, _ = KeySignature(2, keySign(recovery, h))
	land(op)
	acct.Passkey, _ = PasskeyOwner(&phone.PublicKey)
	acct.PasskeyIndex = 3
	op = prepare(acct, must(pay(100)))
	byPasskey(&op, lost, OwnerPasskey)
	refused("the lost passkey", op)
	byPasskey(&op, phone, 3)
	land(op)

	// Rotation: the agent's key hands its place to the provider's next key, which goes in at index 4.
	k2 := newKey("agt_b4815:4")
	op = prepare(acct, must(RetireCall(acct, k2)))
	if err := a.SignAsAgent(ctx, signers, acct, k1, &op); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a retirement signed as a payment: %v, want refused", err)
	}
	if err := a.SignRetirementAsAgent(ctx, signers, acct, k1, k2, &op); err != nil {
		t.Fatal(err)
	}
	land(op)
	op = prepare(acct, must(pay(200)))
	byProvider(&op, k1, OwnerAgent) // Talyvor's gate bypassed
	refused("the rotated-out key", op)
	acct.AgentKey, acct.AgentIndex = k2, 4
	if err := a.SignAsAgent(ctx, signers, acct, k1, &op); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the old key asked to sign: %v, want refused before the provider signs", err)
	}
	if err := a.SignAsAgent(ctx, signers, acct, k2, &op); err != nil {
		t.Fatal(err)
	}
	land(op)
	if got := balance(t, a, payee); got.Int64() != 300 {
		t.Fatalf("the payee holds %s wei, not 300", got)
	}

	// Lockdown: every agent of the person's takes its own key out — this one, and another not yet deployed on chain.
	k3 := newKey("agt_b4815_other")
	other, err := New(factory, &phone.PublicKey, k3, "")
	if err != nil {
		t.Fatal(err)
	}
	fund(other.Address)
	for _, x := range []struct {
		acct  Account
		key   string
		index uint64
	}{{acct, k2, 4}, {other, k3, OwnerAgent}} {
		op := prepare(x.acct, must(FreezeCall(x.acct)))
		if err := a.SignRetirementAsAgent(ctx, signers, x.acct, x.key, "", &op); err != nil {
			t.Fatal(err)
		}
		land(op)
		op = prepare(x.acct, must(pay(1)))
		byProvider(&op, x.key, x.index)
		refused("a key after the lockdown", op)
	}

	// Sweep: the person's one tap pays everything the account holds into their own wallet; gas comes from a deposit
	// at the EntryPoint, as B48.5's paymaster would pay it.
	usdc := mockToken(t, a, dir, "USD Coin", "USDC", 6)
	mint(t, a, usdc, acct.Address, big.NewInt(42_000000))
	aw, _ := addressW(acct.Address)
	var tx string
	if err := a.rpc(ctx, a.RPCURL, "eth_sendTransaction", []any{map[string]string{"from": funded, "to": EntryPoint,
		"value": "0xde0b6b3a7640000", "data": hexBytes(call("depositTo(address)", aw))}}, &tx); err != nil {
		t.Fatal(err)
	}
	held, err := a.Holdings(ctx, acct.Address)
	if err != nil {
		t.Fatal(err)
	}
	coin := new(big.Int).Set(held[0].Amount)
	const mine = "0x0000000000000000000000000000000000000E1F"
	op = prepare(acct, must(SweepCall(acct, mine, held)))
	byPasskey(&op, phone, 3)
	land(op)
	after, err := a.Holdings(ctx, acct.Address)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range after {
		if h.Amount.Sign() != 0 {
			t.Fatalf("after the sweep the account still holds %s of %q", h.Amount, h.Address)
		}
	}
	mw, _ := addressW(mine)
	got, err := ethCall(a, usdc, call("balanceOf(address)", mw))
	if err != nil || new(big.Int).SetBytes(got).Int64() != 42_000000 || balance(t, a, mine).Cmp(coin) != 0 || coin.Sign() <= 0 {
		t.Fatalf("the person's wallet holds %x USDC and %s wei, want 42000000 and %s", got, balance(t, a, mine), coin)
	}
}
