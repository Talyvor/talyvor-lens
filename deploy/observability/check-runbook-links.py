#!/usr/bin/env python3
"""check-runbook-links.py — the by-hand network probe for the runbook links in alerts.yaml.

WHY THIS IS NOT A GO TEST. `go test` would then depend on github.com being reachable, and no
talyvor-lens CI job has network guarantees to it. So the split is the same one
`deploy/helm/lens/check-image-reach.py` uses for images:

  · cmd/lens/runbook_link_reach_test.go, OFFLINE, runs in CI: the URL prefix is a hardcoded
    literal and every target must exist in this repo.
  · THIS SCRIPT, by hand: the real HTTP probe that says whether that literal is still right.

CONTROLS, BOTH DIRECTIONS — a probe that reports 200 for everything is not a probe:

  · POSITIVE   each of the six live URLs must return 200.
  · NEGATIVE   a filename that does not exist, under the SAME repo, must return 404. Without this,
               a repo-level redirect or a soft-404 would make every row above meaningless.
  · NEGATIVE   the old `talyvor/lens` URL must still 404 — that is the defect this fixed. The `talyvor`
               organisation exists since 12 Oct 2026 (the repositories moved there), but it has no
               `lens` repository, so this row stays a 404.
  · NEGATIVE   `gaboracnicolai/lens` — the ORG-ONLY correction — must 404 too. It was written down
               that only the org was wrong. It was not: the repository is `talyvor-lens`, and a
               half-correction is another 404 one path segment later.

Usage:  python3 deploy/observability/check-runbook-links.py
Exit 0 iff every row matches its expected status.
"""

import pathlib
import re
import sys
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
ALERTS = ROOT / "deploy/observability/prometheus/alerts.yaml"
REAL = "https://github.com/Talyvor/talyvor-lens/blob/main/deploy/observability/runbooks/"


def status(url):
    req = urllib.request.Request(url, method="HEAD", headers={"User-Agent": "talyvor-runbook-probe"})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception as e:                                  # noqa: BLE001 — a network error is data
        return f"ERR {type(e).__name__}"


def main():
    urls = re.findall(r'^\s*runbook_url:\s*"([^"]+)"', ALERTS.read_text(), re.M)
    if not urls:
        print(f"no runbook_url found in {ALERTS} — the probe read nothing, which is not a clean result")
        return 1

    rows = [(u, 200, "live link in alerts.yaml") for u in urls]
    rows += [
        (REAL + "NoSuchRunbook.md", 404,
         "control: an absent filename under the SAME repo must 404, or the 200s above mean nothing"),
        ("https://github.com/talyvor/lens/blob/main/deploy/observability/runbooks/LensHighErrorRate.md",
         404, "control: the old link — the defect that was fixed (the org exists now; `lens` does not)"),
        ("https://github.com/gaboracnicolai/lens/blob/main/deploy/observability/runbooks/LensHighErrorRate.md",
         404, "control: the ORG-ONLY correction — still 404, the repo is `talyvor-lens` not `lens`"),
    ]

    ok = True
    print(f"{len(urls)} runbook_url values read from {ALERTS.relative_to(ROOT)}\n")
    for url, want, why in rows:
        got = status(url)
        good = got == want
        ok &= good
        print(f"[{'ok ' if good else 'BAD'}] {str(got):>4} (want {want})  {url}")
        print(f"       {why}")
    print("\nRESULT:", "every link and every control behaved" if ok else "SOMETHING IS WRONG")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
