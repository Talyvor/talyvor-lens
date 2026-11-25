package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/economy"
)

// identity_gate.go — B61.52: AN ACCOUNT THAT HAS NOT CONFIRMED WHO IT IS MOVES NO MONEY, SANDBOX MONEY INCLUDED.
//
// Every money route (moneyRoutes) that changes something is refused to an account below the level its type needs —
// Personal L2, Business L3 (economy.NeededLevel) — 403 with code VERIFICATION_NEEDED, the level it has and the one it
// needs, and one sentence (economy.ConfirmIdentitySentence). Looking stays open: every GET, and each change named in
// identityOpenRoutes with why it moves no money. A money route added later is refused until it is named there, so no
// new route moves money unasked. The gate runs in the authed group after the tenant and kind-of-money gates; the
// workspace judged is the path's {wsID}, else the caller's. The operator (the global admin key) is no person moving
// money and passes. A failed read answers 500: it fails closed.
//
// Behind the door the economy asks the same of every AMBER and RED capability (economy.refuseUnconfirmed), for the
// money that does not come through a route: the connector's tools, schedules and approvals that run later.

// identityOpenRoutes is every change on a money route an unconfirmed account may still make, as "METHOD pattern" with
// every parameter written {}, and why it moves no money. TestB6152_EveryMoneyRouteIsGatedOrNamedOpen fails on an entry
// no route has.
var identityOpenRoutes = map[string]string{
	// Controls that stop money are never refused.
	"POST /v1/money/lockdown":                                  "a control: it stops money",
	"POST /v1/money/lockdown/lift":                             "a control on the account's own sign-in",
	"POST /v1/workspaces/{}/agents/lockdown":                   "a control: it stops money",
	"POST /v1/workspaces/{}/agents/pause-all":                  "a control: it stops money",
	"POST /v1/workspaces/{}/agents/resume-all":                 "a control: it lets the agents ask again, and each movement is asked",
	"POST /v1/workspaces/{}/agents/resume-all/challenge":       "a control's passkey challenge",
	"POST /v1/workspaces/{}/agents/{}/freeze":                  "a control: it stops money",
	"POST /v1/workspaces/{}/agents/{}/pause":                   "a control: it stops money",
	"POST /v1/workspaces/{}/agents/{}/resume":                  "a control: each movement after it is asked",
	"POST /v1/workspaces/{}/agents/{}/resume/challenge":        "a control's passkey challenge",
	"POST /v1/workspaces/{}/agents/{}/archive":                 "a control: it stops an agent",
	"POST /v1/workspaces/{}/agents/{}/card/freeze":             "a control: it stops a card",
	"POST /v1/workspaces/{}/agents/{}/card/unfreeze":           "a control on a card already made",
	"PUT /v1/workspaces/{}/agents/{}/pots/{}/lock":             "a control: it holds a pot",
	"POST /v1/workspaces/{}/agents/approvals/{}/deny":          "a refusal: nothing moves",
	"POST /v1/workspaces/{}/money-requests/{}/decline":         "a refusal: nothing moves",
	"POST /v1/workspaces/{}/agents/{}/crypto-sends/{}/decline": "a refusal: nothing moves",
	"POST /v1/money/mandates/{}/decline":                       "a refusal: nothing moves",
	"POST /v1/money/mandates/{}/revoke":                        "a refusal: it ends a mandate",
	"POST /v1/money/payment-requests/{}/cancel":                "a refusal: nothing moves",
	"POST /v1/money/invoices/{}/void":                          "a refusal: it ends an invoice",
	"POST /v1/money/permission-mandates/{}/revoke":             "a refusal: it ends a mandate",
	"POST /v1/workspaces/{}/escrows/{}/dispute":                "a dispute holds the money where it is",
	"POST /v1/money/card-disputes":                             "a dispute of a spend the card made",
	"POST /v1/money/card-disputes/{}/sandbox-outcome":          "the Sandbox deciding a dispute, as an issuer would",
	"POST /v1/money/card-refunds":                              "the Sandbox merchant giving money back for a spend",

	// Rules and settings.
	"PUT /v1/money/allowances":                                        "a rule",
	"DELETE /v1/money/allowances/{}":                                  "a rule",
	"PUT /v1/money/assistant-limits":                                  "a rule",
	"PUT /v1/money/budgets":                                           "a rule",
	"DELETE /v1/money/budgets/{}":                                     "a rule",
	"PUT /v1/money/notices/settings":                                  "a setting",
	"PUT /v1/money/entries/{}/tags":                                   "a label on a statement line",
	"PUT /v1/money/spending/merchants":                                "a label on the spending view",
	"PUT /v1/money/plan/income":                                       "a figure for the spending plan",
	"PUT /v1/money/payout-details":                                    "a setting: where earnings would go",
	"POST /v1/money/payout-details/address-proof":                     "a document for a setting",
	"POST /v1/money/alerts/channels":                                  "a setting",
	"DELETE /v1/money/alerts/channels/{}":                             "a setting",
	"PATCH /v1/money/reusable-cards/{}":                               "a setting on a card already made",
	"PUT /v1/workspaces/{}/account/money-kind":                        "a setting (refused to an owner anyway, B61.6)",
	"POST /v1/workspaces/{}/account/money-kind/challenge":             "a setting's passkey challenge",
	"PATCH /v1/workspaces/{}/agents/{}":                               "an agent's name and settings",
	"PUT /v1/workspaces/{}/agents/{}/handle":                          "an agent's handle",
	"PUT /v1/workspaces/{}/agents/{}/wallet-kind":                     "an agent's setting",
	"PUT /v1/workspaces/{}/agents/{}/rules":                           "a rule",
	"POST /v1/workspaces/{}/agents/{}/rules/boosts":                   "a rule",
	"DELETE /v1/workspaces/{}/agents/{}/rules/boosts/{}":              "a rule",
	"POST /v1/workspaces/{}/agents/{}/rules/challenge":                "a rule's passkey challenge",
	"POST /v1/workspaces/{}/agents/{}/rules/proposals":                "a rule",
	"POST /v1/workspaces/{}/agents/{}/rules/proposals/{}/approve":     "a rule",
	"POST /v1/workspaces/{}/agents/{}/rules/proposals/{}/challenge":   "a rule's passkey challenge",
	"POST /v1/workspaces/{}/agents/{}/rules/rollback":                 "a rule",
	"POST /v1/workspaces/{}/agents/{}/rules/simulate":                 "a rule tried on paper",
	"POST /v1/workspaces/{}/agents/{}/rules/template":                 "a rule",
	"PUT /v1/workspaces/{}/agents/{}/topup":                           "a rule: each top-up it makes is asked",
	"DELETE /v1/workspaces/{}/agents/{}/topup":                        "a rule",
	"PUT /v1/workspaces/{}/agents/{}/spend-permission":                "a rule on the agent's own wallet",
	"POST /v1/workspaces/{}/agents/{}/spend-permission/prepare":       "a rule's preparation",
	"POST /v1/workspaces/{}/agents/{}/card/draws-on":                  "a setting on a card already made",
	"POST /v1/workspaces/{}/agents/{}/card/merchant-lock":             "a setting on a card already made",
	"PUT /v1/workspaces/{}/agents/{}/approver":                        "a rule: who approves",
	"DELETE /v1/workspaces/{}/agents/{}/approver":                     "a rule: who approves",
	"POST /v1/workspaces/{}/agents/{}/approver/challenge":             "a rule's passkey challenge",
	"POST /v1/money/permission-mandates":                              "a rule: each purchase under it is asked",
	"POST /v1/money/permission-mandates/prepare":                      "a rule's preparation",
	"POST /v1/money/permission-mandates/{}/evaluate":                  "a rule tried on paper",
	"DELETE /v1/workspaces/{}/agents/schedules/{}":                    "it ends a schedule",
	"POST /v1/workspaces/{}/agents/schedules/{}/skip":                 "it skips a run",
	"POST /v1/workspaces/{}/agents/{}/prompt-schedules":               "a prompt on a timer: no money",
	"DELETE /v1/workspaces/{}/agents/prompt-schedules/{}":             "a prompt on a timer: no money",
	"POST /v1/workspaces/{}/agents/{}/pots":                           "a pot is made empty; money into it is asked",
	"POST /v1/workspaces/{}/agents/{}/scan-watches":                   "a watch: no money",
	"POST /v1/workspaces/{}/agents/{}/portfolios":                     "a simulated portfolio is made empty; its orders are asked",
	"POST /v1/workspaces/{}/agents/{}/portfolios/{}/orders/{}/cancel": "a refusal: nothing moves",
	"POST /v1/money/credit-score/review":                              "a request to look again",
	"POST /v1/workspaces/{}/agents/{}/smart-account":                  "the agent's own wallet, opened with the agent",
	"POST /v1/workspaces/{}/agents/{}/smart-account/challenge":        "its passkey challenge",
	"POST /v1/workspaces/{}/agents/{}/rotate-signer":                  "a key on the agent's own wallet",
	"PUT /v1/workspaces/{}/agents/{}/recovery-signer":                 "a key on the agent's own wallet",
	"POST /v1/workspaces/{}/agents/{}/recovery-signer/challenge":      "its passkey challenge",
	"POST /v1/workspaces/{}/agents/{}/recover":                        "a key on the agent's own wallet",
	"POST /v1/workspaces/{}/agents/{}/recover/challenge":              "its passkey challenge",
	"POST /v1/workspaces/{}/agents/{}/agent-card/rotate":              "the agent's identity card, not a payment card",
	"POST /v1/workspaces/{}/agents/{}/credential/tap":                 "the agent's identity credential",
	"POST /v1/money/physical-cards/{}/activate":                       "a card already made",
	"POST /v1/money/physical-cards/{}/replace":                        "a card already made, replaced when lost",
	"POST /v1/workspaces/{}/agents/approvals/challenge":               "a passkey challenge: the approval itself is asked",
	"POST /v1/workspaces/{}/agents/approvals/{}/challenge":            "a passkey challenge: the approval itself is asked",
	"POST /v1/money/card-challenges":                                  "a card's challenge: the spend itself is asked",
	"POST /v1/money/accounts/{}/withdraw/challenge":                   "a passkey challenge: the movement itself is asked",
	"POST /v1/workspaces/{}/agents/{}/cash-outs/challenge":            "a passkey challenge: the movement itself is asked",
	"POST /v1/money/convert/challenge":                                "a passkey challenge: the movement itself is asked",
	"POST /v1/money/payments/challenge":                               "a passkey challenge: the movement itself is asked",
	"POST /v1/money/people/pay/challenge":                             "a passkey challenge: the movement itself is asked",
	"POST /v1/money/mandates/challenge":                               "a passkey challenge: the mandate itself is asked",
	"POST /v1/money/payouts/{}/approve/challenge":                     "a passkey challenge: the movement itself is asked",
	"POST /v1/workspaces/{}/agents/{}/crypto-sends/challenge":         "a passkey challenge: the send itself is asked", // B48.49

	// Agents, keys and sign-in.
	"POST /v1/workspaces/{}/agents":                              "an agent is made with nothing in it",
	"POST /v1/workspaces/{}/agents/{}/keys":                      "an agent's key",
	"POST /v1/workspaces/{}/agents/{}/claim":                     "an agent's owner",
	"POST /v1/workspaces/{}/agents/keys/enrolments/{}/approve":   "an agent's key",
	"POST /v1/workspaces/{}/agents/keys/enrolments/{}/challenge": "an agent's key's passkey challenge",
	"POST /v1/workspaces/{}/agents/keys/enrolments/{}/decline":   "an agent's key",
	"POST /v1/workspaces/{}/agents/passkeys":                     "a passkey",
	"POST /v1/workspaces/{}/agents/passkeys/challenge":           "a passkey's challenge",
	"DELETE /v1/workspaces/{}/agents/passkeys/{}":                "a passkey",
	"POST /v1/workspaces/{}/agents/push/subscriptions":           "notifications",
	"DELETE /v1/workspaces/{}/agents/push/subscriptions":         "notifications",
	"POST /v1/money/devices":                                     "a device key",
	"POST /v1/money/devices/challenge":                           "a device key's challenge",
	"POST /v1/money/devices/{}/push-token":                       "notifications",
	"POST /v1/money/devices/{}/live-activity-token":              "notifications",
	"POST /v1/money/devices/{}/live-activities":                  "notifications",
	"POST /v1/money/devices/{}/revoke":                           "a device key",

	// Looking: a quote, a search, a question, a payee saved, a bank linked to read, a bill read.
	"POST /v1/money/convert/quote":                         "a quote",
	"POST /v1/money/payments/quote":                        "a quote",
	"POST /v1/workspaces/{}/agents/{}/crypto-sends/quote":  "a quote",
	"POST /v1/workspaces/{}/agents/{}/money/convert/quote": "a quote",
	"POST /v1/workspaces/{}/money/instant-transfers/quote": "a quote",
	"POST /v1/money/coach/ask":                             "a question",
	"POST /v1/money/people/find":                           "a search",
	"POST /v1/money/payees":                                "a payee saved: paying them is asked",
	"POST /v1/money/payees/{}/address-proof":               "a payee's document",
	"POST /v1/money/payees/{}/called":                      "a payee confirmed",
	"POST /v1/money/payees/{}/called/challenge":            "a payee's passkey challenge",
	"POST /v1/money/payees/{}/challenge":                   "a payee's passkey challenge",
	"POST /v1/money/payees/{}/confirm":                     "a payee confirmed",
	"POST /v1/money/payees/{}/details":                     "a payee's details",
	"POST /v1/money/payees/{}/details/challenge":           "a payee's passkey challenge",
	"POST /v1/money/banks":                                 "the owner's own bank, linked to read",
	"DELETE /v1/money/banks/{}":                            "the owner's own bank, unlinked",
	"POST /v1/money/banks/{}/refresh":                      "the owner's own bank, read again",
	"POST /v1/money/banks/{}/renew":                        "the owner's own bank's consent",
	"POST /v1/money/bills":                                 "a bill read: paying it is asked",
	"POST /v1/money/chain/webhooks/coinbase":               "the wallet provider's callback, not a person; outside the authed group",
	"POST /v1/money/webhooks/{}":                           "a partner's callback, not a person; outside the authed group",
	"POST /v1/agent-cards/authorizations":                  "the card issuer's callback, not a person; outside the authed group",
	"POST /v1/pay/{}/card":                                 "a payer outside Talyvor paying an invoice; outside the authed group",
}

