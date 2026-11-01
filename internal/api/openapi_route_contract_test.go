package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// openapi_route_contract_test.go — the served OpenAPI document against the routes the binary
// actually registers.
//
// openapi.go is not a README. It is SERVED at GET /openapi.json, which means a client generates
// against it. W6.15 (#483) compared one route's RESPONSE BODY with components.schemas.LocalEndpoint
// and found it shipping four undeclared fields. That was one route and one schema; nobody had
// compared the PATH LIST.
//
// ⚠ THE TWO DIRECTIONS ARE NOT THE SAME KIND OF FACT, and this file treats them differently.
//
//	PUBLISHED BUT NOT REGISTERED   unambiguous. A client reads the contract, calls it, gets 404.
//	                               Measured at ZERO today, and guarded so it stays there.
//	REGISTERED BUT NOT PUBLISHED   a judgement. 122 non-admin /v1 routes are outside the document
//	                               and openapi.go's own header says why: "Lens has too many routes
//	                               to spec every single one". Counted and classified, NOT called a
//	                               defect — reporting it as one would be manufacturing a finding.
//
// ⚠ AND A FALSE FINDING GOT THIS FAR BEFORE BEING CAUGHT. The first run reported
// `POST /v1/proxy/openai/{path}` and `/v1/proxy/anthropic/{path}` as published-but-unregistered.
// They are registered — as `/v1/proxy/openai/*`. chi spells a wildcard `*`; OpenAPI spells it
// `{path}`. Two confident, serious-sounding, entirely false findings from a spelling difference,
// and it is the flattering direction. normalisePath below is what makes the comparison honest, and
// TestNormalisationCollapsesChiWildcardsAndOpenAPIParams is what keeps it that way.

// registration matches a chi route registration in main.go. The `With(...)` clause is optional and
// carries middleware, not path.
//
// ⚠ THE LEADING `/` IN THE PATH GROUP IS LOAD-BEARING. Without it this matches `q.Get("model")` —
// url.Values.Get, not a route — and the scoping count for W6.29 was inflated by 13 before that was
// noticed. A route census that counts query-parameter reads as routes overstates coverage.
var registration = regexp.MustCompile(`\b(\w+)\.(?:With\([^)]*\)\.)?(Get|Post|Put|Delete|Patch|Head|Options)\("(/[^"]*)"`)

var pathParam = regexp.MustCompile(`\{[^}]*\}`)

// normalisePath makes a chi path and an OpenAPI path comparable: parameter NAMES are not part of
// the route's identity, and chi's trailing `/*` is the same thing as OpenAPI's `/{path}`.
func normalisePath(p string) string {
	p = strings.ReplaceAll(p, "/*", "/{}")
	return pathParam.ReplaceAllString(p, "{}")
}

func repoRootForAPI(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("root %s has no go.mod", root)
	}
	return root
}

// lensRouteSource is the source of every non-test file in cmd/lens: main.go and the mount*Routes
// files it calls. B28.12: the agent wallet routes are registered in agent_*_handler.go, not main.go,
// so a parse of main.go alone reported every published wallet path as unregistered.
func lensRouteSource(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRootForAPI(t), "cmd", "lens", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list cmd/lens: %v (%d files)", err, len(files))
	}
	var b strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		b.Write(src)
		b.WriteByte('\n')
	}
	return b.String()
}

