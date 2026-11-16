package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B61.52 — an account that has not confirmed who it is moves no money, Sandbox money included: every change on a money
// route is refused 403 VERIFICATION_NEEDED, "Confirm who you are to move money.", unless identityOpenRoutes names why it
// moves none; looking is never refused.

// b6152Moves is every path the item names that moves money, by its route: each must stay refused to an unconfirmed
// account, so none of them can be moved into identityOpenRoutes unnoticed.
var b6152Moves = []string{
	"POST /v1/money/accounts",                                  // a currency account
	"POST /v1/money/accounts/{}/funding",                       // a top-up from the owner's own bank
	"POST /v1/money/accounts/{}/test-money",                    // a Sandbox top-up
	"POST /v1/money/accounts/{}/withdraw",                      // money out to the owner's bank
	"POST /v1/workspaces/{}/agents/{}/money/instant-transfers", // Instant transfer (B59.14 to B59.18)
	"POST /v1/workspaces/{}/agents/{}/money/convert",           // a conversion
	"POST /v1/money/convert",                                   // a conversion
	"POST /v1/money/payments",                                  // paying a company outside Talyvor
	"POST /v1/money/people/pay",                                // paying a person
	"POST /v1/workspaces/{}/agents/{}/send",                    // agent to agent, another owner
	"POST /v1/workspaces/{}/agents/{}/pay",                     // an agent paying a company's agent
	"POST /v1/workspaces/{}/agents/{}/requests",                // asking another owner for money
	"POST /v1/workspaces/{}/money-requests/{}/accept",          // paying what was asked
	"POST /v1/workspaces/{}/transfers/{}/refund",               // giving a transfer back
	"POST /v1/workspaces/{}/agents/{}/fund",                    // the owner's money into an agent
	"POST /v1/workspaces/{}/agents/{}/withdraw",                // an agent's money back to the owner
	"POST /v1/workspaces/{}/agents/{}/pots/{}/in",              // into a pot
	"POST /v1/workspaces/{}/agents/{}/pots/{}/out",             // out of a pot
	"POST /v1/workspaces/{}/agents/{}/card",                    // a card
	"POST /v1/money/cards",                                     // a card
	"POST /v1/money/reusable-cards",                            // a card
	"POST /v1/money/physical-cards",                            // a card
	"POST /v1/money/card-spends",                               // a card spend
	"POST /v1/money/payment-requests",                          // a payment request
	"POST /v1/money/payment-requests/{}/approve",               // paying it
	"POST /v1/money/payment-requests/{}/pay",                   // paying it
	"POST /v1/money/bills/pay",                                 // paying bills
	"POST /v1/money/invoices",                                  // asking to be paid
	"POST /v1/pay/{}/agent",                                    // an agent paying an invoice
	"POST /v1/money/mandates",                                  // a mandate to be pulled from
	"POST /v1/money/mandates/{}/pull",                          // a pull under it
	"POST /v1/money/payouts",                                   // paying people for tasks
	"POST /v1/workspaces/{}/agents/{}/cash-outs",               // cashing credits out
	"POST /v1/workspaces/{}/agents/{}/crypto-sends",            // stablecoins on chain
	"POST /v1/workspaces/{}/agents/{}/crypto-sends/{}/approve", // approving one
	"POST /v1/workspaces/{}/agents/{}/sweep",                   // a sweep on chain
	"POST /v1/workspaces/{}/agents/gas-budget/top-up",          // gas
	"POST /v1/workspaces/{}/agents/{}/escrows",                 // escrow
	"POST /v1/workspaces/{}/escrows/{}/confirm",                // releasing it
	"POST /v1/workspaces/{}/agents/{}/loans",                   // a loan
	"POST /v1/workspaces/{}/agents/approvals/decide",           // an approval: the money then moves
	"POST /v1/workspaces/{}/agents/approvals/{}/approve",       // an approval
	"POST /v1/workspaces/{}/agents/{}/approvals/{}/approve",    // an approval
	"POST /v1/workspaces/{}/agents/{}/schedules",               // a payment on a timer
	"POST /v1/workspaces/{}/agents/{}/portfolios/{}/orders",    // an order
}

