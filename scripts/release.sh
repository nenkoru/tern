#!/usr/bin/env bash
# Cross-builds ternchain (people and their agents) and ternchain-ops (operators
# and validators: devnet, faucet serve, deploy, validator, relay serve, ceremony;
# the same code built with -tags ops) for darwin, linux and windows (amd64 and
# arm64) with the version, commit and build date in the binary ('ternchain
# version'), and writes SHA256SUMS. Pure Go (CGO_ENABLED=0), so it runs on any
# machine with Go. An operator puts ternchain-ops next to ternchain (renamed to
# plain "ternchain-ops", or "ternchain-ops.exe"): ternchain forwards operator
# commands to it.
#
#   scripts/release.sh [VERSION] [OUT-DIR]     default: VERSION=dev-YYYYMMDD, OUT-DIR=dist/VERSION
#
# Then publish OUT-DIR's files with the public net.json, START.md, FOR-AGENTS.md
# and TERMS.md (never the operator file: the script refuses to put a file that
# holds a faucet key next to them).
set -euo pipefail
cd "$(dirname "$0")/.."

version="${1:-dev-$(date -u +%Y%m%d)}"
out="${2:-dist/$version}"
commit="${COMMIT:-}"
if [ -z "$commit" ] && git rev-parse --git-dir >/dev/null 2>&1; then
  commit="$(git rev-parse --short=12 HEAD)$(git diff --quiet || echo -dirty)"
fi
built="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ldflags="-s -w -X main.version=$version -X main.commit=${commit:-none} -X main.buildDate=$built"

mkdir -p "$out"
built_files=()
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${target%/*}" arch="${target#*/}"
  for bin in ternchain ternchain-ops; do
    # ternchain-ops does not build for windows (scale_sim.go uses Setpgid/syscall.Kill): operators run
    # linux or macOS. Ship the people's binary only there until the Go side gets a windows variant.
    if [ "$bin" = ternchain-ops ] && [ "$os" = windows ]; then echo "skipping $bin for $os/$arch (unix-only)"; continue; fi
    name="$bin-$version-$os-$arch"
    [ "$os" = windows ] && name="$name.exe"
    tags=()
    [ "$bin" = ternchain-ops ] && tags=(-tags ops)
    echo "building $name"
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build "${tags[@]+"${tags[@]}"}" -trimpath -ldflags "$ldflags" -o "$out/$name" .
    built_files+=("$name")
  done
done

if grep -l faucet_key "$out"/* 2>/dev/null | grep -v '/ternchain-' ; then
  echo "refusing: a file in $out holds a faucet key (the operator file must never be published)" >&2
  exit 1
fi

( cd "$out"
  if command -v sha256sum >/dev/null; then sha256sum "${built_files[@]}" > SHA256SUMS; else shasum -a 256 "${built_files[@]}" > SHA256SUMS; fi )
echo "built $version ($commit, $built) into $out:"
cat "$out/SHA256SUMS"
