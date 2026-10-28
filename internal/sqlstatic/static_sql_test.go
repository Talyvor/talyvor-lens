// Package sqlstatic holds B50.14's static SQL test: every Exec, Query and QueryRow in Lens
// takes SQL text that is a compile-time constant, or a reviewed builder listed in reviewed below.
package sqlstatic

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var sqlMethods = map[string]bool{
	"Exec": true, "Query": true, "QueryRow": true,
	"ExecContext": true, "QueryContext": true, "QueryRowContext": true,
}

type finding struct{ pos, key string }

func moduleRoot(t *testing.T) string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// scan parses every non-test Go file under cmd/ and internal/ and returns each SQL call
// whose text is not a constant, keyed by file, enclosing function and the SQL expression.
func scan(t *testing.T, root string) []finding {
	fset := token.NewFileSet()
	files := map[string][]*ast.File{} // module-relative dir → files
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			files[filepath.ToSlash(rel)] = append(files[filepath.ToSlash(rel)], f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	consts := map[string]map[string]bool{} // dir → const names declared anywhere in it
	for dir, fs := range files {
		consts[dir] = map[string]bool{}
		for _, f := range fs {
			ast.Inspect(f, func(n ast.Node) bool {
				if g, ok := n.(*ast.GenDecl); ok && g.Tok == token.CONST {
					for _, s := range g.Specs {
						for _, name := range s.(*ast.ValueSpec).Names {
							consts[dir][name.Name] = true
						}
					}
				}
				return true
			})
		}
	}
	const modPath = "github.com/talyvor/lens/"
	var out []finding
	for dir, fs := range files {
		for _, f := range fs {
			imports := map[string]string{} // local name → module-relative dir
			for _, im := range f.Imports {
				path, _ := strconv.Unquote(im.Path.Value)
				if !strings.HasPrefix(path, modPath) {
					continue
				}
				name := filepath.Base(path)
				if im.Name != nil {
					name = im.Name.Name
				}
				imports[name] = strings.TrimPrefix(path, modPath)
			}
			var isConst func(e ast.Expr) bool
			isConst = func(e ast.Expr) bool {
				switch e := e.(type) {
				case *ast.BasicLit:
					return e.Kind == token.STRING
				case *ast.Ident:
					return consts[dir][e.Name]
				case *ast.SelectorExpr:
					x, ok := e.X.(*ast.Ident)
					return ok && imports[x.Name] != "" && consts[imports[x.Name]][e.Sel.Name]
				case *ast.ParenExpr:
					return isConst(e.X)
				case *ast.BinaryExpr:
					return e.Op == token.ADD && isConst(e.X) && isConst(e.Y)
				}
				return false
			}
			rel, _ := filepath.Rel(root, fset.File(f.Pos()).Name())
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				fname := fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) > 0 {
					fname = exprString(fset, fn.Recv.List[0].Type) + "." + fname
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !sqlMethods[sel.Sel.Name] || len(call.Args) == 0 {
						return true
					}
					arg := call.Args[0]
					if isContext(fset, arg) {
						if len(call.Args) < 2 {
							return true
						}
						arg = call.Args[1]
					}
					if isConst(arg) {
						return true
					}
					out = append(out, finding{
						pos: fset.Position(call.Pos()).String(),
						key: filepath.ToSlash(rel) + " " + strings.TrimLeft(fname, "*") + " " + exprString(fset, arg),
					})
					return true
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func isContext(fset *token.FileSet, e ast.Expr) bool {
	s := strings.ToLower(exprString(fset, e))
	return strings.Contains(s, "ctx") || strings.Contains(s, "context")
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, e)
	return strings.Join(strings.Fields(b.String()), " ")
}

func TestNoSQLBuiltFromRequestData(t *testing.T) {
	found := scan(t, moduleRoot(t))
	seen := map[string]bool{}
	for _, f := range found {
		seen[f.key] = true
		if _, ok := reviewed[f.key]; !ok {
			t.Errorf("%s: SQL text is not a constant (%s). Pass request data as $n arguments; "+
				"if the text is built only from constants, add the key to reviewed with why", f.pos, f.key)
		}
	}
	for k := range reviewed {
		if !seen[k] {
			t.Errorf("reviewed entry %q matches no call any more: delete it", k)
		}
	}
}
