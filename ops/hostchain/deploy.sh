#!/usr/bin/env bash
# ternchain on a chain someone else runs (Base Sepolia, or Base mainnet), from a fresh identity in one command:
# new keys, the zk keys (ceremony solo), the contracts, net.json, the relay unit and the files to publish.
# Every name, URL and key is a parameter: nothing here ties the network to its operator.
#
#   T=/path/to/ternchain-ops CONTRACTS=/path/to/checkout/contracts HOME_DIR=~/tern-base OPERATOR=NAME CONTACT=HOW \
#     ops/hostchain/deploy.sh
#
# Then fund the printed deployer address (Base Sepolia ETH, about 0.0006 for the contracts) and run it
# again: it goes on where it stopped (deploy resumes from net.deploy.json).
#
# HOST=base: Base MAINNET (chain 8453), real money: networks/tern-base-1/README.md is the runbook. It needs
# CONFIRM_MAINNET=yes, refuses an RPC of another chain, and adds what a launch without an owner can have:
# net.json caps (CAP_PER_PAYMENT, CAP_PER_NAME USDC; CAP_COIN_PER_PAYMENT, CAP_COIN_PER_NAME ETH) that clients
# refuse to exceed, and a notice that says unaudited, a few dollars at most. Its zk keys are the deployer's
# alone (--solo-zk: secret ballots only; the public set has no shielded pool) and net.json says so.
#
# PROFILE=public (default): no shielded pool, stealth, coin house, pots or fiat; no founders (nobody can
# change the contracts); users pay their own gas (GAS=usdc: in USDC through Circle Paymaster); mail goes
# on chain (InboxForwarder), no relay; the bundler is a public one (Pimlico's, no key). The deployer gives
# up every role (check: ternchain roles). PUBLIC_URL/RELAY_PRICE: an optional relay of your own.
set -euo pipefail
: "${T:?T=path to ternchain-ops}"
: "${HOME_DIR:?HOME_DIR=the operator folder (keys, zk keys, net.json)}"
HOST="${HOST:-base-sepolia}"
case "$HOST" in
  base-sepolia)
    CHAIN_ID=84532
    NAME="${NAME:-tern-base-sepolia-1}"
    RPC="${RPC:-https://base-sepolia-rpc.publicnode.com}"
    RPCS="${RPCS:-https://base-sepolia-rpc.publicnode.com,https://base-sepolia.gateway.tenderly.co}"
    EXPLORER="${EXPLORER:-https://base-sepolia.blockscout.com}"
    zk=(--test-network)
    ;;
  base)
    CHAIN_ID=8453
    NAME="${NAME:-tern-base-1}"
    RPC="${RPC:-https://base-rpc.publicnode.com}"
    RPCS="${RPCS:-https://base-rpc.publicnode.com,https://base.drpc.org,https://mainnet.base.org}" # 10,000-block log queries (tenderly's keyless one: 1,000)
    EXPLORER="${EXPLORER:-https://base.blockscout.com}"
    zk=(--solo-zk --cap-per-payment "${CAP_PER_PAYMENT:-5}" --cap-per-name "${CAP_PER_NAME:-20}"
      --cap-coin-per-payment "${CAP_COIN_PER_PAYMENT:-0.002}" --cap-coin-per-name "${CAP_COIN_PER_NAME:-0.005}")
    if [ "${CONFIRM_MAINNET:-}" != yes ]; then
      echo "HOST=base is Base MAINNET: real ETH pays the deploy and users put in real USDC, on unaudited contracts." >&2
      echo "Read networks/tern-base-1/README.md, then run again with CONFIRM_MAINNET=yes." >&2
      exit 2
    fi
    [ "${PROFILE:-public}" = public ] || { echo "HOST=base: PROFILE=public only" >&2; exit 2; }
    [ "${GAS:-usdc}" != sponsor ] || { echo "HOST=base: nobody sponsors gas (GAS=usdc or eth)" >&2; exit 2; }
    ;;
  *) echo "HOST=base-sepolia|base" >&2; exit 2 ;;
