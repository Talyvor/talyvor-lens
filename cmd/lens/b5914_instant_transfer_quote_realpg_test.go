package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B59.14 on real Postgres — the Instant transfer quote, locked for 30 seconds (launch pack 4.1, 4.3), through the money
// kind gate and the handler as main.go mounts them. At the Test FX partner's fixed rates ($1.25 a pound, $1.10 a euro,
// USDC at the dollar) and a 50 basis point FX margin: for each pair Lens supports, the fee rounds up and each
// conversion rounds what it buys down, so the quote's "they get" is what arrives; the quote expires 30 seconds after it
// was given; a bank account paid in the currency sent is paid straight, as fast and free of conversion; and a fiat or
// stablecoin workspace is refused with wrong_money_kind as a field, at the route and in economy.
func TestB5914_InstantTransferQuote(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	for _, kind := range []string{"fiat", "stablecoin", "hybrid"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', $2)`,
			"ws-b5914-"+kind, kind); err != nil {
			t.Fatal(err)
		}
	}
	const marginBPS = 50
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetFXMargin(func(context.Context, economy.FeeQuerier, string) (int64, string, error) {
		return marginBPS, "team", nil
	})
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(registry, "b5914"), Limits: store.MoneyLimits(), Audit: store,
		Hooks: store.InstantTransferHooks(&partners.TestTransferHooks{})})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			owner := &auth.AuthContext{WorkspaceID: req.Header.Get("X-Test-Workspace"), AuthMethod: auth.MethodJWT, UserID: "owner",
				Scopes: []string{auth.ScopeKeys}}
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), owner)))
		})
	})
	r.Group(func(g chi.Router) {
		g.Use(moneyKindGate(store))
		mountInstantTransferRoutes(g, store)
	})
	quote := func(kind string, in economy.InstantQuoteRequest) (int, map[string]any, economy.InstantQuote) {
		t.Helper()
		body, _ := json.Marshal(in)
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-b5914-"+kind+"/money/instant-transfers/quote", strings.NewReader(string(body)))
		req.Header.Set("X-Test-Workspace", "ws-b5914-"+kind)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var fields map[string]any
		var q economy.InstantQuote
		_ = json.Unmarshal(rec.Body.Bytes(), &fields)
		_ = json.Unmarshal(rec.Body.Bytes(), &q)
		return rec.Code, fields, q
	}

	t.Run("each pair rounds the fee up and what arrives down", func(t *testing.T) {
		usdPerUnit := map[string]*big.Rat{"GBP": big.NewRat(5, 4), "EUR": big.NewRat(11, 10), "USD": big.NewRat(1, 1)}
		decimals := map[string]int64{"GBP": 2, "EUR": 2, "USD": 2, "USDC": 6}
		floor := func(r *big.Rat) int64 { return new(big.Int).Quo(r.Num(), r.Denom()).Int64() }
		scale := func(from, to string) *big.Rat {
			return new(big.Rat).SetFrac(new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals[to]), nil),
				new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals[from]), nil))
		}
		// £100.01 at 50 basis points is a fee of 50.005p: rounded up, 51p.
		const send = 100_01
		const fee = 51
		for _, from := range []string{"GBP", "EUR", "USD"} {
			for _, to := range []string{"GBP", "EUR", "USD", "USDC"} {
				code, body, q := quote("hybrid", economy.InstantQuoteRequest{SendMinor: send, SendCurrency: from, ReceiveCurrency: to, To: economy.InstantToAccount})
				if code != http.StatusCreated {
					t.Fatalf("%s to %s: %d %v", from, to, code, body)
				}
				onchain := floor(new(big.Rat).Mul(new(big.Rat).Mul(big.NewRat(send-fee, 1), usdPerUnit[from]), scale(from, "USDC")))
				want := onchain
				if to != "USDC" {
					want = floor(new(big.Rat).Mul(new(big.Rat).Quo(big.NewRat(onchain, 1), usdPerUnit[to]), scale("USDC", to)))
				}
				if q.SendMinor != send || q.SendCurrency != from || q.FeeMinor != fee || q.FeeBPS != marginBPS || q.ReceiveMinor != want ||
					q.ReceiveCurrency != to || !q.ArrivesInSeconds || q.ArrivesWithinSeconds != 0 || q.Rate == "" {
					t.Errorf("%s to %s: quoted %+v, want %d sent, a fee of %d, %d %s arriving in seconds", from, to, q, send, fee, want, to)
				}
				if kept, err := store.InstantTransferQuote(ctx, "ws-b5914-hybrid", q.ID); err != nil || kept.ReceiveMinor != q.ReceiveMinor ||
					kept.FeeMinor != q.FeeMinor || !kept.ExpiresAt.Equal(q.ExpiresAt) {
					t.Errorf("%s to %s: the quote kept is %+v (%v), want the one given, %+v", from, to, kept, err, q)
				}
			}
		}
		// By hand, one pair: £100.01 less 51p is £99.50, $124.375 in USDC, €113.068… — rounded down, €113.06.
		if _, _, q := quote("hybrid", economy.InstantQuoteRequest{SendMinor: send, SendCurrency: "GBP", ReceiveCurrency: "EUR", To: economy.InstantToAccount}); q.ReceiveMinor != 113_06 || q.Rate != "1.13628141" {
			t.Errorf("£100.01 to euros: they get %d at %s, want 11306 at 1.13628141", q.ReceiveMinor, q.Rate)
		}
	})

	t.Run("it is locked for 30 seconds", func(t *testing.T) {
		_, body, q := quote("hybrid", economy.InstantQuoteRequest{SendMinor: 50_00, SendCurrency: "EUR", ReceiveCurrency: "USDC", To: economy.InstantToWallet})
		if got := q.ExpiresAt.Sub(q.CreatedAt); got != 30*time.Second || body["expires_at"] == nil {
			t.Fatalf("the quote is good for %s (%v), want 30s", got, body)
		}
		if err := q.Live(q.ExpiresAt.Add(-time.Millisecond)); err != nil {
			t.Errorf("a millisecond before 30 seconds: %v, want it live", err)
		}
		if err := q.Live(q.ExpiresAt); !errors.Is(err, economy.ErrQuote) {
			t.Errorf("at 30 seconds: %v, want ErrQuote", err)
		}
	})

	t.Run("the route is picked for the owner", func(t *testing.T) {
		// Pounds into a UK bank account: paid straight on Faster Payments, as fast and with nothing taken by a conversion.
		_, body, q := quote("hybrid", economy.InstantQuoteRequest{SendMinor: 100_01, SendCurrency: "GBP", ReceiveCurrency: "GBP", To: economy.InstantToBank, Country: "GB"})
		if q.FeeMinor != 0 || q.ReceiveMinor != 100_01 || q.ArrivesInSeconds || q.ArrivesWithinSeconds != 2*60*60 || q.Rate != "1.00000000" {
			t.Errorf("pounds to a UK bank: %v, want £100.01 arriving, no fee, within Faster Payments' 2 hours", body)
		}
		if _, plumbing := body["route"]; plumbing {
			t.Errorf("the quote shows its route: %v", body)
		}
		// Pounds into a euro account: converted, moved and paid out on SEPA Instant, whose real time the quote carries.
		_, body, q = quote("hybrid", economy.InstantQuoteRequest{SendMinor: 100_01, SendCurrency: "GBP", ReceiveCurrency: "EUR", To: economy.InstantToBank, Country: "DE"})
		if q.FeeMinor != 51 || q.ReceiveMinor != 113_06 || q.ArrivesInSeconds || q.ArrivesWithinSeconds != 10 {
			t.Errorf("pounds to a euro bank: %v, want a 51p fee, €113.06 arriving within SEPA Instant's 10 seconds", body)
		}
	})

	t.Run("only a hybrid workspace gets one", func(t *testing.T) {
		in := economy.InstantQuoteRequest{SendMinor: 10_00, SendCurrency: "GBP", ReceiveCurrency: "USDC", To: economy.InstantToWallet}
		for _, kind := range []string{"fiat", "stablecoin"} {
			if code, body, _ := quote(kind, in); code != http.StatusConflict || body["code"] != "wrong_money_kind" || body["money_kind"] != kind {
				t.Errorf("a %s workspace's quote: %d %v, want 409 wrong_money_kind naming %s", kind, code, body, kind)
			}
			_, err := store.QuoteInstantTransfer(ctx, "ws-b5914-"+kind, in)
			var refusal *economy.MoneyKindRefusal
			if !errors.As(err, &refusal) || refusal.Field != "instant_transfer" || refusal.MoneyKind != kind {
				t.Errorf("a %s workspace quoting past the route (the connector's path): %v, want a refusal on instant_transfer", kind, err)
			}
		}
		var quotes int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM instant_transfer_quotes WHERE workspace_id <> 'ws-b5914-hybrid'`).Scan(&quotes); err != nil || quotes != 0 {
			t.Errorf("%d quotes kept for a workspace that is not hybrid (%v)", quotes, err)
		}
	})

	t.Run("each leg's record is kept", func(t *testing.T) {
		hooks := store.InstantTransferHooks(&partners.TestTransferHooks{})
		leg := partners.LegRecord{Workspace: "ws-b5914-hybrid", Transfer: "itx_1", Leg: "convert", Method: "convert", Key: "k1", Partner: "sandbox",
			Outcome: "ok", Amount: partners.Money{Minor: 99_50, Currency: "GBP"}}
		for range 2 {
			if err := hooks.RecordLeg(ctx, leg); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM instant_transfer_legs WHERE transfer_id = 'itx_1' AND amount_minor = 9950`).Scan(&n); err != nil || n != 1 {
			t.Errorf("a leg recorded twice under its key keeps %d rows (%v), want 1", n, err)
		}
	})
}
