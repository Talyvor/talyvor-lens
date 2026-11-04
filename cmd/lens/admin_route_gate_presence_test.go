package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// admin_route_gate_presence_test.go — EVERY /v1/admin REGISTRATION MUST REACH AN
// AUTHORIZATION DECISION, and the existing classification guard cannot tell that
// it does.
//
// ⚠ THE HOLE THIS CLOSES, STATED PRECISELY.
// admin_route_classification_test.go's TestClassificationMatchesTheGateAtEach-
// RegistrationSite asserts two things: a route in operatorReadable IS wrapped in
// requireAdminOrOperatorRead, and a route in operatorMustNotReach is NOT. A route
// in operatorMustNotReach registered with NO GATE AT ALL satisfies the second
// condition and passes. So the guard whose entire subject is "which routes does
// the operator credential reach" is blind to the one state that matters most:
// a money route reachable by any authenticated tenant's API key, because every
// one of these sits on the `authed` group.
//
// It is LATENT, not live: measured at main, all 25 /v1/admin paths ARE gated, by
// FOUR different mechanisms — the wrapper (requireAdmin / requireAdminOrOperator-
// Read), a handler CONSTRUCTOR that takes authManager and checks inside
// (newAdjudicateHandler, newTrafficRevokeHandler — six routes, every payout
// revocation among them), an inline closure, and a handler VALUE whose struct
// carries an IsAdmin closure (distillPreview). That variety is exactly why a
// window-scan for one wrapper name cannot answer the question.
//
// ⚠ THE RULE HERE IS UNIFORM AND NOT A NAME SHAPE. Every one of those four
// mechanisms ends in the same place: a read of `.IsAdmin` on an authenticated
// context (requireAdmin and requireAdminOrOperatorRead both do it directly —
// authz_admin_handlers.go). So the assertion is "the authorization decision is
// REACHABLE from the registration site", following named references one level
// into package main. A new gate invented tomorrow passes automatically as long as
// it actually decides; a route with no gate cannot pass by being named well.
//
// ⚠ THIS GUARD PASSES ON ITS FIRST RUN, WHICH IS WHY THE CONTROL IS THE
// DELIVERABLE. scripts/w612-admin-gate-presence-controls-v9c2.py removes the gate
// from /v1/admin/lxc/grant (MINTS LXC) and from /v1/admin/pool-royalty/adjudicate
// (REVOKES A PAYOUT) and records that the EXISTING classification tests stay GREEN
// while this one reds.

var (
	adminLiteralRe = regexp.MustCompile(`"(/v1/admin[^"]*)"`)
	// A registration line calls a chi verb, directly or through one of the
	// flag-gated registrars (econ/bill/batchGate).
	registerVerbRe = regexp.MustCompile(`\.(Get|Post|Put|Patch|Delete|Head|Options|Handle|HandleFunc|Method|get|post|put|patch|del|handle)\(`)
	// Identifiers that could name a gate or a handler we must follow into.
	identRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\b`)
	// varDefRe is varDef's pattern after the line start; it begins with the literal name so the
	// engine jumps to candidates instead of walking every line under -race (B54.2).
	varDefRe = func(name string) *regexp.Regexp {
		return regexp.MustCompile(regexp.QuoteMeta(name) + `\s*:?=\s`)
	}
)

// funcDef is the index of the line `func name(` starts in src, or -1.
func funcDef(name, src string) int {
	if i := strings.Index(src, "\nfunc "+name+"("); i >= 0 {
		return i + 1
	}
	return -1
}

// varDef is the index of the line where `name =` or `name :=` is the first thing on it, or -1.
func varDef(re *regexp.Regexp, src string) int {
	for _, loc := range re.FindAllStringIndex(src, -1) {
		start := strings.LastIndexByte(src[:loc[0]], '\n') + 1
		if strings.Trim(src[start:loc[0]], " \t\f\r") == "" {
			return start
		}
	}
	return -1
}

// adminAuthDecisionToken is the READ every gate in this package ends at, and the
// leading dot is load-bearing.
//
// ⚠ MEASURED, NOT CHOSEN: the token was `IsAdmin` on the first draft and control
// E3 caught it. distillpreview.Handler carries its gate in a struct FIELD called
// IsAdmin, so hollowing the closure out — `actx, err := …; if err != nil ||
// !actx.IsAdmin` becoming `_, err := …; if err != nil` — left the field NAME in
// the block and the guard reported the route as gated. The bare token was
// satisfied by a declaration rather than by a decision: a guard keyed on a name
// shape, which is the exact kind this queue keeps catching. `.IsAdmin` matches
// the selector (`actx.IsAdmin`, `!actx.IsAdmin`) and not the field declaration
// (`IsAdmin:`), so it can only be satisfied by reading the value.
const adminAuthDecisionToken = ".IsAdmin"

// packageMainSources returns every non-test .go file in this package, by name.
func packageMainSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Clean(n))
		if rerr != nil {
			t.Fatalf("read %s: %v", n, rerr)
		}
		out[n] = string(raw)
	}
	if len(out) < 10 {
		t.Fatalf("only %d non-test .go files found in package main — the sweep is broken, and a "+
			"broken sweep finds no ungated route", len(out))
	}
	return out
}

