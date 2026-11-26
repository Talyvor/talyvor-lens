#!/usr/bin/env bash
# B28.256: every ghcr.io/talyvor/talyvor-lens image that images.yaml pushes carries a cosign
# signature, and anyone can check it before running the image.
#
#   bash scripts/image-signature.sh verify ghcr.io/talyvor/talyvor-lens:<sha>   (or …@sha256:<hex>)
#       passes only for an image signed by .github/workflows/images.yaml in Talyvor/talyvor-lens
#       on main. Needs cosign on PATH (the install command below puts the pinned one there).
#   bash scripts/image-signature.sh sign ghcr.io/talyvor/talyvor-lens@sha256:<hex>
#       images.yaml's sign job only: a digest, never a tag, because a tag can move between the
#       push and the signature.
#   bash scripts/image-signature.sh install <dir>
#       the pinned, checksum-verified cosign in <dir>.
#
# Keyless: there is no signing key to leak or rotate. cosign trades the job's OIDC token for a
# short-lived Fulcio certificate naming the workflow, the repository, the ref and the commit, and
# records the signature in the public Rekor log. The same signer as edge-infra's images
# (deploy/hack/supply-chain.sh there), so one Kyverno rule shape admits both.
set -euo pipefail

SIGNER_REPO="${SIGNER_REPO:-Talyvor/talyvor-lens}"
SIGNER_REF="${SIGNER_REF:-refs/heads/main}"
ISSUER=https://token.actions.githubusercontent.com
WORKFLOW=.github/workflows/images.yaml
COSIGN_VERSION=v2.6.5

die() { echo "image-signature: $*" >&2; exit 1; }
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

install_cosign() { # dir
	local dir="$1" os arch sum tmp
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	arch="$(uname -m)"
	case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
	# From cosign v2.6.5's own checksums file (the same pins as edge-infra).
	case "$os-$arch" in
	linux-amd64) sum=c3b4f5410e608af03a5eb0aaac84a4313d8da131248e08ff1759ac70c79d1644 ;;
	darwin-arm64) sum=4d41cc18f0563907c0c785b51db76e1d1af10db4422b605ba876b1758e1771ab ;;
	*) die "no pinned cosign for $os-$arch" ;;
	esac
	tmp="$(mktemp -d)"
	curl -fsSL -o "$tmp/cosign" "https://github.com/sigstore/cosign/releases/download/$COSIGN_VERSION/cosign-$os-$arch"
	[ "$(sha256 "$tmp/cosign")" = "$sum" ] || die "cosign $COSIGN_VERSION $os-$arch does not match its pinned sha256"
	mkdir -p "$dir"
	install -m 0755 "$tmp/cosign" "$dir/"
	rm -rf "$tmp"
	"$dir/cosign" version 2>&1 | grep '^GitVersion'
}

sign() { # image@sha256:<hex>
	[[ "$1" =~ ^[^@]+@sha256:[0-9a-f]{64}$ ]] || die "sign takes <image>@sha256:<hex>, not '$1'"
	cosign sign --yes "$1"
}

verify() { # image...
	[ "$#" -gt 0 ] || die "verify takes one or more images"
	# The repository is pinned on its own as well as in the identity, so a certificate another
	# repository got by calling this workflow cannot pass.
	local id=(--certificate-oidc-issuer "$ISSUER"
		--certificate-identity "https://github.com/$SIGNER_REPO/$WORKFLOW@$SIGNER_REF"
		--certificate-github-workflow-repository "$SIGNER_REPO")
	[ -z "${SIGNER_SHA:-}" ] || id+=(--certificate-github-workflow-sha "$SIGNER_SHA")
	local ref
	for ref in "$@"; do
		cosign verify "${id[@]}" "$ref" >/dev/null || die "$ref: no valid signature from $SIGNER_REPO/$WORKFLOW@$SIGNER_REF${SIGNER_SHA:+ at $SIGNER_SHA}"
		echo "$ref: signed by $SIGNER_REPO/$WORKFLOW@$SIGNER_REF${SIGNER_SHA:+ at $SIGNER_SHA}"
	done
}

cmd="${1:-}"
shift || true
case "$cmd" in
install) [ "$#" -eq 1 ] || die "install takes <dir>"; install_cosign "$1" ;;
sign) [ "$#" -eq 1 ] || die "sign takes <image>@sha256:<hex>"; sign "$1" ;;
verify) verify "$@" ;;
*) die "usage: $0 install <dir> | sign <image>@sha256:<hex> | verify <image>..." ;;
esac
