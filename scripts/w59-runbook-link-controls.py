#!/usr/bin/env python3
"""w59-runbook-link-controls.py — positive controls for cmd/lens/runbook_link_reach_test.go.

A guard that passes on its first run is a guard nobody has proved can fail. These controls mutate
the tree one edit at a time and require the PREDICTED assertion to be the one that fires — not
merely "something went red", because a crash, a compile error and a real catch all look identical
in a list of test names.

Each control names its catcher as a falsifiable claim. A control caught by the *wrong* assertion is
recorded as a MISPREDICTION and is a finding about the guard, not about the product.

Run from the repo root:  python3 scripts/w59-runbook-link-controls.py
Exit 0 iff every control produced its predicted verdict.
"""

import hashlib
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
ALERTS = ROOT / "deploy/observability/prometheus/alerts.yaml"
VALUES = ROOT / "deploy/helm/lens/values.yaml"
GUARD = ROOT / "cmd/lens/runbook_link_reach_test.go"

TESTS = "TestAlertRunbookURLsResolve|TestNoDeadOrgURLInShippedYAML"

GOOD = "https://github.com/Talyvor/talyvor-lens/blob/main/deploy/observability/runbooks/"


def run_guard():
    """Return (passed, combined_output). -v so a PASS is visible; absence from a failure list is
    not green, and a panic would otherwise read as 'no failures'."""
    p = subprocess.run(
        ["go", "test", "./cmd/lens/", "-run", TESTS, "-v", "-count=1"],
        cwd=ROOT, capture_output=True, text=True,
    )
    return p.returncode == 0, p.stdout + p.stderr


def which_fired(out):
    """The set of assertion ids that actually printed a failure. Reading verdicts from test NAMES
    would make a crash and a real catch look identical; these are the assertion messages."""
    return {m for m in re.findall(r"\b(R1a|R1b|R1c|R1d|R2)\b", out) if f"{m}:" in out}


class Mutation:
    """Applies edits under a context manager and restores in a finally — a crash between mutate and
    restore must not leave a broken tree on disk, and the closing digest check must always run."""

    def __init__(self, edits, hide=None):
        self.edits = edits          # list of (path, old, new)
        self.hide = hide            # a file to move aside for the duration
        self.originals = {}
        self.hidden = None

    def __enter__(self):
        # Every anchor is asserted BEFORE any write: a half-applied control leaves the tree
        # mutated and the verdict meaningless.
        for path, old, _ in self.edits:
            n = path.read_text().count(old)
            if n != 1:
                raise AssertionError(
                    f"anchor appears {n}x (want 1) in {path.relative_to(ROOT)}: {old[:70]!r}"
                )
        for path, old, new in self.edits:
            if path not in self.originals:
                self.originals[path] = path.read_text()
            path.write_text(path.read_text().replace(old, new, 1))
        # Prove every edit landed: two edits in one file are the case where the second write
        # silently erases the first.
        for path, _, new in self.edits:
            assert new in path.read_text(), f"edit did not land in {path}"
        if self.hide:
            if not self.hide.exists():
                raise AssertionError(f"cannot hide a file that is not there: {self.hide}")
            self.hidden = self.hide.with_suffix(self.hide.suffix + ".control-hidden")
            self.hide.rename(self.hidden)
            assert not self.hide.exists(), "hide did not take effect"
        return self

    def __exit__(self, *exc):
        if self.hidden and self.hidden.exists():
            self.hidden.rename(self.hide)
        for path, body in self.originals.items():
            path.write_text(body)
        return False


def digest():
    """Everything a control may touch, including the runbooks directory LISTING — C1 hides a file
    rather than editing one, and a content-only digest would call that tree restored."""
    d = {
        p.relative_to(ROOT).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
        for p in (ALERTS, VALUES, GUARD)
    }
    d["deploy/observability/runbooks/ (listing)"] = ",".join(
        sorted(p.name for p in (ROOT / "deploy/observability/runbooks").iterdir())
    )
    return d


