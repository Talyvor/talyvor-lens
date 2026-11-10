package partners

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// StablecoinPartner holds and sends USDC: the capabilities stablecoins and x402.
type StablecoinPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// Address is a deposit address on a network for the caller's id: the same id, the same address.
	Address(ctx context.Context, req AddressRequest) (Address, error)
	// Send sends USDC to an address. Idempotent on req.ID.
	Send(ctx context.Context, req StablecoinTransfer) (Result, error)
	// Status says where a transfer stands now.
	Status(ctx context.Context, ref string) (Result, error)
}

// AddressRequest asks for a deposit address.
type AddressRequest struct {
	ID      string `json:"id"` // the caller's id for the address: an account's or an agent's
	Network string `json:"network"`
}

// Address is a deposit address on a network.
type Address struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

// StablecoinTransfer sends USDC to an address.
type StablecoinTransfer struct {
	ID      string `json:"id"`   // the caller's id for the transfer
	From    string `json:"from"` // the sending account's address at the partner
	Network string `json:"network"`
	To      string `json:"to"`
	Amount  Money  `json:"amount"`
	// Originator and Beneficiary are the payer's and the payee's names, carried on a transfer over the Travel Rule
	// threshold (B30.33).
	Originator  string `json:"originator,omitempty"`
	Beneficiary string `json:"beneficiary,omitempty"`
}

// TestStablecoinPartner is test mode for USDC: it holds and sends none, and reaches no network. Its addresses are
// made up from the caller's id; transfers answer by their amount (see the package comment).
type TestStablecoinPartner struct {
	book testBook
}

// Name is "test".
func (*TestStablecoinPartner) Name() string { return "test" }

// Address makes up an address from the id and network. Nothing is listening at it.
func (*TestStablecoinPartner) Address(_ context.Context, req AddressRequest) (Address, error) {
	if err := checkID(req.ID); err != nil {
		return Address{}, err
	}
	if strings.TrimSpace(req.Network) == "" {
		return Address{}, fmt.Errorf("%w: an address is on a network", ErrInvalid)
	}
	sum := sha256.Sum256([]byte("talyvor-test-address\x00" + req.Network + "\x00" + req.ID))
	return Address{Network: req.Network, Address: "0x" + hex.EncodeToString(sum[:20])}, nil
}

// Send sends nothing: it answers by the amount.
func (p *TestStablecoinPartner) Send(_ context.Context, req StablecoinTransfer) (Result, error) {
	if err := checkMoney(req.Amount); err != nil {
		return Result{}, err
	}
	if req.Amount.Currency != "USDC" || strings.TrimSpace(req.To) == "" || strings.TrimSpace(req.Network) == "" {
		return Result{}, fmt.Errorf("%w: a transfer sends USDC to an address on a network", ErrInvalid)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("usdc", req.ID, req, byAmount(req.Amount))
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

// Status is where a transfer stands now.
func (p *TestStablecoinPartner) Status(_ context.Context, ref string) (Result, error) {
	return p.book.status("usdc", ref)
}
