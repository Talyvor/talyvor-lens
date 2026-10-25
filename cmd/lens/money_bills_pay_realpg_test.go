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
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B41.102 — pay all due: three due bills prepared as one request show one card (the count, the total and every
// payee), are approved once with a passkey, and post three payments out with their invoice numbers as references,
// each under its own key. Changing an amount or a payee after the statement was signed invalidates the approval; a
// bill in a live request cannot be put in another; and a paid bill is never paid again.
func TestMoneyBillsPay_ThreeDueBillsOneApprovalEachPaidOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b41102-pay-due", "app.talyvor.com", "https://app.talyvor.com"
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
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -500000}, {AccountID: company.ID, AmountMinor: 500000}}}); err != nil {
		t.Fatal(err)
	}
	payees := map[string]string{}
	for _, name := range []string{"Ada Plumbing", "Grace Groceries", "Carol Consulting"} {
		p, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
		if err != nil {
			t.Fatal(err)
		}
		payees[name] = p.ID
	}
	// Three bills from saved payees, and one from a supplier who is not a saved payee yet.
	bills := map[string]string{}
	for _, b := range []struct{ supplier, number, amount, due string }{
		{"Ada Plumbing", "INV-101", "£120.00", "2026-11-01"}, {"Grace Groceries", "INV-102", "£45.50", "2026-11-03"},
		{"Carol Consulting", "INV-103", "£300.00", "2026-11-05"}, {"Unknown Widgets", "INV-104", "£10.00", "2026-11-07"}} {
		text := "Invoice\nFrom: " + b.supplier + "\nInvoice number: " + b.number + "\nAmount due: " + b.amount + "\nDue date: " + b.due
		bill, _, err := store.AddBill(ctx, ws, economy.BillSourceUpload, b.number+".txt", []byte(text), text)
		if err != nil {
			t.Fatal(err)
		}
		bills[b.number] = bill.ID
	}

	r := chi.NewRouter()
	mountMoneyBillRoutes(r, store, inProcessConverter{})
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
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}); err != nil {
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
	signed := func(id string) string {
		t.Helper()
		code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approval", "", "")
		if code != http.StatusOK {
			t.Fatalf("approval statement for %s = %d %v", id, code, out)
		}
		b, _ := json.Marshal(map[string]any{"statement": out["statement"], "assertion": sign(out["challenge"].(string))})
		return string(b)
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
	payAllDue := func(key, body string) (int, map[string]any) {
		t.Helper()
		return call(http.MethodPost, "/v1/money/bills/pay", key, body)
	}

	// Changing a payee, or an amount, after the person signed invalidates the approval: nothing is paid.
	for i, tamper := range []string{
		`UPDATE payment_request_items SET payee_id = '` + payees["Carol Consulting"] + `' WHERE request_id = $1 AND position = 1`,
		`UPDATE payment_request_items SET amount_minor = amount_minor + 1 WHERE request_id = $1 AND position = 2`} {
		k := []string{"tamper-payee", "tamper-amount"}[i]
		code, req := payAllDue(k, `{"client":"chat"}`)
		if code != http.StatusCreated {
			t.Fatalf("%s: pay all due = %d %v", k, code, req)
		}
		id := req["id"].(string)
		body := signed(id)
		if _, err := pool.Exec(ctx, tamper, id); err != nil {
			t.Fatal(err)
		}
		if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approve", "", body); code != http.StatusBadRequest {
			t.Errorf("%s: approving after the change = %d %v, want 400", k, code, out)
		}
		if n, _ := paymentsOut(); n != 0 {
			t.Fatalf("%s: the ledger has %d payments out after an invalid approval, want 0", k, n)
		}
		if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/cancel", "", ""); code != http.StatusOK {
			t.Fatalf("%s: cancel = %d %v", k, code, out)
		}
	}

	// Pay all due: one card with the count, the total and every payee; the unknown supplier's bill is blocked.
	code, card := payAllDue("pay-all-1", `{"client":"chat"}`)
	if code != http.StatusCreated || card["status"] != "pending_approval" || card["totals"].(map[string]any)["GBP"] != float64(46550) {
		t.Fatalf("pay all due = %d %v, want pending approval of GBP 465.50", code, card)
	}
	id := card["id"].(string)
	var names []string
	for _, x := range card["items"].([]any) {
		it := x.(map[string]any)
		if it["decision"] == economy.ItemNeedsApproval {
			names = append(names, it["payee_name"].(string))
		} else if it["bill_id"] != bills["INV-104"] || !strings.Contains(it["reason"].(string), "not a saved payee") {
			t.Errorf("item %v, want only the unknown supplier's bill blocked as not a saved payee", it)
		}
	}
	if slices.Sort(names); strings.Join(names, ",") != "Ada Plumbing,Carol Consulting,Grace Groceries" {
		t.Errorf("the card's payees are %v", names)
	}
	if code, again := payAllDue("pay-all-1", `{"client":"chat"}`); code != http.StatusOK || again["id"] != id {
		t.Errorf("the same key again = %d %v, want 200 with %s", code, again["id"], id)
	}
	// While it waits, a bill in it cannot be put in another request.
	if code, other := payAllDue("other-1", `{"client":"chat","bill_ids":["`+bills["INV-101"]+`"]}`); code != http.StatusCreated ||
		other["status"] != economy.ItemBlocked || !strings.Contains(other["items"].([]any)[0].(map[string]any)["reason"].(string), id) {
		t.Errorf("a bill in a live request, named again = %d %v, want blocked naming %s", code, other, id)
	}

	// One approval pays the three bills, each under its own key, its invoice number the reference.
	body := signed(id)
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approve", "", body); code != http.StatusOK || out["status"] != "approved" {
		t.Fatalf("approving = %d %v", code, out)
	}
	if n, minor := paymentsOut(); n != 3 || minor != 46550 {
		t.Fatalf("after one approval the ledger has %d payments out of %d, want 3 of 46550", n, minor)
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
	if strings.Join(refs, ",") != "INV-101,INV-102,INV-103" || len(slices.Compact(slices.Sorted(slices.Values(keys)))) != 3 {
		t.Errorf("the payments' references are %v and keys %v, want INV-101..103 each under its own key", refs, keys)
	}
	for _, n := range []string{"INV-101", "INV-102", "INV-103"} {
		if b, _ := store.BillByID(ctx, ws, bills[n]); b.Status != "paid" {
			t.Errorf("bill %s is %s, want paid", n, b.Status)
		}
	}

	// No bill is paid twice: the approval again, and the paid bill named again, pay nothing.
	if code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approve", "", body); code != http.StatusConflict {
		t.Errorf("replaying the approval = %d %v, want 409", code, out)
	}
	if code, again := payAllDue("again-1", `{"client":"chat","bill_ids":["`+bills["INV-101"]+`"]}`); code != http.StatusCreated ||
		again["status"] != economy.ItemBlocked || again["items"].([]any)[0].(map[string]any)["reason"] != "the bill was paid already" {
		t.Errorf("a paid bill named again = %d %v, want blocked as paid already", code, again)
	}
	if n, _ := paymentsOut(); n != 3 {
		t.Errorf("after the replays the ledger has %d payments out, want still 3", n)
	}
}
