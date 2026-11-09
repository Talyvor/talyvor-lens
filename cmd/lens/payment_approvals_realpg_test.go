package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// paymentRequestWorld is a workspace for payment requests: an account in pounds holding topUpMinor of test money, the
// saved payees named, the payment request routes as its owner, and the person's passkey to sign with.
type paymentRequestWorld struct {
	pool                    *pgxpool.Pool
	store                   *economy.DualTokenStore
	company, partnerAccount string
	payee                   map[string]string
	call                    func(method, path, key, body string) (int, map[string]any)
	sign                    func(challenge string) *passkey.Assertion
}

func newPaymentRequestWorld(t *testing.T, ws string, topUpMinor int64, payees ...string) paymentRequestWorld {
	t.Helper()
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const rpID, origin = "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if topUpMinor > 0 {
		topUp(t, store, ws, "top-up", partnerAccount, company.ID, topUpMinor)
	}
	payee := map[string]string{}
	for _, name := range payees {
		p, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
		if err != nil {
			t.Fatal(err)
		}
		payee[name] = p.ID
	}

	r := chi.NewRouter()
	mountPaymentRequestRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, key, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// The person's passkey: this test is the authenticator, its counter rising with each signature.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	regChallenge, _, err := store.PasskeyRegistrationChallenge(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	attested := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	attested = append(append(binary.BigEndian.AppendUint16(append(attested, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
	if _, err := store.RegisterPasskey(ctx, ws, "Ada's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}, nil); err != nil {
		t.Fatal(err)
	}
	counter := uint32(0)
	sign := func(challenge string) *passkey.Assertion {
		counter++
		cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		return &passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
	}
	return paymentRequestWorld{pool, store, company.ID, partnerAccount, payee, call, sign}
}

// topUp pays amountMinor of test money into account from the partner.
func topUp(t *testing.T, store *economy.DualTokenStore, ws, key, partnerAccount, account string, amountMinor int64) {
	t.Helper()
	if _, err := store.PostMoney(context.Background(), economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: key, Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -amountMinor}, {AccountID: account, AmountMinor: amountMinor}}}); err != nil {
		t.Fatal(err)
	}
}

