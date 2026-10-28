// Package secretstatic holds B50.3's static tests on keys, signatures and secrets.
package secretstatic

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mathRandAllowed are the only non-test files that may import math/rand: each draws a
// timing or load-spreading number, never an id, key, nonce, token or challenge.
var mathRandAllowed = map[string]string{
	"internal/localrouter/multi.go":       "spreads load across local nodes",
	"internal/economy/agent_subbudget.go": "jitters a lock retry wait",
	"internal/retry/retry.go":             "backoff jitter",
	"internal/retry/policy.go":            "backoff jitter",
}

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

// TestNoMathRandOutsideTests fails when a non-test Go file imports math/rand or
// math/rand/v2 without being listed above: every random id, key, nonce and challenge
// in Lens comes from crypto/rand.
func TestNoMathRandOutsideTests(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == "vendor" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) && p != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path != "math/rand" && path != "math/rand/v2" {
				continue
			}
			seen[rel] = true
			if _, ok := mathRandAllowed[rel]; !ok {
				t.Errorf("%s imports %s: draw ids, keys, nonces and challenges from crypto/rand (or, for jitter only, list the file in mathRandAllowed with why)", rel, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel := range mathRandAllowed {
		if !seen[rel] {
			t.Errorf("mathRandAllowed lists %s, which no longer imports math/rand: remove it", rel)
		}
	}
}
