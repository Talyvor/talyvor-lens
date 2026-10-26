package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/swan"
	"github.com/talyvor/lens/internal/money/swan/swanfake"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// sandboxMoneyLayer is the money layer as main.go builds it: the sandbox on reg's Test partners, the store's limits,
// audit trail and ledger.
func sandboxMoneyLayer(t *testing.T, pool *pgxpool.Pool, store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
	t.Helper()
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b468"), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

// b468Pushes keeps what is pushed to the workspace's phones.
type b468Pushes chan nativepush.Message

func (p b468Pushes) Send(_ context.Context, _, _ string, m nativepush.Message) error {
	p <- m
	return nil
}

func (p b468Pushes) next(t *testing.T) nativepush.Message {
	t.Helper()
	select {
	case m := <-p:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was pushed to the workspace's phones")
	}
	return nativepush.Message{}
}

type b468Cleared struct{}

func (b468Cleared) CapabilityCleared(context.Context, string) (bool, error) { return true, nil }

// b468Payer is a workspace whose agent has an account in currency holding 500.00 of test money, a payee, and a phone
// that is told when a payment is not made.
type b468Payer struct {
	store  *economy.DualTokenStore
	acct   economy.CurrencyAccount
	payee  economy.OutsidePayee
	pushes b468Pushes
}

func newB468Payer(t *testing.T, pool *pgxpool.Pool, ws, currency string, payee economy.OutsidePayee, layer func(*economy.DualTokenStore, *partners.Registry) *money.Registry) b468Payer {
	t.Helper()
	ctx := context.Background()
	reg := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, reg)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(reg)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetMoneyLayer(layer(store, reg))
	pushes := make(b468Pushes, 4)
	store.SetNativePush(pushes)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation, push_token)
		VALUES ($1, $2, 'test', '\x00', '\x00', 'tok')`, "dev_"+ws, ws); err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, ws, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, ws, "", currency, "test"); err != nil {
		t.Fatal(err)
	}
	acct, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, currency, "test")
	if err != nil {
		t.Fatal(err)
	}
	if payee, err = store.CreateOutsidePayee(ctx, ws, payee); err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = $2`, ws, currency).
		Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -50000}, {AccountID: acct.ID, AmountMinor: 50000}}}); err != nil {
		t.Fatal(err)
	}
	return b468Payer{store: store, acct: acct, payee: payee, pushes: pushes}
}

func (b b468Payer) pay(t *testing.T, key string, minor int64) economy.OutsidePayment {
	t.Helper()
	p, err := b.store.PayOutside(context.Background(), b.acct.WorkspaceID, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: b.payee.ID,
		Currency: b.acct.Currency, AmountMinor: minor, Reference: "Invoice " + key, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("pay %s: %v", key, err)
	}
	return p
}

func (b b468Payer) balance(t *testing.T, want int64) {
	t.Helper()
	if got, err := b.store.Balance(context.Background(), b.acct.WorkspaceID, b.acct.ID); err != nil || got.TestMinor != want {
		t.Fatalf("the agent's account holds %+v (%v), want %d", got, err, want)
	}
}

