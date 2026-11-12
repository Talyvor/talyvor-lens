package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B59.7 — Lens refuses a money route of the wrong kind (launch pack 3.1): a fiat workspace calling an on-chain route,
// and a stablecoin workspace calling a fiat route, is refused with code wrong_money_kind and the workspace's kind.

// b597Scope is what a money route is: every route cmd/lens registers under one of these, written with every parameter
// as {}. A route under them that moneyRoutes does not name fails TestB597_EveryMoneyRouteHasASide.
var b597Scope = []string{
	"/v1/money/", "/v1/pay/", "/v1/wallets", "/v1/agent-cards",
	"/v1/workspaces/{}/agents", "/v1/workspaces/{}/wallet", "/v1/workspaces/{}/transfers",
	"/v1/workspaces/{}/money-requests", "/v1/workspaces/{}/lxc", "/v1/workspaces/{}/escrows",
	"/v1/workspaces/{}/cash-outs", "/v1/workspaces/{}/credit-line", "/v1/workspaces/{}/account",
}

// b597Registration is a route registration in cmd/lens's source: the method and the path literal. A path built with
// "+" (a loop over its last segment) reads as its literal part, ending in "/".
var b597Registration = regexp.MustCompile(`\b\w+\.(?:With\([^)]*\)\.)?(Get|Post|Put|Delete|Patch)\("(/[^"]*)"`)

type b597Route struct{ method, pattern string }

// b597MoneyRoutes is every money route cmd/lens registers, read from its source; a pattern ending in "/" is expanded
// to the table's paths that continue it.
func b597MoneyRoutes(t *testing.T) []b597Route {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list cmd/lens: %v", err)
	}
	seen := map[b597Route]bool{}
	var out []b597Route
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range b597Registration.FindAllStringSubmatch(string(src), -1) {
			blank := blankRouteParams(m[2])
			if !b597InScope(blank) {
				continue
			}
			patterns := []string{m[2]}
			if strings.HasSuffix(m[2], "/") {
				patterns = nil
				for p := range moneyRoutes {
					if rest, ok := strings.CutPrefix(p, blank); ok && rest != "" && !strings.Contains(rest, "/") {
						patterns = append(patterns, m[2]+rest)
					}
				}
				if len(patterns) == 0 {
					patterns = []string{m[2]} // no entry continues it: the census names it
				}
			}
			for _, p := range patterns {
				r := b597Route{strings.ToUpper(m[1]), p}
				if !seen[r] {
					seen[r] = true
					out = append(out, r)
				}
			}
		}
	}
	if len(out) < 150 {
		t.Fatalf("read %d money routes from cmd/lens: the scan is not matching", len(out))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pattern+out[i].method < out[j].pattern+out[j].method })
	return out
}

func b597InScope(blank string) bool {
	for _, p := range b597Scope {
		if blank == strings.TrimSuffix(p, "/") || strings.HasPrefix(blank, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

// Every money route is named in moneyRoutes, and every entry there is a route Lens registers.
func TestB597_EveryMoneyRouteHasASide(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range b597MoneyRoutes(t) {
		blank := blankRouteParams(r.pattern)
		registered[blank] = true
		if _, ok := moneyRoutes[blank]; !ok {
			t.Errorf("%s %s is a money route with no side in moneyRoutes (money_kind_gate.go): name it fiat, on-chain or either", r.method, r.pattern)
		}
	}
	for p := range moneyRoutes {
		if !registered[p] {
			t.Errorf("moneyRoutes names %s, which cmd/lens does not register", p)
		}
	}
}

// b597Kinds answers every workspace's kind of money as kind.
type b597Kinds struct{ kind string }

func (k b597Kinds) WorkspaceMoneyKind(context.Context, string) (string, error) { return k.kind, nil }

// b597Router is every money route behind the gate, each answering 200 when it is reached, called by the workspace's
// owner.
func b597Router(t *testing.T, routes []b597Route, kind string) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "ws-b597"})))
		})
	})
	r.Group(func(g chi.Router) { // as main.go's authed group: a group's middleware runs once the route is matched
		g.Use(moneyKindGate(b597Kinds{kind}))
		for _, rt := range routes {
			g.Method(rt.method, rt.pattern, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		}
	})
	return r
}

var b597Param = regexp.MustCompile(`\{[^}]*\}`)

// A fiat workspace is refused every on-chain route and a stablecoin workspace every fiat one, with code wrong_money_kind
// and the workspace's kind; a hybrid workspace reaches every money route, and each kind every route of its own side and
// every route of either.
func TestB597_AMoneyRouteOfTheWrongKindIsRefused(t *testing.T) {
	routes := b597MoneyRoutes(t)
	refusedTo := map[moneySide]string{moneySideFiat: economy.MoneyKindStablecoin, moneySideOnChain: economy.MoneyKindFiat}
	sides := map[moneySide]int{}
	for _, kind := range []string{economy.MoneyKindFiat, economy.MoneyKindStablecoin, economy.MoneyKindHybrid} {
		h := b597Router(t, routes, kind)
		for _, rt := range routes {
			side := moneyRoutes[blankRouteParams(rt.pattern)]
			if kind == economy.MoneyKindHybrid {
				sides[side]++
			}
			t.Run(kind+" "+rt.method+" "+rt.pattern, func(t *testing.T) {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(rt.method, b597Param.ReplaceAllString(rt.pattern, "ws-b597"), nil))
				if refusedTo[side] != kind {
					if rec.Code != http.StatusOK {
						t.Fatalf("a %s workspace calling a %s route = %d %s, want it reached", kind, side, rec.Code, rec.Body.String())
					}
					return
				}
				var out map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				if rec.Code != http.StatusConflict || out["code"] != "wrong_money_kind" || out["money_kind"] != kind {
					t.Fatalf("a %s workspace calling a %s route = %d %s, want 409 wrong_money_kind naming %s", kind, side, rec.Code,
						rec.Body.String(), kind)
				}
			})
		}
	}
	if sides[moneySideFiat] == 0 || sides[moneySideOnChain] == 0 {
		t.Fatalf("money routes by side %v: no fiat or no on-chain route was tested", sides)
	}
}

// The gate is on the authed group, after the tenant check, so it judges every money route an owner or an agent calls.
func TestB597_TheGateIsOnTheAuthedGroup(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	gate, tenant := strings.Index(s, "authed.Use(moneyKindGate(dualToken))"), strings.Index(s, "authed.Use(workspaceIsolationMiddleware)")
	if gate < 0 || tenant < 0 || gate < tenant {
		t.Fatal("main.go does not put moneyKindGate(dualToken) on the authed group after workspaceIsolationMiddleware")
	}
}

var b597CardRoute = regexp.MustCompile(`(^|/|-)cards?(/|$)`)

// Every kind gets cards (launch pack 3.3): a stablecoin workspace's card spends from its stablecoin balance, so no card
// route is fiat. The test above then proves each kind reaches them.
func TestB597_EveryKindGetsCards(t *testing.T) {
	cards := 0
	for p, side := range moneyRoutes {
		if !b597CardRoute.MatchString(p) {
			continue
		}
		cards++
		if side != moneySideEither {
			t.Errorf("moneyRoutes names the card route %s %s: every kind gets cards, so it is either", p, side)
		}
	}
	if cards < 10 {
		t.Fatalf("matched %d card routes in moneyRoutes: the match is not finding them", cards)
	}
}
