package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// crypto_travel_rule.go — B48.7: SCREENING AND THE TRAVEL RULE ON EVERY TRANSFER.
//
// Every USDC address an agent pays, or earnings are paid out to, is screened, as its payee's name is, before anything
// is held: a sanctioned address, a mixer, a bridge into a privacy coin or an address linked to Russia or Belarus is
// refused (screenAddress). Every payment in, on every rail, is screened against who it is from before it posts, and a
// hit is quarantined for an operator rather than refused, with the owner told once: a webhook's (quarantineDeposit,
// posted once released by PostReleasedDeposits) and a deposit the chain indexer sees (ChainIndexer.quarantined).
//
// With the Travel Rule adapter set (SetTravelRule; LENS_TRAVEL_RULE), a payment to an address another regulated
// provider holds carries the payer's and the payee's identity to that provider just before it is sent, and the record
// is kept (sendTravelRule); one that keeps failing to go ends the payment as failed after TravelRuleMaxAttempts, and
// each retry screens the address again. A payment that takes what is paid to a self-hosted address over
// SelfHostedProofAboveMinor waits until the owner has proved, once, that they hold its key, by signing
// AddressProofMessage with it (ProveAddressControl, ProvePayoutAddress). Unset, the Travel Rule step is off: no
// provider is chosen, so nothing could tell an exchange's address from a self-hosted one.

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

// TravelRuleMaxAttempts is how many times a payment's Travel Rule data is tried before the payment ends as failed.
const TravelRuleMaxAttempts = 5

// SetTravelRule is the Travel Rule adapter (B48.7). Until it is set the Travel Rule step is off.
//
// shortcut: set beside the registry rather than in it, as no live Travel Rule partner is chosen; move it into
// internal/partners' Registry, behind its clearances, when one is.
func (s *DualTokenStore) SetTravelRule(p partners.TravelRuleProvider) { s.travelRule = p }

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
	if s.travelRule == nil {
		return false, nil
	}
	vasp, err := s.travelRule.Provider(ctx, payee.Network, payee.Address)
	return vasp == "", err
}

// addressProofTx refuses p, to a self-hosted address, while the owner has not proved it is theirs and p takes what is
// paid, or on its way, to that address over SelfHostedProofAboveMinor — so the threshold is not dodged in pieces. It
// runs under the locks every payment out in the currency waits on, so two at once are counted one after the other.
func addressProofTx(ctx context.Context, tx pgx.Tx, p OutsidePayment, payee OutsidePayee) error {
	var paid int64
	var proved bool
	// Each payment's own address, as it was paid: a payee's address can change (B41.112), what was paid to it cannot.
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT sum(amount_minor) FROM money_payments
			WHERE workspace_id = $1 AND address = $3 AND currency = $4 AND status NOT IN ('failed', 'returned')), 0)::bigint,
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

// travelRuleBeforeSend is the Travel Rule step before p is sent to payee's address. stop false: p goes on to be sent.
// stop true: it is not sent now — res and ok are sendOutside's answer: a failed result when a retry screens the
// address as refused, or once its Travel Rule data has failed TravelRuleMaxAttempts times; else ok false, tried again
// when p is read. Once p was handed to its rail (send_started_at) it may have gone (B54.17): the step is skipped, so
// the rail is asked under the same key and p never fails on the Travel Rule or screening.
func (s *DualTokenStore) travelRuleBeforeSend(ctx context.Context, p OutsidePayment, from payingAccount, payee OutsidePayee) (res partners.Result, ok, stop bool) {
	var attempts int
	var started bool
	if err := s.pool.QueryRow(ctx, `SELECT travel_rule_attempts, send_started_at IS NOT NULL FROM money_payments WHERE id = $1`,
		p.ID).Scan(&attempts, &started); err != nil {
		return partners.Result{}, false, true
	}
	if started {
		return partners.Result{}, false, false
	}
	var err error
	if attempts > 0 { // a retry screens the address again
		_, err = s.screenAddress(ctx, p, payee)
		if errors.As(err, new(*screening.Refusal)) {
			return partners.Result{Status: partners.StatusFailed, Detail: err.Error()}, true, true
		}
	}
	if err == nil {
		err = s.sendTravelRule(ctx, p.ID, p.WorkspaceID, p.Currency, p.AmountMinor, from.partnerRef, payee.Name, payee.Network, payee.Address, payee.Country)
	}
	if err == nil {
		return partners.Result{}, false, false
	}
	if qerr := s.pool.QueryRow(ctx, `UPDATE money_payments SET travel_rule_attempts = travel_rule_attempts + 1 WHERE id = $1
		RETURNING travel_rule_attempts`, p.ID).Scan(&attempts); qerr != nil {
		return partners.Result{}, false, true
	}
	slog.Warn("economy: the Travel Rule data was not sent; the payment waits and is sent again", "payment_id", p.ID, "attempt", attempts, "err", err)
	if attempts >= TravelRuleMaxAttempts {
		return partners.Result{Status: partners.StatusFailed,
			Detail: fmt.Sprintf("the identity data the Travel Rule needs could not be sent to the payee's provider after %d tries: %v", attempts, err)}, true, true
	}
	return partners.Result{}, false, true
}

