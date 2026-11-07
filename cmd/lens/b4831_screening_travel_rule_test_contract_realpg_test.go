package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B48.31 — what the nightly needs to test screening and the Travel Rule end to end on a Lens in test mode: the owner
// puts test money into an agent's USDC wallet and the agent can spend it on the sandbox; the Test screening provider
// always flags one address and the Test Travel Rule provider always names another; and a payment to an address says
// where it stands under the Travel Rule.

const b4831Flagged, b4831Exchange = "0x7e57000000000000000000000000000000000001", "0x7e57000000000000000000000000000000000002"

// b4831Lens is a workspace on the sandbox as the deployed Lens is, with LENS_TRAVEL_RULE travel, and an agent whose
// USDC wallet is wallet.
type b4831Lens struct {
	store  *economy.DualTokenStore
	routes chi.Router
	agent  string
	wallet string
}

func newB4831Lens(t *testing.T, pool *pgxpool.Pool, ws, travel string) b4831Lens {
	t.Helper()
	ctx := context.Background()
	reg := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, reg)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(reg)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	tr, err := newTravelRule(travel)
	if err != nil {
		t.Fatal(err)
	}
	if tr != nil {
		store.SetTravelRule(tr)
	}
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, ""), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	agent, err := store.CreateAgent(ctx, ws, "Screened payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.AgentWallet(ctx, ws, agent.ID)
	if err != nil || w.Asset != money.USDC || w.Partner != money.SandboxName {
		t.Fatalf("the agent's wallet = %+v, %v; want a USDC wallet on the sandbox", w, err)
	}
	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	mountMoneyPaymentRoutes(r, store)
	return b4831Lens{store: store, routes: r, agent: agent.ID, wallet: w.AccountID}
}

func (l b4831Lens) owner(t *testing.T, ws, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(),
		&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	l.routes.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func (l b4831Lens) fund(t *testing.T, ws string, minor int64, key string) {
	t.Helper()
	if code, body := l.owner(t, ws, http.MethodPost, "/v1/money/accounts/"+l.wallet+"/test-money",
		`{"amount_minor":`+jsonOf(minor)+`,"idempotency_key":"`+key+`"}`); code != http.StatusCreated {
		t.Fatalf("the owner puts test money into the agent's USDC wallet = %d %s, want 201", code, body)
	}
}

func (l b4831Lens) pay(ws string, payee economy.OutsidePayee, key string, minor int64) (economy.OutsidePayment, error) {
	return l.store.PayOutside(context.Background(), ws, economy.OutsidePayment{AgentID: l.agent, PayeeID: payee.ID, Currency: money.USDC,
		AmountMinor: minor, Reference: "Invoice " + key, IdempotencyKey: key})
}

// (1) Test money goes into an agent's USDC wallet once per key, funding test, and the agent can spend it on the sandbox.
func TestB4831_TheOwnerPutsTestMoneyIntoAnAgentsUSDCWalletAndTheAgentSpendsIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4831-fund"
	l := newB4831Lens(t, pool, ws, "")
	for range 2 {
		l.fund(t, ws, 5000_000000, "k1")
	}
	var got []string
	rows, err := pool.Query(ctx, `SELECT a.purpose || ' ' || p.amount_minor || ' ' || p.funding FROM money_postings p
		JOIN money_accounts a ON a.id = p.account_id WHERE a.workspace_id = $1 ORDER BY p.amount_minor`, ws)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	rows.Close()
	if want := "partner -5000000000 test; agent 5000000000 test"; strings.Join(got, "; ") != want {
		t.Fatalf("the ledger holds %v; want %s: one payment in, into the agent's wallet, posted once", got, want)
	}

	p, err := l.pay(ws, b487Payer{store: l.store}.save(t, pool, ws, "Own wallet", "0x00000000000000000000000000000000000B4831"), "spend", 10_000000)
	if err != nil || p.Status != economy.PaymentCompleted {
		t.Fatalf("the agent pays 10 USDC out of that money = %+v, %v; want completed: the sandbox holds what the ledger does", p, err)
	}
	if got := b468Postings(t, pool, ws, "spend"); got != "payment_out_hold agent -10000000; payment_out_hold hold 10000000; payment_out hold -10000000; payment_out partner 10000000" {
		t.Fatalf("the payment posted %q; want held and paid out, never released", got)
	}
}

// (2) The Test screening provider always flags 0x7e57…0001 as a mixer: a payment to it is refused with nothing held.
func TestB4831_TheTestScreeningProviderAlwaysFlagsItsMixerAddress(t *testing.T) {
	if s, err := (partners.TestScreeningProvider{}).ScreenPayment(context.Background(), partners.PaymentScreen{ID: "p1",
		Amount: partners.Money{Minor: 1, Currency: "USDC"}, Payee: strings.ToUpper(b4831Flagged[:2]) + b4831Flagged[2:]}); err != nil ||
		s.Outcome != partners.ScreenHit || len(s.Matches) != 1 || s.Matches[0].List != partners.AddressMixer {
		t.Fatalf("screen %s = %+v, %v; want a hit as a mixer", b4831Flagged, s, err)
	}
	pool := agentRoutesDB(t)
	const ws = "ws-b4831-flagged"
	l := newB4831Lens(t, pool, ws, "")
	l.fund(t, ws, 100_000000, "k1")
	if _, err := l.pay(ws, b487Payer{store: l.store}.save(t, pool, ws, "Flagged", b4831Flagged), "flagged", 10_000000); !errors.As(err, new(*screening.Refusal)) {
		t.Fatalf("pay the flagged address = %v, want refused by screening", err)
	}
	if got := b468Postings(t, pool, ws, "flagged"); got != "" {
		t.Fatalf("the refused payment posted %q, want nothing held", got)
	}
}

