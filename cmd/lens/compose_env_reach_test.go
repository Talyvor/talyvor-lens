package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// compose_env_reach_test.go — a config value the compose file does not forward is MUTE: it can be
// set in .env, look correct in every review, and never reach the process.
//
// This has now happened twice, both found by accident. LENS_PROVISION_SECRET was mute, so signup
// 404'd every login while Lens looked healthy. LENS_CACHE_POOLABLE_ENABLED was mute, which is the
// global gate on cross-tenant pooling — set it by hand, run the royalty test, see no mint, and
// there is no way to tell "not implemented" from "not switched on".
//
// WHY A TEST AND NOT A REVIEW HABIT. `.env` is NOT passed into containers. Compose reads it only
// for ${VAR} substitution inside the compose file itself, and this stack declares no `env_file:`
// anywhere — so a variable reaches the process if and only if it appears in a service's
// `environment:` list. Nothing about editing .env tells you that. The gap is invisible from
// inside the process (an unset variable and an unforwarded one are the same empty string), so no
// behavioural test can find it. It is only visible from outside, in the file that forwards.
//
// WHAT THIS GUARDS, AND WHAT IT DELIBERATELY DOES NOT. Lens's config reads ~160 LENS_* variables
// and compose forwards ~28. Most of the difference is fine: feature flags that default off and
// nobody wants on, tuning knobs with sane defaults, alternatives to values compose already sets a
// better way. Requiring all 160 would be noise, and noise is how a guard gets deleted.
//
// So the list below is not "everything", it is everything whose absence is SILENT AND COSTLY. The
// inclusion rule, written down so the next person can decide rather than guess:
//
//	A variable belongs in mustForward if being unable to set it would (a) leave money uncollected,
//	unmetered or unbounded, (b) leave a security control unarmed, or (c) make a shipped capability
//	inert with no error anywhere. "It has a safe default" is NOT an exemption when the default is
//	the off position of the thing you are deploying — that is exactly the pooling case.
var mustForward = map[string]string{
	// ── the money path ────────────────────────────────────────────────────────
	"LENS_CACHE_POOLABLE_ENABLED":       "the GLOBAL gate on cross-tenant pooling. Mute ⇒ pooling never fires no matter how many workspaces opt in, and nothing reports it — the royalty test just shows no mint",
	"LENS_ECONOMY_ENABLED":              "the master economy switch. Mute ⇒ the operator cannot turn the economy off (or back on) at all",
	"LENS_POOL_ROYALTY_MINTING_ENABLED": "the royalty mint itself. Mute ⇒ contributors are never paid and the earning half is inert",
	"LENS_POOL_ROYALTY_SHARE":           "s, the contributor share. Also a SECURITY parameter — the anti-self-dealing property is that farming loses (1-s)*C, so an operator who cannot set it cannot tune it either",
	"LENS_DISTILL_POOLABLE_ENABLED":     "the distill half of pooling consent. Mute ⇒ distill reuse never pools",
	"LENS_MINT_RATE_CAP_LENS_24H":       "the 24h mint cap. Mute ⇒ the cap cannot be tightened in response to abuse",
	"LENS_SHADOW_MINTS_ENABLED":         "shadow mode for the six unproven mints. Mute ⇒ shadow mode is silently OFF while the operator believes the trial is being measured — and since a shadowed mint credits nothing, the observable symptom is an empty lens_shadow_mints table, indistinguishable from \"the mints never fired\"",

	// ── customer money in ─────────────────────────────────────────────────────
	"LENS_BILLING_ENABLED":       "Mute ⇒ checkout routes are never registered and a customer cannot buy LXC at all",
	"LENS_STRIPE_SECRET_KEY":     "Mute ⇒ billing cannot charge",
	"LENS_STRIPE_WEBHOOK_SECRET": "Mute ⇒ the webhook cannot verify signatures, so paid credit is never applied",
	"LENS_BILLING_SUCCESS_URL":   "Mute ⇒ a paying customer is returned to the compiled-in default, not this deployment",
	"LENS_BILLING_CANCEL_URL":    "Mute ⇒ same, on the cancel path",

	// ── customer credentials in custody ───────────────────────────────────────
	// Rule (b), and (c) with it. ⚠ ADDED BY HAND, WHICH IS THE POINT AND THE LIMIT: measured
	// 2026-08-26 (W6.10, tab-h3n8) — with LENS_PROVIDER_SECRET_KEK absent from docker-compose.yaml
	// ENTIRELY, this test was GREEN. mustForward is a list, so it is blind by construction to every
	// variable added after it was written, including one that meets its own inclusion rule. The
	// header above argues for the list over a 160-variable census and that argument still holds;
	// what is recorded here is that the rule is enforced by whoever remembers it, not by this file.
	"LENS_PROVIDER_SECRET_KEK": "the envelope KEK for CUSTOMER provider credentials (W6.10). Mute ⇒ Config.ProviderSecretsEnabled() is false, so BYOK custody is silently off — and the symptom is a capability that simply never works, with no error anywhere, which is inclusion rule (c) on top of (b)",
}