// sendTravelRule sends the payer's and the payee's identity to the provider that holds address, when one does, before
// payment id is sent from the wallet fromRef — once under id however often it is sent — and keeps what it sent.
//
// shortcut: the originator is the workspace's name and the paying wallet; add the verified person's address or
// identity number once B30.4's verification keeps them.
func (s *DualTokenStore) sendTravelRule(ctx context.Context, id, workspaceID, asset string, amountMinor int64, fromRef, name, network, address, country string) error {
	tr := s.travelRule
	vasp, err := tr.Provider(ctx, network, address)
	if err != nil || vasp == "" {
		return err
	}
	payer, err := s.payerName(ctx, workspaceID)
	if err != nil {
		return err
	}
	t := partners.TravelRuleTransfer{ID: id, Network: network, Asset: asset, AmountMinor: amountMinor,
		Originator:  partners.TravelRuleParty{Name: payer, Account: fromRef},
		Beneficiary: partners.TravelRuleParty{Name: name, Address: address, Country: country, Provider: vasp}}
	ref, err := tr.Send(partners.WithWorkspace(ctx, workspaceID), t)
	if err != nil {
		return err
	}
	originator, _ := json.Marshal(t.Originator)
	beneficiary, _ := json.Marshal(t.Beneficiary)
	_, err = s.pool.Exec(ctx, `INSERT INTO travel_rule_transfers (payment_id, workspace_id, provider, provider_ref, vasp, network, asset, amount_minor,
		originator, beneficiary) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (payment_id) DO NOTHING`,
		id, workspaceID, tr.Name(), ref, vasp, network, asset, amountMinor, string(originator), string(beneficiary))
	return err
}

// earningsTravelRule is the Travel Rule step before earnings payout p is paid from the wallet fromRef to d's address:
// held is why p waits — while what is paid to d's recipient takes a self-hosted address over SelfHostedProofAboveMinor
// and the recipient has not proved they hold its key — and otherwise the identity data goes to the provider that holds
// it.
//
// shortcut: what is counted is every USDC payout to the recipient, whatever address it went to: more than the
// address's own, so it asks sooner, never later; count by address once payouts keep the address they were paid to.
func (s *DualTokenStore) earningsTravelRule(ctx context.Context, p EarningsPayout, d PayoutDetails, fromRef string) (held string, err error) {
	vasp, err := s.travelRule.Provider(ctx, d.Network, d.Address)
	if err != nil {
		return "", err
	}
	if vasp != "" {
		return "", s.sendTravelRule(ctx, p.ID, p.WorkspaceID, p.Currency, p.AmountMinor, fromRef, d.Holder, d.Network, d.Address, "")
	}
	var paid int64
	var proved bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT sum(amount_minor) FROM money_earnings_payouts WHERE recipient_workspace_id = $1
			AND currency = $2 AND id <> $3 AND status NOT IN ($6, $7)), 0)::bigint,
		EXISTS (SELECT 1 FROM crypto_address_proofs WHERE workspace_id = $1 AND network = $4 AND address = $5)`,
		p.RecipientWorkspaceID, p.Currency, p.ID, d.Network, d.Address, PaymentFailed, PaymentReturned).Scan(&paid, &proved); err != nil {
		return "", fmt.Errorf("economy: earnings payout: %w", err)
	}
	if proved || paid+p.AmountMinor <= SelfHostedProofAboveMinor {
		return "", nil
	}
	return fmt.Sprintf("held until %s proves it holds the key of %s: sign %q with it (personal_sign) and POST the signature to "+
		"/v1/money/payout-details/address-proof", p.RecipientWorkspaceID, d.Address, AddressProofMessage(p.RecipientWorkspaceID, d.Network, d.Address)), nil
}

// ProvePayoutAddress keeps workspaceID's proof that it holds the key of its USDC payout address — sig, its
// personal_sign of AddressProofMessage — and pays the earnings held for it.
func (s *DualTokenStore) ProvePayoutAddress(ctx context.Context, workspaceID string, sig []byte) error {
	var network, address string
	err := s.pool.QueryRow(ctx, `SELECT network, address FROM money_payout_details WHERE workspace_id = $1 AND asset = $2 AND address <> ''`,
		workspaceID, money.USDC).Scan(&network, &address)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the workspace has no USDC payout address", ErrPayoutDetails)
	}
	if err != nil {
		return fmt.Errorf("economy: address proof: %w", err)
	}
	msg := AddressProofMessage(workspaceID, network, address)
	if !strings.EqualFold(smartaccount.PersonalSigner(msg, sig), address) {
		return fmt.Errorf("%w: sign %q", ErrAddressProof, msg)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO crypto_address_proofs (workspace_id, network, address, message, signature) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`, workspaceID, network, address, msg, sig); err != nil {
		return fmt.Errorf("economy: address proof: %w", err)
	}
	s.payHeldEarnings(ctx, workspaceID, money.USDC)
	return nil
}