// B40.5 — a two-item batch approved once with a passkey posts two payments out with their own references; replaying
// the approval pays nothing; changing an amount after signing makes the approval invalid; an auto item inside a
// signed mandate is paid once with the mandate in its record; and the exported record verifies as an unbroken chain.
func TestPaymentRequests_OneApprovalEachItemPaidOnce(t *testing.T) {
	const ws = "ws-b405-approve"
	w := newPaymentRequestWorld(t, ws, 500000, "Ada Plumbing", "Grace Groceries", "Carol Consulting")
	pool, store, partnerAccount, payee, call, sign := w.pool, w.store, w.partnerAccount, w.payee, w.call, w.sign
	ctx := context.Background()
	canonical, challenge, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, Clients: []string{"chat"},
		Scope:  economy.MandateScope{Payees: []string{payee["Ada Plumbing"]}, Currencies: []string{"GBP"}},
		Limits: economy.MandateLimits{PerPayment: map[string]int64{"GBP": 10000}}, NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee},
		ExpiresAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	mandate, err := store.CreatePermissionMandate(ctx, ws, "ada", canonical, sign(challenge))
	if err != nil {
		t.Fatal(err)
	}

	// The ledger: each payment out is one entry moving its amount from the hold to the partner.
	paymentsOut := func() (n int, minor int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, ws, partnerAccount).Scan(&n, &minor); err != nil {
			t.Fatal(err)
		}
		return n, minor
	}
	approve := func(id string) (string, *passkey.Assertion) {
		t.Helper()
		code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approval", "", "")
		if code != http.StatusOK {
			t.Fatalf("approval statement for %s = %d %v", id, code, out)
		}
		return out["statement"].(string), sign(out["challenge"].(string))
	}
	approveBody := func(statement string, a *passkey.Assertion) string {
		b, _ := json.Marshal(map[string]any{"statement": statement, "assertion": a})
		return string(b)
	}

	// A batch of two: £120 to the grocer and £50 to the consultant, neither inside the mandate, so both need approval.
	code, batch := call(http.MethodPost, "/v1/money/payment-requests", "batch-1", `{"client":"chat","items":[
		{"payee_id":"`+payee["Grace Groceries"]+`","currency":"GBP","amount_minor":12000,"reference":"INV-A1"},
		{"payee_id":"`+payee["Carol Consulting"]+`","currency":"GBP","amount_minor":5000,"reference":"INV-A2"}]}`)
	if code != http.StatusCreated || batch["status"] != "pending_approval" {
		t.Fatalf("the batch = %d %v", code, batch)
	}
	batchID := batch["id"].(string)
	statement, assertion := approve(batchID)
	var st economy.ApprovalStatement
	if err := json.Unmarshal([]byte(statement), &st); err != nil || st.PayloadHash != batch["payload_hash"] || len(st.Items) != 2 || st.Totals["GBP"] != 17000 {
		t.Fatalf("the statement %s does not show the batch's hash, both items and their £170 total", statement)
	}
	code, paid := call(http.MethodPost, "/v1/money/payment-requests/"+batchID+"/approve", "", approveBody(statement, assertion))
	if code != http.StatusOK || paid["status"] != "approved" {
		t.Fatalf("approving the batch = %d %v", code, paid)
	}
	if n, minor := paymentsOut(); n != 2 || minor != 17000 {
		t.Fatalf("after one approval the ledger has %d payments out of %d, want 2 of 17000", n, minor)
	}
	rows, err := pool.Query(ctx, `SELECT reference, idempotency_key FROM money_payments WHERE workspace_id = $1 ORDER BY reference`, ws)
	if err != nil {
		t.Fatal(err)
	}
	var refs, keys []string
	for rows.Next() {
		var ref, k string
		_ = rows.Scan(&ref, &k)
		refs, keys = append(refs, ref), append(keys, k)
	}
	rows.Close()
	if strings.Join(refs, ",") != "INV-A1,INV-A2" || keys[0] == keys[1] {
		t.Errorf("the payments' references are %v and keys %v, want INV-A1, INV-A2 each under its own key", refs, keys)
	}

	// The same approval again, and paying again, pay nothing.
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+batchID+"/approve", "", approveBody(statement, assertion)); code != http.StatusConflict {
		t.Errorf("replaying the approval = %d %v, want 409", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+batchID+"/pay", "", ""); code != http.StatusOK {
		t.Errorf("paying the approved batch again = %d %v", code, out)
	}
	if n, _ := paymentsOut(); n != 2 {
		t.Errorf("after a replay the ledger has %d payments out, want still 2", n)
	}

	// An amount changed after the person signed: the approval is invalid and nothing is paid.
	code, single := call(http.MethodPost, "/v1/money/payment-requests", "single-1",
		`{"client":"chat","items":[{"payee_id":"`+payee["Grace Groceries"]+`","currency":"GBP","amount_minor":3000,"reference":"INV-C"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("the single request = %d %v", code, single)
	}
	statement, assertion = approve(single["id"].(string))
	if _, err := pool.Exec(ctx, `UPDATE payment_request_items SET amount_minor = 300000 WHERE request_id = $1`, single["id"]); err != nil {
		t.Fatal(err)
	}
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+single["id"].(string)+"/approve", "", approveBody(statement, assertion)); code != http.StatusBadRequest {
		t.Errorf("approving after the amount changed = %d %v, want 400", code, out)
	}
	if n, _ := paymentsOut(); n != 2 {
		t.Errorf("after a changed amount the ledger has %d payments out, want still 2", n)
	}

	// £90 to the plumber from chat is auto under the mandate: paid once, the mandate its authority.
	code, auto := call(http.MethodPost, "/v1/money/payment-requests", "auto-1",
		`{"client":"chat","items":[{"payee_id":"`+payee["Ada Plumbing"]+`","currency":"GBP","amount_minor":9000,"reference":"Boiler"}]}`)
	if code != http.StatusCreated || auto["status"] != "auto" {
		t.Fatalf("the plumber's request = %d %v, want auto", code, auto)
	}
	autoID := auto["id"].(string)
	for range 2 {
		code, out := call(http.MethodPost, "/v1/money/payment-requests/"+autoID+"/pay", "", "")
		if it := out["items"].([]any)[0].(map[string]any); code != http.StatusOK || it["authority"] != "mandate:"+mandate.ID || it["payment_id"] == nil {
			t.Errorf("paying the auto item = %d %v, want paid under mandate %s", code, out, mandate.ID)
		}
	}
	if n, minor := paymentsOut(); n != 3 || minor != 26000 {
		t.Errorf("after the auto item the ledger has %d payments out of %d, want 3 of 26000", n, minor)
	}

	// Each record verifies as an unbroken chain, and an altered entry does not.
	for id, want := range map[string]string{batchID: "request,decision,shown,signature,provider_call,provider_call", autoID: "request,decision,provider_call"} {
		code, rec := call(http.MethodGet, "/v1/money/payment-requests/"+id+"/record", "", "")
		var kinds []string
		for _, e := range rec["entries"].([]any) {
			kinds = append(kinds, e.(map[string]any)["kind"].(string))
		}
		if code != http.StatusOK || rec["verified"] != true || strings.Join(kinds, ",") != want {
			t.Errorf("the record of %s = %d verified %v with %v, want %s", id, code, rec["verified"], kinds, want)
		}
		if id == autoID && !strings.Contains(rec["entries"].([]any)[2].(map[string]any)["body"].(string), `"authority":"mandate:`+mandate.ID+`"`) {
			t.Errorf("the auto item's record does not name its mandate: %v", rec["entries"])
		}
	}
	entries, err := store.AuditRecord(ctx, ws, batchID)
	if err != nil {
		t.Fatal(err)
	}
	entries[4].Body = strings.Replace(entries[4].Body, "INV", "XXX", 1) + " "
	if economy.VerifyAuditChain(entries) == nil {
		t.Error("an altered entry still verifies")
	}

	// B55.9 — a mixed request nobody answered for 24 hours: what waits in it is not paid, before or after it is
	// cancelled; its covered item was paid when it was prepared (B55.21).
	age := func(id string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE payment_requests SET created_at = created_at - interval '24 hours 1 minute' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	code, mixed := call(http.MethodPost, "/v1/money/payment-requests", "mixed-1", `{"client":"chat","items":[
		{"payee_id":"`+payee["Ada Plumbing"]+`","currency":"GBP","amount_minor":2000,"reference":"Tap"},
		{"payee_id":"`+payee["Grace Groceries"]+`","currency":"GBP","amount_minor":1000,"reference":"INV-M"}]}`)
	if code != http.StatusCreated || mixed["status"] != "pending_approval" {
		t.Fatalf("the mixed request = %d %v", code, mixed)
	}
	mixedID := mixed["id"].(string)
	prepN, prepMinor := paymentsOut()
	age(mixedID)
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+mixedID+"/pay", "", ""); code == http.StatusOK || out["error"] == nil {
		t.Errorf("paying the expired request = %d %v, want refused", code, out)
	}
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+mixedID+"/cancel", "", ""); code != http.StatusOK || out["status"] != "cancelled" {
		t.Errorf("cancelling the expired request = %d %v, want cancelled", code, out)
	}
	call(http.MethodPost, "/v1/money/payment-requests/"+mixedID+"/pay", "", "")
	if n, minor := paymentsOut(); n != prepN || minor != prepMinor {
		t.Errorf("after the expired request the ledger has %d payments out of %d, want still %d of %d", n, minor, prepN, prepMinor)
	}

	// A bill in a request nobody answered for 24 hours is free to pay in a new one.
	if _, err := pool.Exec(ctx, `INSERT INTO money_bills (id, workspace_id, source, document_sha256, supplier, amount_minor, currency,
		payee_id, payee_match) VALUES ('bill_b559', $1, 'upload', 'bill_b559', 'Carol Consulting', 4000, 'GBP', $2, 'matched')`, ws, payee["Carol Consulting"]); err != nil {
		t.Fatal(err)
	}
	first, _, err := store.PrepareBillsPayment(ctx, ws, "ada", "bills-1", "chat", nil)
	if err != nil || len(first.Items) != 1 || first.Items[0].BillID != "bill_b559" {
		t.Fatalf("the bills request = %+v %v", first, err)
	}
	if _, _, err := store.PrepareBillsPayment(ctx, ws, "ada", "bills-2", "chat", nil); !errors.Is(err, economy.ErrPaymentRequest) {
		t.Errorf("a second bills request while the first waits = %v, want no bill is due", err)
	}
	age(first.ID)
	again, _, err := store.PrepareBillsPayment(ctx, ws, "ada", "bills-3", "chat", []string{"bill_b559"})
	if err != nil || len(again.Items) != 1 || again.Items[0].Decision == economy.ItemBlocked {
		t.Errorf("the bill after its request expired = %+v %v, want it payable in a new request", again, err)
	}

	// One approved in the instant it expires keeps its bill: a new request waits for the approval, then finds the bill taken.
	age(again.ID)
	approving, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = approving.Rollback(ctx) }()
	if _, err := approving.Exec(ctx, `UPDATE payment_requests SET status = 'approved' WHERE id = $1`, again.ID); err != nil {
		t.Fatal(err)
	}
	raced := make(chan economy.PaymentRequest, 1)
	go func() {
		r, _, _ := store.PrepareBillsPayment(ctx, ws, "ada", "bills-4", "chat", []string{"bill_b559"})
		raced <- r
	}()
	time.Sleep(300 * time.Millisecond)
	if err := approving.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-raced; len(r.Items) != 1 || r.Items[0].Decision != economy.ItemBlocked || !strings.Contains(r.Items[0].Reason, again.ID) {
		t.Errorf("the bill while its request was approved as it expired = %+v, want blocked: in %s already", r, again.ID)
	}
}

// B55.21 — a payment a signed mandate covers is paid by the prepare call alone, once however often it is retried; in a
// mixed request the covered item is paid at once and the rest once approved; declining keeps what was paid and
// cancels what waits, and a request with nothing waiting cannot be declined; the runner pays a covered item that failed
// for want of money, but never one paid since under another request, one prepared more than a day ago, nor one
// prepared before covered payments were paid when prepared; a paused agent's covered request is refused, and stays
// unpaid once the agent is resumed; and a covered payment held for its new payee can be declined, and is never paid.
func TestPaymentRequests_CoveredPaidWhenPrepared(t *testing.T) {
	const ws = "ws-b5521-covered"
	w := newPaymentRequestWorld(t, ws, 0, "Ada Plumbing", "Grace Groceries", "Carol Consulting")
	pool, store, payee, call := w.pool, w.store, w.payee, w.call
	ctx := context.Background()
	const agentID = "agt_b5521"
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name) VALUES ($1, $2, 'Bills bot')`, agentID, ws); err != nil {
		t.Fatal(err)
	}
	// Up to £100 a payment and £400 a day to the plumber, from chat or the agent's key.
	canonical, challenge, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, Clients: []string{"chat", "key:k-b5521"},
		Scope:    economy.MandateScope{Payees: []string{payee["Ada Plumbing"]}, Currencies: []string{"GBP"}},
		Limits:   economy.MandateLimits{PerPayment: map[string]int64{"GBP": 10000}, PerDay: map[string]int64{"GBP": 40000}},
		NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee}, ExpiresAt: time.Now().Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePermissionMandate(ctx, ws, "ada", canonical, w.sign(challenge)); err != nil {
		t.Fatal(err)
	}
	// The ledger's payments out, each as "payee amount", in the order they were paid.
	var want []string
	paidOut := func(step string, more ...string) {
		t.Helper()
		want = append(want, more...)
		rows, err := pool.Query(ctx, `SELECT e.counterparty || ' ' || p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2 ORDER BY e.created_at, e.id`, ws, w.partnerAccount)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ", ") != strings.Join(want, ", ") {
			t.Fatalf("%s: the ledger's payments out = %q, want %q", step, got, want)
		}
	}
	item := func(r map[string]any, i int) map[string]any { return r["items"].([]any)[i].(map[string]any) }
	get := func(r map[string]any) map[string]any {
		_, out := call(http.MethodGet, "/v1/money/payment-requests/"+r["id"].(string), "", "")
		return out
	}
	plumber := func(minor int, ref string) string {
		return `{"payee_id":"` + payee["Ada Plumbing"] + `","currency":"GBP","amount_minor":` + strconv.Itoa(minor) + `,"reference":"` + ref + `"}`
	}
	grocer := func(minor int) string {
		return `{"payee_id":"` + payee["Grace Groceries"] + `","currency":"GBP","amount_minor":` + strconv.Itoa(minor) + `,"reference":"Veg"}`
	}
	prepare := func(key string, items ...string) (int, map[string]any) {
		return call(http.MethodPost, "/v1/money/payment-requests", key, `{"client":"chat","items":[`+strings.Join(items, ",")+`]}`)
	}

	// (5) With nothing in the account, covered payments fail for want of money.
	_, failed := prepare("fail-1", plumber(9000, "Fail"))
	_, late := prepare("late-1", plumber(9100, "Late"))
	_, dup := prepare("dup-1", plumber(500, "Dup"))
	_, old := prepare("old-1", plumber(600, "Old"))
	for _, r := range []map[string]any{failed, late, dup, old} {
		if r["status"] != "auto" || item(r, 0)["pay_status"] != economy.PayFailed {
			t.Fatalf("a covered payment from an empty account = %v, want auto and failed", r)
		}
	}
	// The late one was prepared 25 hours ago; the old one an hour ago, before covered payments were paid when prepared.
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET created_at = created_at - interval '25 hours' WHERE id = $1`, late["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payment_request_items SET created_at = created_at - interval '25 hours' WHERE request_id = $1`, late["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET created_at = created_at - interval '1 hour' WHERE id = $1`, old["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payment_request_items SET pay_status = '', pay_detail = '', authority = '' WHERE request_id = $1`, old["id"]); err != nil {
		t.Fatal(err)
	}
	// A mixed request whose covered item failed: declining it leaves nothing for the runner.
	_, held := prepare("held-1", plumber(700, "Held"), grocer(4900))
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+held["id"].(string)+"/cancel", "", ""); code != http.StatusOK ||
		item(out, 0)["pay_status"] != economy.PayNotApproved || item(out, 1)["pay_status"] != economy.PayNotApproved {
		t.Fatalf("declining a request whose covered item failed = %d %v, want both items not approved", code, out)
	}
	topUp(t, store, ws, "top-up", w.partnerAccount, w.company, 500000)
	// The agent prepares the failed £5 again under a new key: paid at once.
	if _, again := prepare("dup-2", plumber(500, "Dup")); item(again, 0)["payment_id"] == nil {
		t.Fatalf("the failed £5 prepared again = %v, want paid", again)
	}
	paidOut("the £5 prepared again", "Ada Plumbing 500")
	if _, err := store.RunCoveredPayments(ctx); err != nil {
		t.Fatal(err)
	}
	paidOut("the runner pays the £90 that failed, and nothing else", "Ada Plumbing 9000")
	if r := get(failed); item(r, 0)["payment_id"] == nil {
		t.Errorf("the failed covered payment after the runner = %v, want paid", r)
	}
	if r := get(late); item(r, 0)["pay_status"] != economy.PayRefused || item(r, 0)["pay_detail"] != "not paid within 24 hours of being prepared: prepare it again" {
		t.Errorf("the covered payment prepared 25 hours ago after the runner = %v, want refused", r)
	}
	if r := get(dup); item(r, 0)["pay_status"] != economy.PayRefused || item(r, 0)["payment_id"] != nil {
		t.Errorf("the failed £5 after it was paid under a new key = %v, want refused, not paid twice", r)
	}
	if r := get(old); item(r, 0)["pay_status"] != nil || item(r, 0)["payment_id"] != nil {
		t.Errorf("a covered payment prepared before they were paid when prepared = %v, want left unpaid", r)
	}

	// (1) One covered item: the prepare call alone pays it.
	code, covered := prepare("covered-1", plumber(9200, "Boiler"))
	if code != http.StatusCreated || item(covered, 0)["payment_id"] == nil {
		t.Fatalf("preparing a covered payment = %d %v, want it paid", code, covered)
	}
	paidOut("a covered payment prepared", "Ada Plumbing 9200")
	// (2) The same Idempotency-Key again pays nothing twice.
	if code, again := prepare("covered-1", plumber(9200, "Boiler")); code != http.StatusOK || item(again, 0)["payment_id"] != item(covered, 0)["payment_id"] {
		t.Errorf("the same key again = %d %v, want the same payment", code, again)
	}
	paidOut("the same key again")
	// Nothing waits in it, so it cannot be declined.
	if code, _ := call(http.MethodPost, "/v1/money/payment-requests/"+covered["id"].(string)+"/cancel", "", ""); code == http.StatusOK || get(covered)["status"] != "auto" {
		t.Errorf("declining a request with every item paid = %d %v, want refused and left as it was", code, get(covered))
	}

	// (3) A mixed request: the covered item is paid at prepare, the grocer once the person approves.
	code, mixed := prepare("mixed-1", plumber(9300, "Taps"), grocer(5000))
	if code != http.StatusCreated || mixed["status"] != "pending_approval" || item(mixed, 0)["payment_id"] == nil || item(mixed, 1)["payment_id"] != nil {
		t.Fatalf("preparing a mixed request = %d %v, want the covered item paid and the other waiting", code, mixed)
	}
	paidOut("a mixed request prepared", "Ada Plumbing 9300")
	_, st := call(http.MethodPost, "/v1/money/payment-requests/"+mixed["id"].(string)+"/approval", "", "")
	body, _ := json.Marshal(map[string]any{"statement": st["statement"], "assertion": w.sign(st["challenge"].(string))})
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+mixed["id"].(string)+"/approve", "", string(body)); code != http.StatusOK || item(out, 1)["payment_id"] == nil {
		t.Errorf("approving the rest = %d %v, want the grocer paid", code, out)
	}
	paidOut("the rest approved", "Grace Groceries 5000")

	// (4) Declining a mixed request keeps the covered payment and cancels the grocer's, which is never paid.
	_, declined := prepare("mixed-2", plumber(9400, "Sink"), grocer(5100))
	code, out := call(http.MethodPost, "/v1/money/payment-requests/"+declined["id"].(string)+"/cancel", "", "")
	if code != http.StatusOK || out["status"] != "cancelled" || item(out, 0)["payment_id"] == nil || item(out, 1)["pay_status"] != economy.PayNotApproved {
		t.Errorf("declining a mixed request = %d %v, want cancelled, the covered item paid and the grocer's not approved", code, out)
	}
	call(http.MethodPost, "/v1/money/payment-requests/"+declined["id"].(string)+"/pay", "", "")
	paidOut("a mixed request declined", "Ada Plumbing 9400")
	// The declined request's paid £94 still counts towards the £400 a day: £385 counted today, so £95 more waits.
	if _, more := prepare("over-day", plumber(9500, "Pipes")); more["status"] != "pending_approval" {
		t.Errorf("a payment past the day's limit after a decline = %v, want it to wait for approval", more)
	}

	// (6) The agent's own covered payment is paid when prepared. One that failed for want of money is refused when
	// every agent is paused, and stays unpaid once they are resumed.
	agentAccount, err := store.OpenCurrencyAccount(ctx, ws, agentID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	topUp(t, store, ws, "agent-top-up", w.partnerAccount, agentAccount.ID, 500)
	agentPays := func(key string, minor int64) economy.PaymentRequest {
		t.Helper()
		r, _, err := store.PreparePaymentRequest(ctx, ws, "agent:"+agentID, key, economy.PaymentRequestIn{Client: "key:k-b5521", AgentID: agentID,
			Items: []economy.PaymentRequestItemIn{{PayeeID: payee["Ada Plumbing"], Currency: "GBP", AmountMinor: minor, Reference: key}}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := agentPays("agent-1", 300); r.Items[0].PaymentID == "" {
		t.Fatalf("the agent's covered request = %+v, want paid", r.Items[0])
	}
	paidOut("the agent's covered request", "Ada Plumbing 300")
	queued := agentPays("agent-2", 400)
	if queued.Items[0].PayStatus != economy.PayFailed {
		t.Fatalf("the agent's covered £4 with £2 in its account = %+v, want failed", queued.Items[0])
	}
	if err := store.PauseAllAgents(ctx, ws, "holiday"); err != nil {
		t.Fatal(err)
	}
	topUp(t, store, ws, "agent-top-up-2", w.partnerAccount, agentAccount.ID, 1000)
	if _, err := store.RunCoveredPayments(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := store.PaymentRequest(ctx, ws, "", queued.ID); r.Items[0].PaymentID != "" || r.Items[0].PayStatus != economy.PayRefused {
		t.Errorf("the agent's failed £4 while every agent is paused = %+v, want refused", r.Items[0])
	}
	if err := store.ResumeAllAgents(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunCoveredPayments(ctx); err != nil {
		t.Fatal(err)
	}
	paidOut("the agents resumed")
	if r := agentPays("agent-3", 200); r.Items[0].Decision != economy.ItemAuto || r.Items[0].PaymentID == "" {
		t.Errorf("the agent's covered request once resumed = %+v, want paid", r.Items[0])
	}
	paidOut("a new covered request once resumed", "Ada Plumbing 200")

	// A covered £1,500 to a payee saved today is held for its 24 hours: declining it cancels it, and it is never paid.
	canonical, challenge, _, err = store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, Clients: []string{"chat"},
		Scope:    economy.MandateScope{Payees: []string{payee["Carol Consulting"]}, Currencies: []string{"GBP"}},
		Limits:   economy.MandateLimits{PerPayment: map[string]int64{"GBP": 200000}},
		NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee}, ExpiresAt: time.Now().Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePermissionMandate(ctx, ws, "ada", canonical, w.sign(challenge)); err != nil {
		t.Fatal(err)
	}
	_, consult := prepare("consult-1", `{"payee_id":"`+payee["Carol Consulting"]+`","currency":"GBP","amount_minor":150000,"reference":"Audit"}`)
	if item(consult, 0)["pay_status"] != economy.PayHeld {
		t.Fatalf("a covered £1,500 to a payee saved today = %v, want held", consult)
	}
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+consult["id"].(string)+"/cancel", "", ""); code != http.StatusOK ||
		item(out, 0)["pay_status"] != economy.PayNotApproved {
		t.Errorf("declining the held payment = %d %v, want cancelled and not approved", code, out)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '25 hours' WHERE id = $1`, payee["Carol Consulting"]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunHeldPayments(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunCoveredPayments(ctx); err != nil {
		t.Fatal(err)
	}
	paidOut("the held payment declined")
}