// registeredRoutes returns the set of "METHOD /normalised/path" the binary registers.
//
// ⚠ IT READS cmd/lens's SOURCE, AND THE REASON IS RECORDED RATHER THAN GLOSSED. chi.Walk over the
// real router would be better — it would see what the process serves. It is not reachable: the
// router is built inline inside run()'s full dependency graph, so there is no exported seam that
// returns a mounted router. cmd/lens/admin_route_classification_test.go states the same limit and
// this file inherits it rather than pretending otherwise.
//
// ⚠ MEASURED BEFORE RELYING ON IT: cmd/lens contains no `.Route(` and no `.Mount(`, so every path
// literal at a registration site IS the served path. TestNoMountOrRouteHidesAPrefix asserts that,
// because the day one appears every comparison here silently mis-attributes.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src := lensRouteSource(t)
	out := map[string]bool{}
	for _, m := range registration.FindAllStringSubmatch(src, -1) {
		// ⚠ ASSERTED, NOT ASSUMED. Control W4 removed the leading `/` from the regex and NOTHING
		// failed: the bogus entries it admits (`q.Get("model")` — url.Values.Get, not a route)
		// never start with /v1, so every downstream filter silently dropped them and the comment
		// claiming the guard was load-bearing was describing a discipline no assertion held. It
		// holds now.
		if !strings.HasPrefix(m[3], "/") {
			t.Errorf("parsed %q as a route path from `%s.%s(...)`. It is not a path — the most "+
				"likely culprit is url.Values.Get, which shares the method name and would inflate "+
				"any coverage count built on this parse.", m[3], m[1], m[2])
			continue
		}
		out[strings.ToUpper(m[2])+" "+normalisePath(m[3])] = true
	}
	if len(out) < 100 {
		t.Fatalf("parsed %d routes from main.go — the regex is not matching and every comparison "+
			"below would report a clean contract over nothing", len(out))
	}
	return out
}

// publishedOps returns "METHOD /normalised/path" for every operation in the served document.
func publishedOps(t *testing.T) map[string]bool {
	t.Helper()
	paths, ok := OpenAPISpec()["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("the served document declares no paths — an empty contract is trivially kept")
	}
	out := map[string]bool{}
	for p, v := range paths {
		ops, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("path %q has no operations map", p)
		}
		for method := range ops {
			out[strings.ToUpper(method)+" "+normalisePath(p)] = true
		}
	}
	return out
}

// ⚠ THE ONE UNAMBIGUOUS DIRECTION. Zero today; this is what keeps it zero.
func TestEveryPublishedPathIsRegistered(t *testing.T) {
	reg, pub := registeredRoutes(t), publishedOps(t)
	if len(pub) < 10 {
		t.Fatalf("only %d published operations — too few for this comparison to mean anything", len(pub))
	}
	var ghosts []string
	for op := range pub {
		if !reg[op] {
			ghosts = append(ghosts, op)
		}
	}
	sort.Strings(ghosts)
	if len(ghosts) > 0 {
		t.Errorf("%d operation(s) are published at GET /openapi.json and registered nowhere:\n  %s\n\n"+
			"A client generates against this document. A path in it that the binary does not serve "+
			"is a 404 the contract promised would work.",
			len(ghosts), strings.Join(ghosts, "\n  "))
	}
	t.Logf("MEASURED: %d published operations, all registered; %d routes registered in total.",
		len(pub), len(reg))
}

// ⚠ THE PREMISE THE PATH COMPARISON RESTS ON. A `.Route(` or `.Mount(` would mean the literal at a
// registration site is not the served path, and every result above becomes mis-attributed —
// silently, and in both directions.
func TestNoMountOrRouteHidesAPrefix(t *testing.T) {
	src := lensRouteSource(t)
	for _, bad := range []string{".Route(", ".Mount("} {
		if strings.Contains(src, bad) {
			t.Errorf("cmd/lens now uses %s. Route registrations are no longer served at the literal "+
				"path written beside them, so registeredRoutes() mis-attributes every route under "+
				"the prefix — resolve it before trusting anything in this file.", bad)
		}
	}
}

