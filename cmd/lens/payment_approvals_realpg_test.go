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
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B40.5 — a two-item batch approved once with a passkey posts two payments out with their own references; replaying
// the approval pays nothing; changing an amount after signing makes the approval invalid; an auto item inside a
// signed mandate is paid once with the mandate in its record; and the exported record verifies as an unbroken chain.
func TestPaymentRequests_OneApprovalEachItemPaidOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b405-approve", "app.talyvor.com", "https://app.talyvor.com"
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
	payee := map[string]string{}
	for _, name := range []string{"Ada Plumbing", "Grace Groceries", "Carol Consulting"} {
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
}
