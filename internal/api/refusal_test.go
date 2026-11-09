package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// refusal_test.go — B55.51: no refusal a person reads starts with a package name or carries a dash.

func TestRefusalDropsThePackageName(t *testing.T) {
	for in, want := range map[string]string{
		"economy: no such agent in this workspace":         "No such agent in this workspace",
		"economy: spending: no such entry":                 "No such entry",
		"economy: payment: the agent is paused":            "The agent is paused",
		"rooms: conflict: the room is closed":              "The room is closed",
		"market: invalid listing: a listing needs a title": "A listing needs a title",
		"sellertax: invalid tax details: a country is due": "A country is due",
		"forbidden: no workspace identity":                 "No workspace identity",
		"invalid JSON body":                                "Invalid JSON body",
		"Three keys already wait to join this workspace.":  "Three keys already wait to join this workspace.",
		"economy: ":                "economy: ",
		"workspace_id is required": "workspace_id is required",
		"spending cap reached: this workspace has used $10": "Spending cap reached: this workspace has used $10",
	} {
		if got := Refusal(in); got != want {
			t.Errorf("Refusal(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNoRefusalCarriesADash walks every errors.New and fmt.Errorf text under internal/ and cmd/lens,
// every literal an Error method returns, and every literal cmd/lens hands writeJSONErr, and fails on
// an em or en dash in any of them.
func TestNoRefusalCarriesADash(t *testing.T) {
	var dashed []string
	for _, root := range []string{"..", "../../cmd/lens"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if fn, ok := n.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == "Error" && fn.Body != nil {
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						if s := literalText(n); strings.ContainsAny(s, "—–") {
							dashed = append(dashed, fset.Position(n.Pos()).String()+": "+s)
							return false
						}
						return true
					})
					return false
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				arg := -1
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					if x, ok := fn.X.(*ast.Ident); ok && ((x.Name == "errors" && fn.Sel.Name == "New") || (x.Name == "fmt" && fn.Sel.Name == "Errorf")) {
						arg = 0
					}
				case *ast.Ident:
					if fn.Name == "writeJSONErr" {
						arg = 2
					}
				}
				if arg < 0 || len(call.Args) <= arg {
					return true
				}
				if s := literalText(call.Args[arg]); strings.ContainsAny(s, "—–") {
					dashed = append(dashed, fset.Position(call.Pos()).String()+": "+s)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(dashed) > 0 {
		t.Errorf("%d refusals carry an em or en dash; say each as a plain sentence:\n%s", len(dashed), strings.Join(dashed, "\n"))
	}
}

// literalText joins the string literals of an expression, so "a — " + b + "c" reads as "a — c".
func literalText(e ast.Node) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, _ := strconv.Unquote(v.Value)
			return s
		}
	case *ast.BinaryExpr:
		return literalText(v.X) + literalText(v.Y)
	case *ast.ParenExpr:
		return literalText(v.X)
	}
	return ""
}