// ⚠ THE NORMALISER IS THE THING THAT NEARLY PRODUCED A FALSE FINDING, so it gets its own test.
func TestNormalisationCollapsesChiWildcardsAndOpenAPIParams(t *testing.T) {
	cases := []struct{ chi, oai string }{
		{"/v1/proxy/openai/*", "/v1/proxy/openai/{path}"},
		{"/v1/workspaces/{wsID}/config", "/v1/workspaces/{workspace_id}/config"},
		{"/v1/api/keys/{keyID}", "/v1/api/keys/{id}"},
	}
	for _, c := range cases {
		if normalisePath(c.chi) != normalisePath(c.oai) {
			t.Errorf("%q and %q normalise differently (%q vs %q) — they are the same route, and a "+
				"comparison that says otherwise reports a confident false finding",
				c.chi, c.oai, normalisePath(c.chi), normalisePath(c.oai))
		}
	}
	// And it must still DISCRIMINATE: a normaliser that collapsed everything would hide every
	// genuinely missing path.
	if normalisePath("/v1/a/{x}") == normalisePath("/v1/b/{x}") {
		t.Error("normalisePath collapses distinct paths — it would hide every real ghost")
	}
	if normalisePath("/v1/a/{x}") == normalisePath("/v1/a/{x}/b") {
		t.Error("normalisePath ignores path depth")
	}
}

// ── the header's own claim ──────────────────────────────────────────────────────────────────────

// declaredSurfaces are the surfaces openapi.go's header says this document covers, each with the
// pattern a published path for it would match.
//
// ⚠ THIS IS THE FALSIFIABLE PART OF A COMMENT NOBODY HAD CHECKED. The header names what the file
// covers; a named surface with no published path is a claim the document does not keep, and it is
// the kind a reader has no way to test short of reading every path.
var declaredSurfaces = []struct{ name, match string }{
	{"agent wallets", "/agents"},
	{"proxy endpoints", "/v1/proxy/"},
	{"key management", "/v1/api/keys"},
	{"workspaces", "/v1/workspaces/"},
	{"tenant config", "/config"},
	{"local-endpoint registry", "/v1/local/endpoints"},
}

func TestTheHeaderNamesOnlySurfacesTheDocumentCovers(t *testing.T) {
	paths, _ := OpenAPISpec()["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("no published paths")
	}
	for _, s := range declaredSurfaces {
		var found bool
		for p := range paths {
			if strings.Contains(p, s.match) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("openapi.go's header says this document covers %q and no published path "+
				"matches %q. A named surface with nothing behind it is a promise a generated "+
				"client cannot fulfil.", s.name, s.match)
		}
	}

	// ⚠ THE OTHER HALF, AND THE ONE THAT MAKES THIS NON-TAUTOLOGICAL. The surface list must be
	// checked against the header TEXT, or someone can satisfy this test by deleting a surface from
	// the list while leaving the header claiming it.
	src, err := os.ReadFile("openapi.go")
	if err != nil {
		t.Fatalf("read openapi.go: %v", err)
	}
	header := string(src)
	if i := strings.Index(header, "func OpenAPISpec"); i > 0 {
		header = header[:i]
	}
	for _, s := range declaredSurfaces {
		if !strings.Contains(header, s.name) {
			t.Errorf("declaredSurfaces lists %q and the header does not mention it — the list has "+
				"drifted from the claim it is checking", s.name)
		}
	}
	// ⚠ THE OMISSIONS MUST BE NAMED, AND NAMED ON THE RIGHT SIDE OF THE LINE. A word-ban would
	// stop the header ever DISCUSSING what it leaves out, which is the opposite of the point. So
	// the check is positional: `attribution` and `A/B` may appear only AFTER the NOT COVERED
	// marker. Claiming them as covered puts them before it and fails.
	const marker = "NOT COVERED"
	cut := strings.Index(header, marker)
	if cut < 0 {
		t.Fatalf("openapi.go's header has no %q section. This document covers 12 of 147 routes; a "+
			"header that lists only what it has is read as a complete contract.", marker)
	}
	claimed, omitted := header[:cut], header[cut:]
	for _, name := range []string{"attribution", "A/B"} {
		if strings.Contains(claimed, name) {
			t.Errorf("openapi.go's header claims to cover %q before the %q marker. Measured: ZERO "+
				"published paths for it, against 8 registered attribution routes and 6 "+
				"experiment/eval-A-B routes outside /v1/admin.", name, marker)
		}
		if !strings.Contains(omitted, name) {
			t.Errorf("openapi.go's header does not record %q as uncovered. An omission nobody wrote "+
				"down is indistinguishable from one nobody noticed.", name)
		}
	}
}

