#!/usr/bin/env bash
# Coordinator: make the testnet genesis (BOOTSTRAP-CHECKLIST.md, step 5).
#
#   ops/testnet/10-make-genesis.sh [--validators validators.txt] [--founders founders.txt]
#       [--out genesis.json] [--deployer-key deployer.key] [--faucet 0xADDR | --faucet-key faucet.key]
#
#   validators.txt   one line per validator:  NAME=0xNODE_ADDRESS   (from 00-validator-init.sh)
#   founders.txt     one line per founder:    NAME=0xOWNER_ADDRESS  (from `ternchain owner keygen`)
#                    every validator's NAME must be a founder: each validator is bound to its citizen
#   deployer.key     a NEW key (made if missing). It gets only DEPLOY_COINS of gas; it must
#                    have sent nothing before `deploy` (the Treasury address depends on its nonce).
#   faucet           the testnet faucet's address; with --faucet-key a key is made if missing
#
# The allocation plan (no premine to the deployer; override with the variables):
#   SUPPLY=1000000000  DEPLOY_COINS=100  FOUNDER_COINS=1000 each  VALIDATOR_COINS=10 each
#   FAUCET_COINS=10000000  the rest: the Treasury (spent only by the timelock, both houses)
#   CHAIN_ID=83760 (testnet). No key but the Treasury may hold more than 1% of the supply.
#
# Writes: genesis.json, genesis.out.json (the command's report), genesis.keccak, genesis.sha256.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

VALS=validators.txt
FOUNDERS=founders.txt
OUT=genesis.json
DEPLOYER_KEY=deployer.key
FAUCET=""
FAUCET_KEY=faucet.key
CHAIN_ID="${CHAIN_ID:-$TESTNET_CHAIN_ID}"
SUPPLY="${SUPPLY:-1000000000}"
DEPLOY_COINS="${DEPLOY_COINS:-100}"
FOUNDER_COINS="${FOUNDER_COINS:-1000}"
VALIDATOR_COINS="${VALIDATOR_COINS:-10}"
FAUCET_COINS="${FAUCET_COINS:-10000000}"

while [ $# -gt 0 ]; do
  case "$1" in
    --validators) VALS="$2"; shift 2 ;;
    --founders) FOUNDERS="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --deployer-key) DEPLOYER_KEY="$2"; shift 2 ;;
    --faucet) FAUCET="$2"; shift 2 ;;
    --faucet-key) FAUCET_KEY="$2"; shift 2 ;;
    -h | --help) sed -n '2,22p' "$0"; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
need python3
REPORT="${OUT%.json}.out.json"

step "Inputs"
V="$(read_pairs "$VALS")"
F="$(read_pairs "$FOUNDERS")"
python3 -I - "$V" "$F" <<'EOF'
import sys
vals = [p.split("=", 1) for p in sys.argv[1].split(",")]
fnd = [p.split("=", 1) for p in sys.argv[2].split(",")]
err = []
def dups(rows, what):
    seen = {}
    for n, a in rows:
        for key, label in ((n, "name"), (a.lower(), "address")):
            if key in seen:
                err.append("%s: %s %s twice" % (what, label, key))
            seen[key] = 1
dups(vals, "validators.txt")
dups(fnd, "founders.txt")
fnames = {n for n, _ in fnd}
for n, _ in vals:
    if n not in fnames:
        err.append("validator %s is not a founder: add %s=0xOWNER to founders.txt (one validator per citizen)" % (n, n))
shared = {a.lower() for _, a in vals} & {a.lower() for _, a in fnd}
for a in shared:
    err.append("%s is both a node address and an owner address: use two keys (owner keygen, validator init)" % a)
if len(vals) < 4:
    err.append("%d validators: QBFT needs at least 4" % len(vals))
