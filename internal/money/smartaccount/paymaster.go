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
	pm, err := c.paymasterData(ctx, "pm_getPaymasterData", *op, map[string]any{})
	if err != nil {
		return nil, err
	}
	op.PaymasterAndData = pm
	return MaxFee(*op), nil
}

// paymasterData asks the chain's ERC-7677 paymaster method for op's paymasterAndData, with pmContext.
func (c Chain) paymasterData(ctx context.Context, method string, op UserOp, pmContext map[string]any) ([]byte, error) {
	var out struct {
		PaymasterAndData string `json:"paymasterAndData"`
	}
	if err := c.rpc(ctx, c.PaymasterURL, method, []any{op, EntryPoint, quantity(big.NewInt(c.ID)), pmContext}, &out); err != nil {
		return nil, err
	}
	pm, err := hex.DecodeString(strings.TrimPrefix(out.PaymasterAndData, "0x"))
	if err != nil || !strings.HasPrefix(out.PaymasterAndData, "0x") || len(pm) < 20 {
		return nil, fmt.Errorf("smartaccount: the paymaster answered no paymasterAndData")
	}
	return pm, nil
}

// B48.25 — on a chain where no one sponsors agents' fees (every EVM network but Base), an ERC-20 paymaster takes each
// operation's network fee in USDC from the agent's own account: Lens asks the chain's ERC-7677 paymaster with a context
// naming the token, and the paymaster's paymasterAndData is
//
//	paymaster (20 bytes) | abi.encode(uint48 validUntil, uint48 validAfter, address token, uint256 price) | signature
//
// price being the token's base units per 1e18 wei; once the operation has run, the paymaster takes
// ceil(actualGasCost × price / 1e18) of the token, which the operation approves it, from the account.

const tokenPaymasterHead = 20 + 4*32

// LayoutSignedPrice names the paymasterAndData layout above, which scripts/devnet-contracts.sh's TokenPaymaster reads.
// B54.12: a chain whose ERC-20 paymaster carries its price any other way is refused by its layout's name until Lens
// reads that one too.
const LayoutSignedPrice = "signed-price"

// tokenPrice reads the paymaster and its price in token from pm, refusing a price in any other token.
func tokenPrice(pm []byte, token string) (string, *big.Int, error) {
	if len(pm) < tokenPaymasterHead || !strings.EqualFold(checksum(pm[20+2*32+12:20+3*32]), token) {
		return "", nil, fmt.Errorf("smartaccount: the paymaster answered no price in %s", token)
	}
	price := new(big.Int).SetBytes(pm[20+3*32 : tokenPaymasterHead])
	if price.Sign() <= 0 {
		return "", nil, fmt.Errorf("smartaccount: the paymaster answered no price in %s", token)
	}
	return checksum(pm[:20]), price, nil
}

var wei = big.NewInt(1e18)