// ── the undocumented surface, counted and classified rather than called a defect ────────────────

// ⚠ 122 IS RECORDED SO IT CANNOT DRIFT IN SILENCE, NOT SO IT CAN BE DRIVEN TO ZERO. openapi.go says
// "Lens has too many routes to spec every single one" and that is a reasonable position for an
// admin-heavy API. What is NOT reasonable is the number changing by fifty without anyone noticing
// which way, so the count is pinned with a tolerance and its message says what each direction means.
//
// 122 → 124 (B6.4/B6.5): PUT /v1/workspaces/{wsID}/tare and GET /v1/workspaces/{wsID}/tare/savings,
// added undocumented beside their compression-policy sibling, which is undocumented too.
//
// 124 → 125 (B10.5): GET /v1/catalog/discovered, the models waiting for a price, beside the
// undocumented GET /v1/catalog/models it complements.
//
// 125 → 117 (B18.10): the A/B experiments API was unregistered — the seven
// /v1/workspaces/{wsID}/experiments* routes and GET /v1/workspaces/{wsID}/eval/ab/{experiment}.
//
// 117 → 118 (B19.12): /v1/workspaces/{wsID}/agents/{agentID}/card (GET and POST), an agent's test-mode
// card, beside the undocumented agent routes it belongs to.
//
// 118 → 119 (B25.2): POST /v1/billing/webhook/test, Stripe test mode's webhook for test users, beside the
// billing webhook it mirrors.
//
// 119 → 120 (B27.35): PUT /v1/workspaces/{wsID}/tare-model, the opt-in for Tare phase 2a's prose model,
// beside the undocumented PUT /v1/workspaces/{wsID}/tare it qualifies.
//
// 120 → 194 (B28.12): the parse now reads every cmd/lens file, not main.go alone, which finds 95 routes
// registered by the mount*Routes files that were never counted (215); and 21 agent wallet operations are
// now published (openapi_wallets.go).
//
// 194 → 195 (B32.17): GET /v1/workspaces/{wsID}/marketplace/journal, a seller's journal and whether it reconciles,
// beside the undocumented GET /v1/workspaces/{wsID}/marketplace/earnings it is reconciled with.
//
// 195 → 196 (B32.18): PUT /v1/workspaces/{wsID}/marketplace/listings/{listingID}/offers, how a listing is sold,
// beside the undocumented publish and version routes of the same listing.
//
// 196 → 198 (B32.19): POST /v1/workspaces/{wsID}/marketplace/listings/{listingID}/licences, buying, renting or
// subscribing to a listing, and GET /v1/workspaces/{wsID}/marketplace/licences, beside the undocumented use route.
//
// 198 → 199 (B32.20): POST /v1/workspaces/{wsID}/marketplace/licences/{licenceID}/cancel, a licence stops renewing,
// beside the undocumented licences routes.
//
// 199 → 201 (B32.24): PUT /v1/workspaces/{wsID}/marketplace/listings/{listingID}/remix-terms, whether a listing may
// be remixed and on what share, and GET /v1/marketplace/listings/{listingID}/lineage, its family tree, beside the
// undocumented publish and listing routes.
//
// 201 → 202 (B32.25): POST /v1/workspaces/{wsID}/marketplace/listings/{listingID}/remix, accepting a listing's remix
// licence and opening its artifact, beside the undocumented publish and listing routes.
//
// 202 → 203 (B35.1): POST /v1/synthetic/workspaces/{wsID}/plan, a test workspace moved to another plan, beside the
// undocumented synthetic-key routes it belongs to, registered only when LENS_SYNTHETIC_KEY is set.
//
// 203 → 210 (B32.28): rooms — POST /v1/workspaces/{wsID}/rooms, GET /v1/rooms, GET /v1/rooms/{roomID}, POST
// /v1/rooms/{roomID}/join, PUT /v1/rooms/{roomID}/terms, PATCH /v1/rooms/{roomID}/members/{ws} and POST
// /v1/rooms/{roomID}/agents, beside the undocumented marketplace routes they build on.
//
// 210 → 215 (B32.29): a private room's invites — POST and GET /v1/rooms/{roomID}/invites, DELETE
// /v1/rooms/{roomID}/invites/{inviteID}, GET /v1/room-invites/{token} and POST /v1/room-invites/{token}/join.
//
// 215 → 220 (B32.30): a room's messages — POST and GET /v1/rooms/{roomID}/messages, PATCH and DELETE
// /v1/rooms/{roomID}/messages/{messageID}, and the event stream GET /v1/rooms/{roomID}/events.
//
// 220 → 226 (B32.31): a room's contributions — POST and GET /v1/rooms/{roomID}/contributions, GET and PATCH
// /v1/rooms/{roomID}/contributions/{contributionID}, POST .../fork and PUT .../vote.
//
// 226 → 228 (B32.33): runs in a room — POST /v1/rooms/{roomID}/runs and POST /v1/rooms/{roomID}/ask.
//
// 228 → 231 (B32.35): room prizes — POST and GET /v1/rooms/{roomID}/prizes, POST .../prizes/{prizeID}/award.
// 231 → 234 (B32.47): IP claims — POST and GET /v1/workspaces/{wsID}/marketplace/ip-claims, POST .../ip-claims/{claimID}/counter.
// 234 → 237 (B32.49): the trust panel — GET /v1/marketplace/listings/{listingID}/trust, PUT
// /v1/workspaces/{wsID}/marketplace/listings/{listingID}/review and PUT .../reviews/{reviewID}/reply.
// 237 → 246 (B32.50): discovery — GET /v1/marketplace/search, GET /v1/marketplace/capabilities, PUT
// /v1/workspaces/{wsID}/marketplace/listings/{listingID}/capabilities, GET /v1/marketplace/collections and
// .../collections/{collectionID}, and GET, POST, PUT and DELETE /v1/workspaces/{wsID}/marketplace/collections[/{collectionID}].
// 246 → 248 (B32.52): POST /v1/rooms/{roomID}/reports and POST /v1/rooms/{roomID}/messages/{messageID}/reports.
// 250 → 252 (B32.40): GET /v1/workspaces/{wsID}/marketplace/receipts and .../receipts/{receiptID}.
// 252 → 254 (B32.41): GET and PUT /v1/workspaces/{wsID}/marketplace/seller-tax.
// 254 → 255 (B32.42): GET /v1/workspaces/{wsID}/marketplace/statements.
// 255 → 259 (B30.4): GET /v1/workspaces/{wsID}/verification and POST .../verification/contact, /identity and /company.
// 259 → 261 (B30.5): POST /v1/kya/verify and GET /v1/workspaces/{wsID}/agents/{id}/credential.
// 261 → 264 (B30.9): GET /v1/workspaces/{wsID}/terms, GET .../terms/{capability} and POST .../terms/{capability}/accept.
// 264 → 266 (B30.13): POST and GET /v1/money/accounts.
// 266 → 267 (B30.14): GET /v1/money/accounts/{id}/details.
// 267 → 268 (B27.36): PUT /v1/workspaces/{wsID}/tare-training, the opt-in to Tare's training set, beside tare-model.
// 268 → 272 (B30.16): POST and GET /v1/money/payees, POST /v1/money/payees/{id}/challenge and .../confirm.
// 272 → 275 (B30.17): POST and GET /v1/money/payments and GET /v1/money/payments/{id}.
// 275 → 276 (B30.18): POST /v1/workspaces/{wsID}/agents/schedules/{sid}/skip.
// 276 → 277 (B30.22): POST /v1/money/payments/quote.
// 277 → 278 (B46.4): GET /v1/workspaces/{wsID}/agents/{agentID}/wallet, an agent's wallet on the money layer.
// 278 → 279 (B41.106): GET /v1/workspaces/{wsID}/agents/briefing, the owner's daily briefing.
// 279 → 278 (B41.108): POST /v1/workspaces/{wsID}/agents/resume-all is now in the contract, beside its new challenge.
const undocumentedNonAdminV1Routes = 278

