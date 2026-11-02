#!/usr/bin/env bash
# Coordinator: deploy the contracts and write net.json (BOOTSTRAP-CHECKLIST.md, step 8).
#
#   ops/testnet/30-deploy.sh --rpc http://127.0.0.1:8545 --rpcs https://rpc1.example.org,https://rpc2.example.org \
#       [--deployer-key deployer.key] [--faucet-key faucet.key] [--founders founders.txt] \
#       [--bootnodes-file bootnodes.txt] [--net net.json] [--report genesis.out.json]
#
#   --rpc     a PRIVATE node to deploy through (a validator's or the archive node's RPC on
#             127.0.0.1, or an SSH tunnel to it). Not a public RPC.
#   --rpcs    the public RPC URLs clients use, in order (written into net.json "rpcs")
#   --report  the report 10-make-genesis.sh wrote (for the deployer address and the chain id)
#
# Runs: ternchain deploy --launch (launch windows: 7 d vote, 7 d exit) with --faucet-key,
# --rpcs and --bootnodes. net.json gets rpcs, bootnodes and genesis_hash.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

RPC=""
RPCS=""
DEPLOYER_KEY=deployer.key
FAUCET_KEY=faucet.key
FOUNDERS=founders.txt
BOOT_FILE=bootnodes.txt
NET=net.json
REPORT=genesis.out.json
while [ $# -gt 0 ]; do
  case "$1" in
    --rpc) RPC="$2"; shift 2 ;;
    --rpcs) RPCS="$2"; shift 2 ;;
    --deployer-key) DEPLOYER_KEY="$2"; shift 2 ;;
    --faucet-key) FAUCET_KEY="$2"; shift 2 ;;
    --founders) FOUNDERS="$2"; shift 2 ;;
    --bootnodes-file) BOOT_FILE="$2"; shift 2 ;;
    --net) NET="$2"; shift 2 ;;
    --report) REPORT="$2"; shift 2 ;;
    -h | --help) sed -n '2,15p' "$0"; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
[ -n "$RPC" ] || die "--rpc URL (a private node) is required"
[ -n "$RPCS" ] || die "--rpcs URL1,URL2 (the public RPCs) is required"
[ -f "$DEPLOYER_KEY" ] || die "no $DEPLOYER_KEY"
[ -f "$FAUCET_KEY" ] || die "no $FAUCET_KEY"
[ -f "$REPORT" ] || die "no $REPORT (written by 10-make-genesis.sh)"
[ ! -e "$NET" ] || die "$NET exists: move it away first (deploy writes a new one)"
need python3 curl
F="$(read_pairs "$FOUNDERS")"
BOOT="$(read_list "$BOOT_FILE")"
[ -n "$BOOT" ] || die "$BOOT_FILE is empty"
n_boot="$(printf '%s' "$BOOT" | awk -F, '{print NF}')"
[ "$n_boot" -ge 3 ] || warn "$n_boot bootnodes: the plan is at least 3, run by different people"

step "Chain"
CID="$(chain_id_of "$RPC")"
[ "$CID" = "$TESTNET_CHAIN_ID" ] || die "$RPC: chain id '${CID:-no answer}', expected $TESTNET_CHAIN_ID"
H0="$(genesis_hash_of "$RPC")"
say "chain $CID, block 0 $H0, head $(rpc "$RPC" eth_blockNumber)"

DEPLOYER="$(jget "$REPORT" 'd["deployer"]')"
NONCE="$(rpc "$RPC" eth_getTransactionCount "[\"$DEPLOYER\",\"latest\"]")"
[ "$NONCE" = '"0x0"' ] || die "deployer $DEPLOYER has nonce $NONCE: it sent something. The Treasury address is wrong now: SEV1, see INCIDENT-RESPONSE.md (key theft, deployer)"
say "deployer $DEPLOYER: nonce 0, good"

step "Public RPCs"
IFS=, read -r -a urls <<<"$RPCS"
[ "${#urls[@]}" -ge 2 ] || warn "only ${#urls[@]} public RPC: the plan is 2, on separate machines"
for u in "${urls[@]}"; do
  c="$(chain_id_of "$u")"
  h="$(genesis_hash_of "$u")"
  [ "$c" = "$TESTNET_CHAIN_ID" ] || die "$u: chain id '${c:-no answer}'"
  [ "$h" = "$H0" ] || die "$u: block 0 $h differs from $H0"
  [ -z "$(rpc "$u" admin_peers)" ] || die "$u answers admin_peers: it is not the public profile (--profile rpc)"
  say "ok: $u"
done

step "Deploy (one transaction a block: a few minutes)"
"$T" deploy --rpc "$RPC" --deployer-key "$DEPLOYER_KEY" --net "$NET" --genesis "$F" --launch \
  --faucet-key "$FAUCET_KEY" --rpcs "$RPCS" --bootnodes "$BOOT"

step "Check net.json"
jget "$NET" '"rpcs: %s\nbootnodes: %d\ngenesis_hash: %s\nchain_id: %s" % (d.get("rpcs"), len(d.get("bootnodes") or []), d.get("genesis_hash"), d.get("chain_id"))'
[ "$(jget "$NET" 'd.get("genesis_hash")')" = "$H0" ] || warn "net.json genesis_hash differs from block 0 of $RPC"
if grep -q 'faucet_key' "$NET"; then die "$NET contains a key field: do not publish it; check ${NET%.json}.operator.json"; fi

step "The deployer has no powers left"
"$T" validator powers --rpc "$RPC" --deployer-key "$DEPLOYER_KEY" || true

step "Next"
say "1. Publish $NET (public). Keep ${NET%.json}.operator.json private (0600)."
say "2. Destroy $DEPLOYER_KEY after the check above (shred -u $DEPLOYER_KEY), or lock it offline."
say "3. Founders: owner init --key …, then gov claim (docs/RUNBOOK.md, 'Founders claim')."
say "4. Run 40-check.sh --net $NET"
