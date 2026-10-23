package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// admin_route_classification_test.go — EVERY /v1/admin ROUTE IS CLASSIFIED, FROM THE ROUTER SOURCE.
//
// W1.3 gives the suite BFF a Lens credential that can perform the cross-tenant ADMIN READS the
// operator screen needs. The dangerous half of that change is not the credential — it is the
// question "which routes does it reach", because the answer has to stay right as routes are added.
//
// ⚠ WHY THE INPUT IS THE SOURCE AND NOT A HAND-WRITTEN LIST. A curated list is already wrong here:
// `grep '"/v1/admin'` over main.go finds 25 registrations, but the real path count is 28 — four of
// the held-mint revocation routes are built in a LOOP from a string slice. A list someone typed
// would omit them, and an omitted route defaults into "nobody classified this", which is exactly
// how a write route ends up reachable. So this file parses main.go, expands that loop from its own
// slice literal, and REFUSES TO PASS if any path it finds is absent from the tables below.
//
// ⚠ WHY `chi.Walk` IS NOT USED. It would be better — it would see the routes the process actually
// serves rather than the text that registers them. It is not reachable from a test: the router is
// built inline inside run()'s full dependency graph (a live pool, redis, nats, ~40 constructors),
// so there is no exported seam that returns a mounted router. Saying that plainly beats shipping a
// walk over an empty router, which would pass while covering nothing.

// operatorReadable — the routes LENS_OPERATOR_READ_KEY may GET.
//
// The bar for membership, applied per route below: it returns DESCRIPTIVE data, it writes no row,
// it moves no money, and the operator screen (W1.4: workspace, spend, purchases, held, last
// activity) or its audit story actually needs it.
var operatorReadable = map[string]string{
	"/v1/admin/economy/flags":             "the live value of every economy flag as the process holds it. Observation of config; econflags.Handler 405s a non-GET on its own.",
	"/v1/admin/keel/findings":             "recorded drift findings. keel.Reader is Query-only.",
	"/v1/admin/keel/haircuts":             "applied drift haircuts, already-happened facts read back.",
	"/v1/admin/output-verdicts":           "forensic verdict read across workspaces.",
	"/v1/admin/workspaces":                "the tenant roster — the first column of the operator table. In-memory workspace.Manager, no DB write path.",
	"/v1/admin/routing-decisions/summary": "override rate + estimated cost delta. The estimate is descriptive, not money.",
	"/v1/admin/usage/summary":             "B17.7: requests, tokens, provider cost and serves by source across workspaces, synthetic apart. opsusage.Reader is Query-only.",
	"/v1/admin/workspaces/spend":          "B18.17: each workspace's spend, the operator table's spend column. opsusage.Reader is Query-only.",
	"/v1/admin/workspaces/held":           "B18.17: each workspace's unsettled LENS, the operator table's held column. Query-only.",
	"/v1/admin/workspaces/last-activity":  "B18.17: each workspace's last request, the operator table's last-activity column. Query-only.",
	"/v1/admin/distill/attribution":       "distill attribution rows. distillattrib.Reader is Query-only by construction.",
	"/v1/admin/billing/purchases":         "the purchases column of the operator table.",
	"/v1/admin/marketplace/parked-uses":   "B26.3: billed marketplace uses Stripe refused too often to retry, with its reason — for an operator to see to. market.Store.ParkedUses is Query-only.",
	"/v1/admin/tax-profiles/flagged":      "B32.38: buyer tax profiles whose Stripe evidence contradicts the declared country, with the reason — for an operator to see to. taxprofile.Store.Flagged is Query-only.",
	"/v1/admin/screening":                 "B30.6: each sanctions list — entries, when it was loaded, a failed download's reason — and the compliance cases screening opened, for an operator to see to. screening.Store.Lists and Screener.Cases are Query-only.",
	"/v1/admin/compliance/alerts":         "B30.7: each alert transaction monitoring raised on a compliance case — the rule, the payments, why — for an operator to see to. monitoring.Monitor.Alerts is Query-only.",
	"/v1/admin/reconciliation":            "B30.11: each day's reconciliation of the ledger against the partner's statements, with every break. DualTokenStore.ReconciliationRuns is Query-only.",
	"/v1/admin/safeguarding":              "B30.11: what customers hold in each currency against what the partner reports holding for them. DualTokenStore.Safeguarding is Query-only.",
	"/v1/admin/operator-audit":            "B27.28: the operator audit trail, filtered — the Operator screen shows it. operatoraudit.Store.List is Query-only.",
	"/v1/admin/operator-audit/export":     "B27.28: the same trail as a CSV download. operatoraudit.Store.List is Query-only.",
	"/v1/admin/waitlist":                  "B44.6: every talyvor.com waitlist signup, oldest first, for the operator's CSV. A SELECT only.",
	"/v1/admin/pool-royalty/detect":       "self-dealing detection read. Query-only reader, explicitly NOT economy-gated so forensics survive the kill switch.",
	"/v1/admin/pool-royalty/resolve":      "the resolution view of the same detection. Query-only.",
	"/v1/admin/pool-royalty/margin":       "margin observability. Query-only.",
	"/v1/admin/worktier/distribution":     "descriptive tier distribution for ONE workspace. Money-decoupled; never feeds mint/earn/billing.",
	"/v1/admin/distill-royalty/detect":    "the distill mirror of pool-royalty/detect. Query-only.",
	"/v1/admin/distill-royalty/resolve":   "the distill mirror of pool-royalty/resolve. Query-only.",
	"/v1/admin/distill-royalty/margin":    "the distill mirror of pool-royalty/margin. Query-only.",
}

