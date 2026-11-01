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
// the chain's coin.
type GasBudget interface {
	// Hold sets fee aside for operation hash, or refuses with an error wrapping ErrGasBudgetSpent when what is left
	// cannot cover it. Holding the same hash again holds nothing more.
	Hold(ctx context.Context, hash string, fee *big.Int) error
	// Release frees hash's hold: the operation never reached the chain.
	Release(ctx context.Context, hash string) error
	// Charge settles hash's hold at charged, what the chain took for it, and frees the rest. Charging it again
	// changes nothing.
	Charge(ctx context.Context, hash string, charged *big.Int) error
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

// SendSponsored holds op's fee in b and hands op to the bundler, answering its hash; an operation the bundler
// refuses releases its hold. op is sponsored (Sponsor) and signed.
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
	charged, ok := new(big.Int).SetString(strings.TrimPrefix(r.ActualGasCost, "0x"), 16)
	if !ok || !strings.HasPrefix(r.ActualGasCost, "0x") {
		return nil, fmt.Errorf("smartaccount: the bundler answered no actualGasCost for %s", hash)
	}
	return r, b.Charge(ctx, hash, charged)
}
