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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B58.5 — in a workspace with the phone's device key, approving a payout batch pays only with that key's signature
// over the challenge Lens issued for exactly that batch. Unsigned, signed over another batch, or signed over this batch
// before a row was added, it is refused 403 with nothing on the ledger and the batch still awaiting approval; signed over
// the batch as it stands, every valid row is paid once.
func TestPayoutBatchApproval_PaysOnlyWithTheKeySignatureOverThatBatch(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b585-payout-signed"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
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
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation)
		VALUES ($1, $2, 'test', $3, '\x00')`, "dev_"+ws, ws, spki); err != nil {
		t.Fatal(err)
	}
	assertion := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		a, _ := json.Marshal(map[string]any{"assertion": map[string]string{"device_id": "dev_" + ws, "challenge": challenge,
			"signature": base64.RawURLEncoding.EncodeToString(sig)}})
		return string(a)
	}

	r := chi.NewRouter()
	mountMoneyPayoutRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, key, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	upload := func(key string, minors ...int) string {
		t.Helper()
		var rows []string
		for _, m := range minors {
			rows = append(rows, `{"payee_id":"`+payee.ID+`","amount_minor":`+jsonOf(m)+`,"currency":"GBP","reference":"Pay `+jsonOf(m)+`"}`)
		}
		code, out := call("/v1/money/payouts", key, `{"rows":[`+strings.Join(rows, ",")+`]}`)
		if code != http.StatusCreated || out["rows_valid"] != float64(len(minors)) {
			t.Fatalf("upload %s = %d %v, want 201 with every row valid", key, code, out)
		}
		return out["id"].(string)
	}
	challenge := func(batch string) string {
		t.Helper()
		code, out := call("/v1/money/payouts/"+batch+"/approve/challenge", "", "")
		c, _ := out["challenge"].(string)
		if code != http.StatusOK || c == "" {
			t.Fatalf("the challenge for %s = %d %v, want 200 with a challenge", batch, code, out)
		}
		return c
	}
	paidOut := func() (n int, minor int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind LIKE 'payment_out%' AND p.account_id = $2`, ws, company.ID).Scan(&n, &minor); err != nil {
			t.Fatal(err)
		}
		return n, minor
	}
	refused := func(name, batch, body string) {
		t.Helper()
		if code, out := call("/v1/money/payouts/"+batch+"/approve", "", body); code != http.StatusForbidden {
			t.Errorf("%s = %d %v, want 403", name, code, out)
		}
		if n, minor := paidOut(); n != 0 {
			t.Fatalf("after %s the company's account shows %d payment postings of %d pence, want nothing on the ledger", name, n, minor)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM money_payout_batches WHERE id = $1`, batch).Scan(&status); err != nil || status != economy.PayoutAwaitingApproval {
			t.Fatalf("after %s the batch is %q (%v), want still awaiting approval", name, status, err)
		}
	}

	payroll := upload("payroll", 2000, 3000)
	other := upload("expenses", 500)

	code, out := call("/v1/money/payouts/"+payroll+"/approve", "", "")
	if code != http.StatusForbidden || out["error"] != economy.ErrPasskeyRequired.Error() {
		t.Errorf("an unsigned approval = %d %v, want 403 in the words a person's payment uses", code, out)
	}
	refused("an unsigned approval", payroll, "")
	refused("a signature over another batch", payroll, assertion(challenge(other)))

	before := challenge(payroll)
	// A row is added to the batch after its challenge was issued: £40.00 more to the same payee.
	if _, err := pool.Exec(ctx, `UPDATE money_payout_batches SET rows = rows || jsonb_build_array(jsonb_build_object('line', 3, 'payee_id', $2::text,
		'amount_minor', 4000, 'currency', 'GBP', 'reference', 'Pay 4000', 'status', 'valid')), rows_total = 3, rows_valid = 3,
		totals_minor = '{"GBP": 9000}' WHERE id = $1`, payroll, payee.ID); err != nil {
		t.Fatal(err)
	}
	refused("a signature over the batch before a row was added", payroll, assertion(before))

	code, out = call("/v1/money/payouts/"+payroll+"/approve", "", assertion(challenge(payroll)))
	if code != http.StatusOK || out["status"] != economy.PayoutSent {
		t.Fatalf("the approval signed over the batch = %d %v, want 200 sent", code, out)
	}
	if n, minor := paidOut(); n != 3 || minor != -9000 {
		t.Fatalf("the signed approval left %d payment postings of %d pence on the company's account, want 3 of -9000", n, minor)
	}
	if code, _ := call("/v1/money/payouts/"+payroll+"/approve", "", ""); code != http.StatusOK {
		t.Errorf("reading the sent batch's approval again = %d, want 200", code)
	}
	if n, _ := paidOut(); n != 3 {
		t.Errorf("after approving again the company's account shows %d payment postings, want still 3", n)
	}
	if b, err := store.Balance(ctx, ws, company.ID); err != nil || b.TestMinor != 500000-9000 {
		t.Errorf("the company's account holds %+v (%v), want %d test pence", b, err, 500000-9000)
	}
}
