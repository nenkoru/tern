#!/bin/sh
# Installs the ternchain client and a network file into one folder, from this
# repository's GitHub Releases. Nothing else: no keys, no accounts, no sudo.
#
#   sh install.sh                      ~/tern/ternchain + ~/tern/net.json for tern-base-sepolia-1
#
# Settings (environment):
#   TERN_HOME      the folder (default ~/tern)
#   TERN_NETWORK   the network's name (default tern-base-sepolia-1): networks/NAME/net.json in the repository
#   TERN_VERSION   a release tag (default: the latest release)
#   TERN_REPO      owner/name on GitHub (default nenkoru/tern)
#   TERN_BASE_URL  take the release's files from this URL (a mirror) instead of GitHub Releases
#   TERN_NET_URL   take net.json from this URL or path instead
#   TERN_TERMS_URL take TERMS.md from this URL instead
#   TERN_BIN       use this ternchain binary instead of downloading one (a build of your own)
#   TERN_OPENSSL   the openssl to check signatures with (OpenSSL 3 or later; found by itself otherwise)
#   TERN_VERIFIER  a ternchain you installed and verified before, to check signatures with when there is
#                  no OpenSSL 3 (default: TERN_HOME/ternchain, when there is one)
#   TERN_ALLOW_UNSIGNED=1  install a release nobody signed, on the hash check only (loud warning).
#                  For release candidates only, before signers exist: it works only while the
#                  signer set below has threshold 0. Once it has signers, a release without
#                  (or with a stripped) SHA256SUMS.sigs is refused whatever this says.
#
# The release's SHA256SUMS must be signed (SHA256SUMS.sigs) by at least the
# threshold of distinct signers of the signer set below, checked by OpenSSL 3
# (or by a ternchain you verified earlier), never by the binary being
# installed; then the binary must match its SHA256SUMS line before it is kept.
# docs/SUPPLY-CHAIN.md has the procedure and what this does not protect.
set -eu

# --- the release signer set: generated from releases/signers.json, do not edit by hand ---
# (UPDATE_INSTALL_SH=1 go test -run TestInstallShSignerSet ./cmd/ternchain rewrites it; the test fails when they differ)
signers_version=1
signers_id=5ab389c886e1679a115b52a8eb6f903b68eadd8877efe057eecd251ca08f17cd
signers_threshold=0
signers_keys='
'
# --- end of the release signer set ---

repo="${TERN_REPO:-nenkoru/tern}"
home="${TERN_HOME:-$HOME/tern}"
network="${TERN_NETWORK:-tern-base-sepolia-1}"
version="${TERN_VERSION:-}"

say() { printf '%s\n' "$*" >&2; }
die() { say "install: $*"; exit 1; }
fetch() { # URL FILE
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then wget -q "$1" -O "$2"
  else die "needs curl or wget"; fi
}
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
bar='!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!'

# ed25519_ok OPENSSL KEY-BASE64 MSG-FILE SIG-FILE: the signature verifies.
# The PEM is the key's SubjectPublicKeyInfo: a fixed 12-byte DER prefix
# (MCowBQYDK2VwAyEA in base64) and the 32 raw bytes.
ed25519_ok() {
  printf '%s\n' '-----BEGIN PUBLIC KEY-----' "MCowBQYDK2VwAyEA$2" '-----END PUBLIC KEY-----' > "$tmp/key.pem"
  ed_out="$("$1" pkeyutl -verify -pubin -inkey "$tmp/key.pem" -rawin -in "$3" -sigfile "$4" 2>&1)" || return 1
  case "$ed_out" in *"Verified Successfully"*) return 0 ;; esac
  return 1
}
b64d() { # OPENSSL BASE64 FILE
  printf '%s\n' "$2" | "$1" base64 -d -A > "$3" 2>/dev/null
}
# capable OPENSSL: it verifies RFC 8032's test vector 2 and refuses it altered
# (LibreSSL, macOS's /usr/bin/openssl, and OpenSSL before 3 can't do ed25519 here).
capable() {
  printf 'r' > "$tmp/t.msg" && printf 'R' > "$tmp/t.bad" || return 1
  b64d "$1" 'kqAJqfDUyrhyDoILX2QlQKKye1QWUD+Ps3YiI+vbadoIWsHkPhWZbkWPNhPQ8R2MOHsurrQwKu6wDSkWErsMAA==' "$tmp/t.sig" || return 1
  ed25519_ok "$1" 'PUAXw+hDiVqStwqnTRt+vJyYLM8uxJaMwM1V8Sr0Zgw=' "$tmp/t.msg" "$tmp/t.sig" &&
    ! ed25519_ok "$1" 'PUAXw+hDiVqStwqnTRt+vJyYLM8uxJaMwM1V8Sr0Zgw=' "$tmp/t.bad" "$tmp/t.sig"
}
find_openssl() {
  for o in "${TERN_OPENSSL:-}" openssl /opt/homebrew/opt/openssl@3/bin/openssl /usr/local/opt/openssl@3/bin/openssl \
    /opt/homebrew/bin/openssl /usr/local/bin/openssl /usr/bin/openssl; do
    [ -n "$o" ] && command -v "$o" >/dev/null 2>&1 && capable "$o" && { printf '%s\n' "$o"; return 0; }
  done
  return 1
}