// TestComposeForwardsEveryCriticalConfigVar fails when a mustForward variable is not present in
// the lens service's environment list.
func TestComposeForwardsEveryCriticalConfigVar(t *testing.T) {
	compose := readCompose(t)
	lensEnv := lensServiceEnv(t, compose)

	var missing []string
	for name := range mustForward {
		if !forwards(lensEnv, name) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		t.Errorf("docker-compose.yaml does not forward %s to the lens service — %s.\n"+
			"    Add `- %s=${%s:-}` to the lens service's environment. `.env` alone does NOT reach "+
			"the container: compose reads it only for ${} substitution, and this stack declares no "+
			"env_file.", name, mustForward[name], name, name)
	}
}

// TestCriticalConfigVarsAreActuallyReadByConfig keeps the list honest in the other direction: a
// name that config no longer reads is a stale entry that would quietly pass forever.
func TestCriticalConfigVarsAreActuallyReadByConfig(t *testing.T) {
	raw, err := os.ReadFile("../../internal/config/config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	cfg := string(raw)
	for name := range mustForward {
		if !strings.Contains(cfg, `"`+name+`"`) {
			t.Errorf("mustForward lists %s but internal/config never reads it — the entry is stale "+
				"and is guarding nothing. Remove it, or fix the name.", name)
		}
	}
}

// requiredIsCorrect — variables for which `${VAR:?}` (refuse to start when absent) is the RIGHT
// choice, with the reason. The bar: the stack cannot do its job at all without a value, so
// failing loudly at boot beats starting into a broken state.
var requiredIsCorrect = map[string]string{
	"LENS_DOMAIN": "Caddy's public hostname. Without it there is no site to serve and no " +
		"certificate to obtain, so an empty value is not a degraded mode — it is a broken one.",
}

// TestForwardedVarsUseEmptyDefault — an OPTIONAL capability must be forwarded as `${VAR:-}`, not
// `${VAR:?}`. `:?` makes the whole stack refuse to start when the variable is absent, turning a
// capability a deployment never wanted into a hard boot dependency. That is the same mistake as
// the mute one, pointed the other way: instead of failing silently it fails everything.
//
// The exceptions are listed above with their justification rather than pattern-matched, so
// adding one is a decision somebody writes down.
func TestForwardedVarsUseEmptyDefault(t *testing.T) {
	compose := readCompose(t)
	re := regexp.MustCompile(`\$\{(LENS_[A-Z0-9_]+):\?`)
	for _, m := range re.FindAllStringSubmatch(compose, -1) {
		if _, ok := requiredIsCorrect[m[1]]; ok {
			continue
		}
		t.Errorf("%s is forwarded with `:?` (required). Use `:-` (empty default) so a deployment "+
			"that does not use this capability still boots — or add it to requiredIsCorrect with "+
			"the reason it genuinely cannot start without a value.", m[1])
	}
}

// forwards reports whether the block actually FORWARDS name — i.e. contains a real list entry
// `- NAME=...` — rather than merely mentioning it.
//
// ⚠ This distinction is the whole guard. A `strings.Contains(block, name)` version of this test
// passed while the forwarding line was deleted, because the comment above it explaining why the
// variable matters still contained the name. A guard that its own documentation satisfies is not
// a guard; it is a green light wired to nothing. Same shape as a grep that trips on the doc
// comment describing the thing it bans.
func forwards(block, name string) bool {
	re := regexp.MustCompile(`(?m)^\s*-\s*` + regexp.QuoteMeta(name) + `=`)
	return re.MatchString(block)
}

func readCompose(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../docker-compose.yaml")
	if err != nil {
		t.Fatalf("read docker-compose.yaml: %v", err)
	}
	return string(raw)
}

// lensServiceEnv returns the text of the `lens:` service block. Scoped deliberately: forwarding a
// variable to the one-shot `migrate` service does not help the process that serves traffic.
func lensServiceEnv(t *testing.T, compose string) string {
	t.Helper()
	start := strings.Index(compose, "\n  lens:")
	if start < 0 {
		t.Fatal("no `lens:` service in docker-compose.yaml — this guard has drifted from the file it protects")
	}
	rest := compose[start+1:]
	// The block ends at the next top-level service key (two-space indent, not four).
	end := len(rest)
	for i := 1; i < len(rest)-3; i++ {
		if rest[i] == '\n' && rest[i+1] == ' ' && rest[i+2] == ' ' && rest[i+3] != ' ' && rest[i+3] != '#' {
			end = i
			break
		}
	}
	return rest[:end]
}

// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE CODE-SIDE HALF. Everything above is a CURATED list, and a curated list catches only what
// someone remembered — which is the failure it exists to prevent. It did not catch
// LENS_MINT_KEY: #402 added the variable, the operator generated it into .env, and the digest
// inside the running container was the SHA-256 of the empty string. Nothing failed.
//
// So this half enumerates FROM THE CODE and asks compose to justify itself, rather than asking a
// human to remember. It cannot go stale, because adding os.Getenv("LENS_NEW_THING") to the tree
// is what makes it fire.
//
// ⚠ WHY A CLASS AND NOT "EVERY VARIABLE". The process reads ~169 LENS_* names; compose forwards
// ~53. Demanding all 169 would produce a 116-line failure nobody can act on, and a guard nobody
// can act on gets deleted — which is worse than no guard. The class chosen is the one whose
// absence is SILENT AND HAS NO SENSIBLE DEFAULT: credentials. A tuning knob's absence is its
// documented default; a credential's absence is a dead feature or an unarmed control. It is also,
// empirically, the class that has bitten: LENS_PROVISION_SECRET, then LENS_MINT_KEY, then
// LENS_AWS_SESSION_TOKEN — three for three.
//
// The rule is MECHANICAL (a name suffix), so it needs no judgement to apply and no memory to
// maintain. Judgement lives only in the exemptions below, and each one states its reason.

// credentialExemptions are credential-shaped names that must NOT be forwarded, with the reason.
// An exemption is a decision; it is written here so the next person can disagree with it rather
// than guess at it.
var credentialExemptions = map[string]string{
	"LENS_APP_DB_PASSWORD": "B39.6: read only by the `lens migrate` subcommand, to give the lens_app role its " +
		"login; docker-compose.yaml forwards it to the migrate service and builds the lens service's " +
		"LENS_DATABASE_URL from it. The server never reads it.",

	"LENS_RETENTION_DB_PASSWORD": "B39.6: read only by `lens migrate`, to give the lens_retention role its " +
		"login; forwarded to the migrate service. The server's sweeper takes LENS_AUDIT_RETENTION_DATABASE_URL.",

	"LENS_JWT_SECRET": "the RETIRED HS256 key. internal/config/config.go REFUSES TO START when it " +
		"is set (Lens moved to ES256), so forwarding it would convert a boot guard into a boot " +
		"failure. It must stay unforwarded.",

	"LENS_SYNTHETIC_KEY": "B17.1's operator key reaches the process through env_file: lens.env, where an " +
		"operator sets it (lens.env.example documents it). An environment: entry `${NAME:-}` would arrive " +
		"as \"\" and override the lens.env value (lens_env_shadow_test.go), so forwarding it would disable it.",

	"LENS_VAPID_PRIVATE_KEY": "B19.16's web-push signing key reaches the process through env_file: lens.env, " +
		"like LENS_SYNTHETIC_KEY and for the same reason: an environment: entry would arrive as \"\" and " +
		"override the lens.env value, turning the approval pushes off.",

	"LENS_APNS_KEY": "B40.15's APNs signing key reaches the process through env_file: lens.env, like " +
		"LENS_VAPID_PRIVATE_KEY and for the same reason: an environment: entry would arrive as \"\" and override the " +
		"lens.env value, turning the phones' approval pushes off.",

	"LENS_FCM_CREDENTIALS": "B40.15's FCM service-account key reaches the process through env_file: lens.env, " +
		"with LENS_APNS_KEY and for the same reason.",

	"LENS_OPERATOR_STEP_UP_SECRET": "B30.8's step-up secret for the compliance case file reaches the process " +
		"through env_file: lens.env, like LENS_SYNTHETIC_KEY and for the same reason: an environment: entry would " +
		"arrive as \"\" and override the lens.env value, so every compliance action would answer 503.",

	"LENS_STRIPE_ISSUING_WEBHOOK_SECRET": "B19.12's real-time card authorisation secret reaches the process " +
		"through env_file: lens.env, like LENS_VAPID_PRIVATE_KEY and for the same reason: an environment: entry " +
		"would arrive as \"\" and override the lens.env value, unregistering the endpoint so no purchase is approved.",

	"LENS_STRIPE_TEST_SECRET_KEY": "B25.2's Stripe test-mode key for synthetic workspaces reaches the process " +
		"through env_file: lens.env, like LENS_SYNTHETIC_KEY and for the same reason: an environment: entry would " +
		"arrive as \"\" and override the lens.env value, so no test user could pay.",

	"LENS_STRIPE_TEST_WEBHOOK_SECRET": "B25.2's test-mode webhook secret reaches the process through env_file: " +
		"lens.env, with LENS_STRIPE_TEST_SECRET_KEY and for the same reason.",

	"LENS_DEEPGRAM_API_KEY": "B40.10's speech key reaches the process through env_file: lens.env, like " +
		"LENS_VAPID_PRIVATE_KEY and for the same reason: an environment: entry would arrive as \"\" and override " +
		"the lens.env value, so /v1/audio/* would answer that speech is not configured.",

	"LENS_KYA_SIGNING_KEY": "B30.5's Know Your Agent signing key reaches the process through env_file: lens.env, " +
		"like LENS_VAPID_PRIVATE_KEY and for the same reason: an environment: entry would arrive as \"\" and override " +
		"the lens.env value, so every start would sign agent credentials with a new key.",

	"LENS_PERSONA_API_KEY": "B30.112's Persona key reaches the process through env_file: lens.env, like " +
		"LENS_KYA_SIGNING_KEY and for the same reason: an environment: entry would arrive as \"\" and override the " +
		"lens.env value, so identity checks would fall back to the Test provider.",

	"LENS_COMPANIES_HOUSE_API_KEY": "B30.112's Companies House key reaches the process through env_file: lens.env, " +
		"with LENS_PERSONA_API_KEY and for the same reason.",

	// ⚠ FOUR MORE EXEMPTIONS WERE WRITTEN HERE AND REMOVED, because
	// TestCredentialExemptionsAreStillRead refused them: LENS_TEST_NVIDIA_EAT{,_NONCE},
	// LENS_POVI_KEY_VALUE and LENS_POVI_KEY_CRASHER are read ONLY from _test.go files, which the
	// enumerator skips, so they were never candidates and exempting them was dead weight that
	// would hide the next real one. The staleness check caught that on its first run — which is
	// the argument for having it.
}

// credentialSuffixes is the mechanical class test. Deliberately name-based: no judgement to
// apply, nothing to keep in sync with a feature list.
var credentialSuffixes = []string{"_KEY", "_SECRET", "_TOKEN", "_PASSWORD", "_CREDENTIALS"}

func looksLikeCredential(name string) bool {
	for _, s := range credentialSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// readsLensEnvVars enumerates every LENS_* name the tree actually reads, from the SOURCE. This is
// the "never a hand-maintained list" requirement: the guard's input is the code.
func readsLensEnvVars(t *testing.T) map[string]string {
	t.Helper()
	pat := regexp.MustCompile(`(?:os\.Getenv|os\.LookupEnv|parseBoolEnv|getEnv|getEnvDuration)\(\s*"(LENS_[A-Z0-9_]+)"`)
	found := map[string]string{} // name -> first file that reads it
	for _, root := range []string{"..", "../../internal"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil // a test reading an env var says nothing about the deployment
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, m := range pat.FindAllStringSubmatch(string(b), -1) {
				if _, seen := found[m[1]]; !seen {
					found[m[1]] = path
				}
			}
			return nil
		})
	}
	return found
}