// 272 → 276 (B30.19): POST and GET /v1/money/mandates, POST /v1/money/mandates/{id}/revoke and /pull — counted on a
// line of their own, so a branch adding routes beside another's adds a line rather than rewriting the other's.
const undocumentedB3019Routes = 4

// 276 → 283 (B30.20): POST and GET /v1/money/invoices, POST /v1/money/invoices/{id}/send and /void, and the pay link's
// GET /v1/pay/{token}, POST /v1/pay/{token}/card and /agent — on a line of their own, as B30.19's are.
const undocumentedB3020Routes = 7

// 283 → 287 (B30.21): POST and GET /v1/money/payouts, GET /v1/money/payouts/{id} and POST .../{id}/approve — on a line
// of their own, as B30.19's and B30.20's are.
const undocumentedB3021Routes = 4

// 292 → 293 (B32.99): POST /v1/synthetic/workspaces/{wsID}/marketplace/payouts/run — on a line of its own, as B30.19's are.
const undocumentedB3299Routes = 1

// 293 → 294 (B32.102): POST /v1/synthetic/workspaces/{wsID}/card-link — on a line of its own, as B30.19's are.
const undocumentedB32102Routes = 1

// 294 → 297 (B28.125): POST /v1/workspaces/{wsID}/agents/{agentID}/prompt-schedules, GET …/agents/prompt-schedules and
// DELETE …/agents/prompt-schedules/{scheduleID} — on a line of their own, as B30.19's are.
const undocumentedB28125Routes = 3

