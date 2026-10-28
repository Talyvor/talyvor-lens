package secretstatic

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// secretName is a variable or field that holds a key, secret, token or password a caller presents.
var secretName = regexp.MustCompile(`(?i)(secret|password|passwd|api_?key|globalkey|mintkey|readkey|gatekey|synthkey|admintoken|bearer)$`)

// TestSecretsAreComparedInConstantTime fails when a non-test file compares a value named like a secret with == or !=
// against anything but a literal or nil: such a compare stops at the first wrong byte and leaks how much was right.
// Use subtle.ConstantTimeCompare or hmac.Equal, or look the secret up by its hash.
func TestSecretsAreComparedInConstantTime(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// *fake packages are the partners' test servers, never mounted by Lens.
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "vendor" || strings.HasSuffix(d.Name(), "fake")) {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				b, ok := n.(*ast.BinaryExpr)
				if !ok || (b.Op != token.EQL && b.Op != token.NEQ) || trivial(b.X) || trivial(b.Y) {
					return true
				}
				if secretName.MatchString(name(b.X)) || secretName.MatchString(name(b.Y)) {
					rel, _ := filepath.Rel(root, p)
					t.Errorf("%s:%d compares %s %s %s: use subtle.ConstantTimeCompare", filepath.ToSlash(rel),
						fset.Position(b.Pos()).Line, name(b.X), b.Op, name(b.Y))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// trivial reports an operand that holds no secret: a literal, nil, a constant-looking name or a len() call.
func trivial(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return v.Name == "nil" || v.Name == "true" || v.Name == "false"
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "len" {
			return true
		}
	}
	return false
}

func name(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.ParenExpr:
		return name(v.X)
	}
	return ""
}
