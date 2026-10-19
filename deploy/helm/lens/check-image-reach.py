#!/usr/bin/env python3
"""Does every image this chart renders resolve to an ANONYMOUS puller?

That is what a self-host evaluator running `helm install` is, and until 2026-08-10 the answer for
the default render was NO: the chart named `ghcr.io/talyvor/talyvor-lens:0.1.0`, an organisation
that did not exist on GitHub at all. (It does since 12 Oct 2026: the repositories moved there and the
image is published as ghcr.io/talyvor/talyvor-lens.)

⚠ RUN THIS BY HAND, NOT IN CI. It needs `helm` (installed in no talyvor-lens CI job) and it talks
to a live registry, which would make `go test` fail whenever GHCR has a bad day. The offline half
of this guard — values.yaml / Chart.yaml / deploy/k8s/manifests.yaml cross-checked against
.github/workflows/images.yaml — lives in cmd/lens/chart_image_reach_test.go and DOES run in CI.

    python3 deploy/helm/lens/check-image-reach.py               # default render
    python3 deploy/helm/lens/check-image-reach.py --all-opt-ins # + nodes, backup, pgbouncer

⚠ CONTROLLED IN BOTH DIRECTIONS, EVERY RUN, BEFORE ANY ROW IS BELIEVED. A probe that denied
everything would "prove" the chart broken; one that accepted everything would "prove" it fine. So
a known-public image must PASS and a known-absent one must FAIL on the same instrument in the same
run, and the script exits non-zero if a control misbehaves — even when every chart row looks good.

⚠ THE TWO FAILURE MODES ARE DIFFERENT AND THIS SCRIPT KEEPS THEM APART:
    token endpoint 403  — the REPOSITORY is not anonymously pullable (private, or no such owner)
    manifest 404        — the repository IS public and THAT TAG was never published
The chart's old default failed the first way. Correcting only the organisation would have failed
the second way, and a probe reporting one boolean would have called that a fix.
"""
import argparse
import json
import re
import subprocess
import sys
import urllib.error
import urllib.request

ACCEPT = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])

# Docker Hub does not serve its own token endpoint: auth lives on auth.docker.io and the registry
# on registry-1.docker.io, and a bare name like `postgres` means `library/postgres`. An earlier
# measurement stopped at "my probe does not implement Docker Hub", which left the backup CronJob's
# image (postgres:16) and pgbouncer unmeasured rather than measured-and-fine.
REGISTRIES = {
    "ghcr.io": {"auth": "https://ghcr.io/token?service=ghcr.io&scope=repository:{repo}:pull",
                "api": "https://ghcr.io/v2/{repo}/manifests/{tag}"},
    "docker.io": {"auth": "https://auth.docker.io/token?service=registry.docker.io&scope=repository:{repo}:pull",
                  "api": "https://registry-1.docker.io/v2/{repo}/manifests/{tag}"},
}


def split_ref(ref):
    """`ghcr.io/o/n:t` / `postgres:16` / `bitnami/pgbouncer:1` -> (host, repo, tag)."""
    # ⚠ The first segment is a REGISTRY HOST only when there is a `/` after it. `postgres:16` has a
    # colon and no slash, and reading that colon as a port made this function crash on the backup
    # image the first time it ran — the one reference nobody had probed before.
    head = ref.split("/")[0]
    if "/" in ref and ("." in head or ":" in head or head == "localhost"):
        host, rest = ref.split("/", 1)
    else:
        host, rest = "docker.io", ref
    if "@" in rest:  # `repo:tag@sha256:…` — the digest is what gets pulled, so probe that
        repo, tag = rest.split("@", 1)
        repo = repo.rsplit(":", 1)[0] if ":" in repo.split("/")[-1] else repo
    elif ":" in rest.split("/")[-1]:
        repo, tag = rest.rsplit(":", 1)
    else:
        repo, tag = rest, "latest"
    if host == "docker.io" and "/" not in repo:
        repo = "library/" + repo
    return host, repo, tag


