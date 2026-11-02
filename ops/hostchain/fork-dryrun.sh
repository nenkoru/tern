#!/usr/bin/env bash
# The Base mainnet deploy, rehearsed on a local fork of Base mainnet: no real ETH or USDC moves.
#
#   anvil --fork-url https://mainnet.base.org --chain-id 8453 --optimism --block-time 1 &
#   T=/path/to/ternchain-ops U=/path/to/ternchain CONTRACTS=/path/to/checkout/contracts WORK=/tmp/tern-fork \
#     ops/hostchain/fork-dryrun.sh
#
# It funds a throwaway deployer on the fork (anvil_setBalance), runs deploy.sh HOST=base against it, starts
# a local ERC-4337 bundler (the fork can't reach Pimlico's), funds three newcomers' accounts from a USDC
# holder it impersonates (Aave's aBasUSDC), and runs: owner init with gas in USDC through Circle Paymaster,
# mail on chain, a 0.1 USDC ERC-8183 job (escrow), a review of it (also ERC-8004 feedback), and the caps.
# Needs Foundry's cast, python3, node. Prints the deploy's gas and its ETH at the fork's gas price.
set -euo pipefail
: "${T:?T=path to ternchain-ops}"
: "${U:?U=path to ternchain}"
: "${WORK:?WORK=an empty folder for the run}"
RPC="${RPC:-http://127.0.0.1:8545}"
CAST="${CAST:-cast}"
USDC=0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913
WHALE="${WHALE:-0x4e65fE4DbA92790696d040ac24Aa414708F5c0AB}" # Aave v3 aBasUSDC: holds millions of USDC on Base
BPORT="${BPORT:-8665}"
[ "$($CAST chain-id --rpc-url "$RPC")" = 8453 ] || { echo "$RPC is not a fork of Base mainnet (chain 8453)" >&2; exit 2; }
SCRIPTS="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$WORK/home/keys" && chmod 700 "$WORK/home" "$WORK/home/keys"
cd "$WORK"
step() { printf '\n== %s\n' "$*"; }
addr() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["address"])' "$1"; }
fund_eth() { $CAST rpc anvil_setBalance "$1" 0x8AC7230489E80000 --rpc-url "$RPC" >/dev/null; } # 10 ETH, on the fork only

if [ ! -f home/deploy/net.json ]; then # a finished deploy is reused (run again: only the smoke test)
  step "deployer (throwaway, funded on the fork only)"
  [ -f home/keys/deployer.key ] || "$T" owner keygen --out home/keys/deployer.key > deployer.json
  [ -f deployer.json ] || { echo "home/keys/deployer.key without deployer.json: start in an empty WORK" >&2; exit 2; }
  fund_eth "$(addr deployer.json)"
  step "deploy.sh HOST=base"
  HOST=base CONFIRM_MAINNET=yes T="$T" CONTRACTS="${CONTRACTS:?CONTRACTS=the contracts dir of a checkout}" HOME_DIR="$WORK/home" \
    RPC="$RPC" RPCS="$RPC" BUNDLER="http://127.0.0.1:$BPORT/bundler" OPERATOR="nobody: no operator" CONTACT="the ternchain community" \
    "$SCRIPTS/deploy.sh"
fi
NET="$WORK/home/deploy/net.json"
export TERNCHAIN_NET="$NET"

step "a local bundler (relay serve --bundler-key)"
[ -f bundler.key ] || "$T" owner keygen --out bundler.key > bundler.json
fund_eth "$(addr bundler.json)"
"$T" relay serve --listen "127.0.0.1:$BPORT" --bundler-key bundler.key --net "$NET" > bundler.log 2>&1 &
BPID=$!
trap 'kill $BPID 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -s -o /dev/null -X POST "http://127.0.0.1:$BPORT/bundler" -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' && break; sleep 1; done

give_usdc() { # $1 address, $2 USDC (6 decimals)
  $CAST rpc anvil_impersonateAccount "$WHALE" --rpc-url "$RPC" >/dev/null
  fund_eth "$WHALE"
  $CAST send "$USDC" 'transfer(address,uint256)' "$1" "$2" --from "$WHALE" --unlocked --rpc-url "$RPC" >/dev/null
  $CAST rpc anvil_stopImpersonatingAccount "$WHALE" --rpc-url "$RPC" >/dev/null
}
init_owner() { # $1 name: the first run asks for USDC in the account, then registers
  local n=$1 acct
  if ! "$U" owner init --dir "id/$n" --name "$n" --guardian-dir "g/$n" > "init-$n.json" 2> "init-$n.err"; then
    acct=$(cat "init-$n.json" "init-$n.err" | grep -oE 'account 0x[0-9a-fA-F]{40}' | head -1 | cut -d' ' -f2)
    [ -n "$acct" ] || { cat "init-$n.json" "init-$n.err"; echo "owner init $n: no account address asked for" >&2; exit 1; }
    give_usdc "$acct" 20000000 # 20 USDC: anvil suggests a 1 gwei tip (Base: ~0.001), so the paymaster's worst-case prefund here is ~150x a real one; it gives back the rest
    "$U" owner init --dir "id/$n" --name "$n" --guardian-dir "g/$n" > "init-$n.json"
  fi
  acct=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["address"])' "init-$n.json")
  echo "$n: $acct, ETH $($CAST balance "$acct" --rpc-url "$RPC"), USDC left $($CAST call $USDC 'balanceOf(address)(uint256)' "$acct" --rpc-url "$RPC")"
}
step "owner init x3, gas in USDC through Circle Paymaster (no ETH)"
for n in ann1 ben1 cat1; do init_owner $n; done
step "mail on chain"
"$U" send --dir id/ann1 --to ben1 "hello from a mainnet fork"
"$U" inbox --dir id/ben1 | tee inbox-ben1.json
grep -q "hello from a mainnet fork" inbox-ben1.json || { echo "ben1 did not get the letter" >&2; exit 1; }
step "a 0.1 USDC ERC-8183 job: ann1 pays ben1, cat1 evaluates"
"$U" bank deposit --dir id/ann1 1
JOB=$("$U" job create --dir id/ann1 ben1 cat1 0.1 a logo | tee job.json | python3 -c 'import json,sys; print(json.load(sys.stdin)["job"])')
LOCK=$("$U" job fund --dir id/ann1 "$JOB" | tee fund.json | python3 -c 'import json,sys; print(json.load(sys.stdin)["lock"])')
"$U" job submit --dir id/ben1 "$JOB" ipfs://logo-v1
"$U" job complete --dir id/cat1 "$JOB" | tee complete.json
"$U" job show --dir id/ann1 "$JOB" | tee show.json
step "the review (ERC-8004 feedback too)"
"$U" erc8004 register --dir id/ben1 "a logo maker" | tee erc8004-ben1.json
"$U" attest --dir id/ann1 ben1 review "great logo, on time" --score 90 --lock "$LOCK" | tee review.json
step "the caps: one payment over 5 USDC, a bank over 20 USDC"
if "$U" bank pay --dir id/ann1 ben1 6 > cap1.txt 2>&1; then echo "a 6 USDC payment went through" >&2; exit 1; fi
grep -q "caps one payment" cap1.txt && echo "refused: $(head -c 160 cap1.txt)"
step "cost"
python3 - "$WORK/home/deploy/deploy.json" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
print("deploy gas", d.get("gas_used"), "ETH spent on the fork", d.get("deployer_spent"), "contracts", d.get("contracts"))
EOF
echo "gas price now: $($CAST gas-price --rpc-url "$RPC") wei"
echo "dry run done: $WORK"
