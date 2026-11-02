#!/usr/bin/env bash
# Each validator: check the genesis, then start the node (VALIDATOR.md, step 4).
#
#   ops/testnet/20-validator-run.sh --genesis genesis.json --keccak 0xKECCAK \
#       --p2p-host PUBLIC_IP --bootnodes-file bootnodes.txt [--dir ~/tern/validator] \
#       [--genesis-hash 0xBLOCK0HASH] [-- EXTRA validator run FLAGS]
#
#   --keccak         the genesis keccak the coordinator published (required; the script refuses
#                    to start on a different file)
#   --bootnodes-file one enode://… a line; your own enode is skipped. Empty: you are the first bootnode.
#   --genesis-hash   block 0's hash, once known (net.json "genesis_hash"): checked after the start
#   after --          more flags for `ternchain validator run`, e.g. -- --archive  or  -- --profile rpc --rpc-allow-hosts rpc1.example.org
#
# The same script starts bootnodes, the archive node and RPC nodes (they do not seal).
# Docker mode (--docker) with --restart unless-stopped and --metrics (127.0.0.1:9545).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

DIR="$HOME/tern/validator"
GENESIS=""
KECCAK=""
P2P_HOST=""
BOOT_FILE=""
GHASH=""
EXTRA=()
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) DIR="$2"; shift 2 ;;
    --genesis) GENESIS="$2"; shift 2 ;;
    --keccak) KECCAK="$2"; shift 2 ;;
    --p2p-host) P2P_HOST="$2"; shift 2 ;;
    --bootnodes-file) BOOT_FILE="$2"; shift 2 ;;
    --genesis-hash) GHASH="$2"; shift 2 ;;
    --) shift; EXTRA=("$@"); break ;;
    -h | --help) sed -n '2,17p' "$0"; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
[ -n "$GENESIS" ] || die "--genesis FILE is required"
[ -n "$KECCAK" ] || die "--keccak 0x… is required (from the coordinator, not from the same place as the file)"
[ -n "$P2P_HOST" ] || die "--p2p-host PUBLIC_IP is required"
[ -f "$DIR/key" ] || die "no $DIR/key: run 00-validator-init.sh first"
need python3 curl docker

step "Genesis check"
GOT="$(keccak_file "$GENESIS")"
if [ "$(printf '%s' "$GOT" | tr 'A-F' 'a-f')" != "$(printf '%s' "$KECCAK" | tr 'A-F' 'a-f')" ]; then
  die "genesis keccak $GOT differs from the expected $KECCAK: do not start; ask the coordinator"
fi
say "keccak ok: $GOT"
CID="$(jget "$GENESIS" 'd["config"]["chainId"]')"
[ "$CID" = "$TESTNET_CHAIN_ID" ] || die "genesis chain id $CID, expected $TESTNET_CHAIN_ID"
say "chain id ok: $CID"

step "Bootnodes"
MY_ID="$(jget "$DIR/validator.json" 'd["public_key"]')"
BOOT=""
if [ -n "$BOOT_FILE" ]; then
  while IFS= read -r e || [ -n "$e" ]; do
    e="$(printf '%s' "${e%%#*}" | tr -d '[:space:]')"
    [ -z "$e" ] && continue
    case "$e" in enode://*@*:*) ;; *) die "$BOOT_FILE: '$e' is not enode://ID@IP:PORT" ;; esac
    case "$e" in "enode://$MY_ID@"*) say "skip my own enode"; continue ;; esac
    BOOT="${BOOT:+$BOOT,}$e"
  done <"$BOOT_FILE"
fi
if [ -z "$BOOT" ]; then warn "no bootnodes: fine for the first bootnode only"; else say "$(printf '%s' "$BOOT" | awk -F, '{print NF}') bootnodes"; fi

step "Start"
args=(validator run --dir "$DIR" --genesis "$GENESIS" --p2p-host "$P2P_HOST" --docker
  --metrics --restart unless-stopped)
if [ -n "$BOOT" ]; then args+=(--bootnodes "$BOOT"); fi
args+=(${EXTRA[@]+"${EXTRA[@]}"})
say "$T ${args[*]}"
OUT="$("$T" "${args[@]}")" || die "validator run failed: $OUT"
printf '%s\n' "$OUT"
RPC="$(printf '%s' "$OUT" | jget - 'd.get("rpc") or "http://127.0.0.1:8545"')"
ENODE="$(printf '%s' "$OUT" | jget - 'd.get("enode")')"

step "Wait for the node"
ok=0
for _ in $(seq 1 60); do
  if [ -n "$(chain_id_of "$RPC")" ]; then ok=1; break; fi
  sleep 2
done
[ "$ok" = 1 ] || die "no answer from $RPC after 2 minutes: docker logs --tail 50 $(printf '%s' "$OUT" | jget - 'd.get("container")')"
[ "$(chain_id_of "$RPC")" = "$TESTNET_CHAIN_ID" ] || die "the node reports chain id $(chain_id_of "$RPC")"
H0="$(genesis_hash_of "$RPC")"
say "block 0 hash: $H0"
if [ -n "$GHASH" ] && [ "$(printf '%s' "$H0" | tr 'A-F' 'a-f')" != "$(printf '%s' "$GHASH" | tr 'A-F' 'a-f')" ]; then
  die "block 0 hash $H0 differs from the expected $GHASH: wrong genesis; stop the node"
fi
sleep 10
"$T" validator status --rpc "$RPC" --blocks 20 | jget - '"head %s (%s s old), peers %s, round changes %s" % (d.get("head"), d.get("head_age_s"), d.get("peers"), d.get("round_changes"))' || true

step "Send this to the coordinator"
say "enode:        $ENODE"
say "block 0 hash: $H0"
say "Then check now and then: ops/testnet/40-check.sh --rpc $RPC"
