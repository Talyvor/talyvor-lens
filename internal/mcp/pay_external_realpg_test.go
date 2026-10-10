package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.17 — an agent pays a saved payee outside Talyvor with wallet_pay_external, from its own account: the money is
// held and then moves out through the partner.
func TestWalletPayExternal_AnAgentPaysFromItsOwnAccount(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws = "ws-b3017-mcp"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	a, err := store.CreateAgent(ctx, ws, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, a.ID, "key-b3017"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, ws, "", "USD", "test"); err != nil {
		t.Fatal(err)
	}
	acct, err := store.OpenCurrencyAccount(ctx, ws, a.ID, "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Grace Hopper", Country: "US", RoutingNumber: "021000021", AccountNumber: "123456789"})
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -10000}, {AccountID: acct.ID, AmountMinor: 10000}}}); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wallet_pay_external","arguments":{"payee_id":"` + payee.ID +
		`","amount_minor":2500,"currency":"USD","reference":"Hosting","idempotency_key":"k1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-b3017", WorkspaceID: ws}))
	w := httptest.NewRecorder()
	srv.HandleRPC(w, req)
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	var paid economy.OutsidePayment
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Result.Content) == 0 || resp.Result.IsError ||
		json.Unmarshal([]byte(resp.Result.Content[0].Text), &paid) != nil || paid.Status != economy.PaymentCompleted || paid.AccountID != acct.ID {
		t.Fatalf("wallet_pay_external = %s; want completed from the agent's own account %s", w.Body.String(), acct.ID)
	}
	var out, hold, sent int64
	if err := pool.QueryRow(ctx, `SELECT
		COALESCE(sum(p.amount_minor) FILTER (WHERE p.account_id = $2), 0),
		COALESCE(sum(p.amount_minor) FILTER (WHERE e.kind LIKE 'payment_out%' AND p.account_id <> $2 AND p.account_id <> $3), 0),
		COALESCE(sum(p.amount_minor) FILTER (WHERE e.kind = 'payment_out' AND p.account_id = $3), 0)
		FROM money_entries e JOIN money_postings p ON p.entry_id = e.id WHERE e.workspace_id = $1 AND e.kind LIKE 'payment_out%'`,
		ws, acct.ID, partnerAccount).Scan(&out, &hold, &sent); err != nil || out != -2500 || hold != 0 || sent != 2500 {
		t.Fatalf("the payment posted %d to the agent, %d net to the hold, %d out through the partner (%v); want -2500, 0, 2500", out, hold, sent, err)
	}
}