// blockFrom returns the source from idx until brace depth returns to zero,
// capped so a runaway never swallows the file.
func blockFrom(src string, idx int) string {
	depth, started := 0, false
	for i := idx; i < len(src) && i < idx+20000; i++ {
		switch src[i] {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth <= 0 {
				return src[idx : i+1]
			}
		}
	}
	end := idx + 20000
	if end > len(src) {
		end = len(src)
	}
	return src[idx:end]
}

// statementAt returns the whole registration call starting on line ln (1-based),
// by balancing parentheses forward. A multi-line inline closure comes back whole.
func statementAt(lines []string, ln int) string {
	var b strings.Builder
	depth := 0
	for i := ln - 1; i < len(lines) && i < ln+400; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
		for _, c := range lines[i] {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		if i >= ln-1 && depth <= 0 {
			break
		}
	}
	return b.String()
}

// gateReachable reports whether an authorization decision is reachable from the
// registration statement, following named references ONE level into package main
// (a gate wrapper, a handler constructor, or a handler value). via names what it
// followed, for the failure message and for the census log. memo keeps each name's
// answer for one sweep — the sources do not change within it, and scanning every
// file for every name at every site took this test 74s under -race (B30.6).
func gateReachable(stmt string, srcs map[string]string, memo map[string]string) (via string, ok bool) {
	if strings.Contains(stmt, adminAuthDecisionToken) {
		return "inline", true
	}
	seen := map[string]bool{}
	for _, m := range identRe.FindAllStringSubmatch(stmt, -1) {
		name := m[1]
		if seen[name] || len(name) < 4 {
			continue
		}
		seen[name] = true
		found, done := memo[name]
		if !done {
			found = nameReachesGate(name, srcs)
			memo[name] = found
		}
		if found != "" {
			return found, true
		}
	}
	return "", false
}

// nameReachesGate is where name is defined with an authorization decision in its
// block — "func name (file)" or "value name (file)" — or "" when nowhere.
func nameReachesGate(name string, srcs map[string]string) string {
	v := varDefRe(name)
	for file, src := range srcs {
		if at := funcDef(name, src); at >= 0 {
			if strings.Contains(blockFrom(src, at), adminAuthDecisionToken) {
				return "func " + name + " (" + file + ")"
			}
		}
		if at := varDef(v, src); at >= 0 {
			if strings.Contains(blockFrom(src, at), adminAuthDecisionToken) {
				return "value " + name + " (" + file + ")"
			}
		}
	}
	return ""
}

type adminSite struct {
	file, path, via string
	line            int
	gated           bool
}