esac
PUBLIC_URL="${PUBLIC_URL:-}"          # an optional relay and bundler of your own (https://HOST/base); empty: none
BUNDLER="${BUNDLER:-https://public.pimlico.io/v2/$CHAIN_ID/rpc}" # anyone's ERC-4337 bundler for EntryPoint v0.8
PROFILE="${PROFILE:-public}"
OPERATOR="${OPERATOR:-the operator}"
CONTACT="${CONTACT:-through the person who invited you}"
GAS="${GAS:-usdc}"                   # usdc: users pay gas in USDC (Circle Paymaster); eth: users pay in ETH; sponsor: the operator pays (SponsorPaymaster)
RELAY_PRICE="${RELAY_PRICE:-0.0001}" # USDC per envelope, paid by the sender
mkdir -p "$HOME_DIR/keys" "$HOME_DIR/deploy" && chmod 700 "$HOME_DIR" "$HOME_DIR/keys"
cd "$HOME_DIR"
for k in deployer; do
  [ -f keys/$k.key ] || "$T" owner keygen --out keys/$k.key >/dev/null
done
if [ ! -f zk-keys/CEREMONY ]; then
  "$T" ceremony solo --keys "$PWD/zk-keys" >/dev/null
  node "${CONTRACTS:?CONTRACTS=the contracts dir of a checkout (build-zk.js, .solc)}/build-zk.js" "$PWD/zk-keys" >/dev/null
fi
gas=()
case "$GAS" in
  usdc) gas=(--gas-paymaster circle) ;;
  eth) ;;
  sponsor) gas=(--sponsor-per-name "${SPONSOR_PER_NAME:-0.0001}" --sponsor-per-day "${SPONSOR_PER_DAY:-0.0004}" --sponsor-deposit "${SPONSOR_DEPOSIT:-0.0005}") ;;
  *) echo "GAS=usdc|eth|sponsor" >&2; exit 2 ;;
esac
export TERNCHAIN_ZK_KEYS="$PWD/zk-keys"
relay=()
[ -n "$PUBLIC_URL" ] && relay=(--relay-url "$PUBLIC_URL") && BUNDLER="$PUBLIC_URL/bundler"
prof=()
[ "$PROFILE" = public ] && prof=(--contract-set public)
TERNCHAIN_NET="" "$T" deploy --rpc "$RPC" --deployer-key keys/deployer.key --net deploy/net.json --launch \
  --name "$NAME" "${zk[@]}" --shared --operator "$OPERATOR" --contact "$CONTACT" ${prof[@]+"${prof[@]}"} \
  --host "$HOST" --entry-point canonical --settle usdc --erc8004 official ${gas[@]+"${gas[@]}"} \
  --bundler "$BUNDLER" ${relay[@]+"${relay[@]}"} --rpcs "$RPCS" --log-range 10000 --explorer "$EXPLORER" \
  --da-stake 0.001 --da-bond 0.0001 | tee deploy/deploy.json
DEPLOYER=$(python3 -c 'import json; print(json.load(open("deploy/deploy.json"))["deployer"])' 2>/dev/null || true)
TERNCHAIN_NET="$PWD/deploy/net.json" "$T" roles $DEPLOYER | tee deploy/roles.json
mkdir -p publish
cp deploy/net.json "publish/net-$NAME.json"
tar czf "publish/zk-keys-$NAME.tar.gz" zk-keys
(cd publish && shasum -a 256 "net-$NAME.json" "zk-keys-$NAME.tar.gz" > "SHA256SUMS-$NAME")
sed -e "s|@NAME@|$NAME|g" -e "s|@PRICE@|$RELAY_PRICE|g" "$(dirname "$0")/relay.service.in" > "deploy/tern-$NAME-relay.service" 2>/dev/null || true
echo "publish/ holds what goes next to the site's downloads; deploy/tern-$NAME-relay.service is the relay unit"
echo "deploy/net.operator.json holds the deployer's private key (mode 0600): never publish or commit it"
