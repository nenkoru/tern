#!/usr/bin/env bash
# Backup of a Besu data folder from a NON-validator node of a ternchain L1
# (the archive node, an RPC node, a bootnode). ops/backup/README.md.
#
#   TERN_NODE_DIR=~/tern/archive ops/backup/besu-backup.sh
#
# Steps: refuse if the node's address is in the validator set; note the head;
# stop the node; archive DIR/besu and DIR/genesis.json (tar + zstd, or an
# rsync snapshot with hard links); start the node again (also on any error);
# keep the newest N; optionally copy offsite.
#
# The node key (DIR/key) is NEVER in the archive. Node keys are backed up
# separately, encrypted and offline (README: "Keys are not data").
#
# Settings (environment):
#   TERN_NODE_DIR     the node's folder (made by `ternchain validator init`)   required
#   TERN_CONTAINER    its Docker container            default: "container" in DIR/validator.json
#   TERN_UNIT         a systemd unit instead of Docker (native besu), e.g. besu-archive.service
#   TERN_RPC          its RPC, to check it is not a validator     default http://127.0.0.1:8545
#   TERN_BACKUP_DIR   where archives go                           default /var/backups/tern-besu
#   TERN_KEEP         how many to keep                            default 7
#   TERN_METHOD       tar (one .tar.zst) or rsync (snapshot folders)   default tar
#   TERN_ZSTD_LEVEL   zstd level                                  default 3
#   TERN_OFFSITE      rsync target for a copy, e.g. backup@host:/srv/tern-besu/   (optional)
#   TERN_STOP_TIMEOUT seconds Besu gets to shut down cleanly      default 120
#   TERNCHAIN         the ternchain binary                        default ternchain
#   TERN_ARCHIVE=1    the node uses FOREST (--archive); found by itself for a Docker node
#   TERN_NOT_VALIDATOR=1  skip the validator check (only when the RPC is down and you are sure)
set -euo pipefail
umask 077

DIR="${TERN_NODE_DIR:?set TERN_NODE_DIR to the node folder (e.g. ~/tern/archive)}"
T="${TERNCHAIN:-ternchain}"
RPC="${TERN_RPC:-http://127.0.0.1:8545}"
DEST="${TERN_BACKUP_DIR:-/var/backups/tern-besu}"
KEEP="${TERN_KEEP:-7}"
METHOD="${TERN_METHOD:-tar}"
LEVEL="${TERN_ZSTD_LEVEL:-3}"
OFFSITE="${TERN_OFFSITE:-}"
STOP_TIMEOUT="${TERN_STOP_TIMEOUT:-120}"
UNIT="${TERN_UNIT:-}"

log() { echo "besu-backup: $*" >&2; }
die() { log "$*"; exit 1; }

json_get() { # FILE KEY: a top-level string field of a JSON file
  python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2]) or "")' "$1" "$2"
}

rpc() { # METHOD PARAMS-JSON
  curl -sf --max-time 10 -X POST -H 'content-type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":$2}" "$RPC"
}

sum_write() { # FILE: FILE.sha256 beside it
  local d b
  d="$(dirname "$1")" b="$(basename "$1")"
  if command -v sha256sum >/dev/null; then (cd "$d" && sha256sum "$b" >"$b.sha256"); else (cd "$d" && shasum -a 256 "$b" >"$b.sha256"); fi
}

[ -d "$DIR/besu" ] || die "no $DIR/besu: not a node folder, or the node never ran"
[ -f "$DIR/validator.json" ] || die "no $DIR/validator.json"
case "$METHOD" in tar) command -v zstd >/dev/null || die "zstd not installed (apt install zstd)";; rsync) command -v rsync >/dev/null || die "rsync not installed";; *) die "TERN_METHOD must be tar or rsync";; esac

ADDR="$(json_get "$DIR/validator.json" address)"
CONTAINER="${TERN_CONTAINER:-$(json_get "$DIR/validator.json" container)}"
[ -n "$UNIT" ] || [ -n "$CONTAINER" ] || die "no container in validator.json: set TERN_CONTAINER or TERN_UNIT"

# 1. Never a validator: stopping one costs the chain a sealer, and its folder holds the node key.
status=""
if status="$("$T" validator status --rpc "$RPC" --blocks 1 2>/dev/null)"; then
  if printf '%s' "$status" | python3 -I -c '
