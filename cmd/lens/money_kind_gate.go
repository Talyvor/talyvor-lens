package main

import (
	"context"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// money_kind_gate.go — B59.7: LENS REFUSES A MONEY ROUTE OF THE WRONG KIND (launch pack 3.1).
//
// Every money route is fiat, on-chain or either, named in moneyRoutes. A fiat workspace calling an on-chain route, and
// a stablecoin workspace calling a fiat one, is refused 409 with code wrong_money_kind, field money_route and the
// workspace's money_kind (B59.6). A hybrid workspace reaches every money route. A route of either side serves both
// kinds; which side its money is on is the currency or the payee it names, not the route.
//
// The gate runs in the authed group after auth and the tenant check, where the route is matched and {wsID} is the
// caller's own. The workspace judged is the path's {wsID}, else the key's; with neither there is nothing to judge and
// the route answers as it does today. A failed read of the kind answers 500: it fails closed.

// moneySide is the side of the money a route moves or shows.
type moneySide string

const (
	moneySideFiat    moneySide = "fiat"     // other banks, cash-outs to a bank: refused to a stablecoin workspace
	moneySideOnChain moneySide = "on_chain" // smart accounts, keys and sweeps on chain, gas: refused to a fiat workspace
	moneySideEither  moneySide = "either"   // serves both kinds: every kind gets cards (launch pack 3.3)
)

// moneyRoutes is every money route's side, by its chi pattern with every parameter written {}, grouped by the file
// that registers it. TestB597_EveryMoneyRouteHasASide fails on a money route missing here, and on an entry no route
// has.
var moneyRoutes = map[string]moneySide{
	// agent_accounts_handler.go
	"/v1/workspaces/{}/agents":                              moneySideEither,
	"/v1/workspaces/{}/agents/alerts":                       moneySideEither,
	"/v1/workspaces/{}/agents/approvals":                    moneySideEither,
	"/v1/workspaces/{}/agents/approvals/challenge":          moneySideEither,
	"/v1/workspaces/{}/agents/approvals/decide":             moneySideEither,
	"/v1/workspaces/{}/agents/approvals/{}/approve":         moneySideEither,
	"/v1/workspaces/{}/agents/approvals/{}/challenge":       moneySideEither,
	"/v1/workspaces/{}/agents/approvals/{}/deny":            moneySideEither,
	"/v1/workspaces/{}/agents/briefing":                     moneySideEither,
	"/v1/workspaces/{}/agents/forecast":                     moneySideEither,
	"/v1/workspaces/{}/agents/gas-budget":                   moneySideOnChain,
	"/v1/workspaces/{}/agents/gas-budget/top-up":            moneySideOnChain,
	"/v1/workspaces/{}/agents/keys":                         moneySideEither,
	"/v1/workspaces/{}/agents/lockdown":                     moneySideEither,
	"/v1/workspaces/{}/agents/owner-wallets":                moneySideEither,
	"/v1/workspaces/{}/agents/passkeys":                     moneySideEither,
	"/v1/workspaces/{}/agents/passkeys/challenge":           moneySideEither,
	"/v1/workspaces/{}/agents/passkeys/{}":                  moneySideEither,
	"/v1/workspaces/{}/agents/pause-all":                    moneySideEither,
	"/v1/workspaces/{}/agents/prompt-schedules":             moneySideEither,
	"/v1/workspaces/{}/agents/prompt-schedules/{}":          moneySideEither,
	"/v1/workspaces/{}/agents/push/public-key":              moneySideEither,
	"/v1/workspaces/{}/agents/push/subscriptions":           moneySideEither,
	"/v1/workspaces/{}/agents/resume-all":                   moneySideEither,
	"/v1/workspaces/{}/agents/resume-all/challenge":         moneySideEither,
	"/v1/workspaces/{}/agents/rule-templates":               moneySideEither,
	"/v1/workspaces/{}/agents/safe-to-spend":                moneySideEither,
	"/v1/workspaces/{}/agents/schedules":                    moneySideEither,
	"/v1/workspaces/{}/agents/schedules/{}":                 moneySideEither,
	"/v1/workspaces/{}/agents/schedules/{}/runs":            moneySideEither,
	"/v1/workspaces/{}/agents/schedules/{}/skip":            moneySideEither,
	"/v1/workspaces/{}/agents/statement":                    moneySideEither,
	"/v1/workspaces/{}/agents/{}":                           moneySideEither,
	"/v1/workspaces/{}/agents/{}/analytics":                 moneySideEither,
	"/v1/workspaces/{}/agents/{}/archive":                   moneySideEither,
	"/v1/workspaces/{}/agents/{}/claim":                     moneySideEither,
	"/v1/workspaces/{}/agents/{}/crypto-balances":           moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/freeze":                    moneySideEither, // a control that stops money is never refused
	"/v1/workspaces/{}/agents/{}/fund":                      moneySideEither,
	"/v1/workspaces/{}/agents/{}/keys":                      moneySideEither,
	"/v1/workspaces/{}/agents/{}/pause":                     moneySideEither,
	"/v1/workspaces/{}/agents/{}/pay":                       moneySideEither,
	"/v1/workspaces/{}/agents/{}/prompt-schedules":          moneySideEither,
	"/v1/workspaces/{}/agents/{}/recover":                   moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/recover/challenge":         moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/recovery-signer":           moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/recovery-signer/challenge": moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/resume":                    moneySideEither,
	"/v1/workspaces/{}/agents/{}/resume/challenge":          moneySideEither,
	"/v1/workspaces/{}/agents/{}/rotate-signer":             moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/rules":                     moneySideEither,
	"/v1/workspaces/{}/agents/{}/rules/boosts":              moneySideEither,
	"/v1/workspaces/{}/agents/{}/rules/boosts/{}":           moneySideEither,
	"/v1/workspaces/{}/agents/{}/rules/challenge":           moneySideEither,
	"/v1/workspaces/{}/agents/{}/rules/history":             moneySideEither,
	"/v1/workspaces/{}/agents/{}/rules/rollback":            moneySideEither,
	"/v1/workspaces/{}/agents/{}/rules/simulate":            moneySideEither,
	"/v1/workspaces/{}/agents/{}/rules/template":            moneySideEither,
	"/v1/workspaces/{}/agents/{}/schedules":                 moneySideEither,
	"/v1/workspaces/{}/agents/{}/smart-account":             moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/smart-account/challenge":   moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/spend-permission":          moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/spend-permission/prepare":  moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/statement":                 moneySideEither,
	"/v1/workspaces/{}/agents/{}/sweep":                     moneySideOnChain,
	"/v1/workspaces/{}/agents/{}/topup":                     moneySideEither,
	"/v1/workspaces/{}/agents/{}/wallet":                    moneySideEither,
	"/v1/workspaces/{}/agents/{}/wallet-kind":               moneySideEither,
	"/v1/workspaces/{}/agents/{}/wallets":                   moneySideEither,
	"/v1/workspaces/{}/agents/{}/withdraw":                  moneySideEither,

	// agent_approvers_handler.go
	"/v1/workspaces/{}/agents/approvals/{}/evidence":   moneySideEither,
	"/v1/workspaces/{}/agents/{}/approvals/{}/approve": moneySideEither,
	"/v1/workspaces/{}/agents/{}/approver":             moneySideEither,
	"/v1/workspaces/{}/agents/{}/approver/challenge":   moneySideEither,

	// agent_card_identity_handler.go
	"/v1/workspaces/{}/agents/{}/agent-card/rotate": moneySideEither,

	// agent_cards_handler.go
	"/v1/workspaces/{}/agents/{}/card":          moneySideEither,
	"/v1/workspaces/{}/agents/{}/card/freeze":   moneySideEither, // a control that stops money is never refused
	"/v1/workspaces/{}/agents/{}/card/unfreeze": moneySideEither,

	// agent_cash_outs_handler.go
	"/v1/workspaces/{}/agents/{}/cash-outs": moneySideFiat,
	"/v1/workspaces/{}/cash-outs":           moneySideFiat,

	// agent_escrows_handler.go
	"/v1/workspaces/{}/agents/{}/escrows":  moneySideEither,
	"/v1/workspaces/{}/escrows":            moneySideEither,
	"/v1/workspaces/{}/escrows/{}":         moneySideEither,
	"/v1/workspaces/{}/escrows/{}/confirm": moneySideEither,
	"/v1/workspaces/{}/escrows/{}/dispute": moneySideEither,

	// agent_pots_handler.go
	"/v1/workspaces/{}/agents/{}/pots":         moneySideEither,
	"/v1/workspaces/{}/agents/{}/pots/{}/in":   moneySideEither,
	"/v1/workspaces/{}/agents/{}/pots/{}/out":  moneySideEither,
	"/v1/workspaces/{}/agents/{}/pots/{}/lock": moneySideEither,

	// agent_transfers_handler.go
	"/v1/wallets/{}":                              moneySideEither,
	"/v1/workspaces/{}/agents/{}/handle":          moneySideEither,
	"/v1/workspaces/{}/agents/{}/requests":        moneySideEither,
	"/v1/workspaces/{}/agents/{}/send":            moneySideEither,
	"/v1/workspaces/{}/agents/{}/transfers":       moneySideEither,
	"/v1/workspaces/{}/money-requests":            moneySideEither,
	"/v1/workspaces/{}/money-requests/{}/accept":  moneySideEither,
	"/v1/workspaces/{}/money-requests/{}/decline": moneySideEither,
	"/v1/workspaces/{}/transfers/{}/refund":       moneySideEither,

	// alert_channels_handler.go
	"/v1/money/alerts/channels":    moneySideEither,
	"/v1/money/alerts/channels/{}": moneySideEither,
	"/v1/money/alerts/deliveries":  moneySideEither,

	// company_loans_handler.go
	"/v1/workspaces/{}/agents/{}/loans": moneySideEither,

	// connected_access_handler.go
	"/v1/money/access": moneySideEither,

	// credit_line_handler.go
	"/v1/workspaces/{}/credit-line": moneySideEither,

	// credit_score_handler.go
	"/v1/money/credit-score":        moneySideEither,
	"/v1/money/credit-score/review": moneySideEither,

	// device_keys_handler.go
	"/v1/money/devices":               moneySideEither,
	"/v1/money/devices/challenge":     moneySideEither,
	"/v1/money/devices/{}/push-token": moneySideEither,
	"/v1/money/devices/{}/revoke":     moneySideEither,

	// key_enrolments_handler.go
	"/v1/workspaces/{}/agents/keys/enrolments":              moneySideEither,
	"/v1/workspaces/{}/agents/keys/enrolments/{}":           moneySideEither,
	"/v1/workspaces/{}/agents/keys/enrolments/{}/approve":   moneySideEither,
	"/v1/workspaces/{}/agents/keys/enrolments/{}/challenge": moneySideEither,
	"/v1/workspaces/{}/agents/keys/enrolments/{}/decline":   moneySideEither,

	// kya_handler.go
	"/v1/workspaces/{}/agents/{}/credential": moneySideEither,

	// lockdown_handler.go
	"/v1/money/lockdown":      moneySideEither,
	"/v1/money/lockdown/lift": moneySideEither,

	// main.go
	"/v1/agent-cards/authorizations":    moneySideEither,
	"/v1/money/chain/webhooks/coinbase": moneySideEither, // the partner's call, outside the authed group
	"/v1/money/webhooks/{}":             moneySideEither, // the partner's call, outside the authed group
	"/v1/workspaces/{}/lxc/balance":     moneySideEither,
	"/v1/workspaces/{}/lxc/history":     moneySideEither,

	// money_accounts_handler.go
	"/v1/money/accounts":               moneySideEither,
	"/v1/money/accounts/{}/details":    moneySideEither,
	"/v1/money/accounts/{}/statement":  moneySideEither,
	"/v1/money/accounts/{}/test-money": moneySideEither,
	"/v1/money/overview":               moneySideEither,

	// money_allowances_handler.go
	"/v1/money/allowances":         moneySideEither,
	"/v1/money/allowances/{}":      moneySideEither,
	"/v1/money/allowances/{}/runs": moneySideEither,

	// money_banks_handler.go
	"/v1/money/banks":            moneySideFiat,
	"/v1/money/banks/{}":         moneySideFiat,
	"/v1/money/banks/{}/refresh": moneySideFiat,
	"/v1/money/banks/{}/renew":   moneySideFiat,

	// money_bills_handler.go
	"/v1/money/bills":     moneySideEither,
	"/v1/money/bills/pay": moneySideEither,
	"/v1/money/bills/{}":  moneySideEither,

	// money_budgets_handler.go
	"/v1/money/budgets":        moneySideEither,
	"/v1/money/budgets/alerts": moneySideEither,
	"/v1/money/budgets/{}":     moneySideEither,

	// money_cards_handler.go
	"/v1/money/cards":            moneySideEither,
	"/v1/money/cards/{}":         moneySideEither,
	"/v1/money/cards/{}/approve": moneySideEither,

	// B59.21: card_spends_handler.go, an agent asking before it pays by card
	"/v1/money/card-spends":    moneySideEither,
	"/v1/money/card-spends/{}": moneySideEither,

	// B59.22: card_challenges_handler.go, 3-D Secure challenges on agents' cards
	"/v1/money/card-challenges": moneySideEither,

	// B59.24: card_disputes_handler.go, card disputes and refunds and the card export
	"/v1/money/card-disputes":                    moneySideEither,
	"/v1/money/card-disputes/{}/sandbox-outcome": moneySideEither,
	"/v1/money/card-refunds":                     moneySideEither,
	"/v1/money/card-exports":                     moneySideEither,

	// money_coach_handler.go
	"/v1/money/coach":     moneySideEither,
	"/v1/money/coach/ask": moneySideEither,

	// money_convert_handler.go
	"/v1/workspaces/{}/agents/{}/money/convert":       moneySideEither,
	"/v1/workspaces/{}/agents/{}/money/convert/quote": moneySideEither,

	// money_earnings_handler.go
	"/v1/money/earnings-payouts":             moneySideEither,
	"/v1/money/payout-details":               moneySideEither,
	"/v1/money/payout-details/address-proof": moneySideEither,

	// money_invoices_handler.go
	"/v1/money/invoices":         moneySideEither,
	"/v1/money/invoices/{}/send": moneySideEither,
	"/v1/money/invoices/{}/void": moneySideEither,
	"/v1/pay/{}":                 moneySideEither,
	"/v1/pay/{}/agent":           moneySideEither,
	"/v1/pay/{}/card":            moneySideEither,

	// money_mandates_handler.go
	"/v1/money/mandates":            moneySideEither,
	"/v1/money/mandates/challenge":  moneySideEither,
	"/v1/money/mandates/{}/approve": moneySideEither,
	"/v1/money/mandates/{}/decline": moneySideEither,
	"/v1/money/mandates/{}/pull":    moneySideEither,
	"/v1/money/mandates/{}/revoke":  moneySideEither,

	// money_notices_handler.go
	"/v1/money/notices":          moneySideEither,
	"/v1/money/notices/settings": moneySideEither,

	// money_payees_handler.go
	"/v1/money/payees":                      moneySideEither,
	"/v1/money/payees/{}/address-proof":     moneySideOnChain,
	"/v1/money/payees/{}/called":            moneySideEither,
	"/v1/money/payees/{}/called/challenge":  moneySideEither,
	"/v1/money/payees/{}/challenge":         moneySideEither,
	"/v1/money/payees/{}/confirm":           moneySideEither,
	"/v1/money/payees/{}/details":           moneySideEither,
	"/v1/money/payees/{}/details/challenge": moneySideEither,

	// money_payments_handler.go
	"/v1/money/payments":           moneySideEither,
	"/v1/money/payments/challenge": moneySideEither,
	"/v1/money/payments/quote":     moneySideEither,
	"/v1/money/payments/{}":        moneySideEither,

	// money_payouts_handler.go
	"/v1/money/payouts":            moneySideEither,
	"/v1/money/payouts/{}":         moneySideEither,
	"/v1/money/payouts/{}/approve": moneySideEither,

	// B58.5: the challenge a payout batch's approval is signed over
	"/v1/money/payouts/{}/approve/challenge": moneySideEither,

	// money_physical_cards_handler.go
	"/v1/money/physical-cards":             moneySideEither,
	"/v1/money/physical-cards/{}/activate": moneySideEither,
	"/v1/money/physical-cards/{}/replace":  moneySideEither,

	// money_plan_handler.go
	"/v1/money/plan":        moneySideEither,
	"/v1/money/plan/income": moneySideEither,

	// money_reusable_cards_handler.go
	"/v1/money/reusable-cards":    moneySideEither,
	"/v1/money/reusable-cards/{}": moneySideEither,

	// money_search_handler.go
	"/v1/money/search": moneySideEither,

	// money_spending_handler.go
	"/v1/money/entries/{}/tags":        moneySideEither,
	"/v1/money/spending":               moneySideEither,
	"/v1/money/spending/merchants":     moneySideEither,
	"/v1/money/spending/subscriptions": moneySideEither,
	"/v1/money/spending/unusual":       moneySideEither,

	// payment_requests_handler.go
	"/v1/money/assistant-limits":             moneySideEither,
	"/v1/money/payment-requests":             moneySideEither,
	"/v1/money/payment-requests/{}":          moneySideEither,
	"/v1/money/payment-requests/{}/approval": moneySideEither,
	"/v1/money/payment-requests/{}/approve":  moneySideEither,
	"/v1/money/payment-requests/{}/cancel":   moneySideEither,
	"/v1/money/payment-requests/{}/pay":      moneySideEither,
	"/v1/money/payment-requests/{}/record":   moneySideEither,

	// permission_mandates_handler.go
	"/v1/money/permission-mandates":             moneySideEither,
	"/v1/money/permission-mandates/prepare":     moneySideEither,
	"/v1/money/permission-mandates/{}/evaluate": moneySideEither,
	"/v1/money/permission-mandates/{}/revoke":   moneySideEither,

	// scan_watches_handler.go
	"/v1/workspaces/{}/agents/{}/scan-watches": moneySideEither,

	// sim_trading_handler.go
	"/v1/workspaces/{}/agents/{}/portfolios":                     moneySideEither,
	"/v1/workspaces/{}/agents/{}/portfolios/{}":                  moneySideEither,
	"/v1/workspaces/{}/agents/{}/portfolios/{}/orders":           moneySideEither,
	"/v1/workspaces/{}/agents/{}/portfolios/{}/orders/{}/cancel": moneySideEither,

	// wallet_capabilities_handler.go
	"/v1/wallets/capabilities": moneySideEither,

	// wallet_statements_handler.go
	"/v1/workspaces/{}/agents/{}/wallet/activity":  moneySideEither,
	"/v1/workspaces/{}/agents/{}/wallet/statement": moneySideEither,
	"/v1/workspaces/{}/wallet/activity":            moneySideEither,
	"/v1/workspaces/{}/wallet/statement":           moneySideEither,
	"/v1/workspaces/{}/wallet/tax":                 moneySideEither,

	// workspace_money_kind_handler.go
	"/v1/workspaces/{}/account":            moneySideEither,
	"/v1/workspaces/{}/account/money-kind": moneySideEither,
}

// moneyKindReader is what moneyKindGate asks; *economy.DualTokenStore answers it.
type moneyKindReader interface {
	WorkspaceMoneyKind(ctx context.Context, workspaceID string) (string, error)
}

// moneyKindGate refuses a fiat or on-chain route to a workspace whose kind of money is the other side.
func moneyKindGate(kinds moneyKindReader) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			side := moneyRoutes[blankRouteParams(chi.RouteContext(req.Context()).RoutePattern())]
			if side != moneySideFiat && side != moneySideOnChain {
				next.ServeHTTP(w, req)
				return
			}
			ws := chi.URLParam(req, "wsID")
			if actx := auth.GetAuthContext(req.Context()); ws == "" && actx != nil {
				ws = actx.WorkspaceID
			}
			if ws == "" {
				next.ServeHTTP(w, req)
				return
			}
			kind, err := kinds.WorkspaceMoneyKind(req.Context(), ws)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "could not read the workspace's kind of money")
				return
			}
			if (side == moneySideFiat && kind == economy.MoneyKindStablecoin) || (side == moneySideOnChain && kind == economy.MoneyKindFiat) {
				writeMoneyKindRefusal(w, &economy.MoneyKindRefusal{Field: "money_route", MoneyKind: kind})
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

// blankRouteParams writes every {name} in a chi pattern as {}.
func blankRouteParams(pattern string) string { return routeParam.ReplaceAllString(pattern, "{}") }

var routeParam = regexp.MustCompile(`\{[^}]*\}`)
