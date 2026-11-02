#!/usr/bin/env bash
# Seal counts per validator, for Prometheus (node_exporter's textfile collector).
#
# Runs `ternchain validator status --rpc RPC --blocks 60` and writes
# $TEXTFILE_DIR/ternchain_seals.prom. Seals are on chain (QBFT commit seals in
# each header), so one exporter, against any node, sees every validator.
#
# cron, every minute (crontab -e):
#   * * * * * TERN_RPC=http://127.0.0.1:8545 /opt/tern/ops/monitoring/seal-exporter.sh 2>>/var/log/tern-seal-exporter.log
#
# Settings (environment):
#   TERN_RPC       the node to ask (a private RPC is best; a public one works)   default http://127.0.0.1:8545
#   TERN_BLOCKS    blocks to look at                                             default 60
#   TEXTFILE_DIR   node_exporter --collector.textfile.directory                  default /var/lib/node_exporter/textfile
#   TERNCHAIN      the ternchain binary                                          default ternchain
#
# Metrics (all gauges):
#   ternchain_status_up                     1 if status worked, else 0 (and nothing else is written)
#   ternchain_head_block, ternchain_head_age_seconds
#   ternchain_blocks_looked_at, ternchain_avg_block_seconds, ternchain_max_gap_seconds
#   ternchain_round_changes                 blocks with round > 0 in the window
#   ternchain_min_seals                     fewest commit seals on one block in the window
#   ternchain_peers                         the asked node's peer count
#   ternchain_validator_count               size of the set (the contract)
#   ternchain_validator_in_set{validator,citizen}
#   ternchain_validator_proposed{validator,citizen}
#   ternchain_validator_sealed{validator,citizen}
#   ternchain_validator_last_sealed{validator,citizen}
#   ternchain_validator_blocks_since_seal{validator,citizen}   head - last_sealed (head+1 if never in the window)
#   ternchain_contract_alerts               lines in the contract's recent alerts
#   ternchain_upgrade_in_blocks{at_block,release}              scheduled upgrades
set -euo pipefail

RPC="${TERN_RPC:-http://127.0.0.1:8545}"
BLOCKS="${TERN_BLOCKS:-60}"
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile}"
T="${TERNCHAIN:-ternchain}"
OUT="$DIR/ternchain_seals.prom"

mkdir -p "$DIR"
tmp="$(mktemp "$DIR/.ternchain_seals.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

if ! status="$("$T" validator status --rpc "$RPC" --blocks "$BLOCKS" 2>/dev/null)"; then
  {
    echo "# HELP ternchain_status_up 1 if ternchain validator status worked."
    echo "# TYPE ternchain_status_up gauge"
    echo "ternchain_status_up 0"
  } >"$tmp"
  chmod 0644 "$tmp"
  mv "$tmp" "$OUT"
  echo "seal-exporter: validator status --rpc $RPC failed" >&2
  exit 0
fi

# The keys come from validatorStatus in validator.go:
#   head, head_age_s, peers, blocks_looked_at, avg_block_s, max_gap_s,
#   round_changes, min_seals, validators_contract (list, or a string on error),
#   bindings [{validator, citizen}], by_validator {ADDR: {in_set, proposed,
#   sealed, last_sealed}}, alerts [..], upgrades [{at_block, release, in_blocks}].
printf '%s' "$status" | python3 -I -c '
import json, sys

d = json.load(sys.stdin)
lines = []

def gauge(name, help_, samples):
    lines.append("# HELP %s %s" % (name, help_))
    lines.append("# TYPE %s gauge" % name)
    for labels, value in samples:
        if value is None:
            continue
        if labels:
            lab = ",".join("%s=\"%s\"" % (k, str(v).replace("\\", "").replace("\"", "")) for k, v in labels.items())
            lines.append("%s{%s} %s" % (name, lab, value))
        else:
            lines.append("%s %s" % (name, value))

def num(k):
    v = d.get(k)
    return v if isinstance(v, (int, float)) and not isinstance(v, bool) else None

gauge("ternchain_status_up", "1 if ternchain validator status worked.", [({}, 1)])
gauge("ternchain_head_block", "Head block number.", [({}, num("head"))])
gauge("ternchain_head_age_seconds", "Seconds since the head block time.", [({}, num("head_age_s"))])
gauge("ternchain_blocks_looked_at", "Blocks in the window.", [({}, num("blocks_looked_at"))])
gauge("ternchain_avg_block_seconds", "Average block interval in the window.", [({}, num("avg_block_s"))])
gauge("ternchain_max_gap_seconds", "Largest block interval in the window.", [({}, num("max_gap_s"))])
gauge("ternchain_round_changes", "Blocks with a QBFT round above 0 in the window.", [({}, num("round_changes"))])
gauge("ternchain_min_seals", "Fewest commit seals on one block in the window.", [({}, num("min_seals"))])
gauge("ternchain_peers", "Peer count of the node asked.", [({}, num("peers"))])

vs = d.get("validators_contract")
vs = vs if isinstance(vs, list) else []
gauge("ternchain_validator_count", "Validators in the set (the contract).", [({}, len(vs))])

citizen = {}
for b in d.get("bindings") or []:
    if isinstance(b, dict) and b.get("validator"):
        citizen[b["validator"].lower()] = b.get("citizen", "")

head = num("head") or 0
per = d.get("by_validator") or {}
rows = []
for addr, x in sorted(per.items()):
    lab = {"validator": addr, "citizen": citizen.get(addr.lower(), "")}
    last = x.get("last_sealed") or 0
    since = head - last if last else head + 1
    rows.append((lab, x))
    x["_since"] = since
gauge("ternchain_validator_in_set", "1 if the address is in the set.", [(l, 1 if x.get("in_set") else 0) for l, x in rows])
gauge("ternchain_validator_proposed", "Blocks proposed in the window.", [(l, x.get("proposed", 0)) for l, x in rows])
gauge("ternchain_validator_sealed", "Commit seals in the window.", [(l, x.get("sealed", 0)) for l, x in rows])
gauge("ternchain_validator_last_sealed", "Last block with its commit seal (0: none in the window).", [(l, x.get("last_sealed", 0)) for l, x in rows])
gauge("ternchain_validator_blocks_since_seal", "Head minus last sealed block.", [(l, x["_since"]) for l, x in rows])

alerts = d.get("alerts")
gauge("ternchain_contract_alerts", "Recent alerts from the Validators contract.", [({}, len(alerts) if isinstance(alerts, list) else 0)])

ups = d.get("upgrades") or []
gauge("ternchain_upgrade_in_blocks", "Blocks until a scheduled upgrade (negative: passed).",
      [({"at_block": u.get("at_block"), "release": u.get("release")}, u.get("in_blocks")) for u in ups if isinstance(u, dict)])

print("\n".join(lines))
' >"$tmp"

chmod 0644 "$tmp"
mv "$tmp" "$OUT"   # atomic: node_exporter never reads half a file
trap - EXIT
