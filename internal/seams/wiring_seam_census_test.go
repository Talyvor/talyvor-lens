package seams

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// wiring_seam_census_test.go — exported Set*/Register*/Enable*/Attach* methods on
// internal/* types that NO PRODUCTION CODE EVER CALLS.
//
// ⚠ WHY A SEAM IS DIFFERENT FROM DEAD CODE. A `Set*` method is a socket somebody
// left for main.go to plug into. An unplugged socket is a feature that exists,
// compiles, is tested, and does nothing — and unlike dead code it READS AS FINISHED.
// W6.17 (#485) is the evidence: BatchRouter.SetSettleHook has no production caller,
// and that single fact is why main.go passes a hardcoded `false` for settleWired and
// the whole /v1/batch lane cannot open in any configuration.
//
// 113 seams found; 103 have a production caller. The TEN that do not are recorded
// below — and the split inside those ten is the point: SIX are unwired and SAY SO in
// their own doc comments (test seams, reserved anchors, test-only threshold
// overrides), and FOUR read as finished. Recording all ten rather than filtering the
// honest ones is what makes the difference visible.
//
// ⚠ THE CENSUS LIED TWICE BEFORE IT WAS RIGHT, and both lies pointed the same
// (flattering-to-me) way — calling a LIVE seam dead:
//
//   - SAME-PACKAGE CALLERS. The first pass looked only for callers OUTSIDE the
//     declaring package and flagged localrouter.Router.SetNodePrice and
//     benchprobe.Store.SetProbeScore. Both are called from their own package's
//     production code (multi.go's price-sync loop; scheduler.go). A seam used
//     internally is wired.
//   - THE FILE NAME IS EVIDENCE. proxy.SetAnthropicURL / SetGoogleURL live in
//     bench_setters.go and carry no doc comment, so a doc-comment keyword scan
//     called them live-looking. The file says what they are.

// seamVerb is the one question here that is genuinely ABOUT A NAME — does this method read as
// wiring — so it stays a pattern. The two STRUCTURAL questions next to it (is this a declaration,
// is that a call) are answered by a parser; see
// TestSeamScannersReadDeclarationsAndCallsNotSpellings for what happened when they were not.
//
// ⚠ THE `[A-Z]` IS LOAD-BEARING: without it `Settle` is a `Set` seam. The census's own
// unwiredSeams map records SetSettleHook, so the two words meet in this package for real.
var seamVerb = regexp.MustCompile(`^(?:Set|Register|Enable|Attach|Wire)[A-Z]\w*$`)

// seamDeclsIn returns every wiring seam DECLARED in src, as {receiverType, method}.
//
// ⚠ EXPORTED METHODS ON A TYPE ONLY. A seam is a socket somebody left for another package to plug
// into; a plain function is not one, and neither is an unexported method.
func seamDeclsIn(filename string, src []byte) ([][2]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name == nil || !seamVerb.MatchString(fd.Name.Name) {
			continue
		}
		if r := seamReceiverName(fd); r != "" {
			out = append(out, [2]string{r, fd.Name.Name})
		}
	}
	return out, nil
}

// seamReceiverName is the bare type a method is declared on, or "" for a plain function.
//
// ⚠ `*T`, `T[P]` and `*T[K, V]` are all ordinary ways to spell a receiver, and the receiver may
// have NO NAME AT ALL — which is idiomatic Go for one the method does not use, and is invisible to
// any pattern that expects an identifier before the type. All three defeated the regex this
// replaced.
func seamReceiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return ""
	}
	expr := fd.Recv.List[0].Type
	for {
		switch t := expr.(type) {
		case *ast.StarExpr: // *T
			expr = t.X
		case *ast.IndexExpr: // T[P]
			expr = t.X
		case *ast.IndexListExpr: // T[K, V]
			expr = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// calledMethodsIn returns which of `want` src actually CALLS.
//
// ⚠ A CALL, NOT A MENTION, AND THAT IS THE WHOLE POINT ON THIS HALF. The text `.SetAutonomous(`
// appears in a doc comment that shows a reader how one WOULD wire the seam, and counting it made
// the census report an empty socket as plugged in — the one thing it exists to notice.
//
// ⚠ A METHOD VALUE IS NOT A CALL EITHER (`_ = s.SetAutonomous`), which the regex also got right,
// for the wrong reason: it needed the open paren. Here it falls out of asking for a CallExpr.
func calledMethodsIn(filename string, src []byte, want map[string]bool) (map[string]bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		return nil, err
	}
	got := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if se, ok := ce.Fun.(*ast.SelectorExpr); ok && want[se.Sel.Name] {
			got[se.Sel.Name] = true
		}
		return true
	})
	return got, nil
}

