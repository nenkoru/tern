# The closed multi-machine testnet

Phase 1 of `L1-READINESS.md` (internal review, not yet published) §2: a ternchain L1 on Besu QBFT, run by independent people on their own machines, for 4–6 weeks. Chain id **83760**. Coins have no value and never become mainnet coins.

## Roles

| Role | How many | Who | Runs |
|---|---|---|---|
| Coordinator | 1 | the organiser | the genesis, the deploy, `net.json`, the channel. Holds no power over the chain after the deploy. Also runs one validator, like everyone else. |
| Validators | 4–7 (aim: 7) | independent people (vetting: L1-READINESS §3.5) | one sealing node each, bound to their citizen name |
| Archive node | 1 | the coordinator, or a volunteer | a FOREST node (`--archive`) from day one: the source for `fork-from-state` and the explorer. On the same box: Blockscout, monitoring, the data node |
| RPC nodes | 2 | different people if possible | public RPC (`--profile rpc`) behind Caddy |
| Bootnodes | 3 | different people | small non-sealing nodes with fixed IPs |
| Citizens | 15+ (2× the validators) | founders who do not run validators | vote, sign checkpoints |

Rules from the plan: at most 2 validators per host and per country; nobody sharing an employer or a family; one validator per citizen.

## Files

| File | For | What |
|---|---|---|
| `BOOTSTRAP-CHECKLIST.md` | coordinator | the ordered steps from people to a running testnet, drills and exit criteria |
| `VALIDATOR.md` | each validator | requirements, firewall, the three commands, checks, monitoring, upgrades, what to send back |
| `00-validator-init.sh` | each node | makes the node key; prints `NAME=0xADDR` and the enode to send |
| `10-make-genesis.sh` | coordinator | `validators.txt` + `founders.txt` → `genesis.json` with the allocation plan; prints the keccak |
| `20-validator-run.sh` | each node | checks the genesis keccak, starts the node (Docker, `--metrics`, `--restart unless-stopped`) |
| `30-deploy.sh` | coordinator | `deploy --launch` with the faucet key, the RPC list and the bootnodes; writes `net.json` |
| `40-check.sh` | anyone | is it healthy: chain id, block 0, sealing, peers, rounds, every RPC |
| `keccak256.py` | anyone | the keccak of a file (as `validator genesis` prints it), plain Python |
| `lib.sh` | the scripts | shared helpers |

All scripts: bash, `set -euo pipefail`, the binary from `${TERNCHAIN:-ternchain}`, `--help` for their options. The operator commands they run (`validator`, `deploy`, `faucet`, `relay serve`) live in `ternchain-ops`: take `ternchain-ops-VERSION-OS-ARCH` from the release (next to `ternchain-…`) or build it with `go build -tags ops -o ternchain-ops ./cmd/ternchain`, and put it beside `ternchain` as plain `ternchain-ops` (plain `ternchain` forwards operator commands to it), or set `TERNCHAIN=/path/to/ternchain-ops`.

## The flow in one picture

```
people ──00-validator-init──▶ NAME=0xADDR + enode ──▶ coordinator
founders ──owner keygen──▶ NAME=0xOWNER ───────────▶ coordinator
coordinator ──10-make-genesis──▶ genesis.json + keccak (published twice)
bootnodes, then validators, archive, RPCs ──20-validator-run──▶ chain makes blocks
coordinator ──30-deploy──▶ contracts + net.json (rpcs, bootnodes, genesis_hash)
coordinator ──faucet serve──▶ https://HOST/faucet (net.json faucet_url)
everyone ──40-check──▶ PASS
```

## The allocation plan (genesis)

`ternchain-ops validator genesis` with the plan flags (no premine to the deployer):

| Who | Coins | Why |
|---|---|---|
| deployer | 100 | gas to deploy the contracts (a fresh key, nonce 0) |
| each founder | 1,000 | gas and a vote in the coin house |
| each validator node | 10 | gas for its own transactions |
| faucet | 10,000,000 | testnet only (refused on chain 8376) |
| Treasury contract | the rest | spent only by the timelock (both houses) |

Total `--supply 1000000000`. No key but the Treasury may hold more than 1% (10,000,000). The Treasury's address is predicted from the deployer's address at nonce 3, so the deployer must send nothing before `deploy`.

## The validator set: contract or header

`validator genesis --selection contract|header` (default `contract`):

- `contract` (this plan, and the mainnet): the Validators contract is in the genesis, each validator bound to its founder; `deploy` hands the set to Parliament as its last step. With fewer than 4 founders `deploy` refuses (nobody could ever change the set) unless `--keep-validators`, which leaves the deployer as the Validators bootstrap.
- `header` (an operator-run testnet with no founders): the set is in the genesis header and changes by the validators' own votes (`qbft_proposeValidatorVote`), no contract, no citizens. Grow it from 1 to 4 in one sitting: `VALIDATOR.md`, "Header voting", or `ternchain-ops help validator`.

## net.json from deploy

`ternchain-ops deploy` writes these into the public `net.json`. `30-deploy.sh` doesn't pass them yet: run its deploy line by hand with them (BOOTSTRAP-CHECKLIST step 19).

| flag | net.json | what |
|---|---|---|
| `--faucet-url URL` | `faucet_url` | the public faucet, `https://HOST/faucet` (below) |
| `--relay-url URL` | `relay` | the default data node |
| `--name NAME` | `network` | e.g. `tern-testnet-1` |
| `--operator WHO --contact HOW` | `operator`, `contact` | who runs it and how to reach them |
| `--shared` | `mode: "shared"`, `refuse` | an invited test network: clients refuse real-money features |
| `--test-network` | `test_network: true`, `notice` | an operator-run test network (zk keys from `ceremony solo` allowed; refused on chain 8376) |
| `--notice TEXT` | `notice` | replaces the default notice |

The default `--test-network` notice says the coins have no value, the network may be reset, and the operator made the zk keys alone, so shielded transfers and secret votes are not private from the operator.

## The faucet

Off a devnet the faucet is its own service, run by the coordinator with the genesis faucet key:

```bash
ternchain-ops faucet serve --key faucet.key --listen 127.0.0.1:8660 --net net.json \
  [--faucet-coins 5] [--gas 0.1] [--faucet-per-hour 100] [--faucet-per-ip-day 5] [--trusted-proxy CIDR] [--datadir DIR]
```

- `--net net.json`, or `--rpc URL` with no net.json (it needs no contracts); `--coins` is the same as `--faucet-coins`.
- `POST /faucet {"address":"0x…"}`, the same as the devnet faucet; `GET /` shows its terms as JSON.
- A grant is the coins plus a little gas. One grant per address a day, `--faucet-per-ip-day` per IPv4 address or IPv6 /64 a day, `--faucet-per-hour` for everyone together.
- The key file must be mode 0600 (refused otherwise). Its log: `DIR/faucet.json` with `--datadir`, else `KEYFILE.faucet.json`.
- Put it behind the TLS proxy (Caddy) and pass the proxy's address as `--trusted-proxy`, or every caller looks like the proxy. Deploy with `--faucet-url https://HOST/faucet`.

A newcomer's `owner init` asks it for coins. With none (no faucet, or a cap reached) it keeps the key and guardian folders, prints the address to send to the operator, and the same `owner init` command can be run again: same folders, same address.

## Related

- `ops/besu/`: the pinned Besu, upgrades, the public RPC profile.
- `ops/monitoring/`, `ops/explorer/`, `ops/backup/`, `ops/incident/`.
