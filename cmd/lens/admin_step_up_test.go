package main

import (
	"context"
	"encoding/base32"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/stepup"
)

// B28.264 — the conversion-rate change, every payout revoke and the LXC grant are registered behind
// requireAdminStepUp, read from main.go's AST: the handler argument of each registration is that call.
func TestAdminStepUp_DangerousRoutesAreRegisteredBehindIt(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", readMainGo(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{ // path → behind requireAdminStepUp
		"/v1/admin/conversion-rate/approve":       false,
		"/v1/admin/held-mints/":                   false, // the loop over the four mint tables
		"/v1/admin/held-mints/traffic/adjudicate": false,
		"/v1/admin/lxc/grant":                     false,
		"/v1/admin/workspaces/{wsID}/money-kind":  false, // B61.6
		"/v1/admin/people/move-number":            false, // B61.30
		"/v1/admin/partner-keys/{id}/revoke":      false, // B62.5
	}
	seen := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		for _, a := range call.Args {
			lit := adminPathLiteral(a)
			if lit == nil {
				continue
			}
			p, _ := strconv.Unquote(lit.Value)
			if _, ok := want[p]; !ok {
				continue
			}
			seen[p] = true
			h, ok := call.Args[len(call.Args)-1].(*ast.CallExpr)
			if ok && types.ExprString(h.Fun) == "requireAdminStepUp" {
				want[p] = true
			}
		}
		return true
	})
	for p, gated := range want {
		switch {
		case !seen[p]:
			t.Errorf("%s is not registered in main.go at all — this check is looking at nothing", p)
		case !gated:
			t.Errorf("%s is registered without requireAdminStepUp: the admin key alone takes it", p)
		}
	}
}

type fakeAdminUses struct {
	rows []operatoraudit.Entry
	err  error
}

func (f *fakeAdminUses) Record(_ context.Context, e operatoraudit.Entry) (operatoraudit.Entry, error) {
	if f.err != nil {
		return operatoraudit.Entry{}, f.err
	}
	f.rows = append(f.rows, e)
	return e, nil
}

// B28.264 — the admin key alone is refused and runs nothing; with the operator's current code the action runs once,
// after its audit row names the operator, the action, the path and the method; a row that cannot be written stops it.
func TestAdminStepUp_AdminKeyAloneIsRefusedAndEachUseIsAudited(t *testing.T) {
	key := []byte("0123456789abcdef0123")
	v, err := stepup.New(base32.StdEncoding.EncodeToString(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	action := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { ran++; w.WriteHeader(http.StatusOK) })
	uses := &fakeAdminUses{}
	h := requireAdminStepUp(headerAdmin{}, v, uses, "lxc.grant", action)
	do := func(h http.Handler, code string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/lxc/grant", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer admin-key")
		req.Header.Set(moderatorOperatorHeader, "nicolai")
		req.Header.Set(stepUpHeader, code)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := do(h, ""); got != http.StatusForbidden || ran != 0 || len(uses.rows) != 0 {
		t.Fatalf("admin key alone: %d, ran %d times, %d audit rows; want 403, never, none", got, ran, len(uses.rows))
	}
	if got := do(h, stepup.CodeAt(key, time.Now())); got != http.StatusOK || ran != 1 {
		t.Fatalf("with a current code: %d, ran %d times; want 200, once", got, ran)
	}
	want := operatoraudit.Entry{Actor: "nicolai", Action: "lxc.grant", Target: "POST /v1/admin/lxc/grant", Detail: "method: admin key + step-up code"}
	if len(uses.rows) != 1 || uses.rows[0] != want {
		t.Fatalf("audit rows = %+v; want exactly %+v", uses.rows, want)
	}

	broken := requireAdminStepUp(headerAdmin{}, v, &fakeAdminUses{err: errors.New("db down")}, "lxc.grant", action)
	if got := do(broken, stepup.CodeAt(key, time.Now().Add(30*time.Second))); got != http.StatusServiceUnavailable || ran != 1 {
		t.Fatalf("audit row not written: %d, ran %d times; want 503 and the action not run", got, ran)
	}
}