import json, sys
d = json.load(sys.stdin)
vs = d.get("validators_contract")
vs = [v.lower() for v in vs] if isinstance(vs, list) else []
sys.exit(0 if sys.argv[1].lower() in vs else 1)' "$ADDR"; then
    die "$ADDR is in the validator set: do not back up a validator this way (README)"
  fi
elif [ "${TERN_NOT_VALIDATOR:-}" != 1 ]; then
  die "validator status --rpc $RPC failed: cannot check the node is not a validator (TERN_NOT_VALIDATOR=1 to skip)"
fi

# 2. Note where the chain is, for the restore test.
head_hex="$(rpc eth_blockNumber '[]' | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["result"])' || true)"
head_hash=""
if [ -n "$head_hex" ]; then
  head_hash="$(rpc eth_getBlockByNumber "[\"$head_hex\",false]" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["result"]["hash"])' || true)"
fi
image="" archive="${TERN_ARCHIVE:-0}"
if [ -z "$UNIT" ]; then
  image="$(docker inspect --format '{{.Config.Image}}' "$CONTAINER" 2>/dev/null || true)"
  if docker inspect --format '{{json .Args}}' "$CONTAINER" 2>/dev/null | grep -q 'data-storage-format=FOREST'; then archive=1; fi
fi

mkdir -p "$DEST"
chmod 700 "$DEST"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
work="$DEST/.work-$stamp"
mkdir -p "$work"
python3 -I - "$work/meta.json" <<EOF
import json, sys
json.dump({"stamp": "$stamp", "node_address": "$ADDR", "head": int("${head_hex:-0x0}", 16),
           "head_hash": "$head_hash", "image": "$image", "method": "$METHOD", "archive": $archive == 1,
           "note": "no node key inside: give a restored node a new key (validator init)"},
          open(sys.argv[1], "w"), indent=2)
EOF

stop_node() {
  if [ -n "$UNIT" ]; then systemctl stop "$UNIT"; else docker stop -t "$STOP_TIMEOUT" "$CONTAINER" >/dev/null; fi
}
start_node() {
  if [ -n "$UNIT" ]; then systemctl start "$UNIT"; else docker start "$CONTAINER" >/dev/null; fi
}
stopped=0
cleanup() {
  if [ "$stopped" = 1 ]; then start_node || log "COULD NOT START THE NODE AGAIN: start it by hand"; fi
  rm -rf "${work:?}"
}
trap cleanup EXIT

# 3. Stop, copy, start. Besu holds its database open: a copy of a running node is not consistent.
t0="$(date +%s)"
log "stopping ${UNIT:-$CONTAINER} (head ${head_hex:-?})"
stop_node
stopped=1

if [ "$METHOD" = tar ]; then
  out="$DEST/besu-$stamp.tar.zst"
  tar -cf - -C "$DIR" --exclude=./key --exclude=key besu genesis.json -C "$work" meta.json \
    | zstd -q -T0 "-$LEVEL" -o "$out"
else
  out="$DEST/snap-$stamp"
  mkdir -p "$out"
  link=()
  if [ -d "$DEST/snap-latest/besu" ]; then link=(--link-dest="$DEST/snap-latest/besu"); fi
  rsync -a --delete --exclude=/key ${link[@]+"${link[@]}"} "$DIR/besu/" "$out/besu/"
  cp -p "$DIR/genesis.json" "$work/meta.json" "$out/"
fi

start_node
stopped=0
log "node started again after $(( $(date +%s) - t0 )) s down"

# 4. Checksums, retention, offsite.
if [ "$METHOD" = tar ]; then
  sum_write "$out"
  log "wrote $out ($(du -h "$out" | cut -f1))"
  ls -1t "$DEST"/besu-*.tar.zst 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
    rm -f "$old" "$old.sha256"; log "removed old $old"
  done
else
  ln -sfn "$(basename "$out")" "$DEST/snap-latest"
  log "wrote $out ($(du -sh "$out" | cut -f1), hard links to the previous snapshot)"
  ls -1dt "$DEST"/snap-2* 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
    rm -rf "${old:?}"; log "removed old $old"
  done
fi

if [ -n "$OFFSITE" ]; then
  if [ "$METHOD" = tar ]; then
    rsync -a --partial "$out" "$out.sha256" "$OFFSITE" && log "copied offsite to $OFFSITE"
  else
    rsync -a --delete "$out/" "${OFFSITE%/}/$(basename "$out")/" && log "copied offsite to $OFFSITE"
  fi
fi
