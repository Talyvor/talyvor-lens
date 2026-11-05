package smartaccount

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/talyvor/lens/internal/money/holdings"
)

// owners.go — B48.15: THE ACCOUNT'S OWNERS CHANGE. The person adds a recovery signer (AddRecoveryCall, signed by their
// passkey); when the passkey is lost, the recovery signer puts a new one in its place (RecoverCall, signed by the
// recovery signer); the agent's key is replaced or taken out by its own signature (RetireCall, SignRetirementAsAgent),
// on demand or for every agent at once in a lockdown, with no tap from a device the person may have lost; and "Sweep to
// me" pays everything the account holds to the person (SweepCall, signed by their passkey).
//
// The account adds an owner at its next index and never uses a removed index again, so an owner is named by its index
// as well as its key (Account.PasskeyIndex, Account.AgentIndex).

// agent is the agent's key now, as an owner, and its index.
func (a Account) agent() ([]byte, uint64, error) {
	if a.AgentKey != "" {
		o, err := AddressOwner(a.AgentKey)
		if err != nil || a.AgentIndex == OwnerPasskey {
			return nil, 0, fmt.Errorf("%w: the agent's key and its place", ErrInvalid)
		}
		return o, a.AgentIndex, nil
	}
	if len(a.Owners) <= OwnerAgent {
		return nil, 0, fmt.Errorf("%w: the account has no agent key", ErrInvalid)
	}
	return a.Owners[OwnerAgent], OwnerAgent, nil
}

// passkey is the person's passkey now, as an owner, and its index.
func (a Account) passkey() ([]byte, uint64, error) {
	if a.Passkey != nil {
		return a.Passkey, a.PasskeyIndex, nil
	}
	if len(a.Owners) == 0 {
		return nil, 0, fmt.Errorf("%w: the account has no passkey", ErrInvalid)
	}
	return a.Owners[OwnerPasskey], OwnerPasskey, nil
}

// batchItem is one call in an executeBatch.
type batchItem struct {
	target string
	value  *big.Int
	data   []byte
}

// executeBatch is the account's call of each item in turn: all of them happen, or none.
func executeBatch(items ...batchItem) ([]byte, error) {
	calls := make(array, len(items))
	for i, it := range items {
		t, err := addressW(it.target)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		calls[i] = tuple{t, bigW(it.value), bytesV(it.data)}
	}
	return call("executeBatch((address,uint256,bytes)[])", calls), nil
}

func removeOwner(index uint64, owner []byte) []byte {
	return call("removeOwnerAtIndex(uint256,bytes)", uintW(index), bytesV(owner))
}

// AddRecoveryCall is the account's call that adds recovery, an address only the person holds the key of, to its
// owners. The person's passkey signs it.
func AddRecoveryCall(a Account, recovery string) ([]byte, error) {
	w, err := addressW(recovery)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return Execute(a.Address, nil, call("addOwnerAddress(address)", w))
}

// RecoverCall is the account's call that puts passkey in the place of the person's passkey now, which they lost. The
// recovery signer signs it.
func RecoverCall(a Account, passkey *ecdsa.PublicKey) ([]byte, error) {
	next, err := PasskeyOwner(passkey)
	if err != nil {
		return nil, err
	}
	lost, index, err := a.passkey()
	if err != nil {
		return nil, err
	}
	if bytes.Equal(next, lost) {
		return nil, fmt.Errorf("%w: that is the account's passkey already", ErrInvalid)
	}
	var x, y word
	copy(x[:], next[:32])
	copy(y[:], next[32:])
	return executeBatch(batchItem{a.Address, nil, call("addOwnerPublicKey(bytes32,bytes32)", x, y)},
		batchItem{a.Address, nil, removeOwner(index, lost)})
}

// RetireCall is the account's call that takes the agent's key now out of its owners, after adding replacement in its
// place unless replacement is "".
func RetireCall(a Account, replacement string) ([]byte, error) {
	old, index, err := a.agent()
	if err != nil {
		return nil, err
	}
	if replacement == "" {
		return Execute(a.Address, nil, removeOwner(index, old))
	}
	w, err := addressW(replacement)
	if err != nil || bytes.Equal(w[:], old) {
		return nil, fmt.Errorf("%w: the agent's next key is another address", ErrInvalid)
	}
	return executeBatch(batchItem{a.Address, nil, call("addOwnerAddress(address)", w)}, batchItem{a.Address, nil, removeOwner(index, old)})
}

// SignRetirementAsAgent has the agent's key agentKey sign op, which must be exactly RetireCall(a, replacement): the one
// change to the account its key signs, since a key that takes itself out, or hands its place to the provider's next
// key, gains nothing. So a lockdown or a rotation needs no tap from the person.
func (c Chain) SignRetirementAsAgent(ctx context.Context, s Signer, a Account, agentKey, replacement string, op *UserOp) error {
	want, err := RetireCall(a, replacement)
	if err != nil {
		return err
	}
	if !bytes.Equal(op.CallData, want) {
		return fmt.Errorf("%w: this is not the agent's key's retirement", ErrInvalid)
	}
	return c.signAsAgent(ctx, s, a, agentKey, op)
}

// SweepCall is the account's call that pays everything in held — what it holds on one chain (Chain.Holdings) — to to,
// the person's own wallet: each token with a balance, then the chain's coin.
//
// shortcut: the whole coin is paid, so the operation's gas must come from a deposit at the EntryPoint or B48.5's
// paymaster; when the account pays its own gas, leave the gas out of held's coin.
func SweepCall(a Account, to string, held []holdings.Held) ([]byte, error) {
	tw, err := addressW(to)
	if err != nil || strings.EqualFold(to, a.Address) {
		return nil, fmt.Errorf("%w: a sweep pays the person's own wallet", ErrInvalid)
	}
	var items []batchItem
	var coin *big.Int
	for _, h := range held {
		switch {
		case h.Amount == nil || h.Amount.Sign() <= 0:
		case h.Address == "":
			coin = h.Amount
		default:
			items = append(items, batchItem{h.Address, nil, call("transfer(address,uint256)", tw, bigW(h.Amount))})
		}
	}
	if coin != nil {
		items = append(items, batchItem{to, coin, nil})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: the account holds nothing to sweep", ErrInvalid)
	}
	return executeBatch(items...)
}

// PersonalSigner is the address whose key made sig, a 65-byte r‖s‖v personal_sign (EIP-191) of message — how a
// recovery key in the person's own wallet signs a challenge; "" when sig is not one.
func PersonalSigner(message string, sig []byte) string {
	if len(sig) != 65 {
		return ""
	}
	v := sig[64]
	if v >= 27 {
		v -= 27
	}
	if v > 1 {
		return ""
	}
	h := keccak([]byte("\x19Ethereum Signed Message:\n"+strconv.Itoa(len(message))), []byte(message))
	pub, _, err := secpecdsa.RecoverCompact(append([]byte{27 + v}, sig[:64]...), h[:])
	if err != nil {
		return ""
	}
	a := keccak(pub.SerializeUncompressed()[1:])
	return checksum(a[12:])
}