print("validators: %d (%s)" % (len(vals), ", ".join(n for n, _ in vals)))
print("founders:   %d" % len(fnd))
if len(vals) < 7:
    print("WARN: fewer than 7 validators: the chain survives only %d down" % ((len(vals) - 1) // 3))
if len(fnd) < 2 * len(vals):
    print("WARN: %d founders for %d validators: aim for at least twice as many citizens (L1-READINESS G5)" % (len(fnd), len(vals)))
if err:
    print("\n".join("ERROR: " + e for e in err), file=sys.stderr)
    sys.exit(1)
EOF

step "Keys"
if [ -f "$DEPLOYER_KEY" ]; then
  warn "$DEPLOYER_KEY exists: it must be a fresh key that never sent a transaction (deploy checks its nonce is 0)"
else
  say "$DEPLOYER_KEY: made by the genesis command (keep it offline until deploy, then destroy it)"
fi
if [ "$CHAIN_ID" = 8376 ]; then
  die "chain 8376 is mainnet: no faucet there. This script is for the testnet."
fi
if [ -z "$FAUCET" ]; then
  if [ -f "$FAUCET_KEY.addr" ]; then
    FAUCET="$(cat "$FAUCET_KEY.addr")"
  elif [ -f "$FAUCET_KEY" ]; then
    die "$FAUCET_KEY exists without $FAUCET_KEY.addr: pass --faucet 0xADDR"
  else
    FAUCET="$("$T" owner keygen --out "$FAUCET_KEY" --name faucet | jget - 'd["address"]')"
    printf '%s\n' "$FAUCET" >"$FAUCET_KEY.addr"
    say "made $FAUCET_KEY (0600) for the faucet: $FAUCET"
  fi
fi
printf '%s' "$FAUCET" | grep -Eq '^0x[0-9a-fA-F]{40}$' || die "faucet address '$FAUCET' is not an address"

step "Genesis"
"$T" validator genesis --validators "$V" --out "$OUT" --deployer-key "$DEPLOYER_KEY" \
  --chain-id "$CHAIN_ID" --supply "$SUPPLY" --deploy-coins "$DEPLOY_COINS" \
  --founders "$F" --founder-coins "$FOUNDER_COINS" --validator-coins "$VALIDATOR_COINS" \
  --faucet "$FAUCET" --faucet-coins "$FAUCET_COINS" >"$REPORT"
say "wrote $OUT and $REPORT"

python3 -I - "$REPORT" "$SUPPLY" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
supply = float(sys.argv[2])
rows = d.get("allocation") or []
if not isinstance(rows, list):
    print("ERROR: allocation: %s (this script is for the plan, not --devnet)" % rows, file=sys.stderr)
    sys.exit(1)
print("%-12s %-44s %16s  %s" % ("who", "address", "coins", "why"))
bad = []
for r in rows:
    coins = float(str(r.get("coins", 0)).replace(",", ""))
    print("%-12s %-44s %16s  %s" % (r.get("who", ""), r.get("address", ""), r.get("coins", ""), r.get("why", "")))
    who = str(r.get("who", "")).lower()
    if "treasury" not in who and coins > supply / 100:
        bad.append("%s holds %s coins: more than 1%% of supply" % (r.get("who"), r.get("coins")))
print("treasury: %s gets %s coins; all keys together: %s coins (%s of supply)" % (
    d.get("treasury"), d.get("treasury_coins"), d.get("keys_coins"), d.get("keys_share")))
print("chain id:", d.get("chain_id"), " deployer:", d.get("deployer"))
if not rows:
    print("WARN: no 'allocation' in the report: is this ternchain build older than the allocation plan?")
if bad:
    print("\n".join("ERROR: " + b for b in bad), file=sys.stderr)
    sys.exit(1)
EOF

step "Hashes"
K_CMD="$(jget "$REPORT" 'd["keccak"]')"
K_FILE="$(keccak_file "$OUT")"
[ "$K_CMD" = "$K_FILE" ] || die "keccak mismatch: command $K_CMD, file $K_FILE"
SHA="$(sha256_file "$OUT")"
printf '%s\n' "$K_FILE" >"${OUT%.json}.keccak"
printf '%s  %s\n' "$SHA" "$(basename "$OUT")" >"${OUT%.json}.sha256"
say "keccak: $K_FILE"
say "sha256: $SHA"

step "Next"
say "1. Publish $OUT, its keccak and sha256 in the operators' channel AND a second place (site, git tag)."
say "   Each validator checks the keccak before starting (20-validator-run.sh --keccak $K_FILE)."
say "2. Keep $DEPLOYER_KEY and $FAUCET_KEY offline. Do not use the deployer key for anything but 30-deploy.sh."