// operatorMustNotReach — everything else, each with the reason it is out.
//
// ⚠ THE FIRST SEVEN ARE NAMED BY THE BRIEF ITSELF and are asserted individually below, so deleting
// a line here cannot quietly widen the credential.
var operatorMustNotReach = map[string]string{
	"/v1/admin/lxc/grant":                                        "MINTS LXC. Moves money.",
	"/v1/admin/workspaces/{wsID}/contract":                       "B32.10: puts a workspace on an Enterprise contract or ends it, with the contract's own fees. Sets a price; requireAdmin only.",
	"/v1/admin/workspaces/{wsID}/rate-limits":                    "B39.9: sets one workspace's request limits. Lifts a protection; requireAdmin only.",
	"/v1/admin/deletion-requests":                                "B21.3: which workspaces asked Talyvor to delete their data. requireAdmin only.",
	"/v1/admin/deletion-requests/{id}/complete":                  "B21.3: DELETES a workspace's stored answers and conversions. Irreversible.",
	"/v1/admin/conversion-rate/approve":                          "CHANGES A PRICE.",
	"/v1/admin/bonds/{bond_id}/settle":                           "slashes or releases a provenance bond. Moves money.",
	"/v1/admin/distill-royalty/adjudicate":                       "REVOKES A PAYOUT.",
	"/v1/admin/pool-royalty/adjudicate":                          "REVOKES A PAYOUT.",
	"/v1/admin/held-mints/traffic/adjudicate":                    "REVOKES A PAYOUT.",
	"/v1/admin/annotation-reputation/reset":                      "writes a reputation event. A write is a write even when it is not money.",
	"/v1/admin/held-mints/eval_contribution_mints/adjudicate":    "REVOKES A PAYOUT (loop-generated).",
	"/v1/admin/held-mints/routing_prediction_mints/adjudicate":   "REVOKES A PAYOUT (loop-generated).",
	"/v1/admin/held-mints/node_latency_mints/adjudicate":         "REVOKES A PAYOUT (loop-generated).",
	"/v1/admin/held-mints/confidential_compute_mints/adjudicate": "REVOKES A PAYOUT (loop-generated).",
	"/v1/admin/catalog/models/{id}/price":                        "SETS A PRICE (B10.5: puts a discovered model in the catalog at the rate a person confirmed).",
	"/v1/admin/marketplace/review":                               "B20.4: the marketplace's held and reported listings, with what reporters wrote about them. requireAdminOrModerator: the global key or a B20.13 moderator key, never the operator read key.",
	"/v1/admin/marketplace/listings/{listingID}/approve":         "B20.4: releases a held listing to every buyer.",
	"/v1/admin/marketplace/listings/{listingID}/takedown":        "B20.4: TAKES A LISTING DOWN AND REFUNDS ITS BUYERS. Moves money.",
	"/v1/admin/marketplace/ip-claims":                            "B32.47: the undecided IP claims, with the claimant's evidence and the seller's counter-notice. requireAdminOrModerator, never the operator read key.",
	"/v1/admin/marketplace/ip-claims/{claimID}/decide":           "B32.47: DECIDES AN IP CLAIM — upheld takes the listing down and refunds its buyers, attributed pays the claimant a royalty on every sale, rejected releases the held earnings. Moves money.",
	"/v1/admin/marketplace/parked-uses/{useID}/retry":            "B27.19: puts a parked use back in the metering queue, onto its buyer's bill. Moves money.",
	"/v1/admin/marketplace/collections/{collectionID}/feature":   "B32.50: features a public collection, listed first in the marketplace, and writes an operator audit row. A write.",
	"/v1/admin/rooms/reports":                                    "B32.52: the reported rooms with each report, its reporter's workspace and the reported message's text. requireAdminOrModerator, never the operator read key.",
	"/v1/admin/rooms/{roomID}/moderate":                          "B32.52: KEEPS, LOCKS, UNLOCKS OR CLOSES A ROOM — a closed room's budget is spent no more — and writes an operator audit row. A write.",
	"/v1/admin/operator-audit/record":                            "B27.28: appends to the append-only operator audit trail. A write is a write even when it is not money; the web app writes it on its moderator key.",
	"/v1/admin/screening/cases/{caseID}/release":                 "B30.6: RELEASES A HELD SANCTIONS MATCH — the payment or payee it held may then move money. requireStepUp (B30.8), with an operator audit row.",
	"/v1/admin/screening/cases/{caseID}/refuse":                  "B30.6: refuses a held sanctions match, so the payment or payee never moves money, and records who decided. requireStepUp (B30.8), with an operator audit row.",
	"/v1/admin/compliance/cases":                                 "B30.8: the compliance cases and a workspace's freeze. requireAdmin only.",
	"/v1/admin/compliance/cases/{caseID}":                        "B30.8: the case file — the owner's verified name and company number, the agents, the alerts, the notes. requireAdmin only.",
	"/v1/admin/compliance/cases/{caseID}/notes":                  "B30.8: adds an operator's note to a case, an operator audit row. A write; requireStepUp.",
	"/v1/admin/compliance/cases/{caseID}/freeze":                 "B30.8: FREEZES A WORKSPACE'S MONEY — every AMBER and RED capability refuses it. A write; requireStepUp.",
	"/v1/admin/compliance/cases/{caseID}/unfreeze":               "B30.8: LIFTS A FREEZE, so the workspace's money moves again. A write; requireStepUp.",
	"/v1/admin/compliance/cases/{caseID}/close":                  "B30.8: closes a monitoring case with a reason. A write; requireStepUp.",
	"/v1/admin/compliance/cases/{caseID}/export":                 "B30.8: the case file as a report draft for filing, recorded in the audit trail. requireStepUp.",
	"/v1/admin/platform-reports":                                 "B32.44: WRITES THE PLATFORM-REPORTING EXPORT, the sellers' TINs, dates of birth and account numbers in clear, and records the run. requireAdmin only, its list of runs too.",

	// ⚠ MEASURED, AND THE PROVISIONAL CLASSIFICATION WAS WRONG. The W1.3 groundwork report listed
	// attest/{output_id} as provisionally READ because it is registered with r.Handle and reads
	// like a lookup. It is not: newAttestHandler runs a sandboxed COMPILE of an uploaded source
	// tree (a 128 MiB request body) and its result carries `recorded` — it WRITES a
	// talyvor_verified verdict attributed to the output's owner. It is a write route with a
	// read-shaped name.
	"/v1/admin/attest/{output_id}": "WRITES a talyvor_verified verdict from an uploaded 128 MiB source tree. Read-shaped name, write behaviour — measured, not assumed.",

	// ⚠ TRAP 3, MEASURED. "preview" reads like a read and the verb says write. It persists nothing
	// (no model call, no token_events, no spend — it is a dry run), so the provisional table parked
	// it in OUT pending measurement. It STAYS OUT, for a reason the dry-run framing hides: it feeds
	// an attacker-supplied document to distill.ProcessIsolator, a 512 MiB subprocess. A credential
	// that can spend server CPU and memory on uploaded files is not a read credential. It is also
	// gated by its own inline IsAdmin closure rather than requireAdmin, so it was never a candidate.
	"/v1/admin/distill/preview": "POST that spawns a 512 MiB converter subprocess over an uploaded document. Persists nothing, but consuming compute on attacker-supplied bytes is not a read.",
}

