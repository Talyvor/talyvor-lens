#!/usr/bin/env bash
#
# secret-scan.sh [dir] — scan a working tree for committed secrets with gitleaks (B28.259).
#
# Runs gitleaks' default ruleset with this repo's .gitleaks.toml over every file in the tree
# (default: the repo root) and exits non-zero on any finding. Findings are printed redacted.
# The image is pinned by digest (v8.30.1, the pin talyvor-code uses) so a new release cannot
# change the verdict without a commit.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
src=$(cd "${1:-$root}" && pwd)

docker run --rm -v "$src:/scan:ro" -v "$root/.gitleaks.toml:/gitleaks.toml:ro" \
  ghcr.io/gitleaks/gitleaks@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f \
  detect --source=/scan --no-git --redact --verbose --no-banner --config=/gitleaks.toml