// 297 → 298 (B17.189): GET /v1/workspaces/{wsID}/marketplace/invoices — on a line of its own, as B30.19's are.
const undocumentedB17189Routes = 1

// 298 → 303 (B40.2): POST /v1/money/permission-mandates/prepare, POST and GET /v1/money/permission-mandates, and POST
// …/{id}/revoke and …/{id}/evaluate — on a line of their own, as B30.19's are.
const undocumentedB402Routes = 5

// 303 → 306 (B40.3): POST /v1/money/payment-requests, GET …/{id} and POST …/{id}/cancel — on a line of their own.
const undocumentedB403Routes = 3

// 306 → 310 (B40.4): POST /v1/money/devices/challenge, POST and GET /v1/money/devices and POST …/{id}/revoke, and
// POST …/{id}/push-token (B40.15) — on a line of their own.
const undocumentedB404Routes = 5

// 310 → 314 (B40.5): POST /v1/money/payment-requests/{id}/approval, …/approve and …/pay, and GET …/record — on a line
// of their own.
const undocumentedB405Routes = 4

// 314 → 315 (B44.6): POST /v1/waitlist, the BFF's waitlist write on the provision secret — on a line of its own.
const undocumentedB446Routes = 1

// 316 → 317 (B40.38): GET /v1/money/payment-requests?status=pending_approval — on a line of its own.
const undocumentedB4038Routes = 1

// 317 → 321 (B45.23): GET /v1/a2a/agents/{agentID}/agent-card.json and …/jwks.json, POST /v1/a2a/verify, and POST
// /v1/workspaces/{wsID}/agents/{id}/agent-card/rotate — on a line of their own.
const undocumentedB4523Routes = 4

// B41.101: POST and GET /v1/money/bills and GET /v1/money/bills/{id} — on a line of their own, as B30.19's are.
const undocumentedB41101Routes = 3

// B41.102: POST /v1/money/bills/pay — on a line of its own, as B41.101's are.
const undocumentedB41102Routes = 1

// B46.3: POST /v1/money/webhooks/{partner}, each money partner's signed webhooks — on a line of its own.
const undocumentedB463Routes = 1

// 321 → 323 (B45.24): GET /v1/a2a/signature-check and POST /v1/a2a/signature-check/money — on a line of their own.
const undocumentedB4524Routes = 2

// B17.255: POST /v1/money/accounts/{id}/test-money, the owner's test money into a company account — on a line of its own.
const undocumentedB17255Routes = 1