func adminRegistrationSites(t *testing.T) []adminSite {
	t.Helper()
	srcs := packageMainSources(t)
	memo := map[string]string{}
	var sites []adminSite
	for file, src := range srcs {
		lines := strings.Split(src, "\n")
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "//") {
				continue
			}
			ms := adminLiteralRe.FindAllStringSubmatch(l, -1)
			if len(ms) == 0 {
				continue
			}
			if !registerVerbRe.MatchString(l) {
				// A /v1/admin literal that is not a registration — a log line, a
				// doc string. Recorded as ungated so it surfaces rather than being
				// silently skipped; skipping is how a real registration in an
				// unexpected shape becomes invisible.
				for _, m := range ms {
					sites = append(sites, adminSite{file: file, line: i + 1, path: m[1],
						via: "NOT A REGISTRATION CALL", gated: false})
				}
				continue
			}
			stmt := statementAt(lines, i+1)
			via, ok := gateReachable(stmt, srcs, memo)
			for _, m := range ms {
				sites = append(sites, adminSite{file: file, line: i + 1, path: m[1], via: via, gated: ok})
			}
		}
	}
	sort.Slice(sites, func(a, b int) bool {
		if sites[a].file != sites[b].file {
			return sites[a].file < sites[b].file
		}
		return sites[a].line < sites[b].line
	})
	return sites
}

// adminSiteFloor — 25 /v1/admin literals were registered when this was written
// (28 paths once the held-mints loop expands, which the classification guard
// counts separately). A sweep that finds materially fewer has broken.
const adminSiteFloor = 20

func TestEveryAdminRegistrationReachesAnAuthorizationDecision(t *testing.T) {
	sites := adminRegistrationSites(t)
	if len(sites) < adminSiteFloor {
		t.Fatalf("only %d /v1/admin registration sites found, want >= %d — the sweep is broken, "+
			"and a broken sweep reports no ungated route", len(sites), adminSiteFloor)
	}

	for _, s := range sites {
		if !s.gated {
			t.Errorf("%s:%d registers %s and NO authorization decision is reachable from it.\n"+
				"    Every other /v1/admin registration reaches a read of `actx%s` — through "+
				"requireAdmin, requireAdminOrOperatorRead, a handler constructor that takes "+
				"authManager, an inline closure, or a handler value. This one reaches none, and "+
				"the route is on the `authed` group, so ANY tenant's API key can call it.\n"+
				"    ⚠ admin_route_classification_test.go will NOT catch this: it only checks "+
				"whether requireAdminOrOperatorRead is present, and absent is what it expects for "+
				"an operatorMustNotReach route.",
				s.file, s.line, s.path, adminAuthDecisionToken)
		}
	}

	if testing.Verbose() || t.Failed() {
		var lines []string
		for _, s := range sites {
			lines = append(lines, s.path+" → "+s.via)
		}
		t.Logf("admin gate census (%d sites):\n  %s", len(sites), strings.Join(lines, "\n  "))
	}
}

// The assumption the EXISTING guard rests on, pinned. admin_route_classification_
// test.go reads main.go and only main.go. Three other files in package main
// register routes (session_key_handler.go, bond_reads_handler.go,
// provision_handler.go), so an admin route added in one of those would be
// invisible to it rather than merely unclassified — and invisible is the failure
// direction that looks fine.
func TestAdminRoutesLiveOnlyInMainGo(t *testing.T) {
	srcs := packageMainSources(t)
	var strays []string
	found := 0
	for file, src := range srcs {
		for _, l := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "//") {
				continue
			}
			for _, m := range adminLiteralRe.FindAllStringSubmatch(l, -1) {
				found++
				if file != "main.go" {
					strays = append(strays, file+": "+m[1])
				}
			}
		}
	}
	if found < adminSiteFloor {
		t.Fatalf("only %d /v1/admin literals found across package main, want >= %d — the sweep "+
			"is broken", found, adminSiteFloor)
	}
	if len(strays) > 0 {
		sort.Strings(strays)
		t.Errorf("/v1/admin paths appear outside main.go:\n  %s\n"+
			"    admin_route_classification_test.go parses main.go ONLY, so these routes are "+
			"invisible to the classification tables — not unclassified, INVISIBLE. Either move "+
			"the registration into main.go or widen adminRoutesFromSource to read the package.",
			strings.Join(strays, "\n  "))
	}
}
