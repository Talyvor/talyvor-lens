package partners

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// TransferHooks is what each leg of an Instant transfer asks outside the provider that moves its money (B59.13): the
// beneficiary screened, a wallet address screened, the originator's and beneficiary's details carried with an on-chain
// move, and a record kept of the leg however it ended. The money layer asks them; the provider never does.
type TransferHooks interface {
	// ScreenBeneficiary screens who a leg pays.
	ScreenBeneficiary(ctx context.Context, req NameScreen) (Screening, error)
	// ScreenAddress screens a wallet address on network a leg pays.
	ScreenAddress(ctx context.Context, network, address string) (Screening, error)
	// OnchainDetails carries t's originator and beneficiary with an on-chain move, once under t.ID, and answers its
	// reference: to the beneficiary's provider when a regulated one holds the address, kept on file when it is
	// self-hosted.
	OnchainDetails(ctx context.Context, t TravelRuleTransfer) (string, error)
	// RecordLeg keeps l.
	RecordLeg(ctx context.Context, l LegRecord) error
}

// LegRecord is one leg of an Instant transfer, as it ended.
type LegRecord struct {
	Workspace string `json:"workspace_id"`
	Transfer  string `json:"transfer_id"` // the Instant transfer the leg is part of
	Leg       string `json:"leg"`         // the money layer's capability: convert, onchain or bank_payout
	Method    string `json:"method"`
	Key       string `json:"key"`
	Partner   string `json:"partner"`
	Ref       string `json:"partner_ref,omitempty"`
	Status    string `json:"status,omitempty"`
	Outcome   string `json:"outcome"` // ok, or the kind of its error
	Amount    Money  `json:"amount"`
}

// TestTransferHooks is the Sandbox's transfer hooks: Screening screens beneficiaries and addresses (nil is
// TestScreeningProvider{}, the test words alone), TravelRule says which addresses a regulated provider holds and takes
// the details sent to it (nil is every address self-hosted), and the details and the leg records are kept in memory.
type TestTransferHooks struct {
	Screening  ScreeningProvider
	TravelRule TravelRuleProvider

	mu      sync.Mutex
	details []TravelRuleTransfer
	legs    []LegRecord
}

func (h *TestTransferHooks) screening() ScreeningProvider {
	if h.Screening == nil {
		return TestScreeningProvider{}
	}
	return h.Screening
}

// ScreenBeneficiary screens the beneficiary's name.
func (h *TestTransferHooks) ScreenBeneficiary(ctx context.Context, req NameScreen) (Screening, error) {
	return h.screening().ScreenName(ctx, req)
}

// ScreenAddress screens the address as a name: a TestScreeningProvider's flagged addresses are hits.
func (h *TestTransferHooks) ScreenAddress(ctx context.Context, _, address string) (Screening, error) {
	if strings.TrimSpace(address) == "" {
		return Screening{}, fmt.Errorf("%w: an address screening names the address", ErrInvalid)
	}
	return h.screening().ScreenName(ctx, NameScreen{Name: address})
}

// OnchainDetails sends t to the provider TravelRule names for its beneficiary's address, when there is one, and keeps
// it either way.
func (h *TestTransferHooks) OnchainDetails(ctx context.Context, t TravelRuleTransfer) (string, error) {
	if err := checkID(t.ID); err != nil {
		return "", err
	}
	if strings.TrimSpace(t.Originator.Name) == "" || strings.TrimSpace(t.Beneficiary.Name) == "" {
		return "", fmt.Errorf("%w: an on-chain move names its originator and its beneficiary", ErrInvalid)
	}
	ref := "tr_kept_" + t.ID
	if h.TravelRule != nil && t.Beneficiary.Address != "" {
		provider, err := h.TravelRule.Provider(ctx, t.Network, t.Beneficiary.Address)
		if err != nil {
			return "", err
		}
		if provider != "" {
			t.Beneficiary.Provider = provider
			if ref, err = h.TravelRule.Send(ctx, t); err != nil {
				return "", err
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.details {
		if d.ID == t.ID {
			return ref, nil
		}
	}
	h.details = append(h.details, t)
	return ref, nil
}

// RecordLeg keeps l.
func (h *TestTransferHooks) RecordLeg(_ context.Context, l LegRecord) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.legs = append(h.legs, l)
	return nil
}

// Details is every on-chain move's details, in order.
func (h *TestTransferHooks) Details() []TravelRuleTransfer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]TravelRuleTransfer(nil), h.details...)
}

// Legs is every leg recorded, in order.
func (h *TestTransferHooks) Legs() []LegRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]LegRecord(nil), h.legs...)
}