// (3) The Test Travel Rule provider always names 0x7e57…0002 as "Test Exchange", beside LENS_TRAVEL_RULE's own list.
func TestB4831_TheTestTravelRuleProviderAlwaysNamesItsExchange(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4831-exchange"
	l := newB4831Lens(t, pool, ws, "test:0x00000000000000000000000000000000000B4832=Other Exchange")
	l.fund(t, ws, 100_000000, "k1")
	p, err := l.pay(ws, b487Payer{store: l.store}.save(t, pool, ws, "Exchange customer", b4831Exchange), "vasp", 10_000000)
	if err != nil {
		t.Fatalf("pay the exchange's address = %v", err)
	}
	var vasp, ref string
	if err := pool.QueryRow(ctx, `SELECT vasp, provider_ref FROM travel_rule_transfers WHERE payment_id = $1`, p.ID).Scan(&vasp, &ref); err != nil ||
		vasp != "Test Exchange" || ref == "" {
		t.Fatalf("its Travel Rule record = %q %q, %v; want sent to Test Exchange", vasp, ref, err)
	}
	other := &partners.TestTravelRuleProvider{VASPs: map[string]string{"0x00000000000000000000000000000000000b4832": "Other Exchange"}}
	if got, _ := other.Provider(ctx, "base", "0x00000000000000000000000000000000000B4832"); got != "Other Exchange" {
		t.Fatalf("the configured address's provider = %q, want Other Exchange: the test address is beside the list, not instead of it", got)
	}
}

// (4) A payment to an address says where it stands under the Travel Rule, on the list and on its own.
func TestB4831_APaymentToAnAddressCarriesItsTravelRuleStatus(t *testing.T) {
	pool := agentRoutesDB(t)
	travelRule := func(l b4831Lens, ws, id string) map[string]string {
		t.Helper()
		code, body := l.owner(t, ws, http.MethodGet, "/v1/money/payments/"+id, "")
		var one struct {
			TravelRule map[string]string `json:"travel_rule"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &one) != nil {
			t.Fatalf("GET the payment = %d %s", code, body)
		}
		code, body = l.owner(t, ws, http.MethodGet, "/v1/money/payments", "")
		var list struct {
			Payments []struct {
				ID         string            `json:"id"`
				TravelRule map[string]string `json:"travel_rule"`
			} `json:"payments"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil {
			t.Fatalf("GET the payments = %d %s", code, body)
		}
		for _, p := range list.Payments {
			if p.ID == id && jsonOf(p.TravelRule) != jsonOf(one.TravelRule) {
				t.Fatalf("the list says %v and the payment says %v", p.TravelRule, one.TravelRule)
			}
		}
		return one.TravelRule
	}

	const off = "ws-b4831-off"
	l := newB4831Lens(t, pool, off, "")
	l.fund(t, off, 100_000000, "k1")
	p, err := l.pay(off, b487Payer{store: l.store}.save(t, pool, off, "Exchange customer", b4831Exchange), "off", 10_000000)
	if err != nil {
		t.Fatal(err)
	}
	if got := travelRule(l, off, p.ID); jsonOf(got) != `{"status":"off"}` {
		t.Fatalf("with LENS_TRAVEL_RULE unset the payment's travel_rule = %v, want off", got)
	}

	const on = "ws-b4831-on"
	l = newB4831Lens(t, pool, on, "test")
	l.fund(t, on, 100_000000, "k1")
	sent, err := l.pay(on, b487Payer{store: l.store}.save(t, pool, on, "Exchange customer", b4831Exchange), "sent", 10_000000)
	if err != nil {
		t.Fatal(err)
	}
	if got := travelRule(l, on, sent.ID); got["status"] != "sent" || got["vasp"] != "Test Exchange" || got["provider_ref"] != "tr_test_"+sent.ID {
		t.Fatalf("a payment to the exchange's address has travel_rule %v, want sent to Test Exchange with its reference", got)
	}
	own, err := l.pay(on, b487Payer{store: l.store}.save(t, pool, on, "Own wallet", "0x00000000000000000000000000000000000B4833"), "own", 10_000000)
	if err != nil {
		t.Fatal(err)
	}
	if got := travelRule(l, on, own.ID); jsonOf(got) != `{"status":"self_hosted"}` {
		t.Fatalf("a payment to an address no provider holds has travel_rule %v, want self_hosted", got)
	}
}
