package economy

import (
	"context"
	"errors"
	"log/slog"

	"github.com/talyvor/lens/internal/partners"
)

// stablecoin_payments.go — B30.33: A COMPANY PAYS USDC FROM ITS OWN ACCOUNT, AT THE STABLECOIN PARTNER.
//
// A payment out in USDC from the company's account (OutsidePayment.viaStablecoin) is judged, screened — the payee's
// name and the address it goes to — and held by PayOutside like any other, to a saved USDC address the owner approved
// only, then sent here: at the stablecoin partner, the Test partner until stablecoins (RED) is cleared and a real one
// is configured, and only the Test partner moves test money. A transfer over the Travel Rule threshold
// (SetTravelRuleMinor) carries the payer's and the payee's names, and with the Travel Rule adapter set (B48.7) the
// identity data goes to the payee's provider first.

// SetTravelRuleMinor is LENS_TRAVEL_RULE_MINOR: a USDC transfer of more micro-USDC than this carries the payer's and
// the payee's names (B30.33). Nicolai sets it; until then 0, so every transfer carries both.
func (s *DualTokenStore) SetTravelRuleMinor(minor int64) { s.travelRuleMinor = minor }

// stablecoinTransfer is p as the stablecoin partner is asked to send it, from from's address to payee's, with both
// names when it is over overMinor.
func stablecoinTransfer(p OutsidePayment, from payingAccount, payee OutsidePayee, payer string, overMinor int64) partners.StablecoinTransfer {
	t := partners.StablecoinTransfer{ID: p.ID, From: from.partnerRef, Network: payee.Network, To: payee.Address,
		Amount: partners.Money{Minor: p.AmountMinor, Currency: p.Currency}}
	if p.AmountMinor > overMinor {
		t.Originator, t.Beneficiary = payer, payee.Name
	}
	return t
}

// sendStablecoin asks the stablecoin partner to make p, as sendOutside does the money layer: once p was handed to
// the partner, only its answer may fail p, so any error on a resend keeps the money held.
//
// shortcut: a resend the partner refuses as a different transfer under the same id (the payer renamed since) stays
// held and is asked again each time it is read; ask the partner by id instead when a real partner says how.
func (s *DualTokenStore) sendStablecoin(ctx context.Context, p OutsidePayment, from payingAccount, payee OutsidePayee) (partners.Result, bool) {
	partner, err := s.accountPartners.Stablecoin(ctx, CapabilityStablecoins)
	if err != nil {
		return partners.Result{}, false
	}
	if p.Funding != FundingLive && partner.Name() != (&partners.TestStablecoinPartner{}).Name() {
		return partners.Result{Status: partners.StatusFailed, Detail: "test money is sent only at the Test stablecoin partner"}, true
	}
	payer, err := s.payerName(ctx, p.WorkspaceID)
	if err != nil {
		return partners.Result{}, false
	}
	if s.travelRule != nil { // B48.7: the identity data travels before the money
		if res, ok, stop := s.travelRuleBeforeSend(ctx, p, from, payee); stop {
			return res, ok
		}
	}
	var resend bool
	if err := s.pool.QueryRow(ctx, `WITH was AS (SELECT send_started_at FROM money_payments WHERE id = $1 FOR UPDATE)
		UPDATE money_payments m SET send_started_at = COALESCE(was.send_started_at, now()) FROM was WHERE m.id = $1
		RETURNING was.send_started_at IS NOT NULL`, p.ID).Scan(&resend); err != nil {
		return partners.Result{}, false
	}
	res, err := partner.Send(partners.WithWorkspace(ctx, p.WorkspaceID), stablecoinTransfer(p, from, payee, payer, s.travelRuleMinor))
	switch {
	case err != nil && resend:
		slog.Error("economy: a USDC payment that may have gone was not answered on its resend; its money stays held and it is asked again",
			slog.String("payment_id", p.ID), slog.String("err", err.Error()))
		return partners.Result{}, false
	case errors.Is(err, partners.ErrInvalid), errors.Is(err, partners.ErrNotFound), errors.Is(err, partners.ErrIDReused):
		return partners.Result{Status: partners.StatusFailed, Detail: err.Error()}, true
	case err != nil:
		return partners.Result{}, false
	}
	return res, true
}

// stablecoinStatus is where the stablecoin partner says p stands.
func (s *DualTokenStore) stablecoinStatus(ctx context.Context, p OutsidePayment) (partners.Result, error) {
	partner, err := s.accountPartners.Stablecoin(ctx, CapabilityStablecoins)
	if err != nil {
		return partners.Result{}, err
	}
	return partner.Status(partners.WithWorkspace(ctx, p.WorkspaceID), p.PartnerRef)
}
