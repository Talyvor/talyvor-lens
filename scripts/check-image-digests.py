#!/usr/bin/env python3
"""Every image in every compose file is pinned by tag AND digest (B37.6).

A floating tag (caddy:2-alpine, redis:7-alpine) changes under the server at the next pull with no
review. A tag alone is not a pin: a publisher can re-point it. The digest is the pin; the tag next
to it says which release the digest is, so a reviewer can read a bump.

Exempt: Talyvor's own images (ghcr.io/talyvor/...; ghcr.io/gaboracnicolai/... until the repositories moved
to the Talyvor organisation on 12 Oct 2026). This repository's pipeline builds them and the deploy
pulls :latest after every green merge, so a digest there would stop deploys.

To bump an image: `docker buildx imagetools inspect <name>:<new-tag> --format '{{.Manifest.Digest}}'`
and write `<name>:<new-tag>@<digest>`.
"""
import re
import subprocess
import sys

OWN = "ghcr.io/talyvor/"
IMAGE_LINE = re.compile(r"^\s*image:\s*[\"']?([^\"'\s#]+)", re.M)
PINNED = re.compile(r"^[^@\s]+:[\w][\w.-]*@sha256:[0-9a-f]{64}$")


def unpinned(text):
    return [i for i in IMAGE_LINE.findall(text) if not i.startswith(OWN) and not PINNED.match(i)]


# The check must go red on a floating tag, a tag without a digest, and a digest without a tag.
assert unpinned("    image: caddy:2-alpine\n") == ["caddy:2-alpine"]
assert unpinned("  image: 'redis:7.4.11-alpine'\n") == ["redis:7.4.11-alpine"]
assert unpinned("image: nats@sha256:" + "a" * 64) == ["nats@sha256:" + "a" * 64]
assert unpinned("image: nats:2.10.29-alpine@sha256:" + "a" * 64 + "\nimage: " + OWN + "x:latest") == []

files = [f for f in subprocess.run(["git", "ls-files"], capture_output=True, text=True, check=True).stdout.split()
         if re.search(r"(^|/)(docker-)?compose[^/]*\.ya?ml$", f)]
if not files:
    sys.exit("check-image-digests: no compose files found — run from the repository root")

bad = [(f, i) for f in files for i in unpinned(open(f).read())]
for f, i in bad:
    print(f"{f}: image {i} is not pinned by tag and digest (name:tag@sha256:...)")
print(f"check-image-digests: {len(files)} compose files, {len(bad)} unpinned images")
sys.exit(1 if bad else 0)
