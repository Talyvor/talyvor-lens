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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// b5951Wallet is a workspace of one kind of money with the phone's device key, the company's GBP and USDC accounts
// holding test money, the owner's own bank and someone else's.
type b5951Wallet struct {
	ws, gbp, usdc, ownBank, otherBank string
	call                              func(path, body string) (int, map[string]any)
	sign                              func(body, challenge string) string
}

func newB5951Wallet(t *testing.T, pool *pgxpool.Pool, ws, kind string) b5951Wallet {
	t.Helper()
	ctx := context.Background()
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, b487Screening{})
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetFXMargin(func(ctx context.Context, q economy.FeeQuerier, workspaceID string) (int64, string, error) {
		return billing.FXMarginBPS(ctx, q, workspaceID, fees.Defaults())
	})
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', 'hybrid')`, ws); err != nil {
		t.Fatal(err)
	}
	w := b5951Wallet{ws: ws}
	for _, c := range []struct {
		currency string
		minor    int64
		into     *string
	}{{"GBP", 500_00, &w.gbp}, {"USDC", 500_000_000, &w.usdc}} {
		acct, err := store.OpenCurrencyAccount(ctx, ws, "", c.currency, "test")
		if err != nil {
			t.Fatal(err)
		}
		*c.into = acct.ID
		var partner string
		if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = $2`,
			ws, c.currency).Scan(&partner); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
			IdempotencyKey: "top-up-" + c.currency, Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
				{AccountID: partner, AmountMinor: -c.minor}, {AccountID: acct.ID, AmountMinor: c.minor}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET money_kind = $2 WHERE id = $1`, ws, kind); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyPayeeRoutes(r, store)
	mountMoneyWalletRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	w.call = func(path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if kind != economy.MoneyKindStablecoin {
		for name, body := range map[*string]string{
			&w.ownBank:   `{"name":"Ada Lovelace","country":"GB","sort_code":"200000","account_number":"55779911","own":true}`,
			&w.otherBank: `{"name":"Grace Hopper","country":"GB","sort_code":"200000","account_number":"11223344"}`,
		} {
			code, out := w.call("/v1/money/payees", body)
			if code != http.StatusCreated {
				t.Fatalf("save a payee = %d %v", code, out)
			}
			*name, _ = out["id"].(string)
		}
	}

	// The phone's device key, added once the money is in: from here on a person's money moves only signed.
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation, push_token)
		VALUES ($1, $2, 'test', $3, '\x00', 'tok')`, "dev_"+ws, ws, spki); err != nil {
		t.Fatal(err)
	}
	w.sign = func(body, challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		a, _ := json.Marshal(map[string]string{"device_id": "dev_" + ws, "challenge": challenge, "signature": base64.RawURLEncoding.EncodeToString(sig)})
		return strings.TrimSuffix(body, "}") + `,"assertion":` + string(a) + `}`
	}
	return w
}

// b5951Postings is every posting of the money entries whose key starts with prefix: "purpose currency amount", in order.
func b5951Postings(t *testing.T, pool *pgxpool.Pool, ws, prefix string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT a.purpose, a.currency, p.amount_minor FROM money_entries e
		JOIN money_postings p ON p.entry_id = e.id JOIN money_accounts a ON a.id = p.account_id
		WHERE e.workspace_id = $1 AND e.idempotency_key LIKE $2 || '%' ORDER BY e.created_at, p.line`, ws, prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var purpose, currency string
		var amount int64
		if err := rows.Scan(&purpose, &currency, &amount); err != nil {
			t.Fatal(err)
		}
		out = append(out, purpose+" "+currency+" "+jsonOf(amount))
	}
	return strings.Join(out, "; ")
}

// B59.51 — the owner withdraws a company account to their own bank: only signed by the phone over that withdrawal,
// only to a bank saved as their own, and once under its key; the money leaves through the payments-out hold to the
// partner, each entry summing to zero.
func TestB5951_OwnerWithdrawsToTheirOwnBank(t *testing.T) {
	pool := agentRoutesDB(t)
	w := newB5951Wallet(t, pool, "ws-b5951-withdraw", economy.MoneyKindHybrid)
	path := "/v1/money/accounts/" + w.gbp + "/withdraw"
	withdrawal := func(payee, key string, minor int) string {
		return `{"payee_id":"` + payee + `","amount_minor":` + jsonOf(minor) + `,"idempotency_key":"` + key + `"}`
	}

	code, ch := w.call(path+"/challenge", withdrawal(w.ownBank, "w1", 2000))
	challenge, _ := ch["challenge"].(string)
	if code != http.StatusOK || challenge == "" {
		t.Fatalf("the withdrawal's challenge = %d %v, want 200 with a challenge", code, ch)
	}
	if code, out := w.call(path, withdrawal(w.ownBank, "w1", 2000)); code != http.StatusForbidden {
		t.Errorf("an unsigned withdrawal = %d %v, want 403", code, out)
	}
	if code, out := w.call(path, w.sign(withdrawal(w.ownBank, "w1", 9000), challenge)); code != http.StatusForbidden {
		t.Errorf("a withdrawal signed for another amount = %d %v, want 403", code, out)
	}
	code, other := w.call(path+"/challenge", withdrawal(w.otherBank, "w2", 2000))
	if code != http.StatusConflict {
		t.Errorf("a withdrawal's challenge to someone else's bank = %d %v, want 409", code, other)
	}
	if got := b5951Postings(t, pool, w.ws, "payment_out"); got != "" {
		t.Fatalf("the refused withdrawals posted %q, want nothing on the ledger", got)
	}

	body := w.sign(withdrawal(w.ownBank, "w1", 2000), challenge)
	code, out := w.call(path, body)
	if code != http.StatusCreated || out["reference"] != "Withdrawal" {
		t.Fatalf("the signed withdrawal = %d %v, want 201 with the reference Withdrawal", code, out)
	}
	want := "company GBP -2000; hold GBP 2000; hold GBP -2000; partner GBP 2000"
	if got := b5951Postings(t, pool, w.ws, "payment_out"); got != want {
		t.Fatalf("the withdrawal posted %q, want %q", got, want)
	}
	if code, again := w.call(path, body); code != http.StatusCreated || again["id"] != out["id"] {
		t.Errorf("the same withdrawal again = %d %v, want the same one", code, again)
	}
	if got := b5951Postings(t, pool, w.ws, "payment_out"); got != want {
		t.Errorf("the withdrawal sent twice posted %q, want it once: %q", got, want)
	}
}

