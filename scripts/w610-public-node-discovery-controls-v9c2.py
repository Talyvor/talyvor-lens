#!/usr/bin/env python3
"""w610-public-node-discovery-controls-v9c2.py — the positive-control campaign for the
public node-discovery leak fix.

Every control is a MUTATION applied to the working tree, run, observed, and reverted. The three
rules this repo's earlier campaigns learned the hard way, enforced mechanically here:

  1. ASSERT THE ANCHOR COUNT BEFORE EDITING. A substitution that matches nothing edits zero bytes,
     and "the control did not apply" is byte-indistinguishable from "the guard caught it". Every
     control asserts its anchor occurs exactly once before it is allowed to run.

  2. EVERY CONTROL NAMES A TEST THAT MUST STAY GREEN. A mutation that breaks the build reds every
     test in the package, including the one the control claims to have tripped — so a control with
     no green companion cannot tell "caught" from "nothing compiled".

  3. RESTORE AND PROVE IT. Each file's sha256 is taken before the edit and re-checked after revert.

The real-Postgres controls (C9, C10) need LENS_TEST_DATABASE_URL; without it the tests they drive
SKIP, and a skip is reported as an UNARMED control rather than counted as a pass.

Run from the repo root:  python3 scripts/w610-public-node-discovery-controls-v9c2.py
"""

import hashlib
import os
import subprocess
import sys

PUB = "cmd/lens/public_node_discovery.go"
PUBTEST = "cmd/lens/public_node_discovery_test.go"
MAIN = "cmd/lens/main.go"
COMPUTE = "internal/mining/compute_mining.go"
EMBED = "internal/mining/embedding_mining.go"

LENSPKG = "./cmd/lens/"
MININGPKG = "./internal/mining/"

LEAK = "TestPublicNodeDiscovery_DoesNotNameTheOwnerOrItsEndpoint"
LEAK_EMBED = "TestPublicEmbeddingNodeDiscovery_DoesNotNameTheOwnerOrItsEndpoint"
CHOOSE = "TestPublicNodeDiscovery_StillAnswersWhichNodeToChoose"
OWNER = "TestOwnerScopedNodeShapeStillCarriesURLAndWorkspace"
HASH_GUARD = "TestNodeSecretHashIsNeverMarshalled"
MOUNT = "TestPublicDiscoveryRoutesAreRegisteredWithoutAuth"
ONLYPROJ = "TestAvailableNodeListersAreReachedOnlyThroughTheProjection"

XTENANT = "TestListAvailableNodes_ReturnsEveryTenantsRowsWithURLAndOwner"
FILTERS = "TestListAvailableNodes_VerifiedAndActiveFiltersActuallyBind"
MODELFILTER = "TestListAvailableNodes_ModelFilterActuallyBinds"


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def write(path, s):
    with open(path, "w", encoding="utf-8") as f:
        f.write(s)


def run_test(pkg, name):
    """(passed, skipped) for the named test in the named package."""
    r = subprocess.run(
        ["go", "test", pkg, "-run", "^" + name + "$", "-count=1", "-v"],
        capture_output=True, text=True,
    )
    skipped = ("--- SKIP: " + name) in r.stdout
    return r.returncode == 0, skipped