// SponsorInToken has the chain's ERC-20 paymaster take op's network fee in token, from the agent's account itself, and
// answers the most that fee can be in token's base units, which the owner is shown before op is signed. op is one
// payment in token (PaymentOf): it becomes that payment and token's approval of the paymaster for exactly that most, so
// the paymaster can never take more, and the owner's permission counts both (Payment.Total). op's gas fields are final.
// What the paymaster does not take stays approved to it until the next such payment replaces it: the permission counted
// it already.
func (c Chain) SponsorInToken(ctx context.Context, op *UserOp, token string) (*big.Int, error) {
	if c.PaymasterURL == "" || c.TokenPaymaster == "" {
		return nil, fmt.Errorf("%w: chain %d has no ERC-20 paymaster", ErrInvalid, c.ID)
	}
	if c.TokenPaymasterLayout != LayoutSignedPrice {
		return nil, fmt.Errorf("%w: chain %d's paymaster carries its price in the layout %q, which Lens does not read", ErrInvalid, c.ID, c.TokenPaymasterLayout)
	}
	if c.CoinPrice == nil {
		return nil, fmt.Errorf("%w: chain %d has no independent price to bound its paymaster's", ErrInvalid, c.ID)
	}
	pay, err := PaymentOf(op.CallData)
	if err != nil || pay.Fee != nil || !strings.EqualFold(pay.Token, token) || Checksum(token) == Checksum(Native) {
		return nil, fmt.Errorf("%w: the operation is not one payment in %s", ErrInvalid, token)
	}
	pmContext := map[string]any{"token": Checksum(token)}
	op.PaymasterAndData, op.Signature = nil, nil
	stub, err := c.paymasterData(ctx, "pm_getPaymasterStubData", *op, pmContext)
	if err != nil {
		return nil, err
	}
	paymaster, price, err := tokenPrice(stub, token)
	if err != nil {
		return nil, err
	}
	if paymaster != Checksum(c.TokenPaymaster) {
		return nil, fmt.Errorf("smartaccount: the paymaster service answered for %s, not chain %d's paymaster", paymaster, c.ID)
	}
	if strings.EqualFold(pay.To, paymaster) {
		return nil, fmt.Errorf("%w: a payment to the paymaster itself", ErrInvalid)
	}
	independent, err := c.CoinPrice(ctx, Checksum(token))
	if err != nil {
		return nil, err
	}
	// price ≤ independent × (1 + spread): the price the final data carries is no more than this one.
	most := new(big.Rat).Mul(independent, big.NewRat(10_000+max(c.TokenPriceSpreadBps, 0), 10_000))
	if independent.Sign() <= 0 || new(big.Rat).SetInt(price).Cmp(most) > 0 {
		return nil, fmt.Errorf("smartaccount: the paymaster's price %s in %s is beyond the spread of %d basis points over the independent %s",
			price, token, c.TokenPriceSpreadBps, independent.FloatString(0))
	}
	op.PaymasterAndData = stub // MaxFee counts the paymaster's verification three times
	fee := new(big.Int).Mul(MaxFee(*op), price)
	fee.Add(fee, new(big.Int).Sub(wei, big.NewInt(1))).Div(fee, wei)
	to, _ := addressW(pay.To)
	pmw, _ := addressW(paymaster)
	if op.CallData, err = executeBatch(batchItem{pay.Token, nil, call("transfer(address,uint256)", to, bigW(pay.Amount))},
		batchItem{pay.Token, nil, call("approve(address,uint256)", pmw, bigW(fee))}); err != nil {
		return nil, err
	}
	pm, err := c.paymasterData(ctx, "pm_getPaymasterData", *op, pmContext)
	if err != nil {
		return nil, err
	}
	final, finalPrice, err := tokenPrice(pm, token)
	if err != nil {
		return nil, err
	}
	if final != paymaster || finalPrice.Cmp(price) > 0 {
		return nil, fmt.Errorf("smartaccount: the paymaster's price moved from %s to %s in %s", price, finalPrice, token)
	}
	op.PaymasterAndData = pm
	return fee, nil
}

// TokenFeeCharged is what op's ERC-20 paymaster (SponsorInToken) took from the account for operation hash's network
// fee, read from the token's own Transfer logs of the operation's transaction, which the chain indexer also posts to
// the agent's history: nil while it is not on the chain.
func (c Chain) TokenFeeCharged(ctx context.Context, op UserOp, hash string) (*big.Int, error) {
	pay, err := PaymentOf(op.CallData)
	if err != nil || pay.Fee == nil {
		return nil, fmt.Errorf("%w: the operation's fee is not paid in a token", ErrInvalid)
	}
	r, err := c.Receipt(ctx, hash)
	if err != nil || r == nil {
		return nil, err
	}
	ts, mined, err := c.TxTransfers(ctx, r.Receipt.TransactionHash)
	if err != nil || !mined {
		return nil, err
	}
	took := new(big.Int)
	for _, t := range ts {
		if t.Token == pay.Token && strings.EqualFold(t.From, op.Sender) && t.To == pay.Paymaster {
			took.Add(took, t.Value)
		}
	}
	return took, nil
}

// FeeApprovalLeft is what op's account still approves op's ERC-20 paymaster in its fee's token now: what the paymaster
// did not take of the approval op made, or of a later one that replaced it, and could still take.
func (c Chain) FeeApprovalLeft(ctx context.Context, op UserOp) (*big.Int, error) {
	pay, err := PaymentOf(op.CallData)
	if err != nil || pay.Fee == nil {
		return nil, fmt.Errorf("%w: the operation's fee is not paid in a token", ErrInvalid)
	}
	owner, err := addressW(op.Sender)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	spender, _ := addressW(pay.Paymaster)
	data := call("allowance(address,address)", owner, spender)
	return c.quantity(ctx, "eth_call", []any{map[string]string{"to": pay.Token, "data": hexBytes(data)}, "latest"})
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
