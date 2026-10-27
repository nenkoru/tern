#!/usr/bin/env bash
# Run on each validator machine, once (ops/testnet/VALIDATOR.md, step 2).
# Makes the node key and prints the line to send to the coordinator.
#
#   ops/testnet/00-validator-init.sh NAME PUBLIC_IP [DIR]
#
#   NAME       your citizen name (the founder name you sent with owner keygen), e.g. ana
#   PUBLIC_IP  this machine's fixed public IPv4 address
#   DIR        the validator folder (default ~/tern/validator)
#
# Also works for a bootnode, the archive node or an RPC node (a node that
# does not seal): the coordinator then uses only the enode.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

NAME="${1:?usage: 00-validator-init.sh NAME PUBLIC_IP [DIR]}"
IP="${2:?usage: 00-validator-init.sh NAME PUBLIC_IP [DIR]}"
DIR="${3:-$HOME/tern/validator}"

case "$NAME" in *[!a-z0-9-]* | -* | *-) die "NAME: lowercase letters, digits and inner dashes only" ;; esac
printf '%s' "$IP" | grep -Eq '^[0-9]{1,3}(\.[0-9]{1,3}){3}$' || die "PUBLIC_IP '$IP' is not an IPv4 address"
need python3

step "Checks"
if command -v docker >/dev/null 2>&1; then
  say "docker: $(docker --version)"
elif command -v besu >/dev/null 2>&1; then
  say "besu (native): $(besu --version 2>/dev/null | head -1). Besu 26.6+ needs Java 25: $(java -version 2>&1 | head -1)"
else
  warn "neither docker nor besu found: install Docker (simplest), see VALIDATOR.md"
fi
if command -v timedatectl >/dev/null 2>&1; then
  timedatectl show -p NTPSynchronized --value 2>/dev/null | grep -q yes && say "clock: synchronized" || warn "clock not synchronized: install chrony"
fi
"$T" version >/dev/null 2>&1 || die "$T does not run (set TERNCHAIN=/path/to/ternchain)"
say "ternchain: $("$T" version 2>/dev/null | jget - 'd.get("version")' 2>/dev/null || echo '?')"

step "Node key"
if [ -f "$DIR/key" ]; then
  say "$DIR/key exists: keeping it (never overwritten)"
else
  "$T" validator init --dir "$DIR" >/dev/null
  say "made $DIR/key (mode 0600)"
fi
ADDR="$(jget "$DIR/validator.json" 'd["address"]')"
ID="$(jget "$DIR/validator.json" 'd["public_key"]')"
ENODE="enode://$ID@$IP:30303"

step "Send this to the coordinator (it is public: no secret in it)"
say "validator: $NAME=$ADDR"
say "enode:     $ENODE"

step "Next"
say "1. Back up the key once, encrypted and offline, e.g.: age -p -o validator-key.age $DIR/key"
say "   Never copy the key to another server. Never send it to anyone."
say "2. Open port 30303 TCP and UDP to everyone; keep 8545, 8546, 9545 closed from outside."
say "3. Wait for genesis.json and its keccak from the coordinator, then: ops/testnet/20-validator-run.sh"