# (id, description, [(file, old, new)], package, must_red, green_pkg, must_stay_green)
CONTROLS = [
    ("C1", "revert the compute fix — the anonymous route marshals the owner struct again",
     [(PUB, "writeJSONOK(w, http.StatusOK, publicNodeView(nodes))",
       "writeJSONOK(w, http.StatusOK, nodes)")],
     LENSPKG, LEAK, LENSPKG, LEAK_EMBED),

    ("C2", "revert the embedding fix — same, on the embedding route",
     [(PUB, "writeJSONOK(w, http.StatusOK, publicEmbeddingNodeView(nodes))",
       "writeJSONOK(w, http.StatusOK, nodes)")],
     LENSPKG, LEAK_EMBED, LENSPKG, LEAK),

    # ⚠ THE ONE THAT MATTERS MOST. The leak test asserts on RAW BYTES, not field names, and this is
    # the control that proves the difference: put the URL back under a different key. A field-name
    # assertion would go green here while the tenant's endpoint is still on the wire.
    ("C3", "re-add the URL under a DIFFERENT field name (`endpoint`) — a rename must not launder it",
     [(PUB, '\tPricePerToken float64   `json:"price_per_token"`\n',
       '\tPricePerToken float64   `json:"price_per_token"`\n\tEndpoint      string    `json:"endpoint"`\n'),
      (PUB, "MaxConcurrent: n.MaxConcurrent, PricePerToken: n.PricePerToken, CreatedAt: n.CreatedAt,",
       "MaxConcurrent: n.MaxConcurrent, PricePerToken: n.PricePerToken, CreatedAt: n.CreatedAt, Endpoint: n.URL,")],
     LENSPKG, LEAK, LENSPKG, CHOOSE),

    # ⚠ THE VACUITY CONTROL. Gut the fixture so the needle is not in the input. Before `armed` was
    # added, this made the leak test PASS while proving nothing; now it must RED on the fixture.
    ("C4", "gut the fixture — ws-A's node has no URL, so the leak assertion has nothing to find",
     [(PUBTEST, 'URL: "http://gpu-a.internal.acme.example:8080",', 'URL: "",')],
     LENSPKG, LEAK, LENSPKG, LEAK_EMBED),

    ("C5", "empty the projection — the route answers nothing at all",
     [(PUB, "\tout := make([]publicInferenceNode, 0, len(nodes))\n\tfor _, n := range nodes {",
       "\tout := make([]publicInferenceNode, 0, len(nodes))\n\tfor _, n := range nodes[:0] {")],
     LENSPKG, LEAK, LENSPKG, LEAK_EMBED),

    ("C6", "put the anonymous discovery group behind AuthMiddleware",
     [(MAIN, "\tr.Group(func(pub chi.Router) {\n\t\tpub.Use(ratelimit.RateLimitMiddleware(rateLimiter))\n\n\t\t// Token earning rates",
       "\tr.Group(func(pub chi.Router) {\n\t\tpub.Use(ratelimit.RateLimitMiddleware(rateLimiter))\n\t\tpub.Use(auth.AuthMiddleware(keyStore, authManager))\n\n\t\t// Token earning rates")],
     LENSPKG, MOUNT, LENSPKG, LEAK),

    ("C7", "break the parse — the route literal main.go registers is not the one the guard looks for",
     [(MAIN, 'pub.Get("/v1/nodes/available", newPublicAvailableNodesHandler(computeMiner))',
       'pub.Get("/v1/nodes/available-RENAMED", newPublicAvailableNodesHandler(computeMiner))')],
     LENSPKG, MOUNT, LENSPKG, LEAK),

    ("C8", "a THIRD caller reaches the cross-tenant lister inline, bypassing the projection",
     [(MAIN, "\t\t// Discovery: GPU nodes available for a model. UNAUTHENTICATED —",
       "\t\tif _, err := computeMiner.ListAvailableNodes(context.Background(), \"control\"); err != nil {\n\t\t\t_ = err\n\t\t}\n\t\t// Discovery: GPU nodes available for a model. UNAUTHENTICATED —")],
     LENSPKG, ONLYPROJ, LENSPKG, LEAK),

    # ⚠ THE INERT-FILTER CONTROL. A filter present in the SQL text and absent from the plan is the
    # class this queue keeps finding. Delete it and the guard must say so.
    ("C9", "drop `verified = TRUE AND active = TRUE` from the published-population query",
     [(COMPUTE, "WHERE verified = TRUE AND active = TRUE AND $1 = ANY(models)",
       "WHERE $1 = ANY(models)")],
     MININGPKG, FILTERS, MININGPKG, MODELFILTER),

    ("C10", "blank the owner on the way out of the scanner",
     [(COMPUTE, "if err := rows.Scan(&n.ID, &n.WorkspaceID, &n.URL, &n.Provider, &n.Models,",
       "var discardWS string\n\t\tif err := rows.Scan(&n.ID, &discardWS, &n.URL, &n.Provider, &n.Models,")],
     MININGPKG, XTENANT, MININGPKG, FILTERS),

    ("C11", "drop the `json:\"-\"` that keeps the node secret hash off the wire",
     [(EMBED, '\tNodeSecretHash string `json:"-"`', '\tNodeSecretHash string `json:"node_secret_hash"`')],
     LENSPKG, HASH_GUARD, LENSPKG, CHOOSE),

    ("C12", "the OWNER's shape loses its url — narrowing must not be allowed to spread",
     [(COMPUTE, '\tURL           string    `json:"url"`', '\tURL           string    `json:"-"`')],
     LENSPKG, OWNER, LENSPKG, CHOOSE),
]


def main():
    have_db = bool(os.getenv("LENS_TEST_DATABASE_URL"))
    if not have_db:
        print("⚠ LENS_TEST_DATABASE_URL is unset — C9/C10 drive real-PG tests that will SKIP.")
        print("  A skip is reported UNARMED, never counted as caught.\n")

    caught, missed, unarmed = [], [], []

    for cid, desc, edits, pkg, must_red, gpkg, must_green in CONTROLS:
        print(f"── {cid}: {desc}")
        originals = {}
        shas = {}
        ok = True
        for path, old, new in edits:
            if path not in originals:
                originals[path] = read(path)
                shas[path] = sha(path)
            # Count against the CURRENT contents, not the saved original: a control
            # with two edits to one file has already changed it by the second edit.
            cur = read(path)
            n = cur.count(old)
            if n != 1:
                print(f"   ✗ ANCHOR ERROR: {path} contains the anchor {n} times, want exactly 1")
                print(f"     anchor: {old[:90]!r}")
                ok = False
                break
            write(path, cur.replace(old, new, 1))
        if not ok:
            for path, s in originals.items():
                write(path, s)
            missed.append((cid, "anchor did not apply"))
            continue

        red_pass, red_skip = run_test(pkg, must_red)
        green_pass, green_skip = run_test(gpkg, must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if red_skip:
            print(f"   ⚠ UNARMED: {must_red} SKIPPED (needs a database) — nothing was tested")
            unarmed.append((cid, must_red))
            continue
        if not green_skip and not green_pass:
            print(f"   ✗ COMPANION RED: {must_green} also failed — the mutation broke the build, "
                  f"so '{must_red} failed' proves nothing")
            missed.append((cid, "companion red"))
            continue
        if red_pass:
            print(f"   ✗ MISSED: {must_red} still PASSES with the defect reintroduced")
            missed.append((cid, "guard blind"))
        else:
            print(f"   ✓ CAUGHT by {must_red} (companion {must_green} stayed green)")
            caught.append(cid)

    print()
    print(f"CAUGHT  {len(caught)}: {', '.join(caught) or '—'}")
    print(f"UNARMED {len(unarmed)}: {', '.join(c for c, _ in unarmed) or '—'}")
    print(f"MISSED  {len(missed)}: {', '.join(f'{c}({why})' for c, why in missed) or '—'}")
    if missed:
        sys.exit(1)
    if unarmed:
        print("\n⚠ exit 0 with UNARMED controls: re-run with LENS_TEST_DATABASE_URL to arm them.")


if __name__ == "__main__":
    main()