// unwiredSeams — declaring package → "Type.Method" → what its absence means.
// A seam here is called by TESTS ONLY.
var unwiredSeams = map[string]string{
	"internal/batch:BatchRouter.SetSettleHook": "⚠ RECORDED BY W6.17 (#485). No production caller, " +
		"and that is exactly why cmd/lens builds the lane gate as newBatchReg(cfg.BatchEnabled, " +
		"false) — a literal false for settleWired — so the /v1/batch routes are never registered " +
		"in ANY configuration. The socket being empty is load-bearing here, deliberately.",

	"internal/cache:SemanticCache.SetPooledWithVariants": "⚠ THE WRITER IS UNWIRED AND THE READER " +
		"IS NOT. SetPooled (no variants) IS called in production — internal/proxy/proxy.go and " +
		"internal/seedcache/seedcache.go — while SetPooledWithVariants, which writes the doc2query " +
		"match targets, is called only by tests. The read path is built for them: " +
		"semanticSelectPooledSQL selects COALESCE(variant_of, id). So doc2query variant rows are " +
		"created by nothing in production, and W2.7 records doc2query as DONE.",

	"internal/routingbrain:Store.SetAutonomous": "⚠ A DOCUMENTED PER-WORKSPACE OPT-IN WITH NO WAY " +
		"TO OPT IN. SetAutonomous is the only writer of routing_brain_autonomous, ModeFor reads " +
		"that table to return ModeAutonomous, and no route, handler or subcommand calls it — the " +
		"only callers are tests. Meanwhile cmd/lens LOGS \"advisory default, autonomous " +
		"per-workspace opt-in\" at boot and internal/config says the same. The only path to " +
		"autonomous routing is direct SQL. ⚠ THIS BOUNDS W6.20 (#488): the routing-brain cost " +
		"floor's consequences reach production only for a workspace in that table, and no product " +
		"surface can put one there.",

	"internal/economy:DualTokenStore.SetAgentCeiling": "⚠ A DOCUMENTED OPERATOR CAPABILITY WITH NO " +
		"SURFACE, ON A MONEY CAP. Its own doc says \"This is how an operator sets a per-agent cap " +
		"other than the default\" — and nothing calls it. No route, no handler, no subcommand. " +
		"Every scoped agent key therefore carries DefaultAgentCeilingLXC (50 LXC, $5) and no " +
		"operator can change it through the product.",

	"internal/economy:DualTokenStore.SetSmartAccounts": "OFF UNTIL CDP'S KEYS ARE ON THE SERVER, and " +
		"says so (B48.2). It takes the wallet provider that holds agents' keys — Coinbase's adapter, " +
		"which cmd/lens builds nowhere yet because real crypto stays off until Nick adds CDP's keys. " +
		"Until then POST …/agents/{id}/smart-account answers 503 and no agent has a smart account.",

	// ── the remaining six are unwired AND SAY SO IN THEIR OWN DOC COMMENTS. They are
	// recorded rather than filtered out, because "unwired and honest about it" is a
	// different fact from "unwired and reads as finished", and a census that silently
	// drops the honest ones cannot show the difference.
	"internal/localrouter:Router.SetRand": "TEST SEAM, and says so: \"injects the selection RNG " +
		"(test-only; production uses math/rand/v2 top-level)\".",
	"internal/poolroyalty:Minter.SetAnchor": "RESERVED, and says so: \"reserved for the future " +
		"held-benchmark caller … Unused on any live path this PR\". main.go's own comment agrees.",
	"internal/poolroyalty:DistillMinter.SetAnchor": "RESERVED, same note as Minter.SetAnchor.",
	"internal/poolroyalty:EvalContributionMinter.SetMinConsensus": "TEST-ONLY OVERRIDE, and says " +
		"so: \"Lower is only for tests; production keeps DefaultMinConsensusAttesters\".",
	"internal/poolroyalty:EvalContributionMinter.SetMinUnlinkedGraders": "TEST-ONLY OVERRIDE, and " +
		"says so: \"production keeps DefaultMinUnlinkedGraders\".",
	"internal/poolroyalty:EvalContributionMinter.SetRequireConsensus": "TEST-ONLY TOGGLE, and says " +
		"so: \"Production keeps it ON (the default)\".",
}

