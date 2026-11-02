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
# The build is reproducible (docs/SUPPLY-CHAIN.md): the same commit and VERSION
# give byte-identical binaries on any machine. Nothing in them depends on the
# clock, the path or the builder's environment:
#   - the build date is the commit's time (SOURCE_DATE_EPOCH when set, else
#     'git log -1 --format=%cd' of HEAD, in UTC), not now;
#   - -trimpath (no local paths), -buildvcs=false (the commit comes from
#     -ldflags, so a tarball and a checkout build the same bytes), CGO_ENABLED=0;
#   - the Go toolchain is pinned: go.mod's "toolchain" line, else its "go" line
#     (GOTOOLCHAIN=goX.Y.Z: the go command fetches exactly that release, checked
#     against the Go checksum database, if the local one differs), and the
#     script refuses to build with any other;
#   - GOFLAGS, GOAMD64, GOARM64, GOEXPERIMENT and friends are reset to the defaults;
#   - the module cache is always /tmp/ternchain-release-gomodcache (GOMODCACHE): -trimpath
#     doesn't rewrite the path of an assembly file a dependency #includes from
#     another folder (gnark-crypto does), so that path ends up in the binary and
#     must be the same for everyone; 'go mod verify' checks it against go.sum
#     before every build; RELEASE_FRESH_MODCACHE=1 empties it first; the build
#     cache is one of its own (RELEASE_GOCACHE), and a binary that still holds a
#     local path (the checkout's, or another module cache's) is refused;
#   - a tree with uncommitted changes is refused (its bytes match no commit),
#     unless RELEASE_ALLOW_DIRTY=1 (the commit is then stamped "-dirty").
#
#   scripts/release.sh [VERSION] [OUT-DIR]     default: VERSION=dev-YYYYMMDD (the commit's day), OUT-DIR=dist/VERSION
#
#   TARGETS="linux/amd64 darwin/arm64"  only these OS/CPU pairs (default: all six)
#   BINS="ternchain"                    only these binaries (default: ternchain ternchain-ops)
#   COMMIT=abc123                       the commit to stamp (default: HEAD, 12 hex digits)
#
# Then publish OUT-DIR's files with the public net.json, START.md, FOR-AGENTS.md
# and TERMS.md (never the operator file: the script refuses to put a file that
# holds a faucet key next to them). Independent signers rebuild the same VERSION
# with scripts/repro.sh, compare, and sign SHA256SUMS (ternchain release sign).
set -euo pipefail
cd "$(dirname "$0")/.."

# The module cache: one fixed path for every builder (see above), ours alone.
modcache=/tmp/ternchain-release-gomodcache
if [ "${RELEASE_FRESH_MODCACHE:-}" = 1 ] && [ -d "$modcache" ]; then
  chmod -R u+w "$modcache" && rm -rf "${modcache:?}"
fi
mkdir -p "$modcache" && chmod 700 "$modcache"
if [ ! -O "$modcache" ]; then
  echo "refusing: $modcache belongs to another user (it could hold planted modules); remove it" >&2
  exit 1
fi
export GOMODCACHE="$modcache"
# The build cache, used by release builds only: an object compiled elsewhere with
# another module cache carries that path (above), and a shared cache would hand
# it back (with the builder's home folder in it). RELEASE_GOCACHE=DIR: another one.
export GOCACHE="${RELEASE_GOCACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/ternchain-release-gocache}"

# The pinned toolchain.
pinned="$(awk '$1=="toolchain"{print $2; exit}' go.mod)"
[ -n "$pinned" ] || pinned="go$(awk '$1=="go"{print $2; exit}' go.mod)"
export GOTOOLCHAIN="$pinned"
have="$(go env GOVERSION)"
if [ "$have" != "$pinned" ]; then
  echo "refusing: the go command runs $have, the pinned toolchain is $pinned (go.mod); set GOTOOLCHAIN=$pinned or install it" >&2
  exit 1
fi
# Anything in the environment that changes the compiler's output, back to the defaults.
unset GOFLAGS GOAMD64 GOARM GOARM64 GO386 GOMIPS GOMIPS64 GOPPC64 GORISCV64 GOWASM GOEXPERIMENT GOFIPS140 CGO_CFLAGS CGO_LDFLAGS
export CGO_ENABLED=0

# Every module the build uses, downloaded and checked against go.sum.
go mod download
go mod verify

in_git=0
git rev-parse --git-dir >/dev/null 2>&1 && in_git=1

dirty=""
if [ "$in_git" = 1 ] && [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
  if [ "${RELEASE_ALLOW_DIRTY:-}" != 1 ]; then
    echo "refusing: the tree has uncommitted or untracked changes (git status): a release is built from a commit." >&2
    echo "Commit them, or set RELEASE_ALLOW_DIRTY=1 for a local build that matches no commit." >&2
    exit 1
  fi
  dirty="-dirty"
fi

commit="${COMMIT:-}"
if [ -z "$commit" ] && [ "$in_git" = 1 ]; then
  commit="$(git rev-parse --short=12 HEAD)$dirty"
fi

# The build date: the commit's time, never the clock.
if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
  built="$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ)"
elif [ "$in_git" = 1 ]; then
  built="$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD)"
else
  echo "refusing: not a git checkout and SOURCE_DATE_EPOCH is not set (the build date must come from the source)" >&2
  exit 1
fi

version="${1:-dev-$(echo "$built" | cut -c1-10 | tr -d -)}"
out="${2:-dist/$version}"
ldflags="-s -w -buildid= -X main.version=$version -X main.commit=${commit:-none} -X main.buildDate=$built"

targets="${TARGETS:-darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64}"
bins="${BINS:-ternchain ternchain-ops}"

mkdir -p "$out"
built_files=()
for target in $targets; do
  os="${target%/*}" arch="${target#*/}"
  for bin in $bins; do
    # ternchain-ops does not build for windows (scale_sim.go uses Setpgid/syscall.Kill): operators run
    # linux or macOS. Ship the people's binary only there until the Go side gets a windows variant.
    if [ "$bin" = ternchain-ops ] && [ "$os" = windows ]; then echo "skipping $bin for $os/$arch (unix-only)"; continue; fi
    name="$bin-$version-$os-$arch"
    [ "$os" = windows ] && name="$name.exe"
    tags=()
    [ "$bin" = ternchain-ops ] && tags=(-tags ops)
    echo "building $name"
    GOOS="$os" GOARCH="$arch" go build "${tags[@]+"${tags[@]}"}" -trimpath -buildvcs=false -ldflags "$ldflags" -o "$out/$name" ./cmd/ternchain
    for leak in "$PWD/" /pkg/mod/; do
      if LC_ALL=C grep -q -F -- "$leak" "$out/$name"; then
        echo "refusing: $name holds a local path ($leak…): it would not reproduce elsewhere" >&2
        exit 1
      fi
    done
    built_files+=("$name")
  done
done

if grep -l faucet_key "$out"/* 2>/dev/null | grep -v '/ternchain-' ; then
  echo "refusing: a file in $out holds a faucet key (the operator file must never be published)" >&2
  exit 1
fi

# The signer set at this commit travels with the release ('ternchain release verify DIR' reads it; it is
# self-authenticating: a new set counts only when the previous set's signers signed it).
[ -f releases/signers.json ] && cp releases/signers.json "$out/signers.json"
( cd "$out"
  if command -v sha256sum >/dev/null; then sha256sum "${built_files[@]}" > SHA256SUMS; else shasum -a 256 "${built_files[@]}" > SHA256SUMS; fi )
echo "built $version ($commit, $built, $pinned) into $out:"
cat "$out/SHA256SUMS"