def resolves(ref):
    host, repo, tag = split_ref(ref)
    reg = REGISTRIES.get(host)
    if reg is None:
        return None, f"no probe implemented for registry {host!r}"
    try:
        req = urllib.request.Request(reg["auth"].format(repo=repo))
        with urllib.request.urlopen(req, timeout=30) as r:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected -- URL is a fixed https registry template from REGISTRIES
            token = json.loads(r.read()).get("token")
    except urllib.error.HTTPError as e:
        return False, f"token endpoint HTTP {e.code}"
    except Exception as e:                                          # noqa: BLE001
        return False, f"token endpoint {type(e).__name__}"
    try:
        req = urllib.request.Request(reg["api"].format(repo=repo, tag=tag),
                                     headers={"Authorization": f"Bearer {token}", "Accept": ACCEPT})
        with urllib.request.urlopen(req, timeout=30) as r:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected -- URL is a fixed https registry template from REGISTRIES
            return r.status == 200, f"manifest HTTP {r.status}"
    except urllib.error.HTTPError as e:
        return False, f"manifest HTTP {e.code}"
    except Exception as e:                                          # noqa: BLE001
        return False, f"manifest {type(e).__name__}"


CONTROLS = [
    ("ghcr.io/gitleaks/gitleaks:latest", True),
    ("ghcr.io/gitleaks/gitleaks:v8.30.1", True),
    ("docker.io/library/postgres:16", True),          # the Docker Hub path must work too
    ("postgres:16@sha256:" + "0" * 64, False),        # a digest must be probed, not a tag  -> 404
    ("ghcr.io/gitleaks/gitleaks:no-such-tag-9f3a2b", False),   # public repo, absent tag -> 404
    ("ghcr.io/talyvor/no-such-image-9f3a2b:latest", False),    # absent image        -> not pullable
    # ⚠ A BADLY CHOSEN CONTROL IS NOT A BROKEN PROBE. An earlier run used
    # ghcr.io/oras-project/oras:latest as a must-PASS row; it returned manifest 404 because that
    # repository has no `latest`, which reads as "the instrument denies everything". Controls are
    # pinned to references that were themselves verified, and that is why this list is explicit.
]

CHART = "deploy/helm/lens"
OPT_INS = ["--set", "nodes.enabled=true", "--set", "backup.enabled=true",
           "--set", "pgbouncer.enabled=true", "--set", "pgbouncer.postgresHost=pg.example"]


def rendered_images(all_opt_ins):
    cmd = ["helm", "template", "lens", CHART] + (OPT_INS if all_opt_ins else [])
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    except FileNotFoundError:
        sys.exit("helm not found — this script is by-hand precisely because CI has no helm")
    except subprocess.CalledProcessError as e:
        sys.exit(f"helm template failed:\n{e.stderr}")
    # ⚠ Count occurrences rather than de-duplicating: the default render emits the SAME image
    # twice (Deployment + migrate Job) and a fix that repaired only one of them must not be able
    # to hide behind a collapsed set.
    return re.findall(r'^\s*image:\s*"?([^"\s]+)"?\s*$', out, re.M)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--all-opt-ins", action="store_true",
                    help="also enable nodes/backup/pgbouncer. ⚠ The node images are PLACEHOLDERS "
                         "(nothing publishes them) — they are EXPECTED to be denied; see values.yaml.")
    args = ap.parse_args()

    bad_control = False
    print("── controls (the instrument must be trustworthy before any chart row counts) ──")
    for ref, expect in CONTROLS:
        ok, detail = resolves(ref)
        note = ""
        if ok is not expect:
            note, bad_control = "   !! CONTROL MISBEHAVED", True
        print(f"  want {'PASS' if expect else 'FAIL'}  {'RESOLVES' if ok else 'DENIED  '}  {ref:46s} {detail}{note}")

    images = rendered_images(args.all_opt_ins)
    print(f"\n── {len(images)} image reference(s) rendered by `helm template` ──")
    denied = []
    for ref in images:
        ok, detail = resolves(ref)
        if not ok:
            denied.append(ref)
        print(f"  {'RESOLVES' if ok else 'DENIED  '}  {ref:46s} {detail}")

    if bad_control:
        print("\nINSTRUMENT NOT TRUSTWORTHY — a control misbehaved. Every row above is unusable.")
        return 2
    if denied:
        print(f"\n{len(denied)} reference(s) an anonymous puller cannot fetch: {', '.join(sorted(set(denied)))}")
        print("With DEFAULT values this must be empty — it is what a self-host evaluator hits first.")
        return 1
    print("\nEvery rendered image resolves anonymously, and both controls behaved.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
