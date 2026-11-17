package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/paypool"
	"github.com/talyvor/lens/internal/workspace"
)

// b631Payer is a b468Payer — an agent account in GBP holding £500.00 and one saved payee — whose settled payments
// the payments pool measures when measured is set.
func b631Payer(t *testing.T, pool *pgxpool.Pool, ws string, payee economy.OutsidePayee, measured bool) b468Payer {
	t.Helper()
	confirmedAccounts(t, pool, ws)
	b := newB468Payer(t, pool, ws, "GBP", payee, func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
		return sandboxMoneyLayer(t, pool, store, r)
	})
	if measured {
		b.store.SetPaymentsPool(paypool.New(pool))
	}
	return b
}

// b631Measure is the measure of payment id once it is written: "kind first_payer owners ruled_out avoided royalty".
func b631Measure(t *testing.T, pool *pgxpool.Pool, store *economy.DualTokenStore, id string) string {
	t.Helper()
	store.WaitPaymentsPool()
	var kind, first string
	var owners int
	var ruledOut bool
	var avoided, royalty int64
	if err := pool.QueryRow(context.Background(), `SELECT kind, first_payer_workspace_id, owners, ruled_out, cost_avoided_minor, would_royalty_minor
		FROM payments_pool_measures WHERE payment_id = $1`, id).Scan(&kind, &first, &owners, &ruledOut, &avoided, &royalty); err != nil {
		t.Fatalf("the measure of %s: %v", id, err)
	}
	if first == "" {
		first = "-"
	}
	return strings.Join([]string{kind, first, jsonOf(owners), jsonOf(ruledOut), jsonOf(avoided), jsonOf(royalty)}, " ")
}

// b631BlockedPool is a payments pool that cannot write: it waits until it is let go, then fails.
type b631BlockedPool struct{ release chan struct{} }

func (p b631BlockedPool) Measure(ctx context.Context, _ economy.SettledPayment) error {
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return errors.New("the payments pool's database is gone")
}

