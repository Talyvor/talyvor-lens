package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
)

// money_payments_layer.go — B46.8: PAYMENTS OUT ON THE MONEY LAYER.
//
// Every payment out — one the owner pays, an approved payment request, a payout batch, a standing order, a transfer
// to a saved contact — is made by PayOutside, and PayOutside hands it to the money layer's Transfers (B46.3): the
// adapter switched on for the workspace and the payee's country once payments_out is cleared, the sandbox until then.
// The agent's limits and the owner's one-tap approval judge it first, as before, and its money is held; the layer's
// gate then asks MoneyLimits, which lets through only that held payment. The adapter is asked under the payment's own
// id, so a retry pays nothing twice, and the partner's signed webhook that it completed, failed or came back moves it
// on once (SettleOutsidePayment). A refusal releases the hold and tells the workspace's phones.
//
// The audit trail links each step: the request and its approval (payment_requests), the call to the adapter
// (money.transfers.outbound, keyed by the payment's id, with the partner's reference) and the partner's confirmation
// (money.webhook.transfer, with the same reference and the payment's id).

// viaMoneyLayer is whether p is made through the money layer.
//
// shortcut: an international payment is made at the account partner's quote, which money.Outbound cannot carry, so it
// still goes to the account partner; upgrade when Transfers takes a quote.
func (s *DualTokenStore) viaMoneyLayer(p OutsidePayment) bool {
	return s.moneyLayer != nil && p.QuoteID == ""
}

func outsideRoute(p OutsidePayment, payee OutsidePayee) money.Route {
	return money.Route{Workspace: p.WorkspaceID, Agent: p.AgentID, Country: payee.Country, Capability: CapabilityPaymentsOut}
}

// outsidePartner is the name of who makes p: the money layer's adapter for it, or the account partner.
func (s *DualTokenStore) outsidePartner(ctx context.Context, p OutsidePayment, payee OutsidePayee) (string, error) {
	if s.viaMoneyLayer(p) {
		return s.moneyLayer.Partner(ctx, money.CapTransfers, outsideRoute(p, payee))
	}
	partner, err := s.accountPartners.Account(ctx, CapabilityPaymentsOut)
	if err != nil {
		return "", err
	}
	return partner.Name(), nil
}

// sendOutside asks who makes p to make it, from from's account at the partner, to payee. ok false: nobody answered,
// or not certainly, so the money stays held and p is asked again — under the same id — when it is read. A refusal is
// a failed result, which releases the hold.
func (s *DualTokenStore) sendOutside(ctx context.Context, p OutsidePayment, from payingAccount, payee OutsidePayee) (partners.Result, bool) {
	if !s.viaMoneyLayer(p) {
		partner, err := s.accountPartners.Account(ctx, CapabilityPaymentsOut)
		if err != nil {
			return partners.Result{}, false
		}
		res, err := partner.SendPayment(partners.WithWorkspace(ctx, p.WorkspaceID), p.sendRequest(from, payee))
		switch {
		case errors.Is(err, partners.ErrInvalid), errors.Is(err, partners.ErrNotFound), errors.Is(err, partners.ErrIDReused):
			return partners.Result{Status: partners.StatusFailed, Detail: err.Error()}, true
		case err != nil:
			return partners.Result{}, false
		}
		return res, true
	}
	t, err := s.moneyLayer.Transfers(ctx, outsideRoute(p, payee))
	if err != nil {
		return partners.Result{}, false
	}
	r, err := t.Outbound(partners.WithWorkspace(ctx, p.WorkspaceID), money.Outbound{Key: p.ID, From: from.partnerRef, Reference: p.Reference,
		Amount: money.Amount{Minor: p.AmountMinor, Asset: p.Currency}, Payee: money.Payee{Name: payee.Name, IBAN: payee.IBAN, BIC: payee.BIC,
			SortCode: payee.SortCode, AccountNumber: payee.AccountNumber, RoutingNumber: payee.RoutingNumber}})
	for _, refused := range []error{money.ErrRefused, money.ErrInvalid, money.ErrNotFound, money.ErrKeyReused, money.ErrInsufficientFunds,
		money.ErrLimit, money.ErrApprovalNeeded} {
		if errors.Is(err, refused) {
			return partners.Result{Status: partners.StatusFailed, Detail: err.Error()}, true
		}
	}
	if err != nil { // the partner did not answer, or its answer was not recorded: asked again, it answers the same
		return partners.Result{}, false
	}
	return partners.Result{Ref: r.Ref, Status: partners.Status(r.Status), Detail: r.Detail}, true
}

