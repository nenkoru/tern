#!/usr/bin/env bash
# Publishes a draft release (.github/workflows/release.yml makes it) only when
# it is signed: its SHA256SUMS and SHA256SUMS-ops carry at least the threshold
# of signatures of releases/signers.json, checked by a ternchain built from
# this checkout (docs/SUPPLY-CHAIN.md, "Signer onboarding"). And it refuses a
# signer set with no signers, so no public release goes out unsigned by
# accident: install.sh and ternchain would refuse it anyway, as UNSIGNED.
#
#   scripts/publish.sh --signers-only [SIGNERS.json]   only the signer-set check (release.yml runs it before a draft)
#   scripts/publish.sh TAG                             check the draft TAG's signatures (gh), then publish it
#
#   RELEASE_ALLOW_UNSIGNED=1    allow threshold 0 (a test release nobody signed; its users must opt in to install it)
#   RELEASE_ALLOW_ONE_SIGNER=1  allow threshold 1 (one person's key is the whole defence; the first real set needs 2 or more)
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "publish: $*" >&2; exit 1; }

# signer_gate FILE: the set's threshold is 2 or more, or an override says otherwise.
signer_gate() {
  [ -f "$1" ] || die "no signer set $1"
  local th
  th="$(tr -d ' \t\r\n' < "$1" | sed -n 's/.*"threshold":\([0-9][0-9]*\).*/\1/p')"
  [ -n "$th" ] || die "$1 names no threshold"
  if [ "$th" -eq 0 ]; then
    [ "${RELEASE_ALLOW_UNSIGNED:-}" = 1 ] ||
      die "$1 has no signers (threshold 0): a release would go out UNSIGNED. Add the signers first (docs/SUPPLY-CHAIN.md, \"Signer onboarding\"), or RELEASE_ALLOW_UNSIGNED=1 for a test release nobody signed"
    echo "warning: RELEASE_ALLOW_UNSIGNED=1: threshold 0, this release is UNSIGNED" >&2
  elif [ "$th" -eq 1 ]; then
    [ "${RELEASE_ALLOW_ONE_SIGNER:-}" = 1 ] ||
      die "$1 has threshold 1: one signer's key alone could sign a backdoored release. Use 2 or more (2 of 3 keys on separate devices), or RELEASE_ALLOW_ONE_SIGNER=1"
    echo "warning: RELEASE_ALLOW_ONE_SIGNER=1: threshold 1" >&2
  fi
  echo "signer set $1: threshold $th" >&2
}

if [ "${1:-}" = --signers-only ]; then
  signer_gate "${2:-releases/signers.json}"
  exit 0
fi
tag="${1:?usage: scripts/publish.sh TAG | --signers-only [SIGNERS.json]}"
command -v gh >/dev/null || die "needs the GitHub CLI (gh)"
[ "$(git rev-parse "$tag^{commit}")" = "$(git rev-parse HEAD)" ] ||
  die "check out $tag first (the signer set and the verifier come from the tag's commit)"
[ -z "$(git status --porcelain --untracked-files=no)" ] || die "the tree has uncommitted changes"
signer_gate releases/signers.json
[ "$(gh release view "$tag" --json isDraft -q .isDraft)" = true ] || die "$tag is not a draft release"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/rel"
for f in SHA256SUMS SHA256SUMS.sigs SHA256SUMS-ops SHA256SUMS-ops.sigs; do
  gh release download "$tag" -D "$tmp/rel" -p "$f" 2>/dev/null || true
done
[ -f "$tmp/rel/SHA256SUMS" ] || die "draft $tag has no SHA256SUMS"
go build -o "$tmp/ternchain" ./cmd/ternchain
allow=()
[ "${RELEASE_ALLOW_UNSIGNED:-}" = 1 ] && allow=(--allow-unsigned)
for f in SHA256SUMS SHA256SUMS-ops; do
  [ -f "$tmp/rel/$f" ] || continue
  # A fresh pin: checked against this commit's releases/signers.json (built into the verifier).
  TERNCHAIN_SIGNERS_PIN="$tmp/pin.json" "$tmp/ternchain" release verify "$tmp/rel/$f" "${allow[@]+"${allow[@]}"}" ||
    die "$f of draft $tag is not signed by the threshold (above): ask the signers to rebuild and sign it; not published"
  rm -f "$tmp/pin.json"
done
gh release edit "$tag" --draft=false
echo "published $tag" >&2
