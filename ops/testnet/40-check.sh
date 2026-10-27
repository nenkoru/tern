#!/usr/bin/env bash
# Anyone: is the testnet healthy? (VALIDATOR.md "Monitoring", BOOTSTRAP-CHECKLIST.md)
#
#   ops/testnet/40-check.sh [--rpc URL] [--net net.json] [--genesis-hash 0x…] [--min-peers 2] [--blocks 60]
#
# Checks, with validator status over the last N blocks:
#   chain id 83760; block 0 hash (from --genesis-hash or net.json genesis_hash);
#   the head is fresh; every validator in the set sealed within 2n blocks and
#   proposed at least once; round changes; peers; scheduled upgrades;
#   and every RPC in net.json "rpcs": same chain id, same block 0.
# Prints PASS / WARN / FAIL lines; exits 1 if any FAIL. Good for cron.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

RPC=""
NET=""
GHASH=""
MIN_PEERS=2
BLOCKS=60
while [ $# -gt 0 ]; do
  case "$1" in
    --rpc) RPC="$2"; shift 2 ;;
    --net) NET="$2"; shift 2 ;;
    --genesis-hash) GHASH="$2"; shift 2 ;;
    --min-peers) MIN_PEERS="$2"; shift 2 ;;
    --blocks) BLOCKS="$2"; shift 2 ;;
    -h | --help) sed -n '2,12p' "$0"; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
need python3 curl

RPCS=""
if [ -n "$NET" ]; then
  [ -f "$NET" ] || die "no $NET"
  RPCS="$(jget "$NET" '",".join(d.get("rpcs") or ([d["rpc"]] if d.get("rpc") else []))')"
  [ -n "$GHASH" ] || GHASH="$(jget "$NET" 'd.get("genesis_hash")')"
fi
[ -n "$RPC" ] || RPC="${RPCS%%,*}"
[ -n "$RPC" ] || RPC="http://127.0.0.1:8545"

fails=0
pass() { printf 'PASS  %s\n' "$*"; }
wrn() { printf 'WARN  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; fails=$((fails + 1)); }
lc() { printf '%s' "$1" | tr 'A-F' 'a-f'; }

check_rpc() { # URL: chain id and block 0
  local u="$1" c h
  c="$(chain_id_of "$u")"
  if [ -z "$c" ]; then fail "$u: no answer"; return; fi
  if [ "$c" = "$TESTNET_CHAIN_ID" ]; then pass "$u: chain id $c"; else fail "$u: chain id $c, expected $TESTNET_CHAIN_ID"; fi
  h="$(genesis_hash_of "$u")"
  if [ -z "$GHASH" ]; then
    wrn "$u: block 0 $h (no expected hash given: --genesis-hash or --net)"
  elif [ "$(lc "$h")" = "$(lc "$GHASH")" ]; then
    pass "$u: block 0 matches"
  else
    fail "$u: block 0 $h, expected $GHASH (another chain)"
  fi
}

echo "== $RPC"
check_rpc "$RPC"

if ! ST="$("$T" validator status --rpc "$RPC" --blocks "$BLOCKS" 2>&1)"; then
  fail "validator status: $ST"
else
  # The keys are validatorStatus's in validator.go.
  set +e
  printf '%s' "$ST" | python3 -I -c '
import json, sys
d = json.load(sys.stdin)
min_peers = int(sys.argv[1])
out = []
def p(level, msg): out.append("%-5s %s" % (level, msg))

age = d.get("head_age_s", 0)
p("PASS" if age <= 10 else "FAIL", "head %s, %s s old" % (d.get("head"), age))

vs = d.get("validators_contract")
if not isinstance(vs, list):
    p("FAIL", "no validator set: %s" % vs)
    vs = []
n = len(vs)
if n:
    f = (n - 1) // 3
    p("PASS" if n >= 4 else "FAIL", "%d validators: blocks need %d seals, the chain survives %d down" % (n, n - f, f))
names = {b.get("validator", "").lower(): b.get("citizen", "?") for b in d.get("bindings") or [] if isinstance(b, dict)}
head = d.get("head", 0)
looked = d.get("blocks_looked_at", 0)
per = {k.lower(): v for k, v in (d.get("by_validator") or {}).items()}
for v in vs:
    x = per.get(v.lower(), {})
    who = "%s (%s)" % (names.get(v.lower(), "?"), v[:10])
    last = x.get("last_sealed", 0)
    since = head - last if last else None
    if since is None:
        p("FAIL", "%s: no seal in the last %d blocks" % (who, looked))
    elif since > 2 * n:
        p("FAIL", "%s: last seal %d blocks ago (more than 2n = %d)" % (who, since, 2 * n))
    else:
        p("PASS", "%s: sealed %d, proposed %d, last seal %d blocks ago" % (who, x.get("sealed", 0), x.get("proposed", 0), since))
    if looked >= 3 * n and x.get("proposed", 0) == 0:
        p("WARN", "%s: proposed no block in %d" % (who, looked))

rc = d.get("round_changes", 0)
p("PASS" if rc == 0 else "WARN", "round changes in %d blocks: %s (avg block %.2f s, max gap %s s)" % (looked, rc, d.get("avg_block_s", 0) or 0, d.get("max_gap_s")))

peers = d.get("peers")
if peers is not None:
    p("PASS" if peers >= min_peers else "WARN", "peers of this node: %s (want >= %d)" % (peers, min_peers))

for u in d.get("upgrades") or []:
    ib = u.get("in_blocks", 0)
    if ib > 0:
        p("WARN" if ib < 7 * 86400 else "PASS", "upgrade at block %s (in %s blocks, ~%.1f days): release %s %s" % (u.get("at_block"), ib, ib / 86400.0, u.get("release"), u.get("uri")))
alerts = d.get("alerts") or []
if alerts:
    p("WARN", "%d recent alerts from the Validators contract (read: ternchain validator status)" % len(alerts))
print("\n".join(out))
sys.exit(1 if any(l.startswith("FAIL") for l in out) else 0)
' "$MIN_PEERS"
  rc=$?
  set -e
  [ "$rc" = 0 ] || fails=$((fails + 1))
fi

if [ -n "$RPCS" ]; then
  echo "== RPCs in $NET"
  IFS=, read -r -a urls <<<"$RPCS"
  for u in "${urls[@]}"; do
    [ "$u" = "$RPC" ] && continue
    check_rpc "$u"
  done
  [ "${#urls[@]}" -ge 2 ] || wrn "net.json lists ${#urls[@]} RPC: the plan is at least 2"
fi

echo
if [ "$fails" -gt 0 ]; then echo "RESULT: $fails check group(s) failed"; exit 1; fi
echo "RESULT: ok"
