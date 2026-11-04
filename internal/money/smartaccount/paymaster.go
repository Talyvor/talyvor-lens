package smartaccount

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// paymaster.go — B48.5: an agent never needs the network's coin. A paymaster pays each of its operations' network fee
// — on Base the provider's, CDP's paymaster — asked through ERC-7677's pm_getPaymasterData, within the workspace's
// budget for sponsored fees (GasBudget). The most an operation's fee can be is quoted before anyone signs it, held
// from the budget when it is sent, and charged at what the chain charged once it is on the chain.

// ErrGasBudgetSpent: the workspace's budget for sponsored network fees cannot cover an operation's fee.
var ErrGasBudgetSpent = errors.New("smartaccount: the workspace's budget for network fees is used up")

// GasBudget is one workspace's budget for the network fees a paymaster pays for its agents on one chain, in wei of
// the chain's coin. Each operation is held under ref: its hash when that is known before it is sent, or else the
// sender's idempotency key for it.
type GasBudget interface {
	// Hold sets fee aside for operation ref, or refuses with an error wrapping ErrGasBudgetSpent when what is left
	// cannot cover it. Holding the same ref again holds nothing more.
	Hold(ctx context.Context, ref string, fee *big.Int) error
	// Sent records that ref is user operation hash, which may now reach the chain: from then on it is charged from its
	// receipt (FeeCharged), even once released.
	Sent(ctx context.Context, ref, hash string) error
	// Release frees ref's hold: the operation was refused.
	Release(ctx context.Context, ref string) error
	// Charge settles ref's hold at charged, what the chain took for it, and frees the rest. Charging it again
	// changes nothing.
	Charge(ctx context.Context, ref string, charged *big.Int) error
}

// Sponsor has the chain's paymaster pay op's network fee: it puts the paymaster's paymasterAndData on op and answers
// the most the fee can be, which the owner is shown before op is signed. op's gas fields are final: the paymaster
// signs over them.
func (c Chain) Sponsor(ctx context.Context, op *UserOp) (*big.Int, error) {
	if c.PaymasterURL == "" {
		return nil, fmt.Errorf("%w: chain %d has no paymaster", ErrInvalid, c.ID)
	}
	op.PaymasterAndData, op.Signature = nil, nil
	var out struct {
		PaymasterAndData string `json:"paymasterAndData"`
	}
	if err := c.rpc(ctx, c.PaymasterURL, "pm_getPaymasterData", []any{op, EntryPoint, quantity(big.NewInt(c.ID)), map[string]any{}}, &out); err != nil {
		return nil, err
	}
	pm, err := hex.DecodeString(strings.TrimPrefix(out.PaymasterAndData, "0x"))
	if err != nil || !strings.HasPrefix(out.PaymasterAndData, "0x") || len(pm) < 20 {
		return nil, fmt.Errorf("smartaccount: the paymaster answered no paymasterAndData")
	}
	op.PaymasterAndData = pm
	return MaxFee(*op), nil
}

// MaxFee is the most the EntryPoint can take for op's network fee, in wei: v0.6's required prefund, whose
// verification gas counts three times over when a paymaster pays.
func MaxFee(op UserOp) *big.Int {
	mul := big.NewInt(1)
	if len(op.PaymasterAndData) > 0 {
		mul = big.NewInt(3)
	}
	gas := new(big.Int).Mul(nz(op.VerificationGasLimit), mul)
	gas.Add(gas, nz(op.CallGasLimit)).Add(gas, nz(op.PreVerificationGas))
	return gas.Mul(gas, nz(op.MaxFeePerGas))
}

func nz(n *big.Int) *big.Int {
	if n == nil {
		return new(big.Int)
	}
	return n
}

// QuoteSponsored is the most a sponsored operation given Lens's gas limits can be charged at the node's gas price now:
// what is held from a workspace's budget for an operation whose gas fields its provider fills in.
//
// shortcut: Lens's fixed limits, far above a USDC transfer's, so the hold is generous and the charge is the receipt's;
// upgrade with the provider's own estimate when it answers one.
func (c Chain) QuoteSponsored(ctx context.Context) (*big.Int, error) {
	price, err := c.quantity(ctx, "eth_gasPrice", nil)
	if err != nil {
		return nil, err
	}
	return MaxFee(UserOp{CallGasLimit: callGas, VerificationGasLimit: verificationGas, PreVerificationGas: preVerification,
		MaxFeePerGas: price.Mul(price, big.NewInt(2)), PaymasterAndData: []byte{1}}), nil
}

// SendSponsored holds op's fee in b and hands op to the bundler, answering its hash; an operation the bundler
// refuses releases its hold, and is still charged if it reaches the chain after all. op is sponsored (Sponsor) and
// signed.
func (c Chain) SendSponsored(ctx context.Context, b GasBudget, op UserOp) (string, error) {
	if len(op.PaymasterAndData) == 0 {
		return "", fmt.Errorf("%w: the operation is not sponsored", ErrInvalid)
	}
	h, err := op.Hash(c.ID)
	if err != nil {
		return "", err
	}
	hash := hexBytes(h[:])
	if err := b.Hold(ctx, hash, MaxFee(op)); err != nil {
		return "", err
	}
	if err := b.Sent(ctx, hash, hash); err != nil {
		return "", err
	}
	sent, err := c.Send(ctx, op)
	if err != nil {
		if rerr := b.Release(ctx, hash); rerr != nil {
			return "", errors.Join(err, rerr)
		}
		return "", err
	}
	if !strings.EqualFold(sent, hash) {
		return "", fmt.Errorf("smartaccount: the bundler hashes the operation %s, Lens %s", sent, hash)
	}
	return hash, nil
}

// Settle charges b what the chain took for operation hash once it is on the chain, and answers its receipt: nil
// while it is not there yet.
func (c Chain) Settle(ctx context.Context, b GasBudget, hash string) (*Receipt, error) {
	r, err := c.Receipt(ctx, hash)
	if err != nil || r == nil {
		return r, err
	}
	charged, err := actualGasCost(r, hash)
	if err != nil {
		return nil, err
	}
	return r, b.Charge(ctx, hash, charged)
}

// FeeCharged is what the chain took for operation hash's network fee, from its bundler's receipt: nil while it is not
// on the chain.
func (c Chain) FeeCharged(ctx context.Context, hash string) (*big.Int, error) {
	r, err := c.Receipt(ctx, hash)
	if err != nil || r == nil {
		return nil, err
	}
	return actualGasCost(r, hash)
}

func actualGasCost(r *Receipt, hash string) (*big.Int, error) {
	charged, ok := new(big.Int).SetString(strings.TrimPrefix(r.ActualGasCost, "0x"), 16)
	if !ok || !strings.HasPrefix(r.ActualGasCost, "0x") {
		return nil, fmt.Errorf("smartaccount: the bundler answered no actualGasCost for %s", hash)
	}
	return charged, nil
}
