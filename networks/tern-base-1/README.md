# tern-base-1: ternchain on Base mainnet (not deployed yet)

> [!CAUTION]
> **Real money. Unaudited. A few dollars at most.**
> tern-base-1 would put ternchain's public contract set on Base mainnet (chain 8453), where USDC and ETH are worth real money. Nobody has audited these contracts. Once they are deployed, nobody can change, pause or upgrade them, and nobody can give back funds that a bug locks. The only guard rails are in the client, not in the contracts.

This folder is the owner's runbook. It holds no net.json yet: the owner deploys, then commits it here (step 5). Until then the network does not exist.

> [!IMPORTANT]
> **Blocked until the next contract version.** The pre-release review ([docs/AUDIT.md](../../docs/AUDIT.md)) found two high-severity bugs in the current public set: TernPaymaster can be drained, and an owner account's mail key can sign USDC transfers. Don't deploy this set to mainnet. tern-base-1 is deployed only on the next contract version, which fixes them.

## What gets deployed

The same public profile as tern-base-sepolia-1 (`docs/base-sepolia.md`):

- **Contracts.** The public set (`--contract-set public`). No shielded pool (PrivacyPool), no stealth payments (StealthPay), no pots, no fiat, no coin house.
- **Nobody's.** No founders. The deployer gets zero roles, and the deploy checks it (`deploy/roles.json` must say `deployer_roles 0`, `citizens 0`). With no citizens nobody can propose in Parliament, so nobody can change anything.
- **Gas.** Each user pays their own, in USDC through Circle Paymaster or in ETH from their own key. Nobody sponsors anyone.
- **Fees.** Mail is on chain through `InboxForwarder` at the fee's floor (1 wei an envelope, from the forwarder's 1e12-wei seed). Community post fees stay at zero: the public network doesn't offer `commons fee`.
- **Mail and bundler.** No relay. Pimlico's public bundler (no key, not ours).

## Mainnet parameters, checked on chain

Every address below was checked on 2026-11-02 against https://mainnet.base.org with `eth_getCode` and a call. `ternchain-ops deploy --host base` refuses an RPC of another chain and checks each address again.