// heldOutsidePayment is whether op is the money layer making a payment out that PayOutside has already judged by the
// agent's limits and the owner's approval, and whose money it holds: the payment pending under op's key, of that
// amount, from that agent, from the partner account it pays from and to its own payee's account (B46.19) — so the
// pass covers that one payment, never the same key and amount sent elsewhere.
func (s *DualTokenStore) heldOutsidePayment(ctx context.Context, op money.Op) (bool, error) {
	if op.Capability != money.CapTransfers || op.Method != "outbound" || !strings.HasPrefix(op.Key, "mpay_") ||
		op.To.Network != "" || op.To.Address != "" {
		return false, nil
	}
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_payments m
		JOIN money_accounts a ON a.id = m.account_id
		JOIN money_accounts c ON c.id = COALESCE(a.parent_account_id, a.id)
		JOIN money_payees y ON y.id = m.payee_id AND y.workspace_id = m.workspace_id
		WHERE m.id = $1 AND m.workspace_id = $2 AND m.agent_id = $3 AND m.status = $4 AND m.amount_minor = $5 AND m.currency = $6
		AND c.partner_account_ref = $7 AND c.partner_account_ref <> '' AND y.name = $8 AND y.iban = $9 AND y.bic = $10
		AND y.sort_code = $11 AND y.account_number = $12 AND y.routing_number = $13)`,
		op.Key, op.Workspace, op.Agent, PaymentPending, op.Amount.Minor, op.Amount.Asset, op.Ref,
		op.To.Name, op.To.IBAN, op.To.BIC, op.To.SortCode, op.To.AccountNumber, op.To.RoutingNumber).Scan(&held)
	return held, err
}

// SettleOutsidePayment moves the payment out that partner knows as e.PartnerRef to where its webhook says it stands —
// completed, failed (the hold released) or returned — once: told again, it moves nothing. money.ErrNotFound when no
// payment is that partner's transfer.
//
// shortcut: money_payments has no index on (partner, partner_ref), so each webhook scans it; add one when live
// partners settle many payments.
func (s *DualTokenStore) SettleOutsidePayment(ctx context.Context, partner string, e money.Event) (OutsidePayment, error) {
	p, err := scanOutsidePayment(s.pool.QueryRow(ctx, `SELECT `+outsidePaymentCols+` FROM money_payments WHERE partner = $1 AND partner_ref = $2`,
		partner, e.PartnerRef))
	if errors.Is(err, ErrOutsidePaymentNotFound) {
		return p, fmt.Errorf("%w: no payment is %s's transfer %s", money.ErrNotFound, partner, e.PartnerRef)
	}
	if err != nil {
		return p, fmt.Errorf("economy: settle payment: %w", err)
	}
	from, err := s.payingAccount(ctx, p.WorkspaceID, OutsidePayment{AccountID: p.AccountID, AgentID: p.AgentID, Currency: p.Currency})
	if err != nil {
		return p, err
	}
	return s.applyPartnerResult(ctx, p, from.partnerID, partners.Result{Ref: e.PartnerRef, Status: partners.Status(e.Status), Detail: e.Detail})
}

// notifyPaymentFailed tells the workspace's phones, in the background, that p was not paid and its money is back.
func (s *DualTokenStore) notifyPaymentFailed(p OutsidePayment) {
	if s.nativePusher == nil {
		return
	}
	m := nativepush.Message{Title: "Payment not made",
		Body: formatMinor(p.Currency, p.debit()) + " to " + p.PayeeName + " wasn't paid. The money is back in the account it came from.",
		Data: map[string]string{"type": "payment_failed", "payment_id": p.ID, "workspace_id": p.WorkspaceID}}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.pushDevices(ctx, p.WorkspaceID, m)
	}()
}
