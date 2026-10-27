#!/usr/bin/env bash
# Restore test of a Besu backup made by besu-backup.sh. ops/backup/README.md.
#
#   TERN_BOOTNODES=enode://…,enode://… ops/backup/besu-restore-test.sh /var/backups/tern-besu/besu-STAMP.tar.zst
#   TERN_BOOTNODES_FILE=bootnodes.txt  ops/backup/besu-restore-test.sh /var/backups/tern-besu/snap-STAMP
#
# Unpacks the backup into a scratch folder, gives it a NEW node key
# (`validator init`), starts a Besu node on it in Docker on spare ports, and
# checks that:
#   1. the node opens the data at the backed-up head (not at block 0),
#   2. its block at that height has the hash noted at backup time,
#   3. it syncs new blocks from its peers.
# Then it removes the container and the scratch folder.
#
# Settings (environment):
#   TERN_BOOTNODES / TERN_BOOTNODES_FILE   the network's bootnodes (one is needed to sync)
#   TERN_SCRATCH       where to unpack (needs the data's size free)   default: beside the backup
#   TERN_SYNC_WAIT     seconds to wait for new blocks                  default 600
#   TERN_P2P_PORT      p2p port of the test node (TCP+UDP)             default 30313
#   TERN_RPC_PORT      RPC port of the test node (127.0.0.1)            default 18545
#   TERN_REF_RPC       another node's RPC: the synced head is compared to it (optional)
#   TERN_IMAGE         Besu image                                       default: the one in meta.json
#   TERNCHAIN          the ternchain binary                             default ternchain
set -euo pipefail
umask 077

SRC="${1:?usage: besu-restore-test.sh BACKUP.tar.zst | SNAPSHOT-FOLDER}"
T="${TERNCHAIN:-ternchain}"
WAIT="${TERN_SYNC_WAIT:-600}"
P2P="${TERN_P2P_PORT:-30313}"
RPCP="${TERN_RPC_PORT:-18545}"
REF="${TERN_REF_RPC:-}"
NAME="tern-restore-test"
RPC="http://127.0.0.1:$RPCP"

log() { echo "restore-test: $*" >&2; }
die() { log "FAIL: $*"; exit 1; }

BOOT="${TERN_BOOTNODES:-}"
if [ -z "$BOOT" ] && [ -n "${TERN_BOOTNODES_FILE:-}" ]; then
  BOOT="$(grep -v '^[[:space:]]*\(#\|$\)' "$TERN_BOOTNODES_FILE" | paste -sd, -)"
fi
[ -n "$BOOT" ] || die "set TERN_BOOTNODES or TERN_BOOTNODES_FILE (the test node must sync from peers)"

rpc() { # URL METHOD PARAMS-JSON
  curl -sf --max-time 5 -X POST -H 'content-type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":$3}" "$1"
}
head_of() { rpc "$1" eth_blockNumber '[]' | python3 -I -c 'import json,sys; print(int(json.load(sys.stdin)["result"],16))' 2>/dev/null || echo -1; }
hash_at() { rpc "$1" eth_getBlockByNumber "[\"$(printf '0x%x' "$2")\",false]" | python3 -I -c 'import json,sys; r=json.load(sys.stdin)["result"]; print(r["hash"] if r else "")' 2>/dev/null || true; }
sum_check() {
  local d b
  d="$(dirname "$1")" b="$(basename "$1")"
  if command -v sha256sum >/dev/null; then (cd "$d" && sha256sum -c "$b.sha256" >/dev/null); else (cd "$d" && shasum -a 256 -c "$b.sha256" >/dev/null); fi
}

base="${TERN_SCRATCH:-$(dirname "$SRC")/.restore-test}"
mkdir -p "$base"
scratch="$(mktemp -d "$base/run.XXXXXX")"
cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  # Besu in Docker may write files as its own user: remove through a container if needed.
  rm -rf "${scratch:?}" 2>/dev/null || docker run --rm -v "$base:/b" busybox:1.36 rm -rf "/b/$(basename "$scratch")" || log "remove $scratch by hand"
}
trap cleanup EXIT

# 1. Unpack.
if [ -f "$SRC" ]; then
  if [ -f "$SRC.sha256" ]; then sum_check "$SRC" || die "checksum mismatch: $SRC"; else log "no $SRC.sha256: checksum not checked"; fi
  zstd -dc "$SRC" | tar -xf - -C "$scratch"
elif [ -d "$SRC/besu" ]; then
  cp -a "$SRC/besu" "$SRC/genesis.json" "$SRC/meta.json" "$scratch/"
else
  die "$SRC: not a .tar.zst file or a snapshot folder"
fi
[ -d "$scratch/besu" ] && [ -f "$scratch/genesis.json" ] && [ -f "$scratch/meta.json" ] || die "backup lacks besu/, genesis.json or meta.json"
[ ! -e "$scratch/key" ] || die "the backup holds a node key: it must not (README)"

meta() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2]) or "")' "$scratch/meta.json" "$1"; }
want_head="$(meta head)"
want_hash="$(meta head_hash)"
image="${TERN_IMAGE:-$(meta image)}"
log "backup $(meta stamp): head $want_head, hash ${want_hash:-?}, image ${image:-default}"

# 2. A new key, then start (Docker, spare ports).
"$T" validator init --dir "$scratch" >/dev/null
img=()
if [ -n "$image" ]; then img=(--image "$image"); fi
if [ "$(meta archive)" = True ]; then img+=(--archive); log "archive node backup: FOREST storage"; fi
docker rm -f "$NAME" >/dev/null 2>&1 || true
"$T" validator run --dir "$scratch" --genesis "$scratch/genesis.json" --docker --name "$NAME" \
  --p2p-port "$P2P" --rpc-port "$RPCP" --bootnodes "$BOOT" ${img[@]+"${img[@]}"} >/dev/null

# 3. It opens the data at the backed-up head.
h=-1
for _ in $(seq 1 120); do
  h="$(head_of "$RPC")"
  [ "$h" -ge 0 ] && break
  sleep 2
done
[ "$h" -ge 0 ] || { docker logs --tail 40 "$NAME" >&2 || true; die "the restored node did not answer on $RPC"; }
[ "$h" -ge "${want_head:-0}" ] || die "the node opened at block $h, below the backup's head $want_head (data not restored?)"
log "opened at block $h"
if [ -n "$want_hash" ] && [ "${want_head:-0}" -gt 0 ]; then
  got="$(hash_at "$RPC" "$want_head")"
  [ "$got" = "$want_hash" ] || die "block $want_head hash $got, backup noted $want_hash"
  log "block $want_head hash matches"
fi

# 4. It syncs new blocks.
start="$h"
deadline=$(( $(date +%s) + WAIT ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  h="$(head_of "$RPC")"
  if [ "$h" -gt "$start" ]; then break; fi
  sleep 5
done
[ "$h" -gt "$start" ] || { docker logs --tail 40 "$NAME" >&2 || true; die "no new block in $WAIT s (peers? bootnodes? port $P2P open?)"; }
log "syncing: $start -> $h"
if [ -n "$REF" ]; then
  ref="$(head_of "$REF")"
  log "reference node head $ref (gap $((ref - h)) blocks; the gap closes as it syncs)"
  got="$(hash_at "$RPC" "$h")"
  refh="$(hash_at "$REF" "$h")"
  [ -z "$refh" ] || [ "$got" = "$refh" ] || die "block $h differs from the reference node: $got vs $refh"
fi
log "OK: $SRC restores and syncs"
