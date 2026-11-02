#!/usr/bin/env bash
# Rebuilds a release and compares it with its published SHA256SUMS: anyone can
# check that the binaries are what the source at that commit builds to, without
# trusting whoever built or signed them (docs/SUPPLY-CHAIN.md).
#
#   scripts/repro.sh VERSION COMMIT SHA256SUMS|URL            rebuild in a temporary clone of this repository at COMMIT
#   scripts/repro.sh VERSION COMMIT SHA256SUMS|URL --in-tree  rebuild in this checkout (HEAD must be COMMIT, the tree clean)
#   scripts/repro.sh VERSION -      SHA256SUMS|URL --built DIR  no build: compare DIR's files (another build) with SHA256SUMS
#
# Prints match / MISMATCH / not rebuilt for every file SHA256SUMS lists, and
# exits nonzero on any mismatch (or when nothing could be compared).
#
#   TARGETS="linux/amd64"     rebuild only these OS/CPU pairs (default: every one SHA256SUMS lists)
#   BINS="ternchain"          rebuild only these binaries (default: every one SHA256SUMS lists)
#   REPRO_REPO=URL|PATH       clone this instead of the local repository (e.g. https://github.com/nenkoru/tern.git)
#   REPRO_FRESH_CACHE=1       a fresh build cache in the temporary folder, and the release module cache
#                             (/tmp/ternchain-release-gomodcache) emptied and downloaded again: nothing reused
#   REPRO_KEEP=1              keep the temporary folder (the rebuilt binaries) and print where it is
set -euo pipefail

if [ $# -lt 3 ]; then
  sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
  exit 2
fi
version="$1" commit="$2" sums_src="$3" mode="${4:-clone}"
root="$(cd "$(dirname "$0")/.." && pwd)"

tmp="$(mktemp -d "${TMPDIR:-/tmp}/ternchain-repro.XXXXXX")"
cleanup() {
  if [ "${REPRO_KEEP:-}" = 1 ]; then echo "kept $tmp"; return; fi
  chmod -R u+w "$tmp" 2>/dev/null || true # the module cache is read-only
  rm -rf "$tmp"
}
trap cleanup EXIT

published="$tmp/SHA256SUMS.published"
case "$sums_src" in
  http://*|https://*) curl -fsSL --proto '=https,http' "$sums_src" -o "$published" ;;
  *) cp "$sums_src" "$published" ;;
esac

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }

# What SHA256SUMS lists: NAME per line, "*" (binary mode) dropped.
names="$(awk 'NF==2{n=$2; sub(/^\*/,"",n); print n}' "$published")"
[ -n "$names" ] || { echo "$sums_src lists no files" >&2; exit 1; }

case "$mode" in
  --built)
    out="${5:?--built DIR}"
    ;;
  --in-tree|clone)
    if [ -z "${TARGETS:-}" ] || [ -z "${BINS:-}" ]; then
      t="" b=""
      for n in $names; do
        base="${n%.exe}"
        bin="${base%%-"$version"-*}"
        osarch="${base#*-"$version"-}"
        if [ "$bin" = "$base" ] || [ "$osarch" = "$base" ]; then echo "$n does not carry version $version" >&2; exit 1; fi
        t="$t ${osarch%-*}/${osarch##*-}"
        b="$b $bin"
      done
      TARGETS="${TARGETS:-$(echo $t | tr ' ' '\n' | sort -u | tr '\n' ' ')}"
      BINS="${BINS:-$(echo $b | tr ' ' '\n' | sort -u | tr '\n' ' ')}"
    fi
    export TARGETS BINS
    if [ "$mode" = --in-tree ]; then
      src="$root"
      head="$(git -C "$src" rev-parse HEAD)"
      want="$(git -C "$src" rev-parse "$commit^{commit}")"
      if [ "$head" != "$want" ]; then echo "--in-tree: HEAD is $head, not $commit ($want)" >&2; exit 1; fi
    else
      src="$tmp/src"
      git clone --quiet "${REPRO_REPO:-$root}" "$src"
      git -C "$src" checkout --quiet --detach "$commit"
    fi
    if [ "${REPRO_FRESH_CACHE:-}" = 1 ]; then
      export RELEASE_GOCACHE="$tmp/gocache" RELEASE_FRESH_MODCACHE=1
    fi
    if ! grep -q SOURCE_DATE_EPOCH "$src/scripts/release.sh"; then
      echo "note: release.sh at $commit predates reproducible builds (its build date is the clock): expect mismatches" >&2
    fi
    out="$tmp/out"
    echo "rebuilding $version at $(git -C "$src" rev-parse --short=12 HEAD) for $TARGETS ($BINS)"
    start=$(date +%s)
    "$src/scripts/release.sh" "$version" "$out" >"$tmp/build.log" 2>&1 || { cat "$tmp/build.log" >&2; exit 1; }
    echo "rebuilt in $(( $(date +%s) - start )) s with $(grep -o 'go1[0-9.]*' "$tmp/build.log" | tail -1)"
    ;;
  *)
    echo "unknown mode $mode (--in-tree, --built DIR)" >&2; exit 2 ;;
esac

match=0 mismatch=0 missing=0
while read -r want n; do
  [ -n "${n:-}" ] || continue
  n="${n#\*}"
  if [ ! -f "$out/$n" ]; then
    echo "not rebuilt  $n"; missing=$((missing+1)); continue
  fi
  got="$(sha256 "$out/$n")"
  if [ "$got" = "$want" ]; then
    echo "match        $n"; match=$((match+1))
  else
    echo "MISMATCH     $n  (published $want, rebuilt $got)"; mismatch=$((mismatch+1))
  fi
done < "$published"

echo "$match match, $mismatch mismatch, $missing not rebuilt; SHA256SUMS sha256 $(sha256 "$published")"
if [ "$mismatch" -gt 0 ]; then
  echo "DO NOT SIGN OR INSTALL: the published binaries are not what this source builds to" >&2
  exit 1
fi
if [ "$match" -eq 0 ]; then
  echo "nothing compared" >&2
  exit 1
fi
if [ "$missing" -eq 0 ]; then
  echo "every file matches: a signer may now run 'ternchain release sign --key KEY SHA256SUMS' on the published file"
fi