# ── the controls ──────────────────────────────────────────────────────────────────────────────
# `expect` is the EXACT set of assertions that must fire. An empty set means must-stay-green.
CONTROLS = [
    dict(
        id="C0", must_pass=True, expect=set(),
        why="baseline: the unmutated tree is green. Without this, every red below is unreadable.",
        edits=[],
    ),
    dict(
        id="C1", must_pass=False, expect={"R1b"},
        why="the RUNBOOK FILE deleted, alerts.yaml untouched. Only R1b can see this: the prefix is "
            "intact (R1a green), the alert set is intact (R1c green), the URL still names its own "
            "alert (R1d green), the org is right (R2 green). Deleting the target rather than "
            "editing the URL is what makes it isolate — an edited URL trips R1d as well.",
        edits=[], hide=ROOT / "deploy/observability/runbooks/LensProviderDown.md",
    ),
    dict(
        id="C2", must_pass=False, expect={"R1a"},
        why="the HALF-CORRECTION: right org, wrong repo (`Talyvor/lens`, measured 404). "
            "ONLY R1a. R2 must NOT fire — R2 watches the pre-move owner and this is not it, so this "
            "is the mutation R2 cannot claim credit for. R1b and R1d are not reached: a wrong prefix "
            "makes the filename questions unanswerable, so R1a `continue`s past them by design.",
        edits=[(ALERTS,
                GOOD + "LensRateLimitSpike.md",
                "https://github.com/Talyvor/lens/blob/main/deploy/observability/runbooks/LensRateLimitSpike.md")],
    ),
    dict(
        id="C2b", must_pass=False, expect={"R1d"},
        why="an alert pointed at ANOTHER alert's runbook. The URL is 200 — right prefix, real file "
            "— so R1a and R1b are green and the link reads healthy while the on-call engineer "
            "opens the wrong document. Only R1d can see it, and R1d exists because C1's first "
            "draft could not be isolated without it.",
        edits=[(ALERTS, GOOD + "LensProviderDown.md", GOOD + "LensHighErrorRate.md")],
    ),
    dict(
        id="C3", must_pass=False, expect={"R2"},
        why="the pre-move address resurfacing in a DIFFERENT yaml file, as a value. Only R2 can see "
            "this: R1 reads alerts.yaml alone. This is the control that justifies R2 existing at all.",
        edits=[(VALUES, "\nnameOverride:", "\nsupportURL: https://github.com/gaboracnicolai/talyvor-lens\nnameOverride:")],
    ),
    dict(
        id="C4", must_pass=False, expect={"R1c"},
        why="a whole alert DELETED. Every surviving link still checks out, so R1a, R1b and R1d "
            "stay green — that is precisely why the pinned set exists. A guard derived only from "
            "the file it reads cannot see what was removed from it.",
        edits=[(ALERTS,
                '      - alert: LensRateLimitSpike\n'
                '        expr: sum(rate(lens_rate_limit_rejections_total[5m])) > 1\n'
                '        for: 10m\n'
                '        labels:\n          severity: warning\n'
                '        annotations:\n'
                '          summary: "Rate-limit rejections climbing"\n'
                '          description: "Lens is rejecting {{ $value | humanize }} req/s with HTTP 429'
                ' (sustained >10m). A tenant may be over budget or misbehaving."\n'
                '          runbook_url: "' + GOOD + 'LensRateLimitSpike.md"\n\n',
                '')],
    ),
    dict(
        id="C5", must_pass=False, expect={"R1c"},
        why="an alert ADDED with no runbook_url. R1a/R1b are green about the six that remain and "
            "say nothing about the seventh shipping with no runbook at all.",
        edits=[(ALERTS,
                "  - name: lens-alerts\n    rules:\n",
                "  - name: lens-alerts\n    rules:\n"
                "      - alert: LensQueueBacklog\n        expr: vector(0)\n"
                "        labels:\n          severity: warning\n"
                "        annotations:\n          summary: \"no runbook\"\n\n")],
    ),
    dict(
        id="C6", must_pass=False, expect={"R2"},
        why="THE COMMENT RULE IS LOAD-BEARING, NOT DECORATION. Delete R2's `#` skip and it goes "
            "red on an UNMUTATED tree — on Chart.yaml's deliberate explanatory comment, which quotes "
            "the pre-move address. An "
            "exemption nobody can make fire is indistinguishable from one that does nothing.",
        edits=[(GUARD,
                'if strings.HasPrefix(strings.TrimSpace(ln), "#") {',
                'if false && strings.HasPrefix(strings.TrimSpace(ln), "#") {')],
    ),
    dict(
        id="C7", must_pass=False, expect={"R1a", "R2"},
        why="the PRE-MOVE address restored in alerts.yaml (gaboracnicolai/talyvor-lens, a redirect "
            "since 12 Oct 2026). Both guards must see it: R1a because it is not the canonical "
            "prefix, R2 because it names the old owner. R1b/R1d are not reached — see C2.",
        edits=[(ALERTS, GOOD + "LensInstanceDown.md",
                "https://github.com/gaboracnicolai/talyvor-lens/blob/main/deploy/observability/runbooks/LensInstanceDown.md")],
    ),
    dict(
        id="C7b", must_pass=False, expect={"R1a"},
        why="the ORIGINAL defect restored, verbatim (talyvor/lens). Since the Talyvor organisation "
            "exists, only R1a can see it: the org is right and the repository is not. This is the "
            "only control that reproduces what actually shipped in August.",
        edits=[(ALERTS, GOOD + "LensHighErrorRate.md",
                "https://github.com/talyvor/lens/blob/main/deploy/observability/runbooks/LensHighErrorRate.md")],
    ),
    dict(
        id="C8", must_pass=True, expect=set(),
        why="MUST-STAY-GREEN. A cosmetic edit to alerts.yaml that changes no link and no alert. If "
            "this went red the guards would be reacting to noise and every CAUGHT above would be "
            "worth less.",
        edits=[(ALERTS, "# Alerting rules for Lens.", "# Alerting rules for Lens (observability).")],
    ),
]