# verify_sigs OPENSSL DIR VERSION: at least signers_threshold distinct signers
# of the set above signed DIR/SHA256SUMS for VERSION (DIR/SHA256SUMS.sigs);
# a signer who signed twice counts once, a key outside the set never counts.
verify_sigs() {
  sums_hash="$(sha256 "$2/SHA256SUMS")"
  signed="$(tr '{},' '\n\n\n' < "$2/SHA256SUMS.sigs" | sed -n 's/^[[:space:]]*"sums_sha256"[[:space:]]*:[[:space:]]*"\([0-9a-fA-F]*\)"[[:space:]]*$/\1/p' | head -n 1 | tr 'A-F' 'a-f')"
  [ "$signed" = "$sums_hash" ] || die "SHA256SUMS.sigs signs another SHA256SUMS (sha256 ${signed:-none}, this one is $sums_hash): not installed"
  printf 'ternchain-release/v1\n%s\n%s' "$3" "$sums_hash" > "$2/release.msg"
  tr '{},' '\n\n\n' < "$2/SHA256SUMS.sigs" | sed -n 's/^[[:space:]]*"sig"[[:space:]]*:[[:space:]]*"\([A-Za-z0-9+/=]*\)"[[:space:]]*$/\1/p' > "$2/sigs.txt"
  nsig=0 who=""
  while read -r skey sname; do
    [ -n "$skey" ] || continue
    while read -r sg; do
      b64d "$1" "$sg" "$2/one.sig" || continue
      if ed25519_ok "$1" "${skey#ed25519:}" "$2/release.msg" "$2/one.sig"; then
        nsig=$((nsig + 1)) who="$who $sname"
        break
      fi
    done < "$2/sigs.txt"
  done <<EOF
$signers_keys
EOF
  [ "$nsig" -ge "$signers_threshold" ] ||
    die "release $3 is signed by $nsig distinct signers [${who# }] of the signer set v$signers_version ($signers_id); it needs $signers_threshold: do not install it"
  say "signed: $nsig of the $signers_threshold needed [${who# }] (signer set v$signers_version, $signers_id; checked by $1)"
}

unsigned() { # WHY
  # The signer set this script pins has signers: an unsigned release is what a stripped
  # SHA256SUMS.sigs looks like, and TERN_ALLOW_UNSIGNED can't accept it.
  if [ "$signers_threshold" -gt 0 ]; then
    die "UNSIGNED: $1. Not installed: this install.sh pins the release signer set v$signers_version ($signers_id),
  which needs $signers_threshold signers, so every release must carry a SHA256SUMS.sigs they signed. A missing
  or stripped SHA256SUMS.sigs is what a tampered release looks like. TERN_ALLOW_UNSIGNED=1 is for release
  candidates only, before signers exist (threshold 0); it does not apply here, whatever it is set to."
  fi
  if [ "${TERN_ALLOW_UNSIGNED:-}" = 1 ]; then
    say "$bar"
    say "UNSIGNED: $1."
    say "TERN_ALLOW_UNSIGNED=1: installing on the hash check only. SHA256SUMS comes from the same place as"
    say "the binary, so whoever can serve these files can serve any binary. Rebuild it yourself"
    say "(scripts/repro.sh) or compare its sha256 through an independent channel."
    say "$bar"
    return 0
  fi
  die "UNSIGNED: $1. Not installed: a hash check alone passes a forged release too.
  - build it from source (go build ./cmd/ternchain at the release tag, or scripts/repro.sh; docs/SUPPLY-CHAIN.md), or
  - accept the hash-only check knowingly: TERN_ALLOW_UNSIGNED=1 sh install.sh"
}

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "this script is for macOS and Linux; on Windows download ternchain-VERSION-windows-amd64.exe from https://github.com/$repo/releases by hand" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "no release for $(uname -m)" ;;
esac

mkdir -p "$home"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if [ -n "${TERN_BIN:-}" ]; then
  cp "$TERN_BIN" "$tmp/ternchain"
  say "using your binary $TERN_BIN"