const seamFloor = 90 // 113 seams found 2026-08-28, counted by the parser below

// ⚠ THE THREE FIGURES IN THIS FILE DISAGREED AND ALL THREE WERE CLAIMS ABOUT THE TREE: the
// header said 112 seams / 102 wired and this line said 115, against a measured 113 / 103 / 10.
// Corrected to the counted values; the TEN unwired and their names were and are right.

func seamsRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod", root)
	}
	return root
}

type seam struct {
	pkgDir, typ, method string
}

func (s seam) key() string { return s.pkgDir + ":" + s.typ + "." + s.method }

// findSeams returns every exported wiring seam declared on an internal/* type.
func findSeams(t *testing.T, root string) []seam {
	t.Helper()
	var out []seam
	base := filepath.Join(root, "internal")
	err := filepath.Walk(base, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// ⚠ THE FILE NAME IS EVIDENCE. A *_setters.go / bench_* file is a test seam by
		// construction; treating it as production intent produced two false findings.
		if strings.Contains(filepath.Base(path), "bench_") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		decls, perr := seamDeclsIn(rel, raw)
		if perr != nil {
			t.Fatalf("parse %s: %v — a file this census cannot read is a file it cannot police", rel, perr)
		}
		for _, d := range decls {
			out = append(out, seam{filepath.Dir(rel), d[0], d[1]})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seam walk: %v", err)
	}
	return out
}

// productionCallers reports, for each seam, whether any NON-TEST file anywhere calls
// it — including its own package, which the first version of this census forgot.
func productionCallers(t *testing.T, root string, seams []seam) map[string]string {
	t.Helper()
	want := make(map[string]bool, len(seams))
	for _, s := range seams {
		want[s.method] = true
	}
	hit := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "bin", "rel":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		calls, perr := calledMethodsIn(rel, raw, want)
		if perr != nil {
			t.Fatalf("parse %s: %v — a caller sweep that silently skips a file calls live seams unwired", rel, perr)
		}
		for _, s := range seams {
			if _, done := hit[s.key()]; done {
				continue
			}
			// ⚠ NO "SKIP THE DECLARING FILE" RULE HERE, DELIBERATELY. A package may both
			// declare a seam and call it — localrouter does exactly that — and what is
			// counted is a CALL EXPRESSION, which a `func (r *T) Method(` declaration is not.
			if calls[s.method] {
				hit[s.key()] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("caller walk: %v", err)
	}
	return hit
}

func TestEveryWiringSeamHasAProductionCaller(t *testing.T) {
	root := seamsRepoRoot(t)
	seams := findSeams(t, root)
	if len(seams) < seamFloor {
		t.Fatalf("found %d wiring seams, want >= %d — the declaration parse has broken, and a "+
			"broken parse finds no unwired seams", len(seams), seamFloor)
	}
	callers := productionCallers(t, root, seams)
	if len(callers) < seamFloor-25 {
		t.Fatalf("only %d of %d seams have any caller — the caller sweep has broken, and a broken "+
			"sweep calls every live seam unwired", len(callers), len(seams))
	}

	var unwired []string
	for _, s := range seams {
		if _, ok := callers[s.key()]; !ok {
			unwired = append(unwired, s.key())
		}
	}
	sort.Strings(unwired)

	for _, u := range unwired {
		if _, ok := unwiredSeams[u]; !ok {
			t.Errorf("%s has no production caller — it is a socket nobody plugged in.\n"+
				"    A Set*/Register* method with no caller is a capability that exists, compiles, "+
				"is tested and does nothing, and it READS AS FINISHED. Say what its absence means "+
				"and record it, or wire it.", u)
		}
	}
	for want, why := range unwiredSeams {
		found := false
		for _, u := range unwired {
			if u == want {
				found = true
			}
		}
		if !found {
			t.Errorf("W6.25 records %s as having no production caller and it now has one. Good — "+
				"but the record is stale, and it says things about the system that are no longer "+
				"true:\n    %s", want, why)
		}
	}
	t.Logf("%d wiring seams, %d with a production caller, %d without: %v",
		len(seams), len(callers), len(unwired), unwired)
}

// ⚠ TEETH. The assertion above is "the unwired set is exactly this"; a caller sweep
// that matched everything would report an empty set and satisfy it.
func TestSeamCensusCanActuallyClassify(t *testing.T) {
	root := seamsRepoRoot(t)
	seams := findSeams(t, root)
	callers := productionCallers(t, root, seams)

	byName := map[string]bool{}
	for _, s := range seams {
		byName[s.method] = true
	}
	// A seam that is definitely wired must be seen as wired — SetHTTPClient is called
	// from cmd/lens for several miners (W6.11 measured those call sites).
	if !byName["SetHTTPClient"] {
		t.Error("SetHTTPClient is not seen as a declared seam — the declaration parse is blind")
	}
	wiredSeen := false
	for _, s := range seams {
		if s.method == "SetHTTPClient" {
			if _, ok := callers[s.key()]; ok {
				wiredSeen = true
			}
		}
	}
	if !wiredSeen {
		t.Error("no SetHTTPClient seam is seen as having a production caller — the caller sweep " +
			"is blind, and a blind sweep calls every seam unwired")
	}
	if _, ok := callers["internal/routingbrain:Store.SetAutonomous"]; ok {
		t.Error("SetAutonomous now has a production caller — either autonomous mode gained a " +
			"product surface (update the record) or the sweep counts a test as production")
	}
}

// ── the census reads DECLARATIONS AND CALLS, not spellings ───────────────────────────────────────

// ⚠ THIS CENSUS MATCHED TEXT ON BOTH HALVES, AND MEASURING IT FOUND ONE FAILURE IN EACH DIRECTION.
// Measured 2026-08-28 (tab-q6d3) against the real guard, by adding one production file per arm.
//
// ⚠⚠ SAID FIRST, BECAUSE IT DECIDES WHAT THIS CHANGE IS: THE CENSUS IS CORRECT ON TODAY'S TREE AND
// THIS FIXES NO LIVE MISCOUNT. A parser enumeration run beside the regex agrees exactly — 113 seams
// both ways, 0 seen by one and not the other; and over the 90 distinct seam method names the caller
// sweep agrees too, 0 phantom callers and 0 missed calls. Every Set*/Register*/Enable*/Attach*/Wire*
// method in internal/ today happens to name its receiver, and no comment or string happens to carry
// one of those call shapes. What was missing is that nothing would have noticed when that stopped
// being true.
//
//	DECLARATION HALF — an unwired seam the census cannot see. Each arm declares an exported Set*
//	method on an internal/ type with NO production caller, which is exactly what this guard exists
//	to report:
//	  func (p *T) SetX(...)  named receiver   -> REPORTED   (the positive control)
//	  func (*T) SetX(...)    unnamed pointer  -> NOT REPORTED
//	  func (T) SetX(...)     unnamed value    -> NOT REPORTED
//	  func (p *T[P]) SetX()  generic          -> NOT REPORTED
//	  the same line in a /* block comment */  -> REPORTED as a seam on a type that does not exist
//
//	CALLER HALF — and this one fails in the direction that HIDES what the guard is for. A doc
//	comment in a production file reading
//	    //	store.SetAutonomous(ctx, workspaceID, true)
//	flips internal/routingbrain:Store.SetAutonomous from unwired to WIRED. The socket is still
//	empty; the census now says it is plugged in. Documenting an unwired seam is exactly what one
//	does on finding one, so the failure mode is reachable by the ordinary act of writing it down.
//	⚠ AND THE ERROR MESSAGE MISDIAGNOSES IT — it offers "the sweep counts a test as production",
//	which is the wrong culprit and would send the next reader to the wrong place.
//
// So the structural questions — is this a DECLARATION, is that a CALL — are answered by a parser.
// The question that is genuinely about a NAME (does the method start with Set/Register/Enable/
// Attach/Wire) stays a pattern, because that is what it is.
func TestSeamScannersReadDeclarationsAndCallsNotSpellings(t *testing.T) {
	decls := []struct {
		name string
		src  string
		want [][2]string
	}{
		{"named pointer receiver", "package p\nfunc (r *Alpha) SetThing(v int) {}\n", [][2]string{{"Alpha", "SetThing"}}},
		{"named value receiver", "package p\nfunc (r Bravo) RegisterThing(v int) {}\n", [][2]string{{"Bravo", "RegisterThing"}}},
		{"UNNAMED pointer receiver", "package p\nfunc (*Charlie) SetThing(v int) {}\n", [][2]string{{"Charlie", "SetThing"}}},
		{"UNNAMED value receiver", "package p\nfunc (Delta) EnableThing(v int) {}\n", [][2]string{{"Delta", "EnableThing"}}},
		{"generic receiver, one parameter", "package p\nfunc (r *Echo[T]) AttachThing(v int) {}\n", [][2]string{{"Echo", "AttachThing"}}},
		{"generic receiver, two parameters", "package p\nfunc (r *Fox[K, V]) WireThing(v int) {}\n", [][2]string{{"Fox", "WireThing"}}},

		// ⚠ THE INVERTED ROWS. A census that reds on correct code gets relaxed until it reds on
		// nothing, so these carry as much weight as the blind ones.
		{"a block comment is documentation, not a seam",
			"package p\n\n/*\nfunc (g *Ghost) SetThing(v int) {}\n*/\n", nil},
		{"a raw string is data, not a seam",
			"package p\n\nconst ex = `\nfunc (g *Ghost) SetThing(v int) {}\n`\n", nil},
		{"a plain function is not a seam — there is no socket without a type",
			"package p\nfunc SetThing(v int) {}\n", nil},
		{"a method whose name is not a wiring verb is not a seam",
			"package p\nfunc (r *Hotel) UpdateThing(v int) {}\n", nil},
		{"Settle is not Set — the verb must be followed by an upper-case word",
			"package p\nfunc (r *India) Settle(v int) {}\n", nil},
		{"an unexported wiring method is not a seam anybody outside can plug into",
			"package p\nfunc (r *Juliet) setThing(v int) {}\n", nil},
	}
	for _, tc := range decls {
		got, err := seamDeclsIn("probe.go", []byte(tc.src))
		if err != nil {
			t.Errorf("decl %s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("decl %s:\n  want %v\n  got  %v", tc.name, tc.want, got)
		}
	}

	want := map[string]bool{"SetAutonomous": true, "SetHTTPClient": true}
	calls := []struct {
		name string
		src  string
		want map[string]bool
	}{
		{"a real call is a call",
			"package p\nfunc f(s S) { s.SetAutonomous(1) }\n", map[string]bool{"SetAutonomous": true}},
		{"a chained call is a call",
			"package p\nfunc f(s S) { s.Inner().SetHTTPClient(nil) }\n", map[string]bool{"SetHTTPClient": true}},

		// ⚠ THE ROW THAT MATTERS MOST IN THIS FILE. Counting this as a caller reports an EMPTY
		// socket as plugged in, which is the one thing this census exists to notice.
		{"a doc comment showing how one WOULD call is not a call",
			"package p\n\n// This is how a workspace would opt in:\n" +
				"//\n//\tstore.SetAutonomous(ctx, ws, true)\n//\nfunc f() {}\n", map[string]bool{}},
		{"a string containing the call shape is not a call",
			"package p\n\nconst usage = \"call s.SetAutonomous(ctx, ws, true)\"\n", map[string]bool{}},
		{"a method VALUE that is never called is not a call",
			"package p\nfunc f(s S) { _ = s.SetAutonomous }\n", map[string]bool{}},
	}
	for _, tc := range calls {
		got, err := calledMethodsIn("probe.go", []byte(tc.src), want)
		if err != nil {
			t.Errorf("call %s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("call %s:\n  want %v\n  got  %v", tc.name, tc.want, got)
		}
	}
}

// ⚠ AND BOTH SCANNERS MUST FAIL LOUDLY ON SOURCE THEY CANNOT READ. A scanner that returns nothing
// finds no unwired seams; one that returns nothing on the CALLER side calls every seam unwired.
// Same floor as the census's own seamFloor check, one level down.
func TestSeamScannersRefuseSourceTheyCannotParse(t *testing.T) {
	bad := []byte("package p\nfunc (r *A) SetThing( {{{\n")
	if _, err := seamDeclsIn("broken.go", bad); err == nil {
		t.Error("the declaration scanner read unparseable source and reported no seams")
	}
	if _, err := calledMethodsIn("broken.go", bad, map[string]bool{"SetThing": true}); err == nil {
		t.Error("the caller scanner read unparseable source and reported no calls — a silent zero " +
			"there calls every live seam unwired")
	}
}