// 332 (B48.1): GET …/agents/{agentID}/wallets, PUT …/agents/{agentID}/wallet-kind and GET …/agents/owner-wallets — an
// agent's wallets of each kind it is, and its owner's — on a line of their own.
const undocumentedB481Routes = 3

// 4 (B46.11): PUT and GET /v1/money/payout-details, GET /v1/money/earnings-payouts and POST /v1/rooms/{roomID}/payouts —
// earnings paid out on the money layer.
const undocumentedB4611Routes = 4

// B46.10: POST and GET /v1/money/cards, GET /v1/money/cards/{id} and POST …/{id}/approve — on a line of their own.
const undocumentedB4610Routes = 4

// B51.2: GET /v1/pricing, whether a card can be charged — on a line of its own.
const undocumentedB512Routes = 1

// B41.105: GET /v1/workspaces/{wsID}/agents/safe-to-spend — on a line of its own.
const undocumentedB41105Routes = 1

// B41.107: GET /v1/money/search — on a line of its own.
const undocumentedB41107Routes = 1

// B41.111: POST /v1/money/payees/{id}/called/challenge and POST /v1/money/payees/{id}/called — on a line of their own.
const undocumentedB41111Routes = 2

// B48.2: POST and GET …/agents/{agentID}/smart-account — a crypto agent's own smart account, owned by the person — and
// B50.25: POST …/smart-account/challenge, the challenge its owner key signs — on a
// line of their own.
const undocumentedB482Routes = 3

// B41.116: GET and POST /v1/money/lockdown and POST /v1/money/lockdown/lift — lost-phone lockdown — on a line of their own.
const undocumentedB41116Routes = 3

func TestUndocumentedNonAdminRouteCountIsRecorded(t *testing.T) {
	reg, pub := registeredRoutes(t), publishedOps(t)
	n := 0
	for op := range reg {
		if pub[op] {
			continue
		}
		p := strings.SplitN(op, " ", 2)[1]
		if strings.HasPrefix(p, "/v1/admin/") || !strings.HasPrefix(p, "/v1/") {
			continue
		}
		n++
	}
	if n != undocumentedNonAdminV1Routes+undocumentedB3019Routes+undocumentedB3020Routes+undocumentedB3021Routes+undocumentedB3299Routes+undocumentedB32102Routes+undocumentedB28125Routes+undocumentedB17189Routes+undocumentedB402Routes+undocumentedB403Routes+undocumentedB404Routes+undocumentedB405Routes+undocumentedB446Routes+undocumentedB4038Routes+undocumentedB4523Routes+undocumentedB41101Routes+undocumentedB41102Routes+undocumentedB463Routes+undocumentedB4524Routes+undocumentedB17255Routes+undocumentedB481Routes+undocumentedB4610Routes+undocumentedB512Routes+undocumentedB4611Routes+undocumentedB41105Routes+undocumentedB41107Routes+undocumentedB41111Routes+undocumentedB482Routes+undocumentedB41116Routes {
		t.Errorf("%d non-admin /v1 routes are outside the published contract; W6.29 measured %d.\n"+
			"    FEWER means somebody documented routes — good, and worth saying which.\n"+
			"    MORE means the tenant API grew and the contract did not. Neither is automatically "+
			"wrong; both should be deliberate.", n, undocumentedNonAdminV1Routes+undocumentedB3019Routes+undocumentedB3020Routes+undocumentedB3021Routes+undocumentedB3299Routes+undocumentedB32102Routes+undocumentedB28125Routes+undocumentedB17189Routes+undocumentedB402Routes+undocumentedB403Routes+undocumentedB404Routes+undocumentedB405Routes+undocumentedB446Routes+undocumentedB4038Routes+undocumentedB4523Routes+undocumentedB41101Routes+undocumentedB41102Routes+undocumentedB463Routes+undocumentedB4524Routes+undocumentedB17255Routes+undocumentedB481Routes+undocumentedB4610Routes+undocumentedB512Routes+undocumentedB4611Routes+undocumentedB41105Routes+undocumentedB41107Routes+undocumentedB41111Routes+undocumentedB482Routes+undocumentedB41116Routes)
	}
}