// b6152Unconfirmed answers every workspace as unconfirmed, or as confirmed.
type b6152Unconfirmed struct{ unconfirmed bool }

func (u b6152Unconfirmed) IdentityUnconfirmed(context.Context, string) (*economy.IdentityNeeded, error) {
	if !u.unconfirmed {
		return nil, nil
	}
	return &economy.IdentityNeeded{AccountType: economy.AccountTypePersonal, Needed: economy.LevelIdentity, Have: economy.LevelSignedIn}, nil
}

// b6152Router is every money route behind the gate, each answering 200 when it is reached, called as the caller.
func b6152Router(t *testing.T, routes []b597Route, ids identityReader, open map[string]string, caller *auth.AuthContext) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), caller)))
		})
	})
	r.Group(func(g chi.Router) { // as main.go's authed group: a group's middleware runs once the route is matched
		g.Use(identityGateOpening(ids, open))
		for _, rt := range routes {
			g.Method(rt.method, rt.pattern, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		}
		g.Get("/v1/workspaces/{wsID}/verification", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		g.Post("/v1/workspaces/{wsID}/verification", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})
	return r
}

// b6152Sweep calls every money route as an unconfirmed account and says what answered wrong: a change not named open
// that was reached, or answered other than the one refusal; a GET or an open change that was refused.
func b6152Sweep(t *testing.T, routes []b597Route, open map[string]string) (wrong []string, refused int) {
	t.Helper()
	h := b6152Router(t, routes, b6152Unconfirmed{true}, open, &auth.AuthContext{WorkspaceID: "ws-b6152"})
	for _, rt := range routes {
		key := rt.method + " " + blankRouteParams(rt.pattern)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rt.method, b597Param.ReplaceAllString(rt.pattern, "ws-b6152"), nil))
		_, isOpen := open[key]
		if rt.method == http.MethodGet || isOpen {
			if rec.Code != http.StatusOK {
				wrong = append(wrong, fmt.Sprintf("%s is looking or named open, and was refused: %d %s", key, rec.Code, rec.Body.String()))
			}
			continue
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusForbidden || out["code"] != "VERIFICATION_NEEDED" || out["error"] != economy.ConfirmIdentitySentence ||
			out["level"] != "L0" || out["level_needed"] != "L2" {
			wrong = append(wrong, fmt.Sprintf("%s by an unconfirmed account = %d %s; want 403 VERIFICATION_NEEDED %q", key, rec.Code,
				rec.Body.String(), economy.ConfirmIdentitySentence))
			continue
		}
		refused++
	}
	return wrong, refused
}

// The sweep: every change on a money route answers the one refusal to an unconfirmed account, and every GET and every
// change named open is reached. A money route added later is refused until it is named open, so none moves money unasked.
func TestB6152_AnUnconfirmedAccountIsRefusedEveryMoneyMovementAndLooksFreely(t *testing.T) {
	routes := b597MoneyRoutes(t)
	wrong, refused := b6152Sweep(t, routes, identityOpenRoutes)
	for _, w := range wrong {
		t.Error(w)
	}
	t.Logf("refused %d, open %d, routes %d", refused, len(identityOpenRoutes), len(routes))
	if refused < len(b6152Moves) {
		t.Fatalf("%d money routes refused; want at least the %d the item names", refused, len(b6152Moves))
	}
	// The identity check itself is never refused.
	h := b6152Router(t, nil, b6152Unconfirmed{true}, identityOpenRoutes, &auth.AuthContext{WorkspaceID: "ws-b6152"})
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/v1/workspaces/ws-b6152/verification", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s the identity check by an unconfirmed account = %d; want it reached", m, rec.Code)
		}
	}
}