else
  if [ -z "$version" ]; then
    # releases/latest skips prereleases (404 while there are only release
    # candidates); then the newest published release.
    fetch "https://api.github.com/repos/$repo/releases/latest" "$tmp/latest.json" 2>/dev/null ||
      fetch "https://api.github.com/repos/$repo/releases?per_page=1" "$tmp/latest.json" ||
      die "can't reach GitHub's releases for $repo"
    version="$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
    [ -n "$version" ] || die "no release found for $repo"
  fi
  name="ternchain-$version-$os-$arch"
  base="${TERN_BASE_URL:-https://github.com/$repo/releases/download/$version}"
  rel="$tmp/release"
  mkdir "$rel"
  say "downloading $name"
  fetch "$base/$name" "$rel/$name" || die "no $name in release $version"
  fetch "$base/SHA256SUMS" "$rel/SHA256SUMS" || die "no SHA256SUMS in release $version"
  fetch "$base/SHA256SUMS.sigs" "$rel/SHA256SUMS.sigs" 2>/dev/null || rm -f "$rel/SHA256SUMS.sigs"

  # The signatures, by a verifier that is not the binary being installed.
  verifier="${TERN_VERIFIER:-$home/ternchain}"
  if [ "$signers_threshold" -gt 0 ] && ossl="$(find_openssl)"; then
    if [ -f "$rel/SHA256SUMS.sigs" ]; then
      verify_sigs "$ossl" "$rel" "$version"
    else
      unsigned "release $version has no SHA256SUMS.sigs (the signer set needs $signers_threshold signers)"
    fi
  elif [ -x "$verifier" ]; then
    # A ternchain installed (and verified) before: release verify follows its pinned signer set,
    # rotations included (the release's signers.json), and refuses a set its old signers didn't approve.
    fetch "$base/signers.json" "$rel/signers.json" 2>/dev/null || rm -f "$rel/signers.json"
    say "checking the signatures with $verifier (ternchain release verify)"
    # --allow-unsigned only on a second try: a ternchain older than that flag says "unsigned" with exit 0.
    # Never with signers pinned here (threshold > 0): then the flag doesn't apply.
    if ! verdict="$("$verifier" release verify "$rel")"; then
      if [ "${TERN_ALLOW_UNSIGNED:-}" != 1 ] || [ "$signers_threshold" -gt 0 ] ||
        ! verdict="$("$verifier" release verify "$rel" --allow-unsigned)"; then
        say "$verdict"
        if [ "$signers_threshold" -gt 0 ]; then
          die "$verifier refused release $version (above): not installed (this install.sh pins $signers_threshold signers, so TERN_ALLOW_UNSIGNED=1 does not apply)"
        fi
        die "$verifier refused release $version (above): not installed (if it says UNSIGNED: TERN_ALLOW_UNSIGNED=1 accepts a hash-only check, for release candidates only, before signers exist)"
      fi
    fi
    # "signed": true, or (an older ternchain says "unsigned" with exit 0) the hash check only
    if ! printf '%s\n' "$verdict" | grep -q '"signed": *true'; then
      unsigned "$verifier found no signatures for release $version (its signer set has no signers)"
    fi
  elif [ "$signers_threshold" -eq 0 ]; then
    unsigned "this install.sh carries no release signers yet (the signer set v$signers_version has threshold 0)"
  else
    die "can't check release $version's signatures: no OpenSSL 3 or later (macOS's /usr/bin/openssl is LibreSSL, which can't)
  and no ternchain installed before. Either:
  - install OpenSSL 3 (macOS: brew install openssl@3; Debian/Ubuntu: apt install openssl; Fedora: dnf install openssl)
    and run this again (TERN_OPENSSL=PATH picks one), or
  - point TERN_VERIFIER at a ternchain you verified before, or
  - build it from source (go build ./cmd/ternchain at the release tag, or scripts/repro.sh; docs/SUPPLY-CHAIN.md)"
  fi

  want="$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }' "$rel/SHA256SUMS")"
  [ -n "$want" ] || die "SHA256SUMS has no line for $name"
  [ "$(sha256 "$rel/$name")" = "$want" ] || die "$name does not match its SHA256SUMS line: not installed"
  mv "$rel/$name" "$tmp/ternchain"
fi
chmod +x "$tmp/ternchain"
[ "$os" = darwin ] && xattr -d com.apple.quarantine "$tmp/ternchain" 2>/dev/null || true
mv "$tmp/ternchain" "$home/ternchain"

net="${TERN_NET_URL:-https://raw.githubusercontent.com/$repo/main/networks/$network/net.json}"
case "$net" in
  http://* | https://*) fetch "$net" "$tmp/net.json" || die "can't fetch the network file $net" ;;
  *) cp "$net" "$tmp/net.json" ;;
esac
grep -q '"chain_id"' "$tmp/net.json" || die "$net is not a network file"
mv "$tmp/net.json" "$home/net.json"
terms="${TERN_TERMS_URL:-https://raw.githubusercontent.com/$repo/main/TERMS.md}"
fetch "$terms" "$home/TERMS.md" 2>/dev/null || say "(TERMS.md not fetched: read it at https://github.com/$repo/blob/main/TERMS.md)"
for doc in START.md FOR-AGENTS.md; do # the guide and the agent's rules (START.md, "Folders")
  fetch "https://raw.githubusercontent.com/$repo/main/$doc" "$home/$doc" 2>/dev/null || say "($doc not fetched: read it at https://github.com/$repo/blob/main/$doc)"
done
got="$(sed -n 's/^ *"network": *"\([^"]*\)".*/\1/p' "$home/net.json" | head -n 1)"

say "installed: $home/ternchain ($("$home/ternchain" version 2>/dev/null | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' | head -n 1)) and $home/net.json (${got:-$network})"
say "next: cd $home; export PATH=$home:\$PATH TERNCHAIN_NET=$home/net.json; then AGENTS.md step 2"