// B63.1 — the payments pool, measured only. Each settled Sandbox payment out and payment to a person is one row
// saying what it was: a first, an owner's own repeat (two accounts of one owner, by the identity graph, earn nothing
// from each other), another owner's repeat below and at 3 owners, or a payment inside Talyvor — with the assumed cost
// it avoided and the royalty a rule would have given the first payer. A payee is matched on its details, never its
// name. The payment's own money is byte for byte the same with measuring on and off, a measure that cannot be written
// never fails or delays the payment, and the operator's read is counts and sums by week only.
func TestB631_ThePaymentsPoolMeasuresEachSettledPaymentBesideIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// Assumed for the test: a name check 0.30, a screening 0.20, an outside transfer 1.00; the first payer's share half.
	exec(`INSERT INTO payments_pool_assumed_costs (currency, name_check_minor, screening_minor, outside_transfer_minor) VALUES ('GBP', 30, 20, 100)`)
	exec(`INSERT INTO payments_pool_settings (royalty_share_bps) VALUES (5000)`)
	ada := economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"}
	named := func(name string) economy.OutsidePayee { p := ada; p.Name = name; return p }

	const a1, a2, b, c, d = "ws-b631-a1", "ws-b631-a2", "ws-b631-b", "ws-b631-c", "ws-b631-d"
	for _, ws := range []string{a1, a2} { // one owner, two accounts
		exec(`INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES ($1, 'owner-b631-a')`, ws)
	}
	payerA1 := b631Payer(t, pool, a1, ada, true)
	payerA2 := b631Payer(t, pool, a2, named("A Lovelace"), true)
	payerB := b631Payer(t, pool, b, named("Lovelace Consulting"), true)
	payerC := b631Payer(t, pool, c, named("Ada L"), true)
	// D's payee has Ada's name and other details: it is another payee.
	payerD := b631Payer(t, pool, d, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "11223344"}, true)

	for _, step := range []struct {
		name  string
		payer b468Payer
		key   string
		want  string
	}{
		{"A1's first payment to Ada is a first", payerA1, "a1-1", "first - 1 false 0 0"},
		{"A1 again is its own repeat: the name check avoided, nobody earns", payerA1, "a1-2", "own_repeat " + a1 + " 1 false 30 0"},
		{"A2, the same owner, is the owner's repeat, ruled out by the identity graph", payerA2, "a2-1", "own_repeat " + a1 + " 1 true 30 0"},
		{"B is another owner's repeat with 2 owners: below the floor, nothing avoided", payerB, "b-1", "others_below_floor " + a1 + " 2 false 0 0"},
		{"C makes 3 owners: the name check and screening avoided, half to A1", payerC, "c-1", "others_repeat " + a1 + " 3 false 50 25"},
		{"D pays Ada's name at other details: a first, not matched on the name", payerD, "d-1", "first - 1 false 0 0"},
	} {
		p := step.payer.pay(t, step.key, 2000)
		if p.Status != economy.PaymentCompleted {
			t.Fatalf("%s: the payment is %+v, want completed", step.name, p)
		}
		if got := b631Measure(t, pool, step.payer.store, p.ID); got != step.want {
			t.Errorf("%s: measured %q, want %q", step.name, got, step.want)
		}
	}

	// Inside Talyvor: B pays Grace, a person on Talyvor, from its company account, signed with its phone's key.
	grace := workspace.FirstAccountID("test|per_b631_grace")
	exec(`INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at) VALUES ('per_b631_grace', '+447700900321', 'test|per_b631_grace', true, now())`)
	exec(`INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, 'Grace Hopper', $1, 'personal', 'hybrid')`, grace)
	exec(`INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
		VALUES ($1, 2, 'person', 'test', 'completed', 'ev-b631-grace', 'Grace Hopper'), ($2, 2, 'person', 'test', 'completed', 'ev-b631-b', 'Ada Byron')`, grace, b)
	confirmedAccounts(t, pool, grace)
	if _, err := payerB.store.OpenCurrencyAccount(ctx, grace, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	name, yes := "grace.b631", true
	if _, err := payerB.store.SetPersonFinding(ctx, "per_b631_grace", economy.PersonFindingChange{Name: &name, ByName: &yes}); err != nil {
		t.Fatal(err)
	}
	var companyGBP, partnerGBP string
	if err := pool.QueryRow(ctx, `SELECT (SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'company' AND currency = 'GBP'),
		(SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP')`, b).Scan(&companyGBP, &partnerGBP); err != nil {
		t.Fatal(err)
	}
	if _, err := payerB.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: b, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "company-top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerGBP, AmountMinor: -10_000}, {AccountID: companyGBP, AmountMinor: 10_000}}}); err != nil {
		t.Fatal(err)
	}
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	exec(`UPDATE workspace_device_keys SET public_key = $2 WHERE id = $1`, "dev_"+b, spki)
	in := economy.PersonPayment{To: "grace.b631", AmountMinor: 1_500, Currency: "GBP", Note: "Dinner", IdempotencyKey: "b-grace"}
	challenge, _, err := payerB.store.PersonPaymentChallenge(ctx, b, in)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(devicekey.Message(challenge))
	sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
	paid, err := payerB.store.PayPersonSigned(ctx, b, in, &passkey.Assertion{DeviceID: "dev_" + b, Challenge: challenge,
		Signature: base64.RawURLEncoding.EncodeToString(sig)})
	if err != nil {
		t.Fatalf("B pays Grace: %v", err)
	}
	if got, want := b631Measure(t, pool, payerB.store, paid.ID), "inside - 1 false 100 0"; got != want {
		t.Errorf("B's payment to Grace on Talyvor: measured %q, want %q (the outside transfer avoided)", got, want)
	}

	// On and off: the same payment from two accounts, one measured, moves the same money, at the same fee, to the same balance.
	off := b631Payer(t, pool, "ws-b631-off", ada, false)
	on := b631Payer(t, pool, "ws-b631-on", ada, true)
	pOff, pOn := off.pay(t, "same", 2000), on.pay(t, "same", 2000)
	on.store.WaitPaymentsPool()
	if pOff.Status != pOn.Status || pOff.AmountMinor != pOn.AmountMinor || pOff.FeeMinor != pOn.FeeMinor || pOff.PartnerCostMinor != pOn.PartnerCostMinor ||
		pOff.ValueULXC != pOn.ValueULXC || pOff.Funding != pOn.Funding {
		t.Errorf("measured off %+v, on %+v: want the same payment", pOff, pOn)
	}
	if gOff, gOn := b468Postings(t, pool, "ws-b631-off", "same"), b468Postings(t, pool, "ws-b631-on", "same"); gOff != gOn || gOff == "" {
		t.Errorf("postings measured off %q, on %q: want the same", gOff, gOn)
	}
	off.balance(t, 48000)
	on.balance(t, 48000)
	var measuredOff int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payments_pool_measures WHERE workspace_id = 'ws-b631-off'`).Scan(&measuredOff); err != nil || measuredOff != 0 {
		t.Errorf("measuring off wrote %d rows (%v), want none", measuredOff, err)
	}

	// A measure that cannot be written neither fails nor delays the payment.
	stuck := b631Payer(t, pool, "ws-b631-stuck", ada, false)
	blocked := b631BlockedPool{release: make(chan struct{})}
	stuck.store.SetPaymentsPool(blocked)
	started := time.Now()
	if p := stuck.pay(t, "stuck", 2000); p.Status != economy.PaymentCompleted || time.Since(started) > 5*time.Second {
		t.Fatalf("with the payments pool stuck the payment is %+v after %s, want completed at once", p, time.Since(started))
	}
	stuck.balance(t, 48000)
	close(blocked.release)
	stuck.store.WaitPaymentsPool()

	// The operator's read: counts and sums by week, and nothing of a single payment.
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/payments-pool", nil)
	w := httptest.NewRecorder()
	newPaymentsPoolHandler(paypool.New(pool)).ServeHTTP(w, req)
	body := w.Body.String()
	var out struct {
		Weeks []map[string]json.RawMessage `json:"weeks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil || len(out.Weeks) != 1 {
		t.Fatalf("the read = %d %s (%v), want 200 and this week", w.Code, body, err)
	}
	keys := func(m map[string]json.RawMessage) string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	week := out.Weeks[0]
	if got, want := keys(week), "currencies,firsts,inside_talyvor,others_repeats,others_repeats_below_floor,own_repeats,payments,ruled_out_by_identity_graph,week_start"; got != want {
		t.Errorf("a week holds %s, want only %s", got, want)
	}
	var sums []map[string]json.RawMessage
	_ = json.Unmarshal(week["currencies"], &sums)
	if len(sums) != 1 || keys(sums[0]) != "assumed_cost_avoided_minor,currency,largest_earner_share_bps,would_be_royalties_minor" {
		t.Fatalf("a week's sums are %s, want one currency of counts and sums only", week["currencies"])
	}
	// 6 to Ada's details and her name, 1 to Grace, and the measured one of on and off, another owner's repeat at 4 owners;
	// the stuck one wrote nothing. Avoided 30+30+50+100+50, royalties 25+25, all of them A1's.
	for k, want := range map[string]string{"payments": "8", "firsts": "2", "own_repeats": "2", "others_repeats": "2",
		"others_repeats_below_floor": "1", "inside_talyvor": "1", "ruled_out_by_identity_graph": "1"} {
		if got := string(week[k]); got != want {
			t.Errorf("the week's %s = %s, want %s", k, got, want)
		}
	}
	if got, want := string(sums[0]["assumed_cost_avoided_minor"])+" "+string(sums[0]["would_be_royalties_minor"])+" "+string(sums[0]["largest_earner_share_bps"]),
		"260 50 10000"; got != want {
		t.Errorf("the week's GBP avoided, royalties and largest earner's share = %s, want %s", got, want)
	}
	for _, private := range []string{a1, b, grace, "Lovelace", "55779911", "per_b631_grace", "2000", "1500"} {
		if strings.Contains(body, private) {
			t.Errorf("the read holds %q: want no payee, account or amount of one payment", private)
		}
	}
}