// postings is every posting of the payment under key, in order: "kind purpose amount".
func b468Postings(t *testing.T, pool *pgxpool.Pool, ws, key string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT e.kind, a.purpose, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		JOIN money_accounts a ON a.id = p.account_id WHERE e.workspace_id = $1 AND e.idempotency_key LIKE 'payment_out%:' || $2 ORDER BY e.created_at, p.line`, ws, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, purpose string
		var amount int64
		_ = rows.Scan(&kind, &purpose, &amount)
		out = append(out, kind+" "+purpose+" "+jsonOf(amount))
	}
	return strings.Join(out, "; ")
}

// auditKinds is the kinds of subject's audit links, in order, each with whether its body holds every one of has.
func b468Audit(t *testing.T, store *economy.DualTokenStore, ws, subject string, has ...string) string {
	t.Helper()
	entries, err := store.AuditRecord(context.Background(), ws, subject)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		k := e.Kind
		for _, h := range has {
			if !strings.Contains(e.Body, h) {
				k += " (without " + h + ")"
			}
		}
		out = append(out, k)
	}
	return strings.Join(out, "; ")
}

// B46.8 — through the sandbox adapter: a payment out is the money layer's transfer, made under the payment's own id and
// linked into the audit trail with the partner's reference; one the partner refuses releases its hold and tells the
// workspace's phones in plain words.
func TestB468_APaymentOutIsTheMoneyLayersTransferAndARefusalTellsThePhone(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b468-sandbox"
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})

	p := b.pay(t, "ok", 2000)
	if p.Status != economy.PaymentCompleted || p.Partner != money.SandboxName || p.PartnerRef == "" {
		t.Fatalf("pay £20.00 = %+v, want completed by the sandbox", p)
	}
	if got, want := b468Audit(t, b.store, ws, "money:"+p.ID, `"outcome":"ok"`, `"partner_ref":"`+p.PartnerRef+`"`), "money.transfers.outbound"; got != want {
		t.Errorf("the payment's adapter call in the audit trail = %q, want %q with the partner's reference", got, want)
	}

	p = b.pay(t, "refused", 2013)
	if p.Status != economy.PaymentFailed || p.Detail == "" {
		t.Fatalf("pay £20.13 = %+v, want failed with the partner's reason", p)
	}
	if got, want := b468Postings(t, pool, ws, "refused"), "payment_out_hold agent -2013; payment_out_hold hold 2013; payment_out_failed hold -2013; payment_out_failed agent 2013"; got != want {
		t.Errorf("£20.13 posted %q, want %q", got, want)
	}
	b.balance(t, 48000)
	if got, want := b468Audit(t, b.store, ws, "money:"+p.ID, `"outcome":"refused"`), "money.transfers.outbound"; got != want {
		t.Errorf("the refused call in the audit trail = %q, want %q", got, want)
	}
	if m := b.pushes.next(t); m.Title != "Payment not made" || !strings.Contains(m.Body, "GBP 20.13 to Ada Lovelace wasn't paid") ||
		!strings.Contains(m.Body, "money is back") || m.Data["payment_id"] != p.ID {
		t.Fatalf("pushed %+v, want the person told £20.13 to Ada Lovelace was not paid and the money is back", m)
	}
}

// B46.8 — through the fake Swan, switched on for transfers to Germany: an agent's SEPA payment is initiated once
// however often it is asked, waits held until Swan's webhook says it was booked, and is settled once — the same
// webhook again is refused, and another about the same booking changes nothing; one Swan rejects later releases its
// hold and tells the phone.
func TestB468_ASEPAPaymentOnSwanIsMadeOnceAndItsWebhookSettlesItOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b468-swan"
	f := swanfake.New(t)
	var layer *money.Registry
	b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			var err error
			layer, err = money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, ""), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool), Switches: money.SwitchSet{
					{Capability: money.CapAccounts, Country: "FR", Partner: swan.Name}: true, {Capability: money.CapTransfers, Country: "DE", Partner: swan.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			a, err := swan.New(swan.Config{APIURL: f.URL, TokenURL: f.TokenURL, ClientID: f.ClientID, ClientSecret: f.ClientSecret, ProjectID: f.ProjectID,
				ConsentKey: f.ConsentKey, ConsentRedirectURL: "https://app.talyvor.test/consent", WebhookSecret: f.WebhookSecret, Holders: b468Holder(f.HolderID)})
			if err != nil {
				t.Fatal(err)
			}
			if err := layer.Register(a); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	// The company's euros are held at Swan: its account there holds €500.00, as the ledger does.
	accounts, err := layer.Accounts(ctx, money.Route{Workspace: ws, Country: "FR", Capability: economy.CapabilityPaymentsOut})
	if err != nil {
		t.Fatal(err)
	}
	at, err := accounts.Open(ctx, money.OpenAccount{Key: "b468-eur", Holder: ws, Asset: money.EUR, Country: "FR"})
	if err != nil {
		t.Fatal(err)
	}
	f.Deposit(at.Ref, "tx-in-b468", money.Amount{Minor: 50000, Asset: money.EUR})
	if _, err := pool.Exec(ctx, `UPDATE money_accounts SET partner_account_ref = $2 WHERE workspace_id = $1 AND currency = 'EUR' AND partner_account_ref <> ''`,
		ws, at.Ref); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	notify := func(eventType, id, tx string) int {
		t.Helper()
		body, h := f.NotifyEvent(eventType, id, tx)
		req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/swan", bytes.NewReader(body))
		req.Header = h
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	p := b.pay(t, "sepa", 4000)
	if again := b.pay(t, "sepa", 4000); p.Status != economy.PaymentSent || p.Partner != swan.Name || p.PartnerRef == "" || again.ID != p.ID || f.Sent() != 1 {
		t.Fatalf("pay €40.00 twice = %+v then %+v with Swan paying %d; want one payment sent to Swan, held until it is booked", p, again, f.Sent())
	}
	if read, err := b.store.RefreshOutsidePayment(ctx, ws, "", p.ID); err != nil || read.Status != economy.PaymentSent || f.Sent() != 1 {
		t.Fatalf("reading it = %+v, %v, with Swan paying %d; want still sent, waiting for Swan's webhook", read, err, f.Sent())
	}
	booked := f.Settle(p.PartnerRef, "Booked")
	for i, c := range []struct {
		id   string
		want int
	}{{"evt-b468-booked", http.StatusOK}, {"evt-b468-booked", http.StatusConflict}, {"evt-b468-booked-again", http.StatusOK}} {
		if got := notify("Transaction.Booked", c.id, booked); got != c.want {
			t.Fatalf("booking webhook %d = %d, want %d", i+1, got, c.want)
		}
	}
	if got, want := b468Postings(t, pool, ws, "sepa"), "payment_out_hold agent -4000; payment_out_hold hold 4000; payment_out hold -4000; payment_out partner 4000"; got != want {
		t.Errorf("€40.00 posted %q, want the hold and the payment out once", got)
	}
	if read, err := b.store.RefreshOutsidePayment(ctx, ws, "", p.ID); err != nil || read.Status != economy.PaymentCompleted {
		t.Fatalf("after Swan booked it = %+v, %v; want completed", read, err)
	}
	b.balance(t, 46000)
	if got, want := b468Audit(t, b.store, ws, "money:"+p.ID, `"partner":"swan"`, `"partner_ref":"`+p.PartnerRef+`"`), "money.transfers.outbound"; got != want {
		t.Errorf("Swan's call in the audit trail = %q, want %q", got, want)
	}
	if got, want := b468Audit(t, b.store, ws, "money_webhook:swan:evt-b468-booked", `"payment_id":"`+p.ID+`"`, `"partner_ref":"`+p.PartnerRef+`"`), "money.webhook.transfer"; got != want {
		t.Errorf("Swan's confirmation in the audit trail = %q, want %q linked to the payment", got, want)
	}

	p = b.pay(t, "sepa-rejected", 3000)
	if p.Status != economy.PaymentSent {
		t.Fatalf("pay €30.00 = %+v, want sent", p)
	}
	if got := notify("Transaction.Rejected", "evt-b468-rejected", f.Settle(p.PartnerRef, "Rejected")); got != http.StatusOK {
		t.Fatalf("rejection webhook = %d, want 200", got)
	}
	if got, want := b468Postings(t, pool, ws, "sepa-rejected"), "payment_out_hold agent -3000; payment_out_hold hold 3000; payment_out_failed hold -3000; payment_out_failed agent 3000"; got != want {
		t.Errorf("€30.00 rejected by Swan posted %q, want %q", got, want)
	}
	b.balance(t, 46000)
	if m := b.pushes.next(t); m.Title != "Payment not made" || !strings.Contains(m.Body, "EUR 30.00 to Grace Hopper") {
		t.Fatalf("pushed %+v, want the person told €30.00 to Grace Hopper was not paid", m)
	}
}

type b468Holder string

func (h b468Holder) AccountHolder(context.Context, string) (string, error) { return string(h), nil }
