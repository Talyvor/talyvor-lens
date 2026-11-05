package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// crypto_travel_rule.go — B48.7: SCREENING AND THE TRAVEL RULE ON EVERY TRANSFER.
//
// Every USDC address an agent pays is screened, as its payee's name is, before anything is held: a sanctioned
// address, a mixer, a bridge into a privacy coin or an address linked to Russia or Belarus is refused (screenAddress).
// Every address a deposit comes from is screened before it posts, and a flagged one is quarantined for an operator
// (ChainIndexer.quarantined).
//
// A payment to an address another regulated provider holds carries the payer's and the payee's identity to that
// provider through the Travel Rule adapter, just before it is sent, and the record is kept (sendTravelRule). A payment
// that takes what is paid to a self-hosted address over SelfHostedProofAboveMinor waits until the owner has proved,
// once, that they hold its key, by signing AddressProofMessage with it (ProveAddressControl).

// SelfHostedProofAboveMinor is how much may be paid, in all, to a self-hosted address before the owner proves it is
// theirs: the EU's threshold for a transfer to one, €1,000 (Regulation
// 2023/1113, Art. 14(5)), counted one for one in micro-USDC: a dollar is worth less than a euro, so this asks at or
// below the threshold, never above it.
const SelfHostedProofAboveMinor int64 = 1000_000000

var (
	// ErrAddressProofNeeded: a payment above the threshold to a self-hosted address the owner has not proved is theirs.
	ErrAddressProofNeeded = errors.New("economy: more than 1,000 USDC in all to a self-hosted address waits until the owner proves, once, that they hold its key")
	// ErrAddressProof: a proof that is not a signature of the message by the payee's address.
	ErrAddressProof = errors.New("economy: the signature is not the payee's address signing its proof message")
)

// SetTravelRule is the Travel Rule adapter (B48.7). Until it is set the Test one answers, which knows no regulated
// provider, so every address is self-hosted.
//
// shortcut: set beside the registry rather than in it, as no live Travel Rule partner is chosen; move it into
// internal/partners' Registry, behind its clearances, when one is.
func (s *DualTokenStore) SetTravelRule(p partners.TravelRuleProvider) { s.travelRule = p }

func (s *DualTokenStore) travelRuleProvider() partners.TravelRuleProvider {
	if s.travelRule == nil {
		return &partners.TestTravelRuleProvider{}
	}
	return s.travelRule
}

// AddressProofMessage is what the owner signs with address's key to prove it is theirs.
func AddressProofMessage(workspaceID, network, address string) string {
	return "Talyvor: I hold the key of " + strings.ToLower(address) + " on " + network + ". Workspace " + workspaceID + "."
}

// screenAddress screens the USDC address p pays, before anything is held, and answers whether it is self-hosted.
func (s *DualTokenStore) screenAddress(ctx context.Context, p OutsidePayment, payee OutsidePayee) (selfHosted bool, err error) {
	if err := s.screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: p.WorkspaceID, ID: p.screenKey(), Capability: CapabilityPaymentsOut,
		Direction: "out", Counterparty: payee.Address, AmountMinor: p.AmountMinor, Currency: p.Currency, Funding: p.Funding}); err != nil {
		return false, err
	}
	vasp, err := s.travelRuleProvider().Provider(ctx, payee.Network, payee.Address)
	return vasp == "", err
}

// addressProofTx refuses p, to a self-hosted address, while the owner has not proved it is theirs and p takes what is
// paid, or on its way, to that address over SelfHostedProofAboveMinor — so the threshold is not dodged in pieces. It
// runs under the locks every payment out in the currency waits on, so two at once are counted one after the other.
func addressProofTx(ctx context.Context, tx pgx.Tx, p OutsidePayment, payee OutsidePayee) error {
	var paid int64
	var proved bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT sum(m.amount_minor) FROM money_payments m
			JOIN money_payees y ON y.id = m.payee_id AND y.workspace_id = m.workspace_id
			WHERE m.workspace_id = $1 AND y.network = $2 AND y.address = $3 AND m.currency = $4 AND m.status NOT IN ('failed', 'returned')), 0)::bigint,
		EXISTS (SELECT 1 FROM crypto_address_proofs WHERE workspace_id = $1 AND network = $2 AND address = $3)`,
		p.WorkspaceID, payee.Network, payee.Address, p.Currency).Scan(&paid, &proved); err != nil {
		return fmt.Errorf("economy: payment: %w", err)
	}
	if proved || paid+p.AmountMinor <= SelfHostedProofAboveMinor {
		return nil
	}
	return fmt.Errorf("%w: sign %q with %s's key (personal_sign) and POST the signature to /v1/money/payees/%s/address-proof",
		ErrAddressProofNeeded, AddressProofMessage(p.WorkspaceID, payee.Network, payee.Address), payee.Address, payee.ID)
}

// ProveAddressControl keeps the owner's proof that they hold the key of payeeID's address: sig, its personal_sign of
// AddressProofMessage. Proving it again changes nothing.
func (s *DualTokenStore) ProveAddressControl(ctx context.Context, workspaceID, payeeID string, sig []byte) error {
	p, err := s.outsidePayee(ctx, s.pool, workspaceID, payeeID, "")
	if err != nil {
		return err
	}
	if p.Address == "" {
		return fmt.Errorf("%w: %s is paid at a bank, not at an address", ErrPayee, p.Name)
	}
	msg := AddressProofMessage(workspaceID, p.Network, p.Address)
	if !strings.EqualFold(smartaccount.PersonalSigner(msg, sig), p.Address) {
		return fmt.Errorf("%w: sign %q", ErrAddressProof, msg)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO crypto_address_proofs (workspace_id, network, address, message, signature) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`, workspaceID, p.Network, p.Address, msg, sig)
	if err != nil {
		return fmt.Errorf("economy: address proof: %w", err)
	}
	return nil
}

// sendTravelRule sends the payer's and the payee's identity to the provider that holds payee's address, when one
// does, before p is sent from from's wallet — once under p's id however often p is sent — and keeps what it sent.
//
// shortcut: the originator is the workspace's name and the paying wallet; add the verified person's address or
// identity number once B30.4's verification keeps them.
func (s *DualTokenStore) sendTravelRule(ctx context.Context, p OutsidePayment, from payingAccount, payee OutsidePayee) error {
	tr := s.travelRuleProvider()
	vasp, err := tr.Provider(ctx, payee.Network, payee.Address)
	if err != nil || vasp == "" {
		return err
	}
	name, err := s.payerName(ctx, p.WorkspaceID)
	if err != nil {
		return err
	}
	t := partners.TravelRuleTransfer{ID: p.ID, Network: payee.Network, Asset: p.Currency, AmountMinor: p.AmountMinor,
		Originator:  partners.TravelRuleParty{Name: name, Account: from.partnerRef},
		Beneficiary: partners.TravelRuleParty{Name: payee.Name, Address: payee.Address, Country: payee.Country, Provider: vasp}}
	ref, err := tr.Send(partners.WithWorkspace(ctx, p.WorkspaceID), t)
	if err != nil {
		return err
	}
	originator, _ := json.Marshal(t.Originator)
	beneficiary, _ := json.Marshal(t.Beneficiary)
	_, err = s.pool.Exec(ctx, `INSERT INTO travel_rule_transfers (payment_id, workspace_id, provider, provider_ref, vasp, network, asset, amount_minor,
		originator, beneficiary) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (payment_id) DO NOTHING`,
		p.ID, p.WorkspaceID, tr.Name(), ref, vasp, payee.Network, p.Currency, p.AmountMinor, originator, beneficiary)
	return err
}
