# Shared helpers for ops/testnet/*.sh. Sourced, not run.
# shellcheck shell=bash

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
T="${TERNCHAIN:-ternchain}"
TESTNET_CHAIN_ID=83760

say() { printf '%s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

need() { # need CMD…: stop if a tool is missing
  local c
  for c in "$@"; do command -v "$c" >/dev/null 2>&1 || die "$c is not installed"; done
}

# jget FILE|- EXPR: a Python expression over the JSON in d (d["key"], len(d[…]) …)
jget() {
  local src="$1" expr="$2"
  if [ "$src" = - ]; then
    python3 -I -c 'import json,sys; d=json.load(sys.stdin); v=eval(sys.argv[1]); print("" if v is None else v)' "$expr"
  else
    python3 -I -c 'import json,sys; d=json.load(open(sys.argv[2])); v=eval(sys.argv[1]); print("" if v is None else v)' "$expr" "$src"
  fi
}

# rpc URL METHOD [PARAMS_JSON]: the JSON-RPC result as JSON text ("" on error)
rpc() {
  curl -sf --max-time 10 -X POST -H 'content-type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":${3:-[]}}" "$1" \
    | python3 -I -c 'import json,sys; d=json.load(sys.stdin); r=d.get("result"); print("" if r is None else json.dumps(r))' 2>/dev/null || true
}

chain_id_of() { # URL -> decimal chain id ("" on error)
  local r
  r="$(rpc "$1" eth_chainId)"
  [ -n "$r" ] && python3 -I -c 'import json,sys; print(int(json.loads(sys.argv[1]),16))' "$r" || true
}

genesis_hash_of() { # URL -> hash of block 0 ("" on error)
  local r
  r="$(rpc "$1" eth_getBlockByNumber '["0x0",false]')"
  [ -n "$r" ] && printf '%s' "$r" | jget - 'd["hash"]' || true
}

keccak_file() { python3 -I "$HERE/keccak256.py" "$1"; }

sha256_file() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# read_pairs FILE: "name=0xADDR" lines -> "name=0xADDR,name=0xADDR" (comments and blanks skipped)
read_pairs() {
  local f="$1" line addr out="" n=0
  [ -f "$f" ] || die "no file $f"
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%%#*}"
    line="$(printf '%s' "$line" | tr -d '[:space:]')"
    [ -z "$line" ] && continue
    case "$line" in
      [a-z0-9]*=0x[0-9a-fA-F]*) ;;
      *) die "$f: bad line '$line' (want name=0xADDRESS)" ;;
    esac
    addr="${line#*=}"
    [ "${#addr}" -eq 42 ] || die "$f: '$line': an address is 0x and 40 hex digits"
    out="${out:+$out,}$line"
    n=$((n + 1))
  done <"$f"
  [ "$n" -gt 0 ] || die "$f is empty"
  printf '%s' "$out"
}

# read_list FILE: one item a line -> "a,b,c" (comments and blanks skipped)
read_list() {
  local f="$1"
  [ -f "$f" ] || die "no file $f"
  grep -v '^[[:space:]]*\(#\|$\)' "$f" | tr -d '[:blank:]\r' | paste -sd, -
}
