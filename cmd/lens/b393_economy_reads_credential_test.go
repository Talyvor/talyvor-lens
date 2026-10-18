package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
)

// B39.3 — THE TOKEN-ECONOMY READS NEED A CREDENTIAL.
//
// The router is built inline in run() (see public_node_discovery_mount_test.go), so the wiring is
// read from main.go: for each route, whether its group runs AuthMiddleware and whether its handler
// is wrapped in requireAdmin. That wiring is then replayed onto a chi router with the real
// AuthMiddleware and requireAdmin, and each route is requested with no credential, a non-admin
// credential and the admin key.
func TestB393_EconomyReadsNeedACredential(t *testing.T) {
	type want struct{ none, user, admin int }
	routes := map[string]want{
		"/v1/tokens/rates":            {401, 200, 200},
		"/v1/economy/stats":           {401, 200, 200},
		"/v1/insights/routing":        {401, 401, 200},
		"/v1/oracle/stats":            {401, 401, 200},
		"/v1/economy/conversion-rate": {200, 200, 200}, // the suite's pricing page reads it with none
	}

	wanted := map[string]bool{}
	for p := range routes {
		wanted[strconv.Quote(p)] = true
	}
	mounts := mountsFor(t, wanted)
	admin := adminWrapped(t, wanted)

	am := opDefaultManager(t)
	r := chi.NewRouter()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(nil), am))
		for p := range routes {
			m, found := mounts[strconv.Quote(p)]
			if !found {
				t.Fatalf("%s is not registered in main.go — a parse that finds nothing passes everything", p)
			}
			var h http.Handler = http.HandlerFunc(ok)
			if admin[strconv.Quote(p)] {
				h = requireAdmin(am, h)
			}
			on := chi.Router(authed)
			if !containsName(m.useNames, authMiddlewareName) {
				on = r
			}
			on.Get(p, h.ServeHTTP)
		}
	})

	call := func(p, bearer string) int {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	for p, w := range routes {
		if got := call(p, ""); got != w.none {
			t.Errorf("GET %s with no credential = %d, want %d", p, got, w.none)
		}
		if got := call(p, testOperatorReadKey); got != w.user {
			t.Errorf("GET %s with a non-admin credential = %d, want %d", p, got, w.user)
		}
		if got := call(p, testGlobalKey); got != w.admin {
			t.Errorf("GET %s with the admin key = %d, want %d", p, got, w.admin)
		}
	}
}

// adminWrapped reports, for each wanted path literal registered in main.go, whether the handler
// passed alongside it is a requireAdmin(...) call.
func adminWrapped(t *testing.T, wanted map[string]bool) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		for i, arg := range call.Args {
			if bl, isLit := arg.(*ast.BasicLit); isLit && wanted[bl.Value] && i+1 < len(call.Args) {
				out[bl.Value] = exprName(call.Args[i+1]) == "requireAdmin"
			}
		}
		return true
	})
	return out
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