// Every path the item names that moves money is refused, not named open, and is a route Lens registers.
func TestB6152_EveryPathThatMovesMoneyIsRefused(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range b597MoneyRoutes(t) {
		registered[r.method+" "+blankRouteParams(r.pattern)] = true
	}
	for _, m := range b6152Moves {
		if !registered[m] {
			t.Errorf("%s moves money and cmd/lens does not register it", m)
		}
		if why, open := identityOpenRoutes[m]; open {
			t.Errorf("%s moves money and identityOpenRoutes leaves it open (%q)", m, why)
		}
	}
	for k := range identityOpenRoutes {
		if !registered[k] {
			t.Errorf("identityOpenRoutes names %s, which cmd/lens does not register", k)
		}
	}
}

// Mutation: a path that moves money left open — the check taken out of it — turns the sweep red.
func TestB6152_ThePathLeftOpenTurnsTheSweepRed(t *testing.T) {
	routes := b597MoneyRoutes(t)
	mutated := map[string]string{"POST /v1/workspaces/{}/agents/{}/send": "the mutation"}
	for k, v := range identityOpenRoutes {
		mutated[k] = v
	}
	h := b6152Router(t, routes, b6152Unconfirmed{true}, mutated, &auth.AuthContext{WorkspaceID: "ws-b6152"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-b6152/agents/a1/send", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("with the send left open the gate still refused it: %d", rec.Code)
	}
	// The sweep, judged against the real open list, sees the send reached.
	wrong, _ := b6152Sweep(t, routes, identityOpenRoutes)
	if len(wrong) != 0 {
		t.Fatalf("the unmutated sweep is red: %v", wrong)
	}
	mutatedWrong := 0
	for _, rt := range routes {
		key := rt.method + " " + blankRouteParams(rt.pattern)
		if _, open := identityOpenRoutes[key]; open || rt.method == http.MethodGet {
			continue
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rt.method, b597Param.ReplaceAllString(rt.pattern, "ws-b6152"), nil))
		if rec.Code != http.StatusForbidden {
			mutatedWrong++
		}
	}
	if mutatedWrong != 1 {
		t.Fatalf("with one path left open the sweep found %d routes reached; want exactly that one", mutatedWrong)
	}
}

// A confirmed account and the operator reach every money route.
func TestB6152_AConfirmedAccountAndTheOperatorReachEveryMoneyRoute(t *testing.T) {
	routes := b597MoneyRoutes(t)
	for name, h := range map[string]http.Handler{
		"a confirmed account": b6152Router(t, routes, b6152Unconfirmed{false}, identityOpenRoutes, &auth.AuthContext{WorkspaceID: "ws-b6152"}),
		"the operator":        b6152Router(t, routes, b6152Unconfirmed{true}, identityOpenRoutes, &auth.AuthContext{IsAdmin: true}),
	} {
		for _, rt := range routes {
			rec := httptest.NewRecorder()
			path := b597Param.ReplaceAllString(rt.pattern, "ws-b6152")
			if name == "the operator" && !strings.Contains(rt.pattern, "{wsID}") {
				continue // the operator has no workspace of its own: a route without one judges nobody
			}
			h.ServeHTTP(rec, httptest.NewRequest(rt.method, path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s calling %s %s = %d %s; want it reached", name, rt.method, rt.pattern, rec.Code, rec.Body.String())
			}
		}
	}
}

// The gate is on the authed group, after the kind-of-money gate, so it judges every money route an owner or an agent calls.
func TestB6152_TheGateIsOnTheAuthedGroup(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	gate, kind := strings.Index(s, "authed.Use(identityGate(dualToken))"), strings.Index(s, "authed.Use(moneyKindGate(dualToken))")
	if gate < 0 || kind < 0 || gate < kind {
		t.Fatal("main.go does not put identityGate(dualToken) on the authed group after moneyKindGate")
	}
}

// The economy's own refusal, written by any handler through writeJSONErr, carries the same code.
func TestB6152_TheEconomysRefusalCarriesTheCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONErr(rec, http.StatusForbidden, (&economy.CapabilityRefusal{Identity: &economy.IdentityNeeded{}}).Error())
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["code"] != "VERIFICATION_NEEDED" || out["error"] != economy.ConfirmIdentitySentence {
		t.Fatalf("the economy's refusal through writeJSONErr = %s; want code VERIFICATION_NEEDED and the sentence", rec.Body.String())
	}
}
