package mcp

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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B45.17 — "Pay my bills", from any assistant: the official TypeScript MCP SDK client lists the bills due
// (list_bills_due) and prepares paying them all (prepare_bills_payment) as one request with one approval card. The
// person approves it once with a passkey; each bill is then paid once, on the ledger. Asked again, the assistant sees
// only the bill it cannot pay, and nothing more is paid. Needs node and `npm ci` in testdata/sdk-client, as
// TestOfficialTypeScriptSDKClient does.
func TestOfficialTypeScriptSDKClient_PayMyBills(t *testing.T) {
	dir, _ := filepath.Abs("testdata/sdk-client")
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		if os.Getenv("LENS_MCP_SDK_E2E") != "" {
			t.Fatalf("LENS_MCP_SDK_E2E is set but %s has no node_modules: run npm ci there", dir)
		}
		t.Skip("needs the MCP SDK: npm ci in testdata/sdk-client, or LENS_MCP_SDK_E2E=1 (the mcp-sdk CI job)")
	}
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4517-pay-my-bills", "app.talyvor.com", "https://app.talyvor.com"
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
	for _, name := range []string{"Ada Plumbing", "Grace Groceries"} {
		if _, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", SortCode: "200000", AccountNumber: "55779911"}); err != nil {
			t.Fatal(err)
		}
	}
	// Two bills from saved payees, one from a supplier who is not a saved payee yet.
	for _, b := range []struct{ supplier, number, amount, due, account string }{
		{"Ada Plumbing", "INV-201", "£120.00", "2026-11-01", "55779911"}, {"Grace Groceries", "INV-202", "£45.50", "2026-11-03", "55779911"},
		{"Unknown Widgets", "INV-203", "£10.00", "2026-11-07", "11223344"}} {
		text := "Invoice\nFrom: " + b.supplier + "\nInvoice number: " + b.number + "\nAmount due: " + b.amount + "\nDue date: " + b.due +
			"\nSort code: 20-00-00\nAccount number: " + b.account
		if _, _, err := store.AddBill(ctx, ws, economy.BillSourceUpload, b.number+".txt", []byte(text), text); err != nil {
			t.Fatal(err)
		}
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetPaymentRequests(store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	r := chi.NewRouter()
	r.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer owner-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAuthContext(r.Context(), owner)))
		})
	}).Post("/mcp", srv.HandleRPC)
	lens := httptest.NewServer(r)
	t.Cleanup(lens.Close)

	type card struct {
		ID     string           `json:"id"`
		Status string           `json:"status"`
		Totals map[string]int64 `json:"totals"`
		Items  []struct {
			BillID   string `json:"bill_id"`
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		} `json:"items"`
	}
	type answer struct {
		Due struct {
			Count  int              `json:"count"`
			Totals map[string]int64 `json:"totals"`
			Bills  []map[string]any `json:"bills"`
		} `json:"due"`
		Request card `json:"request"`
	}
	assistant := func(idem string) answer {
		t.Helper()
		cmd := exec.Command("node", filepath.Join(dir, "bills.mjs"), lens.URL, "owner-token", idem)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			stderr := ""
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			t.Fatalf("the SDK client failed: %v\n%s%s", err, out, stderr)
		}
		var a answer
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &a); err != nil {
			t.Fatalf("the SDK client printed %q: %v", out, err)
		}
		return a
	}
	paymentsOut := func() (n int, minor int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, ws, partnerAccount).Scan(&n, &minor); err != nil {
			t.Fatal(err)
		}
		return n, minor
	}

	// The assistant lists the three bills due, soonest first, without the bank details printed on them, and prepares
	// one request: the two payable bills wait for one approval, the unknown supplier's is blocked.
	first := assistant("pay-my-bills-1")
	if first.Due.Count != 3 || first.Due.Totals["GBP"] != 17550 || first.Due.Bills[0]["invoice_number"] != "INV-201" {
		t.Fatalf("list_bills_due = %+v, want 3 bills of GBP 175.50, INV-201 first", first.Due)
	}
	if raw, _ := json.Marshal(first.Due); strings.Contains(string(raw), "55779911") || strings.Contains(string(raw), "11223344") {
		t.Errorf("list_bills_due showed the bills' bank details: %s", raw)
	}
	req := first.Request
	waiting := 0
	for _, it := range req.Items {
		if it.Decision == economy.ItemNeedsApproval {
			waiting++
		} else if !strings.Contains(it.Reason, "not a saved payee") {
			t.Errorf("item %+v, want only the unknown supplier's bill blocked", it)
		}
	}
	if req.Status != "pending_approval" || waiting != 2 || req.Totals["GBP"] != 16550 {
		t.Fatalf("prepare_bills_payment = %+v, want one request of 2 bills, GBP 165.50, pending approval", req)
	}
	if n, _ := paymentsOut(); n != 0 {
		t.Fatalf("preparing paid %d bills, want 0", n)
	}

	// The person approves the card once with a passkey: this test is the authenticator.
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
	statement, challenge, _, err := store.PrepareRequestApproval(ctx, ws, req.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), 1)
	h := sha256.Sum256(cdj)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	assertion := &passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
	if _, err := store.ApprovePaymentRequest(ctx, ws, req.ID, "ada", statement, assertion); err != nil {
		t.Fatal(err)
	}
	if n, minor := paymentsOut(); n != 2 || minor != 16550 {
		t.Fatalf("after one approval the ledger has %d payments out of %d, want 2 of 16550", n, minor)
	}
	if _, err := store.ApprovePaymentRequest(ctx, ws, req.ID, "ada", statement, assertion); err == nil {
		t.Error("replaying the approval was accepted")
	}

	// Asked again, the assistant sees only the bill it cannot pay; preparing again pays nothing more.
	again := assistant("pay-my-bills-2")
	if again.Due.Count != 1 || again.Due.Bills[0]["invoice_number"] != "INV-203" || again.Request.Status != economy.ItemBlocked {
		t.Errorf("asked again: list_bills_due %+v and request %+v, want only INV-203, blocked", again.Due, again.Request)
	}
	if n, _ := paymentsOut(); n != 2 {
		t.Errorf("after asking again the ledger has %d payments out, want still 2", n)
	}
}