// ErrDepositQuarantined: a payment in held for an operator's review; it posts once its case is released.
var ErrDepositQuarantined = errors.New("economy: the payment in is quarantined until its compliance case is decided")

// quarantineDeposit keeps payment in, whose screening r holds for an operator, to post once r's case is released,
// and tells the owner the first time.
func (s *DualTokenStore) quarantineDeposit(ctx context.Context, workspaceID string, r *screening.Refusal, in InboundPayment) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO money_quarantined_deposits (workspace_id, partner_ref, case_id, partner, partner_account_ref, payer,
		reference, amount_minor, currency, funding) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT DO NOTHING`,
		workspaceID, in.PartnerRef, r.Case.ID, in.Partner, in.PartnerAccountRef, in.Payer, in.Reference, in.AmountMinor, in.Currency, in.Funding)
	if err != nil {
		return fmt.Errorf("economy: quarantine payment in: %w", err)
	}
	if tag.RowsAffected() == 1 && s.nativePusher != nil {
		msg := nativepush.Message{Title: "Payment held for review",
			Body: formatMinor(in.Currency, in.AmountMinor) + " came in from " + in.Payer + ", who needs a review first. It isn't in your balance until it is reviewed.",
			Data: map[string]string{"type": "deposit_quarantined", "case_id": r.Case.ID, "workspace_id": workspaceID}}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			s.pushDevices(ctx, workspaceID, msg)
		}()
	}
	return fmt.Errorf("%w: %w", ErrDepositQuarantined, r)
}

// PostReleasedDeposits posts, once each, every quarantined payment in whose case an operator has released, and
// answers how many it posted.
func (s *DualTokenStore) PostReleasedDeposits(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT q.workspace_id, q.partner_ref, q.partner, q.partner_account_ref, q.payer, q.reference, q.amount_minor,
		q.currency, q.funding FROM money_quarantined_deposits q JOIN compliance_cases c ON c.id = q.case_id
		WHERE q.posted_at IS NULL AND c.status = $1 ORDER BY q.quarantined_at`, screening.CaseReleased)
	if err != nil {
		return 0, fmt.Errorf("economy: released payments in: %w", err)
	}
	type kept struct {
		ws string
		in InboundPayment
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (kept, error) {
		var k kept
		err := r.Scan(&k.ws, &k.in.PartnerRef, &k.in.Partner, &k.in.PartnerAccountRef, &k.in.Payer, &k.in.Reference, &k.in.AmountMinor,
			&k.in.Currency, &k.in.Funding)
		return k, err
	})
	if err != nil {
		return 0, fmt.Errorf("economy: released payments in: %w", err)
	}
	n, errs := 0, []error(nil)
	for _, k := range list {
		if _, err := s.ReceivePayment(ctx, k.in); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := s.pool.Exec(ctx, `UPDATE money_quarantined_deposits SET posted_at = now() WHERE workspace_id = $1 AND partner_ref = $2`,
			k.ws, k.in.PartnerRef); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
