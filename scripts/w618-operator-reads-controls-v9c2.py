#!/usr/bin/env python3
"""w618-operator-reads-controls-v9c2.py — positive controls for the two ungated operator reads.

⚠ THIS ITEM MERGES NO FIX. The reads stay reachable by any tenant, because closing that means
gating a documented capability and that is Nicolai's decision (W6.18). So most of what is
merged is a RECORD, and a record that cannot fail is worth nothing — these controls are what
make it a tripwire instead of a note.

K1/K2 are the ones that matter: they put provider KEY MATERIAL on the response, once under its
own name and once renamed, and require the guard to catch both.

Rules, as in the earlier campaigns here: anchor count asserted before editing; a companion test
that must stay green; sha256 re-checked after revert.

Run from the repo root:  python3 scripts/w618-operator-reads-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

POOL = "internal/keypool/pool.go"
MAIN = "cmd/lens/main.go"
TEST = "cmd/lens/operator_reads_handlers_test.go"

PKG = "./cmd/lens/"

DUMP_GUARD = "TestKeyPoolStats_NeverCarriesKeyMaterial"
RECORD = "TestKeyPoolStats_WhatATenantCanReadToday"
CHAINS = "TestFallbackChains_WhatATenantCanReadToday"
WIRED = "TestOperatorReadsGoThroughTheirNamedHandlers"
ASYM = "TestOperatorReadWriteAsymmetryIsStillWhatW618Recorded"

STATS_TAIL = '\tLastUsedAt   time.Time `json:"last_used_at"`\n}'
STATS_ASSIGN = "\t\t\t\tLastUsedAt:   k.LastUsedAt,"


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def write(path, s):
    with open(path, "w", encoding="utf-8") as f:
        f.write(s)


def run_test(name):
    r = subprocess.run(["go", "test", PKG, "-run", "^" + name + "$", "-count=1"],
                       capture_output=True, text=True)
    return r.returncode == 0


CONTROLS = [
    # ⚠ THE ONE THAT MATTERS. keypool.PoolKey holds the real provider key and carries NO json
    # tags; KeyStats omitting it is the entire boundary between operator telemetry and a
    # credential dump on a route any tenant can call.
    ("K1", "KeyStats grows a `key` field carrying the real provider credential",
     [(POOL, STATS_TAIL, '\tLastUsedAt   time.Time `json:"last_used_at"`\n\tKey          string    `json:"key"`\n}'),
      (POOL, STATS_ASSIGN, "\t\t\t\tLastUsedAt:   k.LastUsedAt,\n\t\t\t\tKey:          k.Key,")],
     DUMP_GUARD, CHAINS, "the credential dump, under its own name"),

    ("K2", "the same credential, shipped under an innocent name (`fingerprint`)",
     [(POOL, STATS_TAIL, '\tLastUsedAt   time.Time `json:"last_used_at"`\n\tFingerprint  string    `json:"fingerprint"`\n}'),
      (POOL, STATS_ASSIGN, "\t\t\t\tLastUsedAt:   k.LastUsedAt,\n\t\t\t\tFingerprint:  k.Key,")],
     DUMP_GUARD, CHAINS,
     "the guard asserts on the SECRET VALUE, not on a field name — a rename must not launder it"),

    ("K2b", "the same rename, seen by the exposure RECORD rather than by the secret tripwire",
     [(POOL, STATS_TAIL, '\tLastUsedAt   time.Time `json:"last_used_at"`\n\tFingerprint  string    `json:"fingerprint"`\n}'),
      (POOL, STATS_ASSIGN, "\t\t\t\tLastUsedAt:   k.LastUsedAt,\n\t\t\t\tFingerprint:  k.Key,")],
     RECORD, CHAINS, "a field nobody decided about appears on an ungated route"),

    # ⚠ RETAGGED, NOT DELETED, AND THE REASON IS THE W6.15 LESSON. Deleting the FIELD breaks the
    # only consumer of KeyStats.Alias — this file's own arming line — so the build fails and the
    # companion-green rule (correctly) refuses to score it. Deleting only the ASSIGNMENT would
    # leave the key on the wire as `"alias":""`, and a key-set record cannot see a zeroed value.
    # Retagging changes the KEY SET while leaving a compiling tree, which is precisely what this
    # control is for: it fires in BOTH directions at once — an undeclared key appears and a
    # recorded one vanishes.
    ("K3", "a recorded field is retagged — the record must notice a rename in both directions",
     [(POOL, '\tAlias        string    `json:"alias"`', '\tAlias        string    `json:"alias_v2"`')],
     RECORD, CHAINS, "both directions: the record is a census, not a ban-list"),

    ("K4", "the pool stops storing the key material — the fixture would arm nothing",
     [(POOL, "func (p *Pool) Add(provider, key, alias string, rateLimit int) (*PoolKey, error) {",
       "func (p *Pool) Add(provider, key, alias string, rateLimit int) (*PoolKey, error) {\n\tkey = \"\"")],
     DUMP_GUARD, CHAINS,
     "poolWithASecret asserts the key really is stored; a hollow fixture must fail there, "
     "not pass the absence check"),

    ("K5", "a read stops going through its named handler",
     [(MAIN, 'authed.Get("/v1/api/keys/pool", newKeyPoolStatsHandler(keyPool))',
       'authed.Get("/v1/api/keys/pool", func(w http.ResponseWriter, req *http.Request) {\n'
       '\t\t\twriteJSONOK(w, http.StatusOK, keyPool.Stats())\n'
       '\t\t})')],
     WIRED, DUMP_GUARD, "the handler can be perfect and unreached"),

    # ⚠ THE DECISION-TRACKING DIRECTION. If somebody gates the read — which is what W6.18 asks
    # Nicolai to decide — the record must fire and say "close the decision", not sit there
    # describing a world that no longer exists.
    ("K6", "the ungated read IS gated with requireAdmin — the record must fire, not go quiet",
     [(MAIN, 'authed.Get("/v1/api/keys/pool", newKeyPoolStatsHandler(keyPool))',
       'authed.Get("/v1/api/keys/pool", requireAdmin(authManager, newKeyPoolStatsHandler(keyPool)))')],
     ASYM, DUMP_GUARD, "a stale record is worse than no record on an authz surface"),

    ("K7", "a WRITE sibling loses its requireAdmin — the asymmetry the finding rests on changes",
     [(MAIN, 'authed.Delete("/v1/api/keys/pool/{keyID}", requireAdmin(authManager, http.HandlerFunc(newPoolKeyDeleteHandler(keyPool))))',
       'authed.Delete("/v1/api/keys/pool/{keyID}", http.HandlerFunc(newPoolKeyDeleteHandler(keyPool)))')],
     ASYM, DUMP_GUARD, "the finding is the asymmetry; if the writes move, the record is stale"),
]


def main():
    caught, missed = [], []
    for cid, desc, edits, must_red, must_green, note in CONTROLS:
        print(f"── {cid}: {desc}")
        if note:
            print(f"     ({note})")
        originals, shas = {}, {}
        ok = True
        for path, old, new in edits:
            if path not in originals:
                originals[path] = read(path)
                shas[path] = sha(path)
            cur = read(path)
            n = cur.count(old)
            if n != 1:
                print(f"   ✗ ANCHOR ERROR: {path} contains the anchor {n} times, want exactly 1")
                print(f"     anchor: {old[:110]!r}")
                ok = False
                break
            write(path, cur.replace(old, new, 1))
        if not ok:
            for path, s in originals.items():
                write(path, s)
            missed.append((cid, "anchor did not apply"))
            continue

        red_pass = run_test(must_red)
        green_pass = run_test(must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if not green_pass:
            print(f"   ✗ COMPANION RED: {must_green} also failed — the mutation broke the build")
            missed.append((cid, "companion red"))
            continue
        if red_pass:
            print(f"   ✗ MISSED: {must_red} still PASSES with the defect reintroduced")
            missed.append((cid, "guard blind"))
        else:
            print(f"   ✓ CAUGHT by {must_red} (companion {must_green} stayed green)")
            caught.append(cid)

    print()
    print(f"CAUGHT {len(caught)}: {', '.join(caught) or '—'}")
    print(f"MISSED {len(missed)}: {', '.join(f'{c}({why})' for c, why in missed) or '—'}")
    if missed:
        sys.exit(1)


if __name__ == "__main__":
    main()