// identityReader is what identityGate asks; *economy.DualTokenStore answers it.
type identityReader interface {
	IdentityUnconfirmed(ctx context.Context, workspaceID string) (*economy.IdentityNeeded, error)
}

// identityGate refuses every change on a money route not named in identityOpenRoutes to an account that has not
// confirmed who it is.
func identityGate(ids identityReader) func(http.Handler) http.Handler {
	return identityGateOpening(ids, identityOpenRoutes)
}

// identityGateOpening is identityGate with the changes named in openRoutes left open.
func identityGateOpening(ids identityReader, openRoutes map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			pattern := blankRouteParams(chi.RouteContext(req.Context()).RoutePattern())
			_, money := moneyRoutes[pattern]
			_, open := openRoutes[req.Method+" "+pattern]
			if !money || open || req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions {
				next.ServeHTTP(w, req)
				return
			}
			caller, admin := callerWorkspaceID(req.Context())
			ws := chi.URLParam(req, "wsID")
			if ws == "" {
				ws = caller
			}
			if admin || ws == "" {
				next.ServeHTTP(w, req)
				return
			}
			n, err := ids.IdentityUnconfirmed(req.Context(), ws)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "could not read whether the account has confirmed who it is")
				return
			}
			if n != nil {
				writeIdentityNeeded(w, n)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

// identityNeededCode is the code the apps read on the refusal.
const identityNeededCode = "VERIFICATION_NEEDED"

// writeIdentityNeeded is the one refusal of money to an unconfirmed account.
func writeIdentityNeeded(w http.ResponseWriter, n *economy.IdentityNeeded) {
	writeJSONOK(w, http.StatusForbidden, map[string]any{"error": api.Refusal(economy.ConfirmIdentitySentence), "code": identityNeededCode,
		"level": n.Have, "level_needed": n.Needed, "account_type": n.AccountType})
}
