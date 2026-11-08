package main

import (
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/plans"
)

// B56.8 — GET /v1/public/plan-gates says which switches Lens enforces, and GET /v1/public/fees which fees it
// charges, beside the values.
func TestB568_PublicRoutesSayWhatIsBuiltAndCharged(t *testing.T) {
	for _, c := range []struct {
		path, key string
		handler   http.HandlerFunc
		want      map[string]bool
	}{
		{"/v1/public/plan-gates", "built", publicPlanGatesHandler, plans.Built},
		{"/v1/public/fees", "charged", publicFeesHandler, fees.Charged},
	} {
		w := httptest.NewRecorder()
		c.handler(w, httptest.NewRequest(http.MethodGet, c.path, nil))
		var got map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil {
			t.Fatalf("GET %s = %d %s (%v)", c.path, w.Code, w.Body, err)
		}
		var flags map[string]bool
		if err := json.Unmarshal(got[c.key], &flags); err != nil || len(flags) == 0 || !reflect.DeepEqual(flags, c.want) {
			t.Errorf("GET %s %s = %s, want %v", c.path, c.key, got[c.key], c.want)
		}
	}
	if !plans.Built["audit_export"] || plans.Built["sso"] || fees.Charged["compute_take_bps"] || !fees.Charged["market_take_bps"] {
		t.Errorf("plans.Built = %v, fees.Charged = %v: audit_export is enforced (B56.5), sso is not; the market take is charged, compute's is not", plans.Built, fees.Charged)
	}
}

// B56.8 — plans.Built and fees.Charged are held to the code: each has an entry for every field of plans.Gates (or
// fees.Settings), true exactly when non-test code outside internal/plans (or internal/fees) reads that field, found
// by type-checking every package in the module. So a flag can run neither ahead of the code nor behind it.
func TestB568_BuiltAndChargedFollowTheCode(t *testing.T) {
	reads := fieldReads(t, map[string]string{
		"github.com/talyvor/lens/internal/plans": "Gates",
		"github.com/talyvor/lens/internal/fees":  "Settings",
	})
	for _, c := range []struct {
		name  string
		typ   reflect.Type
		flags map[string]bool
		read  map[string]bool
	}{
		{"plans.Built", reflect.TypeFor[plans.Gates](), plans.Built, reads["github.com/talyvor/lens/internal/plans"]},
		{"fees.Charged", reflect.TypeFor[fees.Settings](), fees.Charged, reads["github.com/talyvor/lens/internal/fees"]},
	} {
		if len(c.flags) != c.typ.NumField() {
			t.Errorf("%s has %d entries; %s has %d fields", c.name, len(c.flags), c.typ, c.typ.NumField())
		}
		for i := range c.typ.NumField() {
			f := c.typ.Field(i)
			key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			on, ok := c.flags[key]
			switch {
			case !ok:
				t.Errorf("%s has no entry for %q (%s.%s)", c.name, key, c.typ, f.Name)
			case on && !c.read[f.Name]:
				t.Errorf("%s[%q] is true, but no code outside its package reads %s.%s", c.name, key, c.typ, f.Name)
			case !on && c.read[f.Name]:
				t.Errorf("%s[%q] is false, but code outside its package reads %s.%s: flip it", c.name, key, c.typ, f.Name)
			}
		}
	}
}

// fieldReads type-checks the non-test files of every package in the module, against the export data go list
// builds, and answers, by the package path of each owner in owners (path → struct type name), the names of that
// struct's fields a package other than the owner reads: in a selector of its own, or in the body of an owner's
// method it calls (how internal/plans enforces a gate: OwnKeysAllowed, CheckSeats, the Refuse methods). The owner's
// plain functions do not count: Current reads a fee only to copy it. A selector assigned to is a write.
func fieldReads(t *testing.T, owners map[string]string) map[string]map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,CgoFiles,Export,DepOnly,Module,Error", "github.com/talyvor/lens/...")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	type pkg struct {
		ImportPath, Dir, Export string
		GoFiles, CgoFiles       []string
		DepOnly                 bool
		Module                  *struct{ Path string }
		Error                   *struct{ Err string }
	}
	exports := map[string]string{}
	var module []pkg
	for dec := json.NewDecoder(strings.NewReader(string(out))); dec.More(); {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("go list: %v", err)
		}
		if p.Error != nil {
			t.Fatalf("go list: %s: %s", p.ImportPath, p.Error.Err)
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly && p.Module != nil && p.Module.Path == "github.com/talyvor/lens" {
			if len(p.CgoFiles) > 0 {
				t.Fatalf("%s has cgo files %v, which this test does not type-check", p.ImportPath, p.CgoFiles)
			}
			module = append(module, p)
		}
	}
	// An owner is type-checked here from source, and imported by the others from export data: the two hold
	// different objects, so fields and functions are matched by owner path and name.
	field := func(v types.Object) (string, bool) {
		if v, ok := v.(*types.Var); ok && v.IsField() && v.Pkg() != nil {
			if name, ok := owners[v.Pkg().Path()]; ok {
				st := v.Pkg().Scope().Lookup(name).Type().Underlying().(*types.Struct)
				for i := range st.NumFields() {
					if st.Field(i) == v {
						return v.Pkg().Path(), true
					}
				}
			}
		}
		return "", false
	}
	reads := map[string]map[string]bool{}
	inMethods := map[string]map[string]bool{} // an owner's method, by FullName → the fields its body reads
	called := map[string]bool{}               // owners' methods other packages call, by FullName
	for path := range owners {
		reads[path] = map[string]bool{}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) })
	for _, p := range module {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
		if _, err := (&types.Config{Importer: imp}).Check(p.ImportPath, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		_, isOwner := owners[p.ImportPath]
		for _, f := range files {
			written := map[ast.Expr]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				if a, ok := n.(*ast.AssignStmt); ok {
					for _, l := range a.Lhs {
						written[l] = true
					}
				}
				return true
			})
			readIn := func(root ast.Node, into map[string]bool) {
				ast.Inspect(root, func(n ast.Node) bool {
					if s, ok := n.(*ast.SelectorExpr); ok && !written[s] && info.Selections[s] != nil {
						if owner, ok := field(info.Selections[s].Obj()); ok && (isOwner || owner != p.ImportPath) {
							into[owner+"."+s.Sel.Name] = true
						}
					}
					return true
				})
			}
			if isOwner {
				for _, d := range f.Decls {
					if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Body != nil {
						body := map[string]bool{}
						readIn(fd.Body, body)
						inMethods[info.Defs[fd.Name].(*types.Func).FullName()] = body
					}
				}
				continue
			}
			direct := map[string]bool{}
			readIn(f, direct)
			for k := range direct {
				owner, name := k[:strings.LastIndex(k, ".")], k[strings.LastIndex(k, ".")+1:]
				reads[owner][name] = true
			}
			for id, obj := range info.Uses {
				if fn, ok := obj.(*types.Func); ok && fn.Pkg() != nil && fileHas(fset, f, id) {
					if _, ok := owners[fn.Pkg().Path()]; ok {
						called[fn.Origin().FullName()] = true
					}
				}
			}
		}
	}
	for fn := range called {
		for k := range inMethods[fn] {
			owner, name := k[:strings.LastIndex(k, ".")], k[strings.LastIndex(k, ".")+1:]
			reads[owner][name] = true
		}
	}
	return reads
}

func fileHas(fset *token.FileSet, f *ast.File, n ast.Node) bool {
	return fset.File(f.Pos()) == fset.File(n.Pos())
}