| what | address | source | on-chain check |
|---|---|---|---|
| USDC (Circle, native) | `0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913` | [Circle: USDC contract addresses](https://developers.circle.com/stablecoins/usdc-contract-addresses) | 1,852 bytes of code (a proxy); `name()` "USD Coin", `symbol()` "USDC", `decimals()` 6, `version()` "2" |
| ERC-4337 EntryPoint v0.8 | `0x4337084D9E255Ff0702461CF8895CE9E3b5Ff108` | [eth-infinitism/account-abstraction v0.8.0](https://github.com/eth-infinitism/account-abstraction/releases/tag/v0.8.0) (its deterministic address) | 21,738 bytes; `eip712Domain()` "ERC4337" "1" |
| Circle Paymaster v0.8 | `0x0578cFB241215b77442a541325d6A4E6dFE700Ec` | [Circle Paymaster: addresses and events](https://developers.circle.com/paymaster/addresses-and-events) (the same on every mainnet) | proxy; `entryPoint()` the v0.8 EntryPoint, `token()` USDC; staked on the EntryPoint (0.25 ETH), 0.038 ETH deposit |
| ERC-8004 Identity Registry | `0x8004A169FB4a3325136EB29fA0ceB6D2e539a432` | [erc-8004/erc-8004-contracts](https://github.com/erc-8004/erc-8004-contracts) | `getVersion()` "2.0.0" |
| ERC-8004 Reputation Registry | `0x8004BAa17C55a88189AE136b182e5fdA19dE9b63` | same | `getVersion()` "2.0.0" |

The services:

| what | value | checked |
|---|---|---|
| bundler | `https://public.pimlico.io/v2/8453/rpc` (Pimlico's public endpoint, no key) | `eth_chainId` 0x2105; `eth_supportedEntryPoints` lists `0x4337…Ff108`; `pimlico_getUserOperationGasPrice` answers |
| RPCs | `https://base-rpc.publicnode.com` (deploy and net.json's first), `https://base.drpc.org`, `https://mainnet.base.org` | all answer chain 8453. publicnode serves 10,000-block log queries; drpc says its limit and the client narrows to it; mainnet.base.org rate-limits hard. Tenderly's keyless endpoint allows only 1,000 blocks, so it is left out. |
| explorer | https://base.blockscout.com (shows Sourcify matches), https://basescan.org | |
| x402 facilitator | none by default: `x402 serve` settles payments itself | x402.org's facilitator lists only test networks (`/supported`: `eip155:84532`, no 8453). Coinbase's CDP facilitator needs an API key. PayAI's (`https://facilitator.payai.network`) lists `eip155:8453` without a key, within a free allowance; a service may pick it with `--facilitator URL` |

Two things differ from Base Sepolia:

- **USDC's EIP-712 name.** It is "USD Coin" on mainnet and "USDC" on Base Sepolia. The client now reads the token's own name and version and checks them against its `DOMAIN_SEPARATOR()`. With the old hard-coded "USDC", every permit for Circle Paymaster failed on the fork (`AA33 … transfer amount exceeds allowance`).
- **Circle Paymaster's fee.** Circle's docs ([Circle Paymaster](https://developers.circle.com/paymaster)) list a 10% surcharge on gas on Base and Arbitrum. The table below includes it.

## Launch safety, without an owner

The contracts can't be capped or paused (contracts/ is frozen, and nobody holds a role). What the network does have:

- **Caps in net.json.** One payment, lock, escrow, deposit, job budget or x402 payment can be at most **5 USDC** (or 0.002 ETH). A deposit may not take a name's bank over **20 USDC** (or 0.005 ETH). The client refuses before it signs: `this network caps one payment, lock, deposit or job at 5.000000 USDC …`.
  - These caps live in the client, not on chain. A modified client, or a direct contract call, can go past them. Money sent straight to an account address is not capped either.
  - `deploy --host base` refuses to run without `--cap-per-payment` and `--cap-per-name`. `deploy.sh` sets them (`CAP_PER_PAYMENT`, `CAP_PER_NAME`, `CAP_COIN_PER_PAYMENT`, `CAP_COIN_PER_NAME`).
- **A loud notice.** net.json's notice starts with "UNAUDITED, REAL MONEY: don't put more than a few dollars in". It says that nobody can rescue locked funds and that the caps are client-side.
- **A first-run warning.** `owner init` on chain 8453 prints `!!! REAL MONEY, UNAUDITED CONTRACTS … Don't put more than a few dollars in …` on stderr. It also puts the warning in its JSON (`warning`) and in the "send it at least 1 USDC" message a newcomer sees first.
- **Refusals in deploy.** On a mainnet host, deploy refuses `--test-network`, any contract set other than `public`, founders, and a sponsor. A known `--host` must match the RPC's chain id.
- **The secret-ballot zk keys** come from the deployer alone (`ceremony solo`). On a mainnet that is allowed only for the public set, with `--solo-zk`, and net.json says so (`zk_solo: true`, and in the notice).
  - The public set has no shielded pool, and with no citizens Parliament can't take proposals. So forged ballots can't move the treasury or change the contracts.
  - They could forge a group's own secret ballot. Don't let one decide anything of value.

## Dry run on a mainnet fork (2026-11-02)

`ops/hostchain/fork-dryrun.sh` ran the whole thing on `anvil --fork-url https://mainnet.base.org --chain-id 8453` (Foundry 1.8.7), from fork block 53,415,505. No real funds moved. The deployer was a throwaway key funded with `anvil_setBalance`.

1. **Deploy.** `deploy.sh HOST=base` ran in 89 s: 47 contracts named in net.json, 44 of them deployed (the EntryPoint and the two ERC-8004 registries are Base's), in 86 transactions. `ternchain roles`: `deployer_roles 0`, `citizens 0`.
2. **owner init x3, gas in USDC.** ann1, ben1 and cat1 each paid for their own account and name in USDC through Circle Paymaster, with a local bundler (`relay serve --bundler-key`; the fork can't reach Pimlico's). USDC came from Aave's aBasUSDC, impersonated. Every account ended with 0 ETH.
3. **Mail on chain.** ann1 wrote to ben1, and the letter was in ben1's inbox.
4. **Escrow.** A 0.1 USDC ERC-8183 job: ann1 deposited 1 USDC, created and funded it (Bank lock 0), ben1 submitted, and cat1 completed it, which paid ben1.
5. **Review.** ben1 registered in the ERC-8004 Identity Registry (agent 1150893). ann1's review of the deal (score 90, lock 0) went in as an attestation and as ERC-8004 Reputation feedback.
6. **Caps.** `bank pay ben1 6` was refused by the client.

Rerun it before deploying for real. It takes a few minutes, plus the zk ceremony the first time:

```sh
anvil --fork-url https://mainnet.base.org --chain-id 8453 --optimism --block-time 1 &
T=$PWD/ternchain-ops U=$PWD/ternchain CONTRACTS=$PWD/contracts WORK=$(mktemp -d) ops/hostchain/fork-dryrun.sh
```

anvil suggests a 1 gwei tip where Base's is about 0.001 gwei. So on the fork everything costs about 170 times what it costs on Base, and the script gives each account 20 USDC for the paymaster's worst-case prefund. The figures below are gas measured on the fork, priced at Base's gas price.

## What the deploy costs

| | |
|---|---|
| gas (measured on the fork) | 73,202,630 over 86 transactions (Base Sepolia's deploy was 72,057,070) |
| Base mainnet gas price (2026-11-02) | 0.006 gwei (base fee 0.005, tip 0.001) |
| L2 execution | 0.000439 ETH |
| L1 data fee (GasPriceOracle `getL1Fee` over the 319 kB of calldata) | 0.000001 ETH |
| InboxForwarder's seed | 0.000001 ETH |
| **total** | **0.00044 ETH, about $1.00** (ETH $2,283, Chainlink ETH/USD on Base) |
| **fund the deployer with** | **0.00066 ETH** (+50%). Round up to **0.0007** |

If Base's gas price is higher on the day, fund 73.2M × the gas price × 1.5. A deploy that runs out of gas stops cleanly. Send more and run the same command again: it goes on from `deploy/net.deploy.json`.

## Owner runbook

Do every step on your own machine. Never paste the key anywhere. Nothing here needs anyone else's server except the public RPC, which only sees signed transactions.

### 0. Build, from this branch

```sh
git switch launch/base-mainnet   # or the release it merges into
go build -tags ops -o ternchain-ops ./cmd/ternchain && go build -o ternchain ./cmd/ternchain
(cd contracts/.solc && npm ci)   # solc 0.8.29, for the zk verifiers (build-zk.js)
```

Optional, and worth it: the fork rehearsal above.

### 1. A fresh deployer key

```sh
mkdir -p ~/tern-base-1/keys && chmod 700 ~/tern-base-1 ~/tern-base-1/keys
./ternchain-ops owner keygen --out ~/tern-base-1/keys/deployer.key
```

It prints the address and writes the key with mode 0600. Use a key that has never sent a transaction, and use it for nothing else.

### 2. Fund it with 0.0007 ETH on Base

Send **0.0007 ETH on Base mainnet** (chain 8453, not Ethereum mainnet) to the printed address, from an exchange that withdraws to Base or through the Base bridge. Check that it arrived on https://basescan.org/address/ADDRESS. That is 0.00044 ETH of deploy plus a 50% buffer. If the gas price is above 0.006 gwei, see "What the deploy costs" above.

### 3. The one command

```sh
HOST=base CONFIRM_MAINNET=yes T=$PWD/ternchain-ops CONTRACTS=$PWD/contracts HOME_DIR=~/tern-base-1 \
  OPERATOR="nobody: no operator" CONTACT="the ternchain community" ops/hostchain/deploy.sh
```

- **What it does.** It runs the zk ceremony (`ceremony solo`, a few minutes), deploys about 86 transactions, writes `~/tern-base-1/deploy/net.json`, and checks the roles (`deploy/roles.json`).
- **Check the roles.** `deploy/roles.json` must say `"deployer_roles": "0"` and `"citizens": 0`. If it doesn't, stop: don't publish.
- **If it stops** (a dropped RPC, gas too low): run the same command again.
- **Never publish `deploy/net.operator.json`.** It holds the deployer's private key (mode 0600). The leftover ETH, about 0.0002, can be swept out; the key holds no power.

### 4. Verify the sources

```sh
node contracts/verify.js ~/tern-base-1/deploy/net.json sourcify ~/tern-base-1/zk-keys
```

It should report every contract the deploy made as a match on Sourcify (chain 8453). Blockscout (https://base.blockscout.com) shows Sourcify's matches.

Basescan needs an Etherscan API key. `contracts/verify.js` appends its query to the URL it is given, so it can't carry `?chainid=8453&apikey=…` yet, and contracts/ is frozen on this branch. Until that is fixed, verify on Basescan by hand ("Verify & Publish", Standard JSON input from `build.js`'s settings), or leave Sourcify as the record.

### 5. Commit net.json

```sh
cp ~/tern-base-1/publish/net-tern-base-1.json networks/tern-base-1/net.json
mkdir -p networks/tern-base-1/zk-keys
cp ~/tern-base-1/zk-keys/{CEREMONY,VoteVerifier.sol,transcript.mpc,vote.pk,vote.vk} networks/tern-base-1/zk-keys/
(cd networks/tern-base-1 && shasum -a 256 net.json zk-keys/* > SHA256SUMS)
git add networks/tern-base-1 && git commit -m "networks: tern-base-1 on Base mainnet"
```

- **Check net.json before committing.** It needs `"chain_id": 8453`, `"host": "base"`, `"caps"`, `"zk_solo": true`, and a notice that starts "UNAUDITED, REAL MONEY".
- **What must never be in git:** `keys/`, `deploy/net.operator.json`, or anything else from `~/tern-base-1` that isn't listed above.
- **Then:** put the same files in the GitHub release, as for tern-base-sepolia-1.

### 6. Announce

Say what it is and what it isn't. A draft:

> tern-base-1 is live on Base mainnet: ternchain's public contracts, owned by nobody (the deployer holds no role: `ternchain roles`). They are **unaudited**. Nobody can pause them or rescue funds. The client refuses more than 5 USDC at once and more than 20 in a bank. Put in a few dollars at most. Gas is paid in USDC through Circle Paymaster. Contracts are verified on Sourcify: LINK. net.json: LINK.

Don't call it audited, safe, production-ready or a launch of anything with value. There is still no token.

## What it costs users

Gas measured on the fork, priced at Base mainnet's 0.006 gwei, ETH at $2,283 and Circle Paymaster's 10% surcharge. A public bundler's gas price and its L1 data share add a little. Numbers move with Base's gas price.

| operation | gas | ETH | USDC (with Circle's 10%) |
|---|---|---|---|
| owner init (makes the account, takes the name: 3 operations) | 1,710,538 | 1.03e-05 | 0.026 |
| send a letter (on chain) | 349,835 | 2.10e-06 | 0.005 |
| bank deposit (approve + deposit) | 595,157 | 3.57e-06 | 0.009 |
| job create (create + budget) | 683,896 | 4.10e-06 | 0.010 |
| job fund (escrow lock + fund) | 873,210 | 5.24e-06 | 0.013 |
| job submit (the provider) | 285,545 | 1.71e-06 | 0.004 |
| job complete (the evaluator) | 363,290 | 2.18e-06 | 0.006 |
| erc8004 register | 795,985 | 4.78e-06 | 0.012 |
| a review of a paid deal (attestation + ERC-8004 feedback) | 1,058,485 | 6.35e-06 | 0.016 |

- **The first step needs a prefund.** `owner init` asks for at least 1 USDC in the account. Circle Paymaster takes a prefund for the worst case and gives back what isn't used.
- **The whole run** (three owners, a letter, a job, a review) used about 10.1M gas, about $0.15 at these prices.

## Not tried on the fork

- `x402 serve` settling by itself on mainnet (`--facilitator self`) and `x402 pay` against it. The USDC domain fix covers its signatures, but no run has paid one yet.
- Pimlico's public bundler with these contracts on mainnet. The fork used a local one. Base Sepolia's network uses Pimlico's in the same way.
