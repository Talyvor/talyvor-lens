package economy

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B50.54 — no move posts through a partner with the store's screener inside its transaction: the screener takes a
// connection of its own, and the transaction already holds one. The moves left below screen inside their transaction
// still and are B50.56's; each is taken off the list as it screens first.
func TestB5054_NoMoveScreensWithTheStoresScreenerInsideItsTransaction(t *testing.T) {
	left := []string{"AuthorizeCardCharge", "applyEarningsResult", "applyPartnerResult", "authorizeReusableCharge", "creditCardPayment", "leaveSuspense"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var inside []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "postMoneyTx" || len(call.Args) != 4 {
					return true
				}
				if sel, ok := call.Args[3].(*ast.SelectorExpr); ok && sel.Sel.Name == "screener" && !slices.Contains(inside, fn.Name.Name) {
					inside = append(inside, fn.Name.Name)
				}
				return true
			})
		}
	}
	slices.Sort(inside)
	if !slices.Equal(inside, left) {
		t.Fatalf("postMoneyTx is given the store's screener inside the transactions of %v; want only %v", inside, left)
	}
}

// The screener inside a transaction clears exactly what was screened before it began, and asks nothing of the store's.
func TestB5054_ScreenedFirstClearsOnlyWhatWasScreened(t *testing.T) {
	ctx := context.Background()
	asked := 0
	s := &DualTokenStore{screener: screenerFunc(func(context.Context, screening.Payment) error { asked++; return nil })}
	e := MoneyEntry{WorkspaceID: "ws", Capability: CapabilityFX, IdempotencyKey: "fx_convert:q", Funding: FundingTest, Counterparty: " Test FX "}
	first, err := s.screenFirst(ctx, e, MoneyPosting{AmountMinor: 9_90, Currency: "gbp"}, MoneyPosting{AmountMinor: -12_50, Currency: "USD"})
	if err != nil || asked != 2 {
		t.Fatalf("screenFirst: %v, after %d screenings; want both legs screened", err, asked)
	}
	partner := func(string) bool { return true }
	in := []MoneyPosting{{AmountMinor: 9_90, Currency: "GBP"}, {AmountMinor: -12_50, Currency: "USD"}}
	e.Counterparty = "Test FX"
	if err := screenOutside(ctx, e, in, partner, first); err != nil {
		t.Fatalf("the same legs inside the transaction: %v; want cleared", err)
	}
	more := []MoneyPosting{{AmountMinor: 9_91, Currency: "GBP"}, {AmountMinor: -12_50, Currency: "USD"}}
	if err := screenOutside(ctx, e, more, partner, first); !errors.Is(err, partners.ErrScreeningUnavailable) {
		t.Fatalf("a leg a penny more inside the transaction: %v; want refused as not screened", err)
	}
	if err := screenOutside(ctx, e, in, partner, noPartnerPostings); !errors.Is(err, partners.ErrScreeningUnavailable) {
		t.Fatalf("a partner leg in a move screened as having none: %v; want refused", err)
	}
	if asked != 2 {
		t.Fatalf("the store's screener was asked %d times; want only the 2 before the transaction", asked)
	}
}

type screenerFunc func(context.Context, screening.Payment) error

func (f screenerFunc) ScreenPayment(ctx context.Context, p screening.Payment) error { return f(ctx, p) }
