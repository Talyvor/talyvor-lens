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
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B41.103 — invoice risk checks: on the approval card a first-time payee, a duplicate invoice (paid before, or twice in
// one batch), a changed bank detail and an amount far above the payee's usual each show their own flag; a duplicate is
// approved only with a passkey and a reason, which the person signs with the statement; and then it is paid.
func TestMoneyBillsRisk_FlagsOnTheCardAndADuplicateNeedsAReason(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b41103-bill-risk", "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetUnusualAmount(3, 1, 90)
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
	for name, account := range map[string]string{"Ada Plumbing": "55779911", "Grace Groceries": "66880022"} {
		if _, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", SortCode: "200000", AccountNumber: account}); err != nil {
			t.Fatal(err)
		}
	}
	addBill := func(supplier, number, amount, account string) string {
		t.Helper()
		text := "Invoice\nFrom: " + supplier + "\nInvoice number: " + number + "\nAmount due: " + amount +
			"\nDue date: 2026-11-01\nSort code: 20-00-00\nAccount number: " + account
		b, created, err := store.AddBill(ctx, ws, economy.BillSourceUpload, number+".txt", []byte(text), text)
		if err != nil || !created {
			t.Fatalf("bill %s from %s: created %v, %v", number, supplier, created, err)
		}
		return b.ID
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
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}, nil); err != nil {
		t.Fatal(err)
	}
	counter := uint32(0)
	approve := func(id, approvalBody string) (int, map[string]any) {
		t.Helper()
		code, out := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approval", "", approvalBody)
		if code != http.StatusOK {
			return code, out
		}
		counter++
		cdj, ad := clientData("webauthn.get", out["challenge"].(string)), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		b, _ := json.Marshal(map[string]any{"statement": out["statement"], "assertion": &passkey.Assertion{CredentialID: b64(credID),
			ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}})
		return call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approve", "", string(b))
	}
	// flags is each card item's flag codes, by its bill.
	flags := func(card map[string]any) map[string][]string {
		out := map[string][]string{}
		for _, x := range card["items"].([]any) {
			it := x.(map[string]any)
			codes := []string{}
			for _, f := range it["flags"].([]any) {
				codes = append(codes, f.(map[string]any)["code"].(string))
			}
			out[it["bill_id"].(string)] = codes
		}
		return out
	}
	paymentsOut := func() (n int, minor int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, ws, partnerAccount).Scan(&n, &minor); err != nil {
			t.Fatal(err)
		}
		return n, minor
	}

	// The first bill from Ada: a first-time payee, flagged as such, approved with a passkey alone.
	first := addBill("Ada Plumbing", "INV-101", "£120.00", "55779911")
	code, card := call(http.MethodPost, "/v1/money/bills/pay", "first", `{"client":"chat"}`)
	if code != http.StatusCreated || strings.Join(flags(card)[first], ",") != economy.FlagFirstPayment {
		t.Fatalf("the first bill's card = %d %v, want flagged first_payment alone", code, card)
	}
	if code, out := approve(card["id"].(string), ""); code != http.StatusOK || out["status"] != "approved" {
		t.Fatalf("approving the first bill = %d %v", code, out)
	}
	if n, minor := paymentsOut(); n != 1 || minor != 12000 {
		t.Fatalf("the ledger has %d payments out of %d, want 1 of 12000", n, minor)
	}

	// Ada sends INV-101 again under another name, Grace's invoice prints other bank details, and Ada bills far above
	// her usual: each shows its own flag on one card.
	dup := addBill("Ada Plumbing Ltd", "INV-101", "£120.00", "55779911")
	changed := addBill("Grace Groceries", "INV-201", "£45.50", "12345678")
	large := addBill("Ada Plumbing", "INV-102", "£400.00", "55779911")
	inBatch := addBill("Ada Plumbing Co", "INV-102", "£400.00", "55779911") // the same invoice again, in the same batch
	code, card = call(http.MethodPost, "/v1/money/bills/pay", "second", `{"client":"chat"}`)
	if code != http.StatusCreated {
		t.Fatalf("pay all due = %d %v", code, card)
	}
	got := flags(card)
	for bill, want := range map[string]string{dup: "duplicate", changed: "bank_details_changed,first_payment", large: "unusual_amount", inBatch: "unusual_amount,duplicate"} {
		if strings.Join(got[bill], ",") != want {
			t.Errorf("bill %s is flagged %v, want %s", bill, got[bill], want)
		}
	}
	id := card["id"].(string)
	if code, again := call(http.MethodGet, "/v1/money/payment-requests/"+id, "", ""); code != http.StatusOK || strings.Join(flags(again)[dup], ",") != "duplicate" {
		t.Errorf("the request read back = %d %v, want its duplicate flag kept", code, again)
	}

	// Without a reason the duplicate (and the changed bank detail) cannot be approved, and nothing is paid.
	if code, out := approve(id, ""); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "needs a reason") {
		t.Errorf("approving the flagged bills without a reason = %d %v, want 400 needing a reason", code, out)
	}
	if n, _ := paymentsOut(); n != 1 {
		t.Fatalf("after the refused approval the ledger has %d payments out, want still 1", n)
	}
	// With a reason it is signed with the statement, and the four bills are paid.
	code, statement := call(http.MethodPost, "/v1/money/payment-requests/"+id+"/approval", "", `{"reason":"Ada re-sent INV-101 for a second visit"}`)
	if code != http.StatusOK || !strings.Contains(statement["statement"].(string), `"overrides":["duplicate"]`) ||
		!strings.Contains(statement["statement"].(string), `"reason":"Ada re-sent INV-101 for a second visit"`) {
		t.Fatalf("the statement with a reason = %d %v, want the reason and the overridden duplicate in it", code, statement)
	}
	if code, out := approve(id, `{"reason":"Ada re-sent INV-101 for a second visit"}`); code != http.StatusOK || out["status"] != "approved" {
		t.Fatalf("approving with a reason = %d %v", code, out)
	}
	if n, minor := paymentsOut(); n != 5 || minor != 12000+12000+4550+40000+40000 {
		t.Errorf("after the approval with a reason the ledger has %d payments out of %d, want 5 of %d", n, minor, 12000+12000+4550+40000+40000)
	}
}