// adminPathRe finds every literal /v1/admin path in the router source.
var adminPathRe = regexp.MustCompile(`"(/v1/admin[^"]*)"`)

func readMainGo(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(raw)
}

// adminRoutesFromSource returns every /v1/admin path the router registers, with the loop expanded.
//
// ⚠ READ FROM THE AST SINCE #518. The regex it replaced matched a /v1/admin literal anywhere
// in the file, so a commented-out registration still counted as a registered route and
// TestClassificationTablesHaveNoGhosts could not see a route go. Enumeration parity was
// measured before the switch, not assumed: the parser and the regex agree EXACTLY on the
// real main.go — 25 paths each, 0 seen by one and not the other.
func adminRoutesFromSource(t *testing.T) map[string]bool {
	t.Helper()
	src := []byte(readMainGo(t))

	mintTypes, err := heldMintTypesFromAST("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if len(mintTypes) == 0 {
		t.Fatal("the held-mints loop's []string literal was not found in main.go — the expansion " +
			"is broken, and four adjudicate routes would silently classify as non-existent")
	}

	scanned, err := scanAdminRoutes("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	paths := map[string]bool{}
	for _, r := range scanned {
		// The loop's fragment `"/v1/admin/held-mints/" + mt + "/adjudicate"` yields the prefix.
		// Expand it into the concrete four rather than classifying a prefix.
		if r.path == "/v1/admin/held-mints/" {
			for _, mt := range mintTypes {
				paths[r.path+mt+"/adjudicate"] = true
			}
			continue
		}
		paths[r.path] = true
	}
	return paths
}

// TestEveryAdminRouteIsClassified — the guard proper. A route nobody classified is a route whose
// reachability nobody decided.
func TestEveryAdminRouteIsClassified(t *testing.T) {
	routes := adminRoutesFromSource(t)

	// Non-vacuity: the groundwork enumeration counted 28. Fewer means the parse broke, and a guard
	// that enumerates nothing passes everything.
	if len(routes) < 28 {
		t.Fatalf("only %d /v1/admin paths parsed from main.go, want >= 28 — the enumeration is "+
			"broken. Found: %v", len(routes), sortedKeys(routes))
	}

	for p := range routes {
		_, read := operatorReadable[p]
		_, out := operatorMustNotReach[p]
		switch {
		case read && out:
			t.Errorf("%s is in BOTH tables — it cannot be both reachable and unreachable", p)
		case !read && !out:
			t.Errorf("%s is registered by the router and classified by NEITHER table.\n"+
				"    Add it to operatorReadable (with why a read credential may GET it) or to "+
				"operatorMustNotReach (with why it must not). Leaving it out is not neutral: an "+
				"unclassified route is one nobody decided about.", p)
		}
	}
}

// TestClassificationTablesHaveNoGhosts — the other direction. A table entry naming a route the
// router no longer registers is a line that guards nothing and reads as though it does.
func TestClassificationTablesHaveNoGhosts(t *testing.T) {
	routes := adminRoutesFromSource(t)
	for _, table := range []struct {
		name string
		m    map[string]string
	}{
		{"operatorReadable", operatorReadable},
		{"operatorMustNotReach", operatorMustNotReach},
	} {
		for p := range table.m {
			if !routes[p] {
				t.Errorf("%s names %s, which the router does not register. Stale entry — remove it, "+
					"or fix the path.", table.name, p)
			}
		}
	}
}

// TestEveryClassificationCarriesAReason — an entry with an empty reason is a decision nobody made.
func TestEveryClassificationCarriesAReason(t *testing.T) {
	for _, table := range []struct {
		name string
		m    map[string]string
	}{
		{"operatorReadable", operatorReadable},
		{"operatorMustNotReach", operatorMustNotReach},
	} {
		for p, why := range table.m {
			if len(strings.TrimSpace(why)) < 10 {
				t.Errorf("%s[%s] has no reason. Every entry states why, so the next person can "+
					"disagree with a decision rather than guess at one.", table.name, p)
			}
		}
	}
}

// ⚠ THE WIRING PROOF. The tables above are prose until something checks the ROUTER agrees with
// them. This reads each route's registration site and asserts the gate actually used there:
// a readable route must be wrapped in requireAdminOrOperatorRead, and an unreachable one must NOT
// be. Without this, moving a path between the tables would change a comment and nothing else.
//
// ⚠ IT DECIDED THE GATE BY A THREE-LINE TEXT WINDOW UNTIL #518, AND A LINE BREAK DEFEATED IT.
// Measured by rewriting the /v1/admin/lxc/grant registration — "MINTS LXC. Moves money.", one of
// the six the brief names by hand — in main.go, arms in
// ~/talyvor-queue/w61-adminclass-mutation-controls-h2r7.py: widening it to
// requireAdminOrOperatorRead on ONE line was CAUGHT; the same widening with the gate three lines
// below the path was MISSED, and so was the same widening through
// `gate := requireAdminOrOperatorRead`. An LXC-minting route became reachable by the operator READ
// credential with all five guards in this file green. The gate is now the handler ARGUMENT of the
// registration call, read from the AST and followed through aliases, so neither spacing nor a
// local binding can hide it — and a comment naming the gate no longer counts as one.
func TestClassificationMatchesTheGateAtEachRegistrationSite(t *testing.T) {
	src := []byte(readMainGo(t))
	scanned, err := scanAdminRoutes("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	// Non-vacuity: a scan that found no registrations would assert nothing about any of them.
	if len(scanned) < 25 {
		t.Fatalf("only %d /v1/admin registrations parsed from main.go — the scan is blind, "+
			"and this proof is vacuous", len(scanned))
	}

	for _, r := range scanned {
		if r.path == "/v1/admin/held-mints/" {
			// Loop-generated adjudicate routes; all four are OUT and none may carry the gate.
			if r.gated {
				t.Errorf("the held-mints adjudicate loop at main.go line %d is wrapped in "+
					"requireAdminOrOperatorRead — four PAYOUT REVOCATION routes just became "+
					"reachable by a read credential", r.line)
			}
			continue
		}
		if _, ok := operatorReadable[r.path]; ok {
			if !r.gated {
				t.Errorf("main.go line %d registers %s, classified operatorReadable, but its gate is "+
					"not requireAdminOrOperatorRead — the classification says reachable and the "+
					"router says otherwise", r.line, r.path)
			}
			continue
		}
		if _, ok := operatorMustNotReach[r.path]; ok && r.gated {
			t.Errorf("main.go line %d registers %s, classified operatorMustNotReach, and it IS "+
				"wrapped in requireAdminOrOperatorRead. %s", r.line, r.path, operatorMustNotReach[r.path])
		}
	}
}

// ⚠ THE BRIEF'S OWN LIST, ASSERTED ONE BY ONE. W1.3 names these as "each its own assertion" —
// a loop over a table would go green if the table lost a row.
func TestBriefNamedRoutesAreOut(t *testing.T) {
	for _, p := range []string{
		"/v1/admin/lxc/grant",
		"/v1/admin/conversion-rate/approve",
		"/v1/admin/bonds/{bond_id}/settle",
		"/v1/admin/distill-royalty/adjudicate",
		"/v1/admin/held-mints/traffic/adjudicate",
		"/v1/admin/annotation-reputation/reset",
	} {
		if _, ok := operatorReadable[p]; ok {
			t.Errorf("%s is in operatorReadable. The brief names it as must-stay-unreachable: "+
				"anything that moves money, revokes a payout or changes a price is OUT.", p)
		}
		if _, ok := operatorMustNotReach[p]; !ok {
			t.Errorf("%s is not in operatorMustNotReach. The brief names it explicitly; dropping "+
				"the row would let TestEveryAdminRouteIsClassified be the only thing standing "+
				"between it and reach.", p)
		}
	}
	// The seventh is not under /v1/admin, so it is asserted here rather than in the tables.
	src := readMainGo(t)
	if idx := strings.Index(src, `"/v1/api/injection/patterns"`); idx >= 0 {
		window := src[idx:min(idx+400, len(src))]
		if strings.Contains(window, "requireAdminOrOperatorRead(") {
			t.Error("/v1/api/injection/patterns is wrapped in requireAdminOrOperatorRead — it " +
				"mutates the process-wide injection detector and accepts arbitrary regex")
		}
	} else {
		t.Error(`"/v1/api/injection/patterns" is not in main.go — the brief names it as ` +
			`must-stay-unreachable and this assertion has drifted from the code`)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
