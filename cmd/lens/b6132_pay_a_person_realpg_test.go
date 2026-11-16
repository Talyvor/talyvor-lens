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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/workspace"
)

// B61.32 — a person on Talyvor is paid by their name, phone number or email, through the real routes on a migrated
// schema. A person claims a name (look-alike and kept names refused) and says yes to each way of being found; an
// account finds them and is answered their shown name and nothing else, and a name nobody holds answers as one whose
// person has not said yes. The owner's payment, signed with the account's key, arrives at once in the person's account
// in the same currency as one entry of two postings that sum to zero, once whatever is retried, and each side's
// statement shows the other by their shown name. A currency they do not hold, a frozen sender and a frozen person are
// refused with nothing moved; an agent's payment is judged by its rules, and its first to a person waits for the
// owner; an account's lookups stop at its limit an hour.
func TestB6132_PayAPersonByTheirNamePhoneNumberOrEmail(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6132-sender"
	b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, r)
		})
	store, agent := b.store, b.acct.AgentID
	store.SetPersonLookupsPerHour(1000)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	verified := func(w, name string) {
		exec(`INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
			VALUES ($1, 2, 'person', 'test', 'completed', $2, $3)`, w, "ev-"+w, name)
	}
	verified(ws, "Ada Lovelace")
	var companyEUR, partnerEUR string
	if err := pool.QueryRow(ctx, `SELECT (SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'company' AND currency = 'EUR'),
		(SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'EUR')`, ws).Scan(&companyEUR, &partnerEUR); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "company-top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerEUR, AmountMinor: -20_000}, {AccountID: companyEUR, AmountMinor: 20_000}}}); err != nil {
		t.Fatal(err)
	}
	// A person: their people row (B61.22), their first account, a Personal one, and its euro account.
	person := func(id, phone, fullName string) (wsID, eur string) {
		t.Helper()
		identity := "test|" + id
		wsID = workspace.FirstAccountID(identity)
		exec(`INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at) VALUES ($1, $2, $3, true, now())`, id, phone, identity)
		exec(`INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $2, $1, 'personal', 'hybrid')`, wsID, fullName)
		if fullName != "" {
			verified(wsID, fullName)
		}
		a, err := store.OpenCurrencyAccount(ctx, wsID, "", "EUR", "test")
		if err != nil {
			t.Fatal(err)
		}
		return wsID, a.ID
	}
	graceWS, graceEUR := person("per_grace", "+447700900123", "Grace Brewster Hopper")
	person("per_alan", "+447700900456", "Alan Turing")
	person("per_ken", "+447700900789", "") // says yes, but has passed no identity check

	r := chi.NewRouter()
	const secret = "provision-secret"
	mountPeopleFindingRoutes(r, secret, store)
	mountPersonPaymentRoutes(r, store)
	call := func(who *auth.AuthContext, path string, body any) (int, map[string]any, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
		if who == nil {
			req.Header.Set(provisionSecretHeader, secret)
		} else {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out, w.Body.String()
	}
	change := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		code, out, _ := call(nil, "/v1/people/finding/change", body)
		return code, out
	}
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}

	// Claiming a name: kept names, look-alike letters and names that read like one held are refused.
	if code, out := change(map[string]any{"person_id": "per_grace", "name": "Grace.H", "by_name": true, "by_phone": true}); code != http.StatusOK ||
		out["name"] != "grace.h" || out["by_name"] != true || out["by_email"] != false {
		t.Fatalf("Grace claims grace.h = %d %v, want 200, kept lower-case, found by name and number only", code, out)
	}
	for name, want := range map[string]int{
		"help":        http.StatusBadRequest, // kept for Talyvor
		"talyvor.ada": http.StatusBadRequest, // holds talyvor
		"grаce.h":     http.StatusBadRequest, // a Cyrillic а
		"ab":          http.StatusBadRequest,
		"grace..h":    http.StatusBadRequest,
		"GRACE.H":     http.StatusConflict, // held, whatever the case
		"grace_h":     http.StatusConflict, // reads like grace.h
		"grace.h0":    http.StatusOK,
	} {
		if code, out := change(map[string]any{"person_id": "per_alan", "name": name}); code != want {
			t.Errorf("Alan claims %q = %d %v, want %d", name, code, out, want)
		}
	}
	if code, out := change(map[string]any{"person_id": "per_alan", "name": "alan.t", "by_name": false}); code != http.StatusOK {
		t.Fatalf("Alan claims alan.t, not found by it = %d %v", code, out)
	}
	if code, out := change(map[string]any{"person_id": "per_ken", "name": "ken.t", "by_name": true}); code != http.StatusOK {
		t.Fatalf("Ken claims ken.t = %d %v", code, out)
	}
	if code, out := change(map[string]any{"person_id": "per_alan", "by_email": true}); code != http.StatusBadRequest {
		t.Errorf("found by email with no email = %d %v, want 400", code, out)
	}

	// Find: the shown name and nothing else; nobody, not yet yes and not yet checked answer alike.
	find := func(to string) (int, map[string]any, string) {
		return call(owner, "/v1/money/people/find", map[string]any{"to": to})
	}
	for _, to := range []string{"grace.h", "@Grace.H", "+44 7700 900123", "0044 7700 900123"} {
		if code, out, _ := find(to); code != http.StatusOK || len(out) != 1 || out["shown_name"] != "Grace H." {
			t.Errorf("find %q = %d %v, want 200 and only {shown_name: Grace H.}", to, code, out)
		}
	}
	nobodyCode, _, nobody := find("nobody.here")
	if nobodyCode != http.StatusNotFound || !strings.Contains(nobody, "Nobody found. Check the name or number, or ask them to turn on being found in Settings.") {
		t.Fatalf("find nobody = %d %s, want 404 and the words", nobodyCode, nobody)
	}
	for _, to := range []string{"alan.t", "+447700900456", "ken.t", "grace@example.com"} {
		if code, _, body := find(to); code != nobodyCode || body != nobody {
			t.Errorf("find %q = %d %s, want exactly as nobody: %d %s", to, code, body, nobodyCode, nobody)
		}
	}
	if code, out := change(map[string]any{"person_id": "per_grace", "email": "Grace@Example.com", "by_email": true}); code != http.StatusOK {
		t.Fatalf("Grace gives her email = %d %v", code, out)
	}
	if code, out, _ := find("grace@example.com"); code != http.StatusOK || out["shown_name"] != "Grace H." {
		t.Errorf("find by email = %d %v", code, out)
	}

	// The owner pays, signed with the account's key (the phone's).
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	exec(`UPDATE workspace_device_keys SET public_key = $2 WHERE id = $1`, "dev_"+ws, spki)
	payment := func(key, to string, minor int64, currency string) map[string]any {
		return map[string]any{"to": to, "amount_minor": minor, "currency": currency, "note": "Dinner", "idempotency_key": key}
	}
	signed := func(p map[string]any) map[string]any {
		t.Helper()
		code, ch, _ := call(owner, "/v1/money/people/pay/challenge", p)
		challenge, _ := ch["challenge"].(string)
		if code != http.StatusOK || challenge == "" {
			t.Fatalf("the challenge = %d %v", code, ch)
		}
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		out := map[string]any{"assertion": map[string]string{"device_id": "dev_" + ws, "challenge": challenge,
			"signature": base64.RawURLEncoding.EncodeToString(sig)}}
		for k, v := range p {
			out[k] = v
		}
		return out
	}
	pay := func(who *auth.AuthContext, body map[string]any) (int, map[string]any) {
		t.Helper()
		code, out, _ := call(who, "/v1/money/people/pay", body)
		return code, out
	}
	ledger := func() string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT e.id, p.account_id, p.amount_minor, p.currency FROM money_entries e
			JOIN money_postings p ON p.entry_id = e.id WHERE e.kind = 'person_payment' ORDER BY e.created_at, p.line`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, acct, cur string
			var amount int64
			_ = rows.Scan(&id, &acct, &amount, &cur)
			out = append(out, id+" "+acct+" "+jsonOf(amount)+" "+cur)
		}
		return strings.Join(out, "; ")
	}
	balance := func(w, acct string, want int64) {
		t.Helper()
		if got, err := store.Balance(ctx, w, acct); err != nil || got.TestMinor != want {
			t.Fatalf("%s holds %+v (%v), want %d", acct, got, err, want)
		}
	}

	if code, out := pay(owner, payment("k1", "grace.h", 2_500, "EUR")); code != http.StatusForbidden || ledger() != "" {
		t.Fatalf("an unsigned payment = %d %v, ledger %q; want 403 and nothing", code, out, ledger())
	}
	first := signed(payment("k1", "grace.h", 2_500, "EUR"))
	code, paid := pay(owner, first)
	if code != http.StatusCreated || paid["to_shown_name"] != "Grace H." {
		t.Fatalf("the signed payment = %d %v, want 201 to Grace H.", code, paid)
	}
	var keys []string
	for k := range paid {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "amount_minor,created_at,currency,from_account_id,funding,id,idempotency_key,note,to_shown_name" {
		t.Errorf("the payment answers %s: want nothing of the person but their shown name", got)
	}
	id, _ := paid["id"].(string)
	want := id + " " + companyEUR + " -2500 EUR; " + id + " " + graceEUR + " 2500 EUR"
	if got := ledger(); got != want {
		t.Fatalf("the ledger = %q, want one entry of two postings %q", got, want)
	}
	var sum int64
	if err := pool.QueryRow(ctx, `SELECT sum(amount_minor)::bigint FROM money_postings WHERE entry_id = $1`, id).Scan(&sum); err != nil || sum != 0 {
		t.Fatalf("the entry's postings sum to %d (%v), want 0", sum, err)
	}
	balance(ws, companyEUR, 17_500)
	balance(graceWS, graceEUR, 2_500)
	if code, again := pay(owner, first); code != http.StatusCreated || again["id"] != id || ledger() != want {
		t.Fatalf("the same request again = %d %v, ledger %q; want the first payment and nothing more", code, again, ledger())
	}
	if code, out := pay(owner, payment("k1", "grace.h", 9_000, "EUR")); code != http.StatusConflict || ledger() != want {
		t.Errorf("another payment under k1 = %d %v, want 409 and nothing", code, out)
	}
	// Each side's statement shows the other by their shown name, and the note.
	for _, s := range []struct{ ws, acct, other string }{{ws, companyEUR, "Grace H."}, {graceWS, graceEUR, "Ada L."}} {
		st, err := store.AccountStatement(ctx, s.ws, "", s.acct, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		l := st.Lines[len(st.Lines)-1]
		if l.EntryID != id || l.Counterparty != s.other || l.Memo != "Dinner" {
			t.Errorf("%s's statement shows %+v, want the payment from or to %s with its note", s.ws, l, s.other)
		}
	}

	// A currency the person does not hold is refused in words; frozen accounts on either side are refused.
	if code, out := pay(owner, signed(payment("k2", "grace.h", 1_000, "USD"))); code != http.StatusConflict ||
		!strings.Contains(out["error"].(string), "Grace H. has no USD account") {
		t.Errorf("pay in USD = %d %v, want 409 in words", code, out)
	}
	freeze := func(w string) {
		exec(`INSERT INTO compliance_cases (id, workspace_id, kind, subject_kind, subject_id, name, outcome, status)
			VALUES ($1, $2, 'screening', 'payee', 'x', 'x', 'review', 'held') ON CONFLICT DO NOTHING`, "case-"+w, w)
		exec(`INSERT INTO compliance_freezes (workspace_id, case_id, reason, frozen_by) VALUES ($1, $2, 'test', 'operator')`, w, "case-"+w)
	}
	freeze(graceWS)
	if code, out := pay(owner, signed(payment("k3", "grace.h", 1_000, "EUR"))); code != http.StatusConflict ||
		strings.Contains(out["error"].(string), "case-") {
		t.Errorf("pay a frozen person = %d %v, want 409 naming nothing of their case", code, out)
	}
	exec(`DELETE FROM compliance_freezes WHERE workspace_id = $1`, graceWS)
	freeze(ws)
	if code, out := pay(owner, signed(payment("k4", "grace.h", 1_000, "EUR"))); code != http.StatusForbidden {
		t.Errorf("a frozen sender pays = %d %v, want 403", code, out)
	}
	exec(`DELETE FROM compliance_freezes WHERE workspace_id = $1`, ws)
	if got := ledger(); got != want {
		t.Fatalf("the refused payments posted: ledger %q, want %q", got, want)
	}

	// An agent's own key: judged by its rules, and its first payment to a person waits for the owner.
	if _, err := store.SetAgentRules(ctx, ws, agent, economy.AgentRules{Currency: "EUR", MaxPerPaymentMinor: 20_000}); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, agent, "key-agent"); err != nil {
		t.Fatal(err)
	}
	agentKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-agent", Scopes: []string{auth.ScopeProxy}}
	if code, out := pay(agentKey, payment("a1", "grace.h", 30_000, "EUR")); code != http.StatusForbidden {
		t.Errorf("the agent pays €300.00 over its €200.00 a payment = %d %v, want 403", code, out)
	}
	if code, out := pay(agentKey, payment("a2", "+447700900123", 5_000, "EUR")); code != http.StatusAccepted || out["status"] != "pending_approval" {
		t.Errorf("the agent's first payment to Grace = %d %v, want 202 waiting for the owner", code, out)
	}
	if got := ledger(); got != want {
		t.Fatalf("the agent's refused and waiting payments posted: ledger %q, want %q", got, want)
	}
	b.balance(t, 50_000)

	// The account's lookups stop at its limit an hour, said in words.
	store.SetPersonLookupsPerHour(2)
	curious := &auth.AuthContext{WorkspaceID: "ws-b6132-curious", AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	for i := 0; i < 2; i++ {
		if code, _, _ := call(curious, "/v1/money/people/find", map[string]any{"to": "nobody.here"}); code != http.StatusNotFound {
			t.Fatalf("lookup %d = %d, want 404 within the limit", i+1, code)
		}
	}
	if code, out, _ := call(curious, "/v1/money/people/find", map[string]any{"to": "grace.h"}); code != http.StatusTooManyRequests ||
		!strings.Contains(out["error"].(string), "at most 2 people an hour. Try again after") {
		t.Errorf("the third lookup = %d %v, want 429 in words", code, out)
	}
}