def main():
    before = digest()
    results = []
    for c in CONTROLS:
        try:
            with Mutation(c["edits"], hide=c.get("hide")):
                passed, out = run_guard()
        except AssertionError as e:
            results.append((c["id"], "BROKEN-CONTROL", str(e)))
            continue

        fired = which_fired(out)
        compiled = "PASS" in out or "FAIL" in out
        if not compiled or "build failed" in out or "[build failed]" in out:
            verdict, detail = "BROKEN-CONTROL", "the package did not build — a compile error is not a catch"
        elif c["must_pass"]:
            verdict = "GREEN" if passed else "REGRESSION"
            detail = "stayed green as required" if passed else f"went red: {sorted(fired)}"
        elif passed:
            verdict, detail = "NOT CAUGHT", "no assertion fired"
        elif fired == c["expect"]:
            verdict, detail = "CAUGHT", f"exactly {sorted(fired)} — as predicted"
        else:
            verdict = "MISPREDICTION"
            detail = f"predicted {sorted(c['expect'])}, got {sorted(fired)}"
        results.append((c["id"], verdict, detail))

    after = digest()
    ok = True
    print("\n=== W5.9 runbook-link controls ===\n")
    for cid, verdict, detail in results:
        c = next(x for x in CONTROLS if x["id"] == cid)
        good = verdict in ("CAUGHT", "GREEN")
        ok &= good
        print(f"[{'ok ' if good else 'BAD'}] {cid}  {verdict:<15} {detail}")
        print(f"       {c['why']}")
    if before != after:
        ok = False
        print("\n!! TREE NOT RESTORED — digests differ:")
        for k in before:
            if before[k] != after[k]:
                print(f"   {k}")
    else:
        print("\ntree restored: sha256 of all three touched files unchanged")
    print("\nRESULT:", "all controls behaved as predicted" if ok else "SOMETHING IS WRONG")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
