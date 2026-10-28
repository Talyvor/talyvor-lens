package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/migrations"
)

// B50.2 — every JSON body the agent, money, marketplace and room routes take, fuzzed against a real database:
// bad input never panics, never answers 500, and no answer carries SQL, a stack trace or an internal address.
// `go test` runs the seeds on every route; `go test -fuzz FuzzRequestBodies` searches further.

const fuzzWS = "ws-b502-fuzz"

// leak is what an answer to a caller may never carry.
// Each is shaped like a real driver or runtime message, so a random token in an answer cannot match it.
var leak = regexp.MustCompile(`SQLSTATE|pgx:|pgconn|pq: |goroutine \d|runtime error|\.go:\d|dial tcp|violates (?:foreign key|unique|not-null|check)|duplicate key value|syntax error at`)

func fuzzBodiesRouter(tb testing.TB) (http.Handler, []string) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		tb.Skip("LENS_TEST_DATABASE_URL not set — skipping the real-PG request body fuzz")
	}
	const schema = "cmd_b502_body_fuzz"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		tb.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		tb.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			tb.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		tb.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		tb.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, fuzzWS); err != nil {
		tb.Fatal(err)
	}

	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetApprovalAuth(passkey.RelyingParty{ID: "app.talyvor.test", Origins: []string{"https://app.talyvor.test"}}, nil)
	marketStore := market.NewStore(pool)
	roomStore := rooms.NewStore(pool, 0)
	roomStore.SetMarket(marketStore)
	r := chi.NewRouter()
	r.Use(api.BodyLimitMiddleware(api.MaxRequestBody))
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	mountWalletCapabilityRoutes(r, bank)
	mountAgentTransferRoutes(r, bank)
	mountCreditLineRoutes(r, bank)
	mountCompanyLoanRoutes(r, bank)
	mountAgentEscrowRoutes(r, bank)
	mountAgentPotRoutes(r, bank)
	mountSimTradingRoutes(r, bank)
	mountCashOutRoutes(r, bank)
	mountMoneyAccountRoutes(r, bank)
	mountMoneyPayeeRoutes(r, bank)
	mountMandateRoutes(r, bank)
	mountPermissionMandateRoutes(r, bank)
	mountPaymentRequestRoutes(r, bank)
	mountDeviceKeyRoutes(r, bank)
	mountMoneyPaymentRoutes(r, bank)
	mountMoneyPayoutRoutes(r, bank)
	mountMoneyCardRoutes(r, bank)
	mountInvoiceRoutes(r, bank)
	mountCapabilityTermsRoutes(r, bank)
	mountMarketRoutes(r, marketStore)
	mountMarketDiscoveryRoutes(r, marketStore)
	mountRoomRoutes(r, roomStore)

	param := regexp.MustCompile(`\{[^}]*\}`)
	var routes []string
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
			path := param.ReplaceAllStringFunc(route, func(p string) string {
				if strings.HasPrefix(strings.ToLower(p), "{ws") || strings.HasPrefix(strings.ToLower(p), "{workspace") {
					return fuzzWS
				}
				return "00000000-0000-4000-8000-000000000001"
			})
			routes = append(routes, method+" "+path)
		}
		return nil
	})
	if len(routes) < 50 {
		tb.Fatalf("only %d write routes mounted — the harness lost its routes", len(routes))
	}
	owner := &auth.AuthContext{WorkspaceID: fuzzWS, AuthMethod: auth.MethodJWT, UserID: "owner",
		Scopes: []string{auth.ScopeKeys, auth.ScopeProxy, auth.ScopeAnalytics, auth.ScopeMint}}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), owner)))
	}), routes
}

var fuzzBodySeeds = []string{
	``, `null`, `[]`, `"x"`, `{`, `{}`, `{"a":`, "\xff\xfe{}", `{"amount":-1,"amount_minor":-1,"amount_ulxc":-1}`,
	`{"amount":1e309,"amount_minor":9223372036854775808,"currency":"\u0000"}`, `{"amount_minor":1.5,"currency":"XXX"}`,
	`{"name":"` + strings.Repeat("A", 70000) + `"}`, strings.Repeat(`[`, 100000), `{"id":{"$gt":""},"name":["a"]}`,
}

func FuzzRequestBodies(f *testing.F) {
	h, routes := fuzzBodiesRouter(f)
	for i := range routes {
		for _, s := range fuzzBodySeeds {
			f.Add(uint16(i), []byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, at uint16, body []byte) {
		route := routes[int(at)%len(routes)]
		method, path, _ := strings.Cut(route, " ")
		req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		done := make(chan any, 1)
		go func() {
			defer func() { done <- recover() }()
			h.ServeHTTP(w, req)
		}()
		select {
		case p := <-done:
			if p != nil {
				t.Fatalf("%s panicked on body %.80q: %v", route, body, p)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not answer in 10s on body %.80q", route, body)
		}
		if w.Code == http.StatusInternalServerError {
			t.Fatalf("%s answered 500 on body %.80q: %s", route, body, w.Body.String())
		}
		if m := leak.FindString(w.Body.String()); m != "" {
			t.Fatalf("%s answered %d carrying %q: %s", route, w.Code, m, fmt.Sprintf("%.300s", w.Body.String()))
		}
	})
}
