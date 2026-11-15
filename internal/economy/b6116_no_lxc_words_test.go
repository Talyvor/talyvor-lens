package economy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// B61.16 — no sentence a person reads says LXC. An amount Lens writes for a person is the dollars it is worth at
// the peg, exactly, and nothing else; the code "LXC" stays only where a machine reads it.

func TestB6116_MoneyWordsNameDollarsAlone(t *testing.T) {
	for ulxc, want := range map[int64]string{
		3_400_000_000: "$340.00",
		1:             "$0.0000001", // the agent-limit-boost refusal of 11 Nov read "$0.00 (0.000001 LXC)"
		12_345_678:    "$1.2345678",
		-40_000_000:   "-$4.00",
		0:             "$0.00",
	} {
		for _, got := range []string{LXCMoney(ulxc), Money(ulxc, "LXC")} {
			if got != want {
				t.Errorf("µLXC %d is written %q, want %q", ulxc, got, want)
			}
		}
	}
}

// machineLXC is a literal that names LXC for a machine, not a person: the currency code itself, SQL, and a wrapped
// database error that answers 500.
var machineLXC = regexp.MustCompile(`^(LXC|economy: [a-z ]*LXC: %w)$|\b(SELECT|INSERT|UPDATE|DELETE)\b`)

// parkedLXC are the literals of LENS→LXC conversion, parked with LXC itself; the Royalties screen that shows them is
// hidden (B61.17). Each is named here so a new one cannot hide among them.
var parkedLXC = map[string]bool{
	"dualtoken.go:economy: conversion below minimum of %d µLXC": true,
	"dualtoken.go:converted to LXC":                             true,
}

func TestB6116_NoSentenceTemplateSaysLXC(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	swept := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		swept++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(s, "LXC") || machineLXC.MatchString(s) || parkedLXC[path+":"+s] {
				return true
			}
			t.Errorf("%s: a sentence says LXC: %q", fset.Position(lit.Pos()), s)
			return true
		})
	}
	if swept < 100 {
		t.Fatalf("swept %d files of internal/economy; the sweep is not reading the package", swept)
	}
}
