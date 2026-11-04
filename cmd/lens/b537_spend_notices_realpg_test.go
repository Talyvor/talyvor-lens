package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
)

// b537Pusher is a browser's push service: it keeps the kind of each push sent.
type b537Pusher struct{ kinds []string }

func (p *b537Pusher) Send(_ context.Context, _, _, _ string, payload []byte) error {
	var m struct{ Kind string }
	_ = json.Unmarshal(payload, &m)
	p.kinds = append(p.kinds, m.Kind)
	return nil
}
func (p *b537Pusher) Allows(string) bool { return true }
func (p *b537Pusher) PublicKey() string  { return "" }

// b537Phones is the phones' push services: they keep the kind of each push sent.
type b537Phones struct{ kinds []string }

func (p *b537Phones) Send(_ context.Context, _, _ string, m nativepush.Message) error {
	p.kinds = append(p.kinds, m.Data["kind"])
	return nil
}

// B53.7 — a fake-issuer payment sends one notice with what is left of its budget; quiet hours hold it until they end
// while an unusual payment goes at once; a second Coach tip the same day waits and is dropped when the day is over;
// a kind switched off makes no notice.
func TestB537_NoticesArriveAtTheRightTime(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b537"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b537"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			issuer = issuerfake.New(store.AuthorizeCardCharge)
			if err := layer.Register(issuer.Adapter()); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	pusher := &b537Pusher{}
	b.store.SetApprovalAuth(passkey.RelyingParty{}, pusher)
	phones := &b537Phones{} // the fixture's phone, with its push token
	b.store.SetNativePush(phones)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_push_subscriptions (endpoint, workspace_id, p256dh, auth) VALUES ($1, $2, 'k', 'a')`,
		"https://fcm.googleapis.com/b537", ws); err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).
		Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "b537-top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -100000}, {AccountID: b.acct.ID, AmountMinor: 100000}}}); err != nil {
		t.Fatal(err)
	}
	charge := func(merchant string, minor int64) string { // a card at merchant charged minor; answers its entry
		t.Helper()
		batch, err := b.store.RequestCardBatch(ctx, ws, economy.CardBatch{AgentID: b.acct.AgentID, Currency: "GBP",
			IdempotencyKey: fmt.Sprintf("b537-%s-%d", merchant, minor), Cards: []economy.MoneyCard{{Merchant: merchant, CapMinor: minor}}})
		if err != nil {
			t.Fatal(err)
		}
		if batch, err = b.store.ApproveCardBatch(ctx, ws, batch.ID, "owner"); err != nil {
			t.Fatal(err)
		}
		c := batch.Cards[0]
		if ok, reason, err := issuer.Charge(ctx, issuerRef(t, pool, c.ID), c.Merchant, money.Amount{Minor: minor, Asset: "GBP"}); !ok || err != nil {
			t.Fatalf("charge at %s = %v %q %v, want approved", merchant, ok, reason, err)
		}
		var entry string
		if err := pool.QueryRow(ctx, `SELECT id FROM money_entries WHERE workspace_id = $1 AND idempotency_key = 'card_charge:' || $2`, ws, c.ID).
			Scan(&entry); err != nil {
			t.Fatal(err)
		}
		return entry
	}

	r := chi.NewRouter()
	mountMoneyNoticeRoutes(r, b.store)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	// A time zone where it is about noon now, so the day's tip is due and the next two hours stay the same day.
	now := time.Now()
	zone := "Etc/UTC"
	if off := 12 - now.UTC().Hour(); off > 0 {
		zone = fmt.Sprintf("Etc/GMT-%d", off)
	} else if off < 0 {
		zone = fmt.Sprintf("Etc/GMT+%d", -off)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	settings := func(body string) {
		t.Helper()
		if w := call(http.MethodPut, "/v1/money/notices/settings", body); w.Code != http.StatusOK {
			t.Fatalf("settings %s = %d %s", body, w.Code, w.Body.String())
		}
	}
	settings(`{"timezone": "` + zone + `"}`)
	if w := call(http.MethodPut, "/v1/money/notices/settings", `{"quiet_from": "22:00"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("quiet hours with no end = %d %s, want 400", w.Code, w.Body.String())
	}
	for _, body := range []string{
		`{"agent_id": "all", "category": "eating_out", "period": "month", "amount_minor": 10000, "currency": "GBP"}`,
		`{"agent_id": "all", "period": "month", "amount_minor": 10000, "currency": "GBP"}`,
	} {
		var in economy.SpendBudget
		_ = json.Unmarshal([]byte(body), &in)
		if _, err := b.store.SetSpendBudget(ctx, ws, in, "owner", now); err != nil {
			t.Fatal(err)
		}
	}
	sweep := func(at time.Time) {
		t.Helper()
		if _, err := b.store.NotifySpending(ctx, ws, nil, "", at); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		Kind, Subject, Status, Held, Body string
		Data                              map[string]any
	}
	notices := func(kind string) []row {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT kind, subject, status, held, body, data FROM spend_notices WHERE workspace_id = $1 AND kind = $2
			ORDER BY subject`, ws, kind)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var n row
			if err := rows.Scan(&n.Kind, &n.Subject, &n.Status, &n.Held, &n.Body, &n.Data); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	payment := func(entry string) row {
		t.Helper()
		for _, n := range notices(economy.NoticePayment) {
			if n.Subject == entry {
				return n
			}
		}
		t.Fatalf("no payment notice for %s", entry)
		return row{}
	}
	pushed := func(kind string) int {
		n := 0
		for _, k := range pusher.kinds {
			if k == kind {
				n++
			}
		}
		return n
	}

	// A £60 lunch: one notice, sent at once, with £40 left of the month's eating-out budget (and 60% of both budgets).
	lunch := charge("Pret A Manger", 6000)
	sweep(time.Now())
	sweep(time.Now()) // a second sweep sends nothing again
	if got := notices(economy.NoticePayment); len(got) != 1 {
		t.Fatalf("payment notices = %+v, want one", got)
	}
	n := payment(lunch)
	if n.Status != "sent" || !strings.Contains(n.Body, "40.00 GBP left of this month's eating out budget") || n.Data["budget_left_minor"] != float64(4000) {
		t.Fatalf("the lunch's notice = %+v, want sent with 40.00 GBP left", n)
	}
	if pushed(economy.NoticePayment) != 1 || strings.Count(strings.Join(phones.kinds, " "), economy.NoticePayment) != 1 {
		t.Fatalf("payment pushes = browser %v, phone %v; want one each", pusher.kinds, phones.kinds)
	}
	// Both budgets are past half: two tips, and only the first goes today.
	tips, heldTip := notices(economy.NoticeTip), ""
	for _, n := range tips {
		if n.Status == "waiting" && n.Held == economy.NoticeHeldTip {
			heldTip = n.Subject
		}
	}
	if len(tips) != 2 || heldTip == "" || pushed(economy.NoticeTip) != 1 {
		t.Fatalf("tips = %+v, pushes %v; want one sent and the other held for the day", tips, pusher.kinds)
	}
	tip := func() row {
		for _, n := range notices(economy.NoticeTip) {
			if n.Subject == heldTip {
				return n
			}
		}
		return row{}
	}

	// Payment notices switched off: a payment makes none.
	settings(`{"timezone": "` + zone + `", "off": ["payment"]}`)
	shop := charge("Tesco", 100)
	sweep(time.Now())
	for _, n := range notices(economy.NoticePayment) {
		if n.Subject == shop {
			t.Fatalf("a payment with payment notices off made %+v", n)
		}
	}

	// Quiet hours from an hour ago to an hour from now: a £10 lunch waits; a bet, unusual, goes at once though a tip
	// was sent today.
	local := time.Now().In(loc)
	settings(`{"timezone": "` + zone + `", "quiet_from": "` + local.Add(-time.Hour).Format("15:04") + `", "quiet_until": "` +
		local.Add(time.Hour).Format("15:04") + `"}`)
	unusualBefore := pushed(economy.NoticeUnusual)
	second := charge("Pret A Manger", 1000)
	charge("Bet365", 500)
	quietAt := time.Now()
	sweep(quietAt)
	if n := payment(second); n.Status != "waiting" || n.Held != economy.NoticeHeldQuiet || n.Data["budget_left_minor"] != float64(3000) {
		t.Fatalf("a payment in quiet hours = %+v, want waiting for them to end, 30.00 GBP left", n)
	}
	var gambling bool
	for _, n := range notices(economy.NoticeUnusual) {
		gambling = gambling || n.Data["rule"] == economy.FindingGambling && n.Status == "sent"
	}
	if !gambling || pushed(economy.NoticeUnusual) == unusualBefore {
		t.Fatalf("unusual notices = %+v, want the bet sent in quiet hours", notices(economy.NoticeUnusual))
	}

	// Quiet hours over: the lunch goes; the day's second tip still waits.
	sweep(quietAt.Add(90 * time.Minute))
	if n := payment(second); n.Status != "sent" {
		t.Fatalf("the lunch after quiet hours = %+v, want sent", n)
	}
	if n := tip(); n.Status != "waiting" || n.Held != economy.NoticeHeldTip {
		t.Fatalf("the second tip after quiet hours = %+v, want still held for the day", n)
	}
	// The next day it is dropped, not sent late.
	sweep(quietAt.Add(24 * time.Hour))
	if n := tip(); n.Status != "dropped" {
		t.Fatalf("yesterday's held tip = %+v, want dropped", n)
	}

	// The owner reads the notices and the settings.
	w := call(http.MethodGet, "/v1/money/notices", "")
	var got struct {
		Settings economy.SpendNoticeSettings
		Notices  []economy.SpendNotice
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil || got.Settings.QuietFrom == "" || len(got.Notices) == 0 {
		t.Fatalf("GET /v1/money/notices = %d %s", w.Code, w.Body.String())
	}
}
