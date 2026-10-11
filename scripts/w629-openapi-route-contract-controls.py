#!/usr/bin/env python3
"""W6.29 control campaign — the OpenAPI/route contract guard, every clause mutated."""
import hashlib, os, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SPEC = os.path.join(ROOT, "internal/api/openapi.go")
TEST = os.path.join(ROOT, "internal/api/openapi_route_contract_test.go")
FILES = [SPEC, TEST]

def sha(p): return hashlib.sha256(open(p, "rb").read()).hexdigest()

def run(test):
    r = subprocess.run(["go", "test", "-count=1", "-run", "^%s$" % test, "./internal/api/"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr

def anchored(path, old, new):
    s = open(path).read()
    n = s.count(old)
    if n != 1:
        raise AssertionError("anchor appears %d times, want 1: %r" % (n, old[:60]))
    open(path, "w").write(s.replace(old, new, 1))

GHOST = "TestEveryPublishedPathIsRegistered"
MOUNT = "TestNoMountOrRouteHidesAPrefix"
NORM  = "TestNormalisationCollapsesChiWildcardsAndOpenAPIParams"
HEAD  = "TestTheHeaderNamesOnlySurfacesTheDocumentCovers"
CNT   = "TestUndocumentedNonAdminRouteCountIsRecorded"

CONTROLS = [
    ("W1 a published path the binary does not serve", SPEC,
     '"/v1/local/endpoints": map[string]any{',
     '"/v1/local/endpoints/GHOST": map[string]any{"get": map[string]any{"summary": "x"}},\n\t\t\t"/v1/local/endpoints": map[string]any{',
     GHOST, NORM, "a 404 the contract promised would work is caught"),

    ("W2 the wildcard normalisation removed", TEST,
     '\tp = strings.ReplaceAll(p, "/*", "/{}")\n', "",
     GHOST, MOUNT, "without it, chi /* vs openapi {path} manufactures two false ghosts"),

    ("W3 the normaliser collapses everything", TEST,
     '\treturn pathParam.ReplaceAllString(p, "{}")',
     '\t_ = p\n\treturn "{}"',
     NORM, MOUNT, "an over-eager normaliser would hide every real ghost"),

    ("W4 the route regex loses its leading-slash guard", TEST,
     'Head|Options)\\("(/[^"]*)"`)', 'Head|Options)\\("([^"]*)"`)',
     CNT, NORM, "counting q.Get(\"model\") as a route overstates coverage"),

    ("W5 the cmd/lens parse neutered", TEST,
     '\tfor _, m := range registration.FindAllStringSubmatch(src, -1) {',
     '\tfor _, m := range registration.FindAllStringSubmatch(src[:0], -1) {',
     GHOST, NORM, "a broken parse hits the floor rather than reporting a clean contract"),

    ("W6 the header claims attribution again", SPEC,
     "// COVERED: agent wallets (agents, keys, funding, rules, approvals, statements, transfers and cards —",
     "// COVERED: attribution, A/B, agent wallets (agents, keys, funding, rules, approvals, statements, transfers and cards —",
     HEAD, GHOST, "re-claiming an uncovered surface before the NOT COVERED marker is caught"),

    ("W7 the NOT COVERED section deleted", SPEC,
     "// ⚠ NOT COVERED, AND THIS PARAGRAPH IS WHY W6.29 EXISTS.",
     "// (paragraph removed)",
     HEAD, GHOST, "an omission nobody wrote down cannot be told from one nobody noticed"),

    ("W8 a declared surface with no published path", TEST,
     '\t{"local-endpoint registry", "/v1/local/endpoints"},',
     '\t{"local-endpoint registry", "/v1/local/endpoints"},\n\t{"tenant config", "/v1/billing/invoices"},',
     HEAD, GHOST, "a named surface with nothing behind it is caught"),

    ("W9 the undocumented count drifts", TEST,
     "const undocumentedNonAdminV1Routes = 277", "const undocumentedNonAdminV1Routes = 60",
     CNT, GHOST, "the undocumented surface cannot change by fifty in silence"),

    ("W10 a Mount appears in main.go", TEST,
     '\tfor _, bad := range []string{".Route(", ".Mount("} {',
     '\tfor _, bad := range []string{".Route(", ".Mount(", "func main("} {',
     MOUNT, GHOST, "the premise the whole path comparison rests on is actually checked"),
]

before = {p: sha(p) for p in FILES}
print("BASELINE sha256")
for p in FILES:
    print("  %-36s %s" % (os.path.basename(p), before[p]))
ok, out = run("Test")
if not ok:
    sys.exit("package not green before the campaign:\n" + out[-2500:])
print("\nbaseline: package GREEN\n")

results = []
for name, path, old, new, red, green, proves in CONTROLS:
    backup = open(path).read()
    try:
        anchored(path, old, new)
        red_ok, red_out = run(red)
        green_ok, _ = run(green)
        verdict = "CAUGHT" if (not red_ok and green_ok) else ("MISSED" if red_ok else "COLLATERAL")
    except AssertionError as e:
        verdict, red_out = "ANCHOR-FAILED: %s" % e, ""
    finally:
        open(path, "w").write(backup)
    print("%-52s %s" % (name, verdict))
    print("     proves: %s" % proves)
    if verdict == "CAUGHT":
        hit = [l for l in red_out.splitlines() if "_test.go:" in l]
        if hit:
            print("     red says: %s" % hit[0].strip()[:140])
    results.append(verdict)
    print()

after = {p: sha(p) for p in FILES}
clean = all(before[p] == after[p] for p in FILES)
print("RESTORE PROOF")
for p in FILES:
    print("  %-36s %s" % (os.path.basename(p), "IDENTICAL" if before[p] == after[p] else "!! MUTATED !!"))
ok, _ = run("Test")
print("\npackage green after restore: %s" % ok)
c = results.count("CAUGHT")
print("\n%d/%d controls CAUGHT" % (c, len(results)))
sys.exit(0 if (c == len(results) and clean and ok) else 1)
