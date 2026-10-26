package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B46.3 — a sandbox webhook signed with its secret posts its money into the company account it names, once: the
// ledger has one entry, a replayed id is refused and posts nothing, a bad signature posts nothing, and the webhook is
// a link of the hash-chained audit trail.
func TestMoneyWebhook_ASignedCreditPostsOnceToTheLedger(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-b463-webhook", "whsec_b463"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, secret), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	body := []byte(fmt.Sprintf(`{"id":"evt_b463","to":%q,"amount":{"amount_minor":4200,"asset":"GBP"},"partner_ref":"test_in_b463","payer":"Acme Supplies Ltd"}`,
		company.PartnerAccountRef))
	post := func(sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
		req.Header.Set(money.SandboxSignatureHeader, sig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := post(money.SignSandbox("whsec_wrong", body, time.Now())); code != http.StatusUnauthorized {
		t.Fatalf("a bad signature = %d, want 401", code)
	}
	if code := post(money.SignSandbox(secret, body, time.Now())); code != http.StatusOK {
		t.Fatalf("a signed credit = %d, want 200", code)
	}
	if code := post(money.SignSandbox(secret, body, time.Now())); code != http.StatusConflict {
		t.Fatalf("the same id again = %d, want 409", code)
	}

	rows, err := pool.Query(ctx, `SELECT p.account_id, p.amount_minor, p.currency, p.funding FROM money_entries e
		JOIN money_postings p ON p.entry_id = e.id WHERE e.workspace_id = $1 AND e.partner_ref = 'test_in_b463' ORDER BY p.line`, ws)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var acct, cur, funding string
		var amount int64
		if err := rows.Scan(&acct, &amount, &cur, &funding); err != nil {
			t.Fatal(err)
		}
		if acct == company.ID {
			acct = "company"
		} else {
			acct = "partner"
		}
		got = append(got, fmt.Sprintf("%s %d %s %s", acct, amount, cur, funding))
	}
	rows.Close()
	if want := "partner -4200 GBP test; company 4200 GBP test"; strings.Join(got, "; ") != want {
		t.Fatalf("ledger postings = %q, want %q", got, want)
	}
	chain, err := store.AuditRecord(ctx, ws, "money_webhook:sandbox:evt_b463")
	if err != nil || len(chain) != 1 || chain[0].Kind != "money.webhook.credit" || economy.VerifyAuditChain(chain) != nil {
		t.Fatalf("audit chain = %+v, %v; want the one webhook link", chain, err)
	}
}
