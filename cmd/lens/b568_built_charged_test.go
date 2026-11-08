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
// struct's fields some package other than the owner selects.
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
		if p.Error != nil || len(p.CgoFiles) > 0 {
			t.Fatalf("go list: %s cannot be type-checked here: %+v, cgo files %v", p.ImportPath, p.Error, p.CgoFiles)
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly && p.Module != nil && p.Module.Path == "github.com/talyvor/lens" {
			module = append(module, p)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) })
	fields := map[*types.Var]string{} // each owner's field → its owner's path
	for path, name := range owners {
		p, err := imp.Import(path)
		if err != nil {
			t.Fatalf("import %s: %v", path, err)
		}
		st := p.Scope().Lookup(name).Type().Underlying().(*types.Struct)
		for i := range st.NumFields() {
			fields[st.Field(i)] = path
		}
	}
	reads := map[string]map[string]bool{}
	for path := range owners {
		reads[path] = map[string]bool{}
	}
	for _, p := range module {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
		if _, err := (&types.Config{Importer: imp}).Check(p.ImportPath, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, sel := range info.Selections {
			v, _ := sel.Obj().(*types.Var)
			if owner, ok := fields[v]; ok && sel.Kind() == types.FieldVal && owner != p.ImportPath {
				reads[owner][v.Name()] = true
			}
		}
	}
	return reads
}
