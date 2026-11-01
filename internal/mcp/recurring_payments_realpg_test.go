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
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

type recordedPushes struct {
	mu   sync.Mutex
	msgs []nativepush.Message
}

func (p *recordedPushes) Send(_ context.Context, _, _ string, m nativepush.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, m)
	return nil
}

func (p *recordedPushes) titled(title string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, m := range p.msgs {
		if m.Title == title {
			out = append(out, m.Body)
		}
	}
	return out
}

// B45.13 — "here's my list of weekly and monthly payments: save them and pay them with Talyvor". The official
// TypeScript MCP SDK client sends the list to prepare_recurring_payments, which pays nothing: it prepares a standing
// order for each and one mandate draft. Nothing runs until the owner signs that draft; then each order's first run
// waits for one approval, its second pays on its own and notifies, and a payment above the mandate's ask-above
// amount still goes to approvals. Every payment is checked on the ledger, on test money.
func TestOfficialTypeScriptSDKClient_RecurringPayments(t *testing.T) {
	dir, _ := filepath.Abs("testdata/sdk-client")
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		if os.Getenv("LENS_MCP_SDK_E2E") != "" {
			t.Fatalf("LENS_MCP_SDK_E2E is set but %s has no node_modules: run npm ci there", dir)
		}
		t.Skip("needs the MCP SDK: npm ci in testdata/sdk-client, or LENS_MCP_SDK_E2E=1 (the mcp-sdk CI job)")
	}
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4513-recurring", "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	pushes := &recordedPushes{}
	store.SetNativePush(pushes)
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
	cleaner, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Grace Cleaning", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	landlord, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lettings", Country: "GB", SortCode: "200000", AccountNumber: "55779912"})
	if err != nil {
		t.Fatal(err)
	}
	// The landlord was saved last week, so the new-payee cooling-off (B41.111) does not hold the GBP 1,200 rent.
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET created_at = now() - interval '7 days' WHERE id = $1`, landlord.ID); err != nil {
		t.Fatal(err)
	}
	// The cleaner was paid once before, so the mandate's new-payee rule alone would not ask on the first run.
	if _, err := store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: cleaner.ID, AmountMinor: 100, Currency: "GBP", Funding: economy.FundingTest,
		Reference: "Trial clean", IdempotencyKey: "before-the-plan"}); err != nil {
		t.Fatal(err)
	}

	// The owner's passkey: this test is the authenticator.
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
	// The owner's phone, for the "Paid" push; added after the passkey, which is the workspace's first key.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, person, platform, public_key, attestation, push_token)
		VALUES ('dev_b4513', $1, 'jwt:user:ada', 'test', '\x00', '\x00', 'tok')`, ws); err != nil {
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

	// /mcp as cmd/lens mounts it, the owner's connector behind an auth stand-in.
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetRecurringPayments(store)
	ownerAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer owner-connector" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAuthContext(r.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
				UserID: "ada", Scopes: []string{auth.ScopeKeys}})))
		})
	}
	r := chi.NewRouter()
	r.With(ownerAuth).Post("/mcp", srv.HandleRPC)
	lens := httptest.NewServer(r)
	t.Cleanup(lens.Close)

	args, _ := json.Marshal(map[string]any{"idempotency_key": "ada-list-1", "owner_name": "Ada Lovelace",
		"until": time.Now().AddDate(1, 0, 0).Format(time.RFC3339), "ask_above": map[string]int64{"GBP": 50000},
		"payments": []map[string]any{
			{"payee_id": cleaner.ID, "currency": "GBP", "amount_minor": 4000, "every": "week", "reference": "Cleaning"},
			{"payee_id": landlord.ID, "currency": "GBP", "amount_minor": 120000, "every": "month", "reference": "Rent"},
		}})
	cmd := exec.Command("node", filepath.Join(dir, "recurring.mjs"), lens.URL, "owner-connector", string(args))
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("the SDK client failed: %v\n%s%s", err, out, stderr)
	}
	var plan economy.RecurringPlan
	if err := json.Unmarshal(out, &plan); err != nil {
		t.Fatalf("prepare_recurring_payments answered %q: %v", out, err)
	}
	if len(plan.Schedules) != 2 || plan.Status != "awaiting_signature" {
		t.Fatalf("plan = %+v, want two standing orders awaiting the owner's signature", plan)
	}
	for _, sc := range plan.Schedules {
		if sc.Active || !sc.AwaitingSignature || sc.RecurringPlanID != plan.ID {
			t.Errorf("standing order %+v runs before the owner signed", sc)
		}
	}
	var terms economy.PermissionMandateTerms
	if err := json.Unmarshal([]byte(plan.Mandate.Canonical), &terms); err != nil {
		t.Fatal(err)
	}
	if len(terms.Clients) != 1 || terms.Clients[0] != "schedule:"+plan.ID || len(terms.Scope.Payees) != 2 ||
		terms.Limits.PerPayment["GBP"] != 120000 || terms.RequiresApprovalAbove["GBP"] != 50000 || terms.NewPayee.Policy != economy.NewPayeeAlwaysAsk {
		t.Errorf("the mandate draft is %s", plan.Mandate.Canonical)
	}
	// The same list again under the same key is the same plan, not two.
	if out2, err := exec.Command("node", filepath.Join(dir, "recurring.mjs"), lens.URL, "owner-connector", string(args)).Output(); err != nil {
		t.Fatalf("the SDK client failed again: %v", err)
	} else {
		var again economy.RecurringPlan
		_ = json.Unmarshal(out2, &again)
		if again.ID != plan.ID || again.Mandate.Canonical != plan.Mandate.Canonical {
			t.Errorf("the same key prepared plan %s, want %s again", again.ID, plan.ID)
		}
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
	lastRun := func(scheduleID string) economy.AgentScheduleRun {
		t.Helper()
		runs, err := store.ListAgentScheduleRuns(ctx, ws, scheduleID)
		if err != nil || len(runs) == 0 {
			t.Fatalf("runs of %s: %v %v", scheduleID, runs, err)
		}
		return runs[0]
	}
	approve := func(requestID string) {
		t.Helper()
		statement, challenge, _, err := store.PrepareRequestApproval(ctx, ws, requestID, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ApprovePaymentRequest(ctx, ws, requestID, "jwt:user:ada", statement, sign(challenge)); err != nil {
			t.Fatal(err)
		}
	}
	tick := func() {
		t.Helper()
		if _, err := store.RunAgentSchedules(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	byPayee := map[string]string{}
	for _, sc := range plan.Schedules {
		byPayee[sc.ToPayeeID] = sc.ID
	}

	// Unsigned, nothing runs.
	tick()
	if n, _ := paymentsOut(); n != 1 {
		t.Fatalf("%d payments out before the owner signed the mandate, want only the trial clean", n-1)
	}
	if _, err := store.CreatePermissionMandate(ctx, ws, "jwt:user:ada", plan.Mandate.Canonical, sign(plan.Mandate.Challenge)); err != nil {
		t.Fatal(err)
	}

	// First run: each standing order waits for one approval; nothing is paid.
	tick()
	first := map[string]economy.AgentScheduleRun{}
	for payee, id := range byPayee {
		run := lastRun(id)
		if run.Outcome != "awaiting_approval" || run.RequestID == "" {
			t.Fatalf("the first run to %s is %+v, want it waiting for approval", payee, run)
		}
		first[payee] = run
	}
	if n, _ := paymentsOut(); n != 1 {
		t.Fatalf("%d payments out on the first run, before any approval", n-1)
	}
	approve(first[cleaner.ID].RequestID)
	approve(first[landlord.ID].RequestID)
	if n, minor := paymentsOut(); n != 3 || minor != 124100 {
		t.Fatalf("after approving both first runs the ledger has %d payments out of %d, want 3 of 124100", n, minor)
	}

	// Second run, as if the first were 40 days ago: cleaning is inside the mandate and pays on its own; rent is above
	// its ask-above amount.
	if _, err := pool.Exec(ctx, `UPDATE agent_schedule_runs SET tick_at = tick_at - interval '40 days'
		WHERE schedule_id IN (SELECT id FROM agent_payment_schedules WHERE recurring_plan_id = $1)`, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_payment_schedules SET next_run_at = now() - interval '1 second' WHERE recurring_plan_id = $1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	tick()
	if run := lastRun(byPayee[cleaner.ID]); run.Outcome != "paid" || run.PaymentID == "" {
		t.Errorf("the second run to the cleaner is %+v, want paid on its own", run)
	}
	if run := lastRun(byPayee[landlord.ID]); run.Outcome != "awaiting_approval" || run.RequestID == first[landlord.ID].RequestID ||
		!strings.Contains(run.Detail, "approval above") {
		t.Errorf("the second run of the rent is %+v, want a new request waiting for approval above 50000", run)
	}
	if n, minor := paymentsOut(); n != 4 || minor != 128100 {
		t.Errorf("after the second run the ledger has %d payments out of %d, want 4 of 128100", n, minor)
	}
	// No caller takes a tick's key before the tick runs.
	if _, _, err := store.PreparePaymentRequest(ctx, ws, "jwt:user:ada", "schedule:"+byPayee[cleaner.ID]+":next", economy.PaymentRequestIn{
		Client: "chat", Items: []economy.PaymentRequestItemIn{{PayeeID: cleaner.ID, Currency: "GBP", AmountMinor: 1}}}); err == nil {
		t.Error("a chat request took a key reserved for recurring payments")
	}
	// Run again at once: nothing is due, nothing is paid twice.
	tick()
	if n, _ := paymentsOut(); n != 4 {
		t.Errorf("a second tick at once made %d payments out, want 4", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(pushes.titled("Paid")) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if paid := pushes.titled("Paid"); len(paid) != 1 || !strings.Contains(paid[0], "Grace Cleaning") || !strings.Contains(paid[0], "40.00") {
		t.Errorf("the owner's phone was told %q of payments made on their own, want one for Grace Cleaning's £40.00", paid)
	}
}