// TestEveryCredentialTheProcessReadsIsForwarded is the guard proper.
func TestEveryCredentialTheProcessReadsIsForwarded(t *testing.T) {
	compose := readCompose(t)
	lensEnv := lensServiceEnv(t, compose)
	reads := readsLensEnvVars(t)

	// The enumeration must actually find things; a broken regex would make this vacuously green.
	if len(reads) < 100 {
		t.Fatalf("only %d LENS_* reads found in the tree — the enumeration is broken, and a "+
			"guard that enumerates nothing passes everything", len(reads))
	}

	var missing []string
	for name, file := range reads {
		if !looksLikeCredential(name) {
			continue
		}
		if _, exempt := credentialExemptions[name]; exempt {
			continue
		}
		if !forwards(lensEnv, name) {
			missing = append(missing, name+"  (read at "+file+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("%s is a CREDENTIAL the process reads and docker-compose.yaml does not forward "+
			"to the lens service.\n"+
			"    It can be set in .env, look right in review, and reach nothing: compose reads .env "+
			"only for ${} substitution and this stack declares no env_file. The container starts "+
			"healthy and the feature is inert with no log line.\n"+
			"    Add `- NAME=${NAME:-}` to the lens service's environment, or add it to "+
			"credentialExemptions WITH ITS REASON.", m)
	}
}

// TestCredentialGuardIsNotVacuous — the class must DISTINGUISH. If the rule matched nothing, or
// matched everything, the test above would be theatre either way.
func TestCredentialGuardIsNotVacuous(t *testing.T) {
	compose := readCompose(t)
	lensEnv := lensServiceEnv(t, compose)
	reads := readsLensEnvVars(t)

	var creds, forwarded int
	for name := range reads {
		if !looksLikeCredential(name) {
			continue
		}
		creds++
		if forwards(lensEnv, name) {
			forwarded++
		}
	}
	if creds == 0 {
		t.Fatalf("the credential rule matched NOTHING — it cannot catch anything")
	}
	if forwarded == 0 {
		t.Fatalf("the credential rule matched %d names and NONE is forwarded — the rule is "+
			"selecting the wrong thing", creds)
	}
	t.Logf("credential class: %d read, %d forwarded, %d exempt", creds, forwarded, len(credentialExemptions))
}

// TestCredentialExemptionsAreStillRead keeps the exemptions honest in the other direction: an
// exemption for a name nothing reads any more is dead weight that hides the next real one.
func TestCredentialExemptionsAreStillRead(t *testing.T) {
	reads := readsLensEnvVars(t)
	for name, reason := range credentialExemptions {
		if _, ok := reads[name]; !ok {
			t.Errorf("credentialExemptions lists %s (%q) but nothing in the tree reads it any "+
				"more — remove the exemption rather than carrying it", name, reason)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is exempt with no reason — every exemption carries its reason", name)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE OTHER TWO SERVICES IN THIS FILE.
//
// ⚠ EVERYTHING ABOVE COVERS `lens:` ONLY, and that is a guard that READS as covering the file.
// docker-compose.yaml now runs three Talyvor binaries — lens, track and docs (#408 committed the
// track/track-migrate/docs blocks that had existed only on the server for twelve days). Track and
// Docs are DIFFERENT BINARIES from DIFFERENT REPOS, so their os.Getenv calls are invisible to the
// tree-walking enumeration above, and their undeclared variables went unnoticed for exactly the
// reason the Lens ones did.
//
// Measured on main before this change: DOCS_LENS_URL, DOCS_LENS_API_KEY, TRACK_LENS_URL and
// TRACK_LENS_API_KEY each appeared ZERO times. Docs' and Track's Lens integration has never been
// configured — third and fourth instances of the trap #406 closed for Lens.
//
// ⚠ WHY A DECLARED CONTRACT AND NOT CROSS-REPO ENUMERATION. The honest options were:
//
//	(a) walk ../talyvor-docs and ../talyvor-track for os.Getenv. Works on a developer's laptop
//	    with all three repos checked out as siblings. In CI it does NOT: this workflow checks out
//	    ONE repository, the others are not on disk, and a guard that silently no-ops when a path
//	    is missing is worse than no guard — it reports coverage it does not have.
//	(b) a git submodule or a vendored copy of their config. Real coupling and a new update
//	    burden, to read two constants.
//	(c) this: a small DECLARED contract per service, in this file, with the source line each
//	    entry was read from.
//
// (c) is chosen, and its weakness is stated rather than hidden: it is hand-maintained, which is
// the failure mode that produced this very bug. What makes it survivable is that it is SMALL and
// PINNED TO A CITATION — each entry names the file and line in the other repo it came from, so a
// reviewer can check it in one grep, and TestOtherServiceContractsCiteTheirSource fails if an
// entry has no citation. It is a contract, not a memory: if Docs renames a variable, this file is
// wrong in a way a person can see, rather than a test that quietly passes.
//
// ⚠ THIS IS NOT EQUIVALENT TO THE LENS GUARD ABOVE and must not be read as such. The Lens half
// cannot go stale because the code IS its input. This half can. It catches the failure that
// actually happened — a variable never declared at all — and not the failure where the other repo
// adds a new one. Closing that would need (a), and (a) needs all three repos in CI.

// otherServiceVar is one variable another service's binary reads, with the citation that proves it.
type otherServiceVar struct {
	name   string
	source string // repo-relative file:line the name was read from
	why    string
}

// docsMustDeclare — read from talyvor-docs @ origin/main.
var docsMustDeclare = []otherServiceVar{
	{"DOCS_LENS_URL", "talyvor-docs internal/config/config.go:168",
		"the Lens base URL. Empty ⇒ lensintegration.IsConfigured() is false and EVERY AI feature is a silent no-op"},
	{"DOCS_LENS_API_KEY", "talyvor-docs internal/config/config.go:169",
		"the ADMIN/MINTING credential (cmd/docs/main.go:186 hands it to lenscreds, which exchanges it at POST /v1/auth/token for a per-workspace JWT). ⚠ MUST hold LENS_MINT_KEY's value, never LENS_API_KEY's"},
}

// trackMustDeclare — read from talyvor-track @ origin/main.
//
// ⚠ FIVE, NOT FOUR. The brief said Track reads four; its config reads TRACK_LENS_URL,
// TRACK_LENS_API_KEY, TRACK_LENS_MINT_KEY, TRACK_LENS_WEBHOOK_SECRET and TRACK_LENS_DASHBOARD_URL
// (config.go:134-138), plus TRACK_LENS_WEBHOOK_FRESHNESS which has a 5m default and is a tuning
// knob, not a credential. The extra one matters: Track has a SEPARATE mint slot, so the mint key
// does NOT go in its API-key slot — see the comment on TRACK_LENS_API_KEY in the compose file.
var trackMustDeclare = []otherServiceVar{
	{"TRACK_LENS_URL", "talyvor-track internal/config/config.go:134",
		"the Lens base URL. Empty ⇒ the whole Lens integration is inert"},
	{"TRACK_LENS_MINT_KEY", "talyvor-track internal/config/config.go:136",
		"the narrow mint credential (cmd/track/main.go:259 → ai.New). ⚠ MUST hold LENS_MINT_KEY's value; Track's own config comment says 'Never set this to Lens's global LENS_API_KEY'"},
	{"TRACK_LENS_API_KEY", "talyvor-track internal/config/config.go:135",
		"the READ client (cmd/track/main.go:246). Declared so it CAN be set; see the compose comment for why it must stay EMPTY today"},
	{"TRACK_LENS_WEBHOOK_SECRET", "talyvor-track internal/config/config.go:137",
		"HMAC secret for the inbound Lens spend-alert webhook. MUST equal Lens's own LENS_TRACK_WEBHOOK_SECRET — different names, one value"},
	{"TRACK_LENS_DASHBOARD_URL", "talyvor-track internal/config/config.go:138",
		"where the AI-cost API links a human. Unset ⇒ no link is emitted (deliberate; talyvor-track #66)"},
}

// declaresMapForm reports whether a MAP-form service block declares `NAME:`.
//
// ⚠ MAP FORM, NOT LIST FORM. The lens service uses `- NAME=${NAME}`; track and docs use
// `NAME: ${NAME}`. forwards() above matches only the list shape, so reusing it here would return
// false for a correctly-declared variable and the test would be unfixable.
//
// It anchors on line-start + optional indent + the name + a colon, so a COMMENT mentioning the
// name does not satisfy it — the same distinction forwards() exists to make, and the reason this
// asserts against the parsed block rather than grepping the file.
func declaresMapForm(block, name string) bool {
	re := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `\s*:`)
	return re.MatchString(block)
}

// serviceEnv returns one top-level service block by name. Generalises lensServiceEnv, which was
// hard-coded to `lens:` — the hard-coding is part of why this gap existed.
func serviceEnv(t *testing.T, compose, service string) string {
	t.Helper()
	start := strings.Index(compose, "\n  "+service+":")
	if start < 0 {
		t.Fatalf("no `%s:` service in docker-compose.yaml — this guard has drifted from the file "+
			"it protects, or the service was renamed", service)
	}
	rest := compose[start+1:]
	end := len(rest)
	for i := 1; i < len(rest)-3; i++ {
		if rest[i] == '\n' && rest[i+1] == ' ' && rest[i+2] == ' ' && rest[i+3] != ' ' && rest[i+3] != '#' {
			end = i
			break
		}
	}
	return rest[:end]
}

func TestDocsServiceDeclaresItsLensConfig(t *testing.T) {
	block := serviceEnv(t, readCompose(t), "docs")
	for _, v := range docsMustDeclare {
		if !declaresMapForm(block, v.name) {
			t.Errorf("the `docs` service does not declare %s — %s.\n"+
				"    Read from %s. Docs is a DIFFERENT BINARY: this compose file is the only thing "+
				"that puts a value in front of it, and a variable set in .env but not named in its "+
				"`environment:` map reaches nothing.", v.name, v.why, v.source)
		}
	}
}

func TestTrackServiceDeclaresItsLensConfig(t *testing.T) {
	block := serviceEnv(t, readCompose(t), "track")
	for _, v := range trackMustDeclare {
		if !declaresMapForm(block, v.name) {
			t.Errorf("the `track` service does not declare %s — %s.\n"+
				"    Read from %s.", v.name, v.why, v.source)
		}
	}
}

// TestOtherServiceContractsCiteTheirSource is what makes a hand-maintained contract checkable: an
// entry with no citation cannot be verified by a reviewer, and an uncheckable entry is how the
// list rots into the thing it was meant to replace.
func TestOtherServiceContractsCiteTheirSource(t *testing.T) {
	for _, set := range [][]otherServiceVar{docsMustDeclare, trackMustDeclare} {
		for _, v := range set {
			if !strings.Contains(v.source, ".go:") {
				t.Errorf("%s cites %q, which is not a file:line in the owning repo — every entry "+
					"must be checkable in one grep", v.name, v.source)
			}
			if strings.TrimSpace(v.why) == "" {
				t.Errorf("%s has no reason; an entry nobody can justify is an entry nobody can remove", v.name)
			}
		}
	}
}

// TestMapFormMatcherIsNotSatisfiedByAComment — the assertion above is only worth anything if a
// comment mentioning the variable does NOT satisfy it. Pinned directly, because the list-form
// matcher exists precisely because that mistake was made once already.
func TestMapFormMatcherIsNotSatisfiedByAComment(t *testing.T) {
	commentOnly := "    environment:\n      # DOCS_LENS_URL: intentionally not set here\n      DOCS_LOG_LEVEL: info\n"
	if declaresMapForm(commentOnly, "DOCS_LENS_URL") {
		t.Error("a COMMENT mentioning DOCS_LENS_URL satisfied the matcher — that is the exact " +
			"failure the list-form matcher was written to avoid")
	}
	real := "    environment:\n      DOCS_LENS_URL: http://lens:8080\n"
	if !declaresMapForm(real, "DOCS_LENS_URL") {
		t.Error("a real map-form declaration did not satisfy the matcher — it would be unfixable")
	}
}