// B59.51 — a hybrid workspace converts between its fiat and stablecoin sides: quoted, signed by the phone over that
// conversion, moved once under its key with each currency summing to zero; a fiat-only or stablecoin-only workspace
// is refused with wrong_money_kind and nothing is quoted or posted.
func TestB5951_HybridConvertsBetweenTheTwoSides(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	w := newB5951Wallet(t, pool, "ws-b5951-convert", economy.MoneyKindHybrid)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_contracts (workspace_id, plan, fx_margin_bps, set_by) VALUES ($1, 'enterprise', 50, 'test')`, w.ws); err != nil {
		t.Fatal(err)
	}

	code, q := w.call("/v1/money/convert/quote", `{"sell_currency":"GBP","buy_currency":"USDC","sell_minor":10000}`)
	quote, _ := q["id"].(string)
	if code != http.StatusCreated || quote == "" {
		t.Fatalf("the quote = %d %v, want 201", code, q)
	}
	convert := func(key string) string { return `{"quote_id":"` + quote + `","idempotency_key":"` + key + `"}` }
	code, ch := w.call("/v1/money/convert/challenge", convert("c1"))
	challenge, _ := ch["challenge"].(string)
	if code != http.StatusOK || challenge == "" {
		t.Fatalf("the conversion's challenge = %d %v, want 200 with a challenge", code, ch)
	}
	if code, out := w.call("/v1/money/convert", convert("c1")); code != http.StatusForbidden {
		t.Errorf("an unsigned conversion = %d %v, want 403", code, out)
	}
	if code, out := w.call("/v1/money/convert", w.sign(convert("c2"), challenge)); code != http.StatusForbidden {
		t.Errorf("a conversion signed under another key = %d %v, want 403", code, out)
	}
	if got := b5951Postings(t, pool, w.ws, "fx_convert"); got != "" {
		t.Fatalf("the refused conversions posted %q, want nothing on the ledger", got)
	}

	body := w.sign(convert("c1"), challenge)
	code, out := w.call("/v1/money/convert", body)
	if code != http.StatusOK || out["status"] != economy.ConversionConverted {
		t.Fatalf("the signed conversion = %d %v, want 200 converted", code, out)
	}
	// The contract's margin on £100.00 is 50 basis points, 50p; £99.50 at the test rate of $1.25 buys 124.375 USDC.
	want := "company GBP -10000; revenue GBP 50; partner GBP 9950; partner USDC -124375000; company USDC 124375000"
	if got := b5951Postings(t, pool, w.ws, "fx_convert"); got != want {
		t.Fatalf("the conversion posted %q, want %q", got, want)
	}
	if code, again := w.call("/v1/money/convert", body); code != http.StatusOK || again["id"] != out["id"] {
		t.Errorf("the same conversion again = %d %v, want the same one", code, again)
	}
	if got := b5951Postings(t, pool, w.ws, "fx_convert"); got != want {
		t.Errorf("the conversion sent twice posted %q, want it once: %q", got, want)
	}

	for _, kind := range []string{economy.MoneyKindFiat, economy.MoneyKindStablecoin} {
		single := newB5951Wallet(t, pool, "ws-b5951-"+kind, kind)
		code, out := single.call("/v1/money/convert/quote", `{"sell_currency":"GBP","buy_currency":"USDC","sell_minor":10000}`)
		if code != http.StatusConflict || out["code"] != "wrong_money_kind" || out["money_kind"] != kind {
			t.Errorf("a %s workspace's quote = %d %v, want 409 wrong_money_kind", kind, code, out)
		}
		var quotes int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_conversions WHERE workspace_id = $1`, single.ws).Scan(&quotes); err != nil {
			t.Fatal(err)
		}
		if got := b5951Postings(t, pool, single.ws, "fx_convert"); quotes != 0 || got != "" {
			t.Errorf("a %s workspace's refused conversion kept %d quotes and posted %q, want none", kind, quotes, got)
		}
	}
}
