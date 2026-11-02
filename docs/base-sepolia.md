# ternchain on Base Sepolia

ternchain runs in three places:

- **tern-testnet-1** is ternchain's own L1 (Besu QBFT, chain 83760). It is the private lab. It has its own validators, a coin of its own and every module, and its contract set is unchanged (`contracts_hash 55034bfd…`).
- **tern-base-sepolia-1** is the public network, on Base Sepolia (chain 84532, a public test L2). It runs the public contract set: nobody runs it, nobody can change it, and users pay their own gas in USDC. Everything below is about this one.
- **tern-base-sepolia** was the first deployment on Base Sepolia, now kept for development, with every module; its gas sponsor is off. Its report is at the end of this page.
- **tern-base-1**, on Base mainnet (chain 8453), is coming but not deployed. The owner deploys it with `ops/hostchain/deploy.sh` (`HOST=base`); `networks/tern-base-1/README.md` is the runbook, with the addresses checked on chain and a dry run on a mainnet fork. It is the same public set, with real USDC on unaudited contracts that nobody can pause or rescue: tiny amounts only. Clients refuse more than net.json's caps (5 USDC at once, 20 in a bank), and the notice and `owner init` say so.

## The public network: nobody runs it

The public contract set is `deploy --contract-set public` (`ops/hostchain/deploy.sh` makes the whole network from a fresh key in one command). The network needs no server of tern's creator, and none it runs itself; it does use Base, a public RPC, an ERC-4337 bundler and Circle's paymaster, all replaceable ([GUARANTEES.md](GUARANTEES.md)):

- **Gas.** Each user pays their own gas, in USDC through Circle Paymaster or in ETH from their own key. The network pays nobody's.
- **Mail.** Letters go on chain directly through `InboxForwarder`. Relays and data nodes are optional and community-run: a client uses one only when told to (`--relay URL`). One that charges, charges the sender in USDC by voucher.
- **Bundler.** Any ERC-4337 bundler works. net.json names Pimlico's public one (no key, not ours), and anyone can run their own (`relay serve --bundler-key`).
- **Contracts and files.** The contracts are verified on Sourcify, and anybody can redeploy them. net.json and the secret-ballot zk keys are in the repo, with checksums (`networks/tern-base-sepolia-1/`: https://raw.githubusercontent.com/nenkoru/tern/main/networks/tern-base-sepolia-1/net.json); the full ceremony and the binaries are in the GitHub release (`.github/workflows/release.yml`: a tag makes a draft, a person publishes it). The network depends on no server of its creator's.

### Roles: who can change what

`ternchain roles 0xDEPLOYER` reads the following from the chain:

| contract | role | holder |
|---|---|---|
| TernTimelock | admin | itself |
| TernTimelock | proposer | Parliament only |
| TernTimelock | executor | anyone (address 0) |
| Directory | owner | the timelock |
| FeeOracle, Citizens, Treasury | governance | the timelock |
| Inbox | `setBank` (once, at deployment) | spent |
| everything else | none (no owner by design) | — |

The contracts with no owner by design are Registry, Bank, Inbox, EAS, SchemaRegistry, AccountFactory, TernPaymaster, TernJobs, ERC8004Registrar, ModuleRegistry, InboxForwarder, NoCoinHouse, Random, Clock and Items.

The deployer holds **none** of these roles, and the deploy proves it (`ternchain roles 0x8eE3…8410`, below).

### Governance: option C, immutable

The public network starts with no founders and no citizens, and with no coin house.

- **No coin house.** `NoCoinHouse` stands in for it and abstains, so the citizens' house would decide alone. With no citizens, Parliament can pass nothing. So nobody can change these contracts, their deployer included.
- **Nothing users need is stuck.**
  - The Inbox fee is at its floor (1 wei), and the InboxForwarder pays it.
  - Gas prices are the chain's.
  - Payments, locks, jobs, ratings, groups and secret ballots in groups work without governance.
  - The fee oracle is never needed: the fee floor is what is charged.
- **New versions are new deployments.** A later version can start with founders drawn from the community. Modules are added without governance (below).

### What the public network leaves out

It leaves out:

- shielded transfers (`PrivacyPool`, `AssociationSets`, `PoolVerifier`)
- stealth payments (`StealthPay`, ERC-5564/6538)
- pots and lotteries
- fiat
- the coin house (`WrappedTern`, `TernGovernor`)

It keeps sealed mail, groups and communities, secret ballots (Semaphore-style), escrow and locks, ratings, fair randomness and game clocks (`Random`, `Clock`), and ERC-8004/8183/x402.

The client reads what a network has from its contracts, not from its name. Where they are missing, it refuses the commands with "isn't on this network". The code stays in the repo, and tern-testnet-1 keeps them all.

### Optional modules, and how to add one

The public network ships without shielded payments. Anyone may add such a module, under their own responsibility. Nobody approves modules, because nobody can:

1. **Deploy** the module's contracts (any contract; level 0).
2. **Publish** it in `ModuleRegistry` under your own name. Each entry is publisher → module name → version, with the contract, its ABI's hash, the client plugin that speaks to it, and a URI to its spec, source and audits. Only the publisher's owner publishes in its namespace, and publishing endorses nothing.
3. **Opt in.** Users opt in per module on their client. An owner trusts the module's contract (`contract trust ADDRESS --note …`), and an agent asks its owner the first time it uses one, as with a new correspondent. Clients show the publisher, the claims about the contract (`contract info`) and a warning for unaudited code.

Payment rails (`IPaymentRail`: a shielded pool, a streaming rail) and proof verifiers (`IProofVerifier`) are opt-in slots the same way: per user or group, never network-wide switches. The core's Bank and locks stay as they are, because changing them would change every running network's contracts.

ternchain's own Privacy Pools code (`contracts/PrivacyPool.sol` and its verifier) stays in the repo as a reference for such a module. It is deployed on tern-testnet-1 only.

### Known limits

- **Community post fees.** They are in the chain's coin (`Commons.PostFee`, a core contract), and users here hold none, so the client doesn't offer `commons fee` on the public network and communities keep the default: none.
- **USDC for a newcomer's first step.** Circle Paymaster takes a prefund for the worst case of the first operation (which makes the account), so `owner init` asks for at least 1 USDC. It gives back what isn't used: the first step costs about 0.05.

## Money: USDC settles, ETH pays gas

The agent payment standards (x402, ERC-8183) are written around ERC-20 USDC, so on Base Sepolia a network coin of our own would only get in the way.

- **Settlement token.** net.json's `settle` names Circle's test USDC: `0x036CbD53842c5426634e7929541eC2318f3dCF7e`, 6 decimals, with EIP-3009 and EIP-2612.
- **What it covers.** Payments (`bank pay`), deposits (`bank deposit`), locks and escrows, pledges, milestones, and agents' spending limits are all in USDC by default. The Bank's per-token limit is the agent's limit. Amounts are read and shown in USDC units, so `bank pay bob 0.5` is half a dollar.
- **Getting test USDC.** Testers take it from https://faucet.circle.com (20 USDC per address every 2 hours, no account).
- **ETH stays the chain's coin.** Envelope fees, a data node's stake and dispute bonds are in Base Sepolia ETH, which ternchain does not print. The public network sets the envelope fee at its floor, paid by `InboxForwarder`. Users pay their own gas in USDC (below), or in ETH from their own key. `--token coin` pays in ETH where someone wants that.
- **Where the floating coin stays.** It stays on tern-testnet-1, where the design needs a network coin: the Treasury, the coin house of governance (wTERN), the fee oracle's basket, and the test faucet. The public network has no coin and no coin house.

## Gas: each user pays their own

The network pays nobody's gas. net.json's `gas` says how users pay.

- **In USDC, through Circle Paymaster** (`0x3BA9A96eE3eFf3A69E2B18886AcF52027EFF8966`, EntryPoint v0.8; `deploy --gas-paymaster circle`):
  - **The default.** `owner init` and `agent init` make ERC-4337 accounts guarded by the identity's own key.
  - **How it pays.** Each operation carries the account's EIP-2612 permit for the paymaster, signed by that key and checked by USDC through ERC-1271. The paymaster takes a prefund in USDC and gives back what wasn't used: about 0.005 USDC an operation, read from the chain.
  - **What a newcomer needs.** Test USDC from Circle's faucet, nothing else. An owner gives its agent's account some with `owner grant … --gas 0.5` (about 100 actions), and `gas show` tells what an account holds.
- **In ETH.** `owner init --key KEYFILE` keeps a plain key that pays its own gas.
- **Bundlers.** Any ERC-4337 bundler. The client takes a public bundler's gas price and estimate (`pimlico_getUserOperationGasPrice`, `eth_estimateUserOperationGas`). net.json names Pimlico's public endpoint.
- **The operator's sponsor, off.** `SponsorPaymaster` (`contracts/ext`, daily caps per name and per network) is kept for networks that want one. The public network has none, and the dev network's is off: its net.json no longer names it.
- **TernPaymaster: not used.** The deployed `TernPaymaster` pools every owner's deposit in one EntryPoint balance, and its `postOp` charges less than the EntryPoint takes. Whoever bundles their own operations can drain the others' deposits (audit core H-1). Both deployed ones hold nothing, and the contract can't change, so the client:
  - refuses `gas fund` (and any deposit into it) except on a local devnet (chain id 1337 or 31337) with `--unsafe-paymaster`;
  - never routes a user operation through it off a devnet. There an account pays its own gas: in USDC through Circle Paymaster, or in ETH it holds. `owner grant --gas` on a network with ETH gas sends the ETH to the agent's account instead of a deposit;
  - lets an owner take a deposit out at any time: `gas withdraw [AMOUNT|all] [--to ADDR]`. `gas show` warns about a deposit left there.

## ERC-8004: identities and deal-based reputation

On Base Sepolia ternchain uses the ERC-8004 registries that are already deployed there (version 2.0.0). net.json's `contracts` names them, and `deploy --erc8004 official` sets that up:

- Identity Registry `0x8004A818BFB912233c491871b3d84c89A494BD9e`
- Reputation Registry `0x8004B663056A597Dffe9eCcC1965A193B7388713`

Devnets deploy look-alikes from `contracts/ext` that have the same calls and events.

- **`ternchain erc8004 register [DESCRIPTION]`** registers a name (an owner's or an agent's) as an ERC-8004 agent.
  - The agent id is an ERC-721 token owned by the name's account. Its URI is the registration file, carried in a `data:` URI with nothing kept off chain. It names the ternchain service (`tern:NAME@eip155:84532`) and the account's address.
  - The ternchain name also goes in the metadata as `tern.name`.
- **A review that cites a paid deal** (`attest NAME review TEXT --lock ID --score S`) is also published as Reputation feedback for NAME's agent id:
  - value = the score (-100..100, 0 decimals)
  - `tag1 = tern-deal`, `tag2 = the lock id`
  - `feedbackURI = tern:lock/eip155:84532:BANK/ID` (the deal is the evidence)
  - `feedbackHash` = the hash of the review

  `erc8004 feedback NAME LOCK SCORE TEXT` publishes one on its own. Only reviews of deals between the two sides are accepted (the same check ternchain's rating makes).
- **Who can give feedback.** The registry refuses feedback from the agent's own owner, so a name cannot review itself there either.
- **Which agent id is a name's.** Anyone can transfer an agent id to a name's address (audit ext M-7). The client counts only an id minted for the name: its own `erc8004.json`, a `Registered` event naming the address as owner, or the Registrar's `RegisteredFor`. The id must still be held by the address, and its `tern.name` metadata (without one, its registration file's name and ternchain service) must be the name. An id that only arrived by transfer is ignored, so feedback never goes to someone else's.
- **Reading it.** ERC-8004 leaves sybil filtering to the reader. `getSummary` counts only the clients the caller names, so `erc8004 show NAME REVIEWER…` asks for the reviewers you trust. ternchain's own `rating` goes further: it counts only reviews that cite paid deals, and it weighs them by deal size.

## ERC-8183: escrowed jobs

`TernJobs` (`contracts/ext`) implements the ERC-8183 job interface over ternchain's escrow. A job has a client, a provider and an evaluator. It moves `Open → Funded → Submitted → Completed | Rejected | Expired`, using the standard's calls (`createJob`, `setProvider`, `setBudget`, `fund`, `submit`, `complete`, `reject`, `claimRefund`, `getJob`) and its events, all in the network's USDC.

- **Funding from the Bank.** `job fund ID PRICE` opens a Bank lock that holds the budget for the provider's name, with `TernJobs` as the lock's condition, and hands the lock to the job.
  - PRICE is the budget you agreed to. Until a job is funded, its provider can change the budget (`setBudget`), and the deployed `TernJobs.fund` takes whatever it is then (audit ext M-6). So the client refuses a budget that differs from PRICE, and reads it again right before it locks and right before it funds. If the budget changes between the lock and the funding, the lock can't fund the job and stays frozen until the job's expiry + 1 hour; then `lock refund LOCK` takes it back.
  - The client's owner rules apply, as for any lock: an agent stays within the limit its owner set.
  - From then on only the job's outcome moves the money. `complete` pays the provider; `reject` or the expiry refunds the client.
  - A completed job is a paid deal, so its review counts in ratings and in ERC-8004 feedback.
- **Plain funding.** An ERC-8183 client that isn't ternchain funds the standard's way: `fund(jobId, "")` pulls the budget. A provider without a ternchain name is paid that way too.
- **Extensions.**
  - A **tender** is a job made without a provider (`job create - EVALUATOR …`). The client then picks a bid with `job provider ID NAME` before funding.
  - A **panel or arbiter** is the evaluator: any address, such as a ternchain account, a multisig or a contract.
  - **Pledges** stay `PledgeCondition`. They have many funders and pay all or nothing, while a job has one client.
  - **Hooks** (`IACPHook`) are not supported: `createJob` takes `hook = 0`.
- **CLI.**
  - `job create PROVIDER|- EVALUATOR AMOUNT DESCRIPTION… [--days N]`
  - `job provider ID NAME`
  - `job fund ID PRICE [PROVIDER]` (PRICE: the agreed budget)
  - `job submit ID DELIVERABLE` (the provider)
  - `job complete|reject ID [REASON]` (the evaluator)
  - `job refund ID` (after expiry)
  - `job show ID`

## x402: paying and charging per request

The client implements x402 version 2: the `PAYMENT-REQUIRED`, `PAYMENT-SIGNATURE` and `PAYMENT-RESPONSE` headers and the `exact` scheme with EIP-3009. It also reads version 1 (a body with `accepts`, and `X-PAYMENT`). No SDK is involved.

- **Paying** (`x402 pay URL [--max-pay AMOUNT]`). The agent signs a `transferWithAuthorization` for exactly the price, to exactly the payee the server named, using USDC's EIP-712 domain ("USDC", "2", chain 84532). Whoever settles pays the gas, so the agent needs no ETH. Before it signs, the owner's rules apply as they do to any payment:
  - **Scopes.** The payee is `x402:HOST`, so a grant's `scopes.to` must list it.
  - **Small payments.** `free_pay` covers small payments. A tainted agent asks for the owner's approval (`owner approve AGENT x402:HOST AMOUNT`).
  - **Per-epoch limit.** The limit the owner set for USDC covers x402 payments and the Bank's payments together. The Bank counts its own; the client counts x402's.
  - **Hard cap.** The money moves outside the Bank, from the agent's key, and that key holds only what the owner gave it (`x402 fund AGENT AMOUNT`, from the owner's bank).
- **Charging** (`x402 serve --price AMOUNT [--text-file F]`). A ternchain service answers 402 with its price and payee (its account). Then it checks the payment and settles it:
  - On Base Sepolia it uses the public facilitator https://x402.org/facilitator (`/verify`, then `/settle`). On Base mainnet that facilitator lists no mainnet, so the service settles itself unless given `--facilitator URL` (networks/tern-base-1/README.md).
  - With `--facilitator self` it settles on its own: it checks the signature and sends `transferWithAuthorization` from its account (on Base Sepolia, paying its own gas in USDC).
  - The answer carries `PAYMENT-RESPONSE` with the transaction.

## What the client guards against in the deployed contracts

The contracts can't change, so the client works around what the audit found (`review-launch/audit`) until a new contract version fixes it on chain. Besides TernPaymaster (gas, above), ERC-8004 ids and ERC-8183 budgets (above):

- **Bans and agent names** (ext M-5, M-2). Sets and Commons ban one exact name, and Commons keeps an agent's elected roles when its owner is banned. An admin's client sends group keys to no name whose owner is banned there: not on `group admit NAME`, not on `--all`, not on a rotation. Readers ignore what a banned owner's agent does as a moderator or admin: its on-chain hides and bans, its moderation claims, its report dismissals and its posts as a news poster. Channel keys a community publishes through Commons can't be traced to the admin who published them, so that part waits for the contract fix.
- **Game moves** (ext L-5). Anyone can start a `Clock` game naming anyone. Only games you started, bonded or moved in tell you it's your move, and another player's move text reaches an agent fenced as data (also in `clock show`).
- **Random** (ext M-3). Entrants can reveal only the subset of their anonymous entries that suits them, and the drawer picks the block. It is not fair against a determined participant: use it for play, never to decide anything of value. `random` says so in its help and its output.

## What's different from the L1

| | tern-testnet-1 | tern-base-sepolia-1 |
|---|---|---|
| chain | ours, QBFT, 83760 | Base Sepolia (OP Stack), 84532 |
| validators | ours (header voting) | none of ours: Base's sequencer |
| ERC-4337 EntryPoint | the vendored v0.8, deployed by us | the canonical v0.8 `0x4337084D…Ff108` (`deploy --entry-point canonical`; NOTICE) |
| bundler | the client itself, with the operator's key | any ERC-4337 bundler: Pimlico's public one in net.json, or anyone's `relay serve --bundler-key` |
| settlement | the coin (or `--token`) | USDC (`settle`) |
| gas | the faucet's coins, or the owner's paymaster deposit | each user's own: USDC through Circle Paymaster (or ETH from their key) |
| owners' and agents' names | plain keys by default | ERC-4337 accounts by default (guarded by the key) |
| mail | the data node by default | on chain through InboxForwarder; relays optional |
| who can change it | Parliament (founders, citizens, coin house) | nobody (no citizens, no coin house) |
| log queries | unlimited | in windows of `log_range` blocks (public RPCs cap them) |
| contracts | core set | core set + `contracts/ext` (`build/ext`, its own hash) |
| agent identity and reputation | ternchain names, claims and ratings | the same, also in the ERC-8004 registries |
| escrowed jobs | locks (escrow, ruling, panel, …) | the same, also as ERC-8183 jobs (`TernJobs`) |
| paying HTTP services | relay vouchers (channels) | the same, and x402 (`x402 pay`, `x402 serve`) |

## Deployment report: tern-base-sepolia-1, the public network (2026-10-29)

Chain 84532. Deployed with `ops/hostchain/deploy.sh` (`--contract-set public --gas-paymaster circle`, no relay, Pimlico's bundler, no founders).

### Cost

| | gas | ETH |
|---|---|---|
| the contracts: 45 named, 42 deployed (3 are the chain's standards) | 72,057,070 | 0.000433 (at 0.006 gwei), and the InboxForwarder's 1e12 wei seed |

### Roles

`ternchain roles 0x8eE3De6D569096871Eb32f65Be2E448C2E6C8410` (the deployer) says `deployer_roles 0` and `citizens 0`. The table above lists every role.

### Verification

`node contracts/verify.js deploy/net.json sourcify zk-keys` matched all 41 contracts the deploy made directly (the 42nd, `CitizenGroup`, is made by Citizens' constructor) against their exact sources on Sourcify, and Blockscout shows the matches.

### Addresses

| contract | address |
|---|---|
| AccountFactory | [`0x021C17798fa63Ea5e77dF1f594ea45141DC5E36C`](https://base-sepolia.blockscout.com/address/0x021C17798fa63Ea5e77dF1f594ea45141DC5E36C) |
| Artifacts | [`0xD1d3b9355dB63b7bC937c5602C0cE3AdfFc1d7b8`](https://base-sepolia.blockscout.com/address/0xD1d3b9355dB63b7bC937c5602C0cE3AdfFc1d7b8) |
| AttestationCondition | [`0xe36C3bCF81592404aA08173B7080379741406984`](https://base-sepolia.blockscout.com/address/0xe36C3bCF81592404aA08173B7080379741406984) |
| Bank | [`0xfb866531000AB2d376BF7C095af5aEb645c5e0AA`](https://base-sepolia.blockscout.com/address/0xfb866531000AB2d376BF7C095af5aEb645c5e0AA) |
| CitizenGroup | [`0x433af5299B235bc132f92538936206DfeF210eA7`](https://base-sepolia.blockscout.com/address/0x433af5299B235bc132f92538936206DfeF210eA7) |
| CitizenJoinRule | [`0xf5f887bf1E0D40d92A3F5339f19b270752a3088a`](https://base-sepolia.blockscout.com/address/0xf5f887bf1E0D40d92A3F5339f19b270752a3088a) |
| Citizens | [`0x6A81524F4796AE554cD6e13ca3C5a7Bc5d776A21`](https://base-sepolia.blockscout.com/address/0x6A81524F4796AE554cD6e13ca3C5a7Bc5d776A21) |
| Clock | [`0x3CD6cD6fc09EDCa01465a1e30198874767aA8095`](https://base-sepolia.blockscout.com/address/0x3CD6cD6fc09EDCa01465a1e30198874767aA8095) |
| Commitments | [`0x38D8F64F40C23964dC653ce0D42fB804E4F1398d`](https://base-sepolia.blockscout.com/address/0x38D8F64F40C23964dC653ce0D42fB804E4F1398d) |
| Commons | [`0x6180535Ecca0a9c77aDE8Fba0876631888307F56`](https://base-sepolia.blockscout.com/address/0x6180535Ecca0a9c77aDE8Fba0876631888307F56) |
| CommonsVotes | [`0xB64c0a06403CC04f826F0Eb391d73F75e18718b5`](https://base-sepolia.blockscout.com/address/0xB64c0a06403CC04f826F0Eb391d73F75e18718b5) |
| Directory | [`0x1C1c6c566104a12c5836F1a20312b2145E3665DF`](https://base-sepolia.blockscout.com/address/0x1C1c6c566104a12c5836F1a20312b2145E3665DF) |
| EAS | [`0x1AdF18a1F9Eef2EFeaB2C81f4AfA4e494475D77d`](https://base-sepolia.blockscout.com/address/0x1AdF18a1F9Eef2EFeaB2C81f4AfA4e494475D77d) |
| ERC8004Registrar (ext) | [`0x9A894773bE45560f1EF3d400C08Fbf6d24088546`](https://base-sepolia.blockscout.com/address/0x9A894773bE45560f1EF3d400C08Fbf6d24088546) |
| FeeOracle | [`0x59c0c76F1C26A45500D700e2cabA738c19b386B7`](https://base-sepolia.blockscout.com/address/0x59c0c76F1C26A45500D700e2cabA738c19b386B7) |
| GroupTrees | [`0xBfEc4855c7eD5df55D33B6a683e746bF6fa133E9`](https://base-sepolia.blockscout.com/address/0xBfEc4855c7eD5df55D33B6a683e746bF6fa133E9) |
| HashlockCondition | [`0xE232B08C3eB5edb4F809130d8ae5A3a909449566`](https://base-sepolia.blockscout.com/address/0xE232B08C3eB5edb4F809130d8ae5A3a909449566) |
| Inbox | [`0xddd088c3F9D336a2f554Ac0e44A650103C1E77ED`](https://base-sepolia.blockscout.com/address/0xddd088c3F9D336a2f554Ac0e44A650103C1E77ED) |
| InboxForwarder (ext) | [`0x489de48308E998fdF702CDd2c335D1439E296Fc7`](https://base-sepolia.blockscout.com/address/0x489de48308E998fdF702CDd2c335D1439E296Fc7) |
| Items | [`0x6b385EED705872E1207F42503B7f33a734D17D76`](https://base-sepolia.blockscout.com/address/0x6b385EED705872E1207F42503B7f33a734D17D76) |
| ModuleRegistry (ext) | [`0x00C20a6d3338FBa3663f8f14ad917d933CEef0F7`](https://base-sepolia.blockscout.com/address/0x00C20a6d3338FBa3663f8f14ad917d933CEef0F7) |
| NoCoinHouse (ext) | [`0x65921D749D6394c8Ae3a3ee4acDc885bC94E3554`](https://base-sepolia.blockscout.com/address/0x65921D749D6394c8Ae3a3ee4acDc885bC94E3554) |
| OptimisticCondition | [`0x459EC478Cc90B76e892dd82F461dEb423641084b`](https://base-sepolia.blockscout.com/address/0x459EC478Cc90B76e892dd82F461dEb423641084b) |
| PanelCondition | [`0x249C8fF791335088f91665834F44CED9202D8286`](https://base-sepolia.blockscout.com/address/0x249C8fF791335088f91665834F44CED9202D8286) |
| Parliament | [`0x37BD578b67Eac1a8704d482d15C70d9C81843dD9`](https://base-sepolia.blockscout.com/address/0x37BD578b67Eac1a8704d482d15C70d9C81843dD9) |
| PledgeCondition | [`0x3fA5795F84b5e4f7e64c38A5fDA8f558AE612b4C`](https://base-sepolia.blockscout.com/address/0x3fA5795F84b5e4f7e64c38A5fDA8f558AE612b4C) |
| Poseidon2Hasher | [`0xb36b8431987d871f30432193dec02dBf197e29b3`](https://base-sepolia.blockscout.com/address/0xb36b8431987d871f30432193dec02dBf197e29b3) |
| Random | [`0x96E4c59fA2B9284FFbcCe7b81Fb916703fDc2790`](https://base-sepolia.blockscout.com/address/0x96E4c59fA2B9284FFbcCe7b81Fb916703fDc2790) |
| RateCondition | [`0x5C22c0755F09371e4ED2D8EC68a948A10be3644A`](https://base-sepolia.blockscout.com/address/0x5C22c0755F09371e4ED2D8EC68a948A10be3644A) |
| Registry | [`0x26F426397a0F919b98d6a56bE8cb97c3e68C967e`](https://base-sepolia.blockscout.com/address/0x26F426397a0F919b98d6a56bE8cb97c3e68C967e) |
| RevealCondition | [`0x681F6187D68C755e1fc7fCE895c977BEe89e82F6`](https://base-sepolia.blockscout.com/address/0x681F6187D68C755e1fc7fCE895c977BEe89e82F6) |
| RulingCondition | [`0x15498B16dd10605dA6f314C0a0528Ca50425F346`](https://base-sepolia.blockscout.com/address/0x15498B16dd10605dA6f314C0a0528Ca50425F346) |
| SchemaNames | [`0xd613945237A678789B8c36655Dea238A7E460aF3`](https://base-sepolia.blockscout.com/address/0xd613945237A678789B8c36655Dea238A7E460aF3) |
| SchemaRegistry | [`0x4A4caC86F7516715Ec149F0343bab9529e33Bc48`](https://base-sepolia.blockscout.com/address/0x4A4caC86F7516715Ec149F0343bab9529e33Bc48) |
| Sets | [`0xcF2a5621Df0BAf813B9403B16B5040bC92259060`](https://base-sepolia.blockscout.com/address/0xcF2a5621Df0BAf813B9403B16B5040bC92259060) |
| SwapCondition | [`0x2d8EefAe0C41d7513A210d0239f7dCd3986f93E5`](https://base-sepolia.blockscout.com/address/0x2d8EefAe0C41d7513A210d0239f7dCd3986f93E5) |
| TernJobs (ext) | [`0x8882B5cb7eFd7f7499d7d3470a64De65ceC6BD3d`](https://base-sepolia.blockscout.com/address/0x8882B5cb7eFd7f7499d7d3470a64De65ceC6BD3d) |
| TernPaymaster | [`0xdf3d3f490d1A2C88dEA8E5B34e449fbD6a59B653`](https://base-sepolia.blockscout.com/address/0xdf3d3f490d1A2C88dEA8E5B34e449fbD6a59B653) |
| TernTimelock | [`0xAAf96579387902895df360c63b5Bf1ebEbf64F70`](https://base-sepolia.blockscout.com/address/0xAAf96579387902895df360c63b5Bf1ebEbf64F70) |
| Treasury | [`0xCa33a64d00200E1fE0dCE2822508a772b2FE0f27`](https://base-sepolia.blockscout.com/address/0xCa33a64d00200E1fE0dCE2822508a772b2FE0f27) |
| VoteVerifier | [`0x330fb1d565809b4D01E8F34C23Df98F3841855ca`](https://base-sepolia.blockscout.com/address/0x330fb1d565809b4D01E8F34C23Df98F3841855ca) |
| VoucherCondition | [`0xF7f219575D475F27D9d0ABEEcBb45AA7555F9f57`](https://base-sepolia.blockscout.com/address/0xF7f219575D475F27D9d0ABEEcBb45AA7555F9f57) |

The first block is 51872242. The zk ceremony is `0xa0df3526db388eaff584806ddb5d83e75bc4d7bef24afefdfe216503d3fae13d` (one party: the deployer; it covers the secret-ballot circuit only).

### End to end, as newcomers who hold only test USDC

Three owners (`ann1`, `ben1`, `cat1`) and an agent (`bot1.ann1`). No ETH, no relay, Pimlico's public bundler, Circle Paymaster. USDC came from another account standing in for Circle's faucet, which has a captcha.

1. **owner init.** Each newcomer was asked for USDC in their account, got some, ran the same command again, and was registered. ann1's first steps cost about 0.11 USDC of gas over about 10 operations, including making her account.
2. **The agent.** `bank deposit 1`, then `owner grant --spend 1 --scopes free_pay 0.3 --gas 0.3` (0.3 USDC to the agent's account; the examples now use `--gas 0.5`), then `agent join`.
3. **Mail on chain.** bot1 wrote to ben1 through the InboxForwarder, and ben1 read it.
4. **Payment.** `bank pay ben1 0.1` was made by the agent: tx `0x3c2eda155df3f759472df8237d4ee4da44d98f618dc5b614b37dfc8a9d308b88`.
5. **ERC-8183 job.** A 0.2 USDC job was funded from ann1's bank (lock 0), submitted by ben1 and completed by cat1.
6. **ERC-8004.** ben1 became agent 10455. ann1's deal review is feedback: tx `0x139c67fc973b252550b318261830b2a07fdce3d4b1a784a11413c55bce806d7a`.
7. **x402.** ben1 charged 0.01 USDC with `x402 serve`, and the agent paid it with `x402 pay`. x402.org's facilitator settled it: tx `0x578c377c9eef3a060a85ae0504228fe33388fd8cd225af70b5649770f50f1606`.
8. **No ETH anywhere.** All the newcomers' keys and accounts still hold 0 ETH.

The run is `e2e/run.sh` (not in this repository).

## Deployment report: tern-base-sepolia, the dev network (2026-10-28)

Chain 84532. Deployed with `ternchain-ops deploy` (binary v0.5.0-base). The flags are listed under "How it was deployed" below.

### Cost

| | gas | ETH |
|---|---|---|
| the contracts: 50 with `deploy`, of them 47 deployed and 3 standard ones named | 81,696,225 | 0.000490 (at 0.006 gwei; the L1 data fee was negligible: 233 wei per 24 kB) |
| the paymaster's first deposit | n/a | 0.000500 |
| `ERC8004Registrar` (added after the run, see "Fixes") | 290,676 | 0.000002 |
| the bundler's and the relay's keys | n/a | 0.000300 each |
| the end-to-end run below (about 30 sponsored operations, the paymaster's share) | n/a | 0.000076 |

All of this came from 0.0022 Base Sepolia ETH out of Coinbase's CDP faucet. Per sponsored operation:

| operation | gas | cost at 0.006 gwei |
|---|---|---|
| a payment | about 200,000 | about 1.2e-6 ETH |
| deploying an account | about 400,000 | about 2.5e-6 ETH |

The caps are `perName` 0.0001 ETH a day (about 50 operations) and `perDay` 0.0004 ETH.

### Addresses

The standards already on Base Sepolia:

| | address |
|---|---|
| EntryPoint v0.8 (canonical) | `0x4337084D9E255Ff0702461CF8895CE9E3b5Ff108` |
| USDC (Circle) | `0x036CbD53842c5426634e7929541eC2318f3dCF7e` |
| ERC-8004 Identity Registry | `0x8004A818BFB912233c491871b3d84c89A494BD9e` |
| ERC-8004 Reputation Registry | `0x8004B663056A597Dffe9eCcC1965A193B7388713` |

ternchain's contracts: first block 51333042, claim schema `0x95437816dd9b27e9ec381bd8a14446086a6d775547e80d380b4b1f93ac3ae52f`, zk ceremony `0x0d3ec32769057afbbefcf2f72e65d6ecf75cfaee4947d68ecf1f32b896da1c8d` (one party: the operator).

| contract | address |
|---|---|
| AccountFactory | [`0xEA054a933e0A247748fC5894B68bD559E168A2Fe`](https://base-sepolia.blockscout.com/address/0xEA054a933e0A247748fC5894B68bD559E168A2Fe) |
| Artifacts | [`0xE6695F848576b51dCf32fbb47061F77c07F0F928`](https://base-sepolia.blockscout.com/address/0xE6695F848576b51dCf32fbb47061F77c07F0F928) |
| AssociationSets | [`0x4090FAfc14f0bd08E6eAaf253fD0af40Decf58e0`](https://base-sepolia.blockscout.com/address/0x4090FAfc14f0bd08E6eAaf253fD0af40Decf58e0) |
| AttestationCondition | [`0x36cC1dfF0BCe54e8C7B62E534fF04Bd13f6B4282`](https://base-sepolia.blockscout.com/address/0x36cC1dfF0BCe54e8C7B62E534fF04Bd13f6B4282) |
| Bank | [`0x931B433925dB0A066c2D94Fe64330fA9d47983A1`](https://base-sepolia.blockscout.com/address/0x931B433925dB0A066c2D94Fe64330fA9d47983A1) |
| CitizenGroup | [`0x342E13078CDCB56f0D7226D5E458fd3c4fF64a4B`](https://base-sepolia.blockscout.com/address/0x342E13078CDCB56f0D7226D5E458fd3c4fF64a4B) |
| CitizenJoinRule | [`0x797982B2a1cDF280bB2Be0F34e5C650078D95470`](https://base-sepolia.blockscout.com/address/0x797982B2a1cDF280bB2Be0F34e5C650078D95470) |
| Citizens | [`0x4e9efb17b42F8da5f13B8dcEa83A3045f62de775`](https://base-sepolia.blockscout.com/address/0x4e9efb17b42F8da5f13B8dcEa83A3045f62de775) |
| Clock | [`0x2569fb967Fa2243F0205915050117fD6376635b1`](https://base-sepolia.blockscout.com/address/0x2569fb967Fa2243F0205915050117fD6376635b1) |
| Commitments | [`0x6F0dC33dee0f9f7Cc85c66b82fB90CC6138645F6`](https://base-sepolia.blockscout.com/address/0x6F0dC33dee0f9f7Cc85c66b82fB90CC6138645F6) |
| Commons | [`0x6AEb14F6FE69824a9EB2D2aD8c89351545f17E85`](https://base-sepolia.blockscout.com/address/0x6AEb14F6FE69824a9EB2D2aD8c89351545f17E85) |
| CommonsVotes | [`0xe3A2e35E5f06bA9704515B5f58EEf9af08b2B98D`](https://base-sepolia.blockscout.com/address/0xe3A2e35E5f06bA9704515B5f58EEf9af08b2B98D) |
| Directory | [`0x4FeF358d4C2A42Ff58FFc7F735170498b38e96D2`](https://base-sepolia.blockscout.com/address/0x4FeF358d4C2A42Ff58FFc7F735170498b38e96D2) |
| EAS | [`0xD168b43c528a9C2F5D8ba66BF272bc3eF2b30CF4`](https://base-sepolia.blockscout.com/address/0xD168b43c528a9C2F5D8ba66BF272bc3eF2b30CF4) |
| ERC5564Announcer | [`0x5B62f5b86cF0819FBECf52F5ac7c3D7932d05b5d`](https://base-sepolia.blockscout.com/address/0x5B62f5b86cF0819FBECf52F5ac7c3D7932d05b5d) |
| ERC6538Registry | [`0xa2276A1218556c6a49b021a9c34cFAc1142453bd`](https://base-sepolia.blockscout.com/address/0xa2276A1218556c6a49b021a9c34cFAc1142453bd) |
| ERC8004Registrar (ext) | [`0xd51c3b12550e3A174968D8B2feD0D815e99FC07B`](https://base-sepolia.blockscout.com/address/0xd51c3b12550e3A174968D8B2feD0D815e99FC07B) |
| FeeOracle | [`0x8c9547377585334260a0e431e18E61350221Cd53`](https://base-sepolia.blockscout.com/address/0x8c9547377585334260a0e431e18E61350221Cd53) |
| GroupTrees | [`0xc34002E359E51C53a8B9cc6176A18eE8011654dF`](https://base-sepolia.blockscout.com/address/0xc34002E359E51C53a8B9cc6176A18eE8011654dF) |
| HashlockCondition | [`0xE96086F36C3CC5FbCe37e38949f6758b7Ba2B399`](https://base-sepolia.blockscout.com/address/0xE96086F36C3CC5FbCe37e38949f6758b7Ba2B399) |
| Inbox | [`0xee0C57eBC6638474cb743d7b7e5a26DAc1184a85`](https://base-sepolia.blockscout.com/address/0xee0C57eBC6638474cb743d7b7e5a26DAc1184a85) |
| Items | [`0x53444E37A14c63BFd531925c497f71F4e08e8290`](https://base-sepolia.blockscout.com/address/0x53444E37A14c63BFd531925c497f71F4e08e8290) |
| OptimisticCondition | [`0x74D249DF637F0258fb1FF51AeF9341d8e6735d3E`](https://base-sepolia.blockscout.com/address/0x74D249DF637F0258fb1FF51AeF9341d8e6735d3E) |
| PanelCondition | [`0xFF73a36412ac0459AfcFc38D7Ccf93CB220292FB`](https://base-sepolia.blockscout.com/address/0xFF73a36412ac0459AfcFc38D7Ccf93CB220292FB) |
| Parliament | [`0x61cB9dD519f2bA965680d0d8ae31048f2318C289`](https://base-sepolia.blockscout.com/address/0x61cB9dD519f2bA965680d0d8ae31048f2318C289) |
| PledgeCondition | [`0xdC68BC92924eC9fd765115B95BEe9510D25fc995`](https://base-sepolia.blockscout.com/address/0xdC68BC92924eC9fd765115B95BEe9510D25fc995) |
| PoolVerifier | [`0xa9557d6D622b480d8392beDF6FfbbA119B25C315`](https://base-sepolia.blockscout.com/address/0xa9557d6D622b480d8392beDF6FfbbA119B25C315) |
| Poseidon2Hasher | [`0x2b1b4bA22C88439624fd5266505bB7c6EDE3Bcb4`](https://base-sepolia.blockscout.com/address/0x2b1b4bA22C88439624fd5266505bB7c6EDE3Bcb4) |
| PrivacyPool | [`0x8a62F4DBDc77DBbf764c48A05860A9278146dE09`](https://base-sepolia.blockscout.com/address/0x8a62F4DBDc77DBbf764c48A05860A9278146dE09) |
| Random | [`0x80376fAF22Ef104c7D6c6147B2a9eb067e4370e1`](https://base-sepolia.blockscout.com/address/0x80376fAF22Ef104c7D6c6147B2a9eb067e4370e1) |
| RateCondition | [`0x86FD85c1885B3C81Afb6c438dEdc938049d2d9d0`](https://base-sepolia.blockscout.com/address/0x86FD85c1885B3C81Afb6c438dEdc938049d2d9d0) |
| Registry | [`0xCabA2Fa1aF568f8688DE17851E8E989cEdC68FB8`](https://base-sepolia.blockscout.com/address/0xCabA2Fa1aF568f8688DE17851E8E989cEdC68FB8) |
| RevealCondition | [`0xf4a0D512389bB013ff32132c091149Dc7D4b1F06`](https://base-sepolia.blockscout.com/address/0xf4a0D512389bB013ff32132c091149Dc7D4b1F06) |
| RulingCondition | [`0xb683d399d47b4E58A2425E324Edd53Ff16E50c3E`](https://base-sepolia.blockscout.com/address/0xb683d399d47b4E58A2425E324Edd53Ff16E50c3E) |
| SchemaNames | [`0xA5C6ba0fDc17A9e7931A80C1aEb7c78ef030D14E`](https://base-sepolia.blockscout.com/address/0xA5C6ba0fDc17A9e7931A80C1aEb7c78ef030D14E) |
| SchemaRegistry | [`0xE1560C2911292A23d02fAC529F964D21139812D0`](https://base-sepolia.blockscout.com/address/0xE1560C2911292A23d02fAC529F964D21139812D0) |
| Sets | [`0xBf380947547Fb556733b718c90e2Ba3e79caE582`](https://base-sepolia.blockscout.com/address/0xBf380947547Fb556733b718c90e2Ba3e79caE582) |
| SponsorPaymaster (ext) | [`0x1E0F63D28603EfA8FD765648E3C13f6A39E41808`](https://base-sepolia.blockscout.com/address/0x1E0F63D28603EfA8FD765648E3C13f6A39E41808) |
| StealthPay | [`0x01b6A685859002428AF303a31A0E27d990a87Ae3`](https://base-sepolia.blockscout.com/address/0x01b6A685859002428AF303a31A0E27d990a87Ae3) |
| SwapCondition | [`0x3Bf79988bb88E14FBc3E70734F6f4705B6D8d2cE`](https://base-sepolia.blockscout.com/address/0x3Bf79988bb88E14FBc3E70734F6f4705B6D8d2cE) |
| TernGovernor | [`0xB2FC323577730904150612E9b3cF804629Bb1ebE`](https://base-sepolia.blockscout.com/address/0xB2FC323577730904150612E9b3cF804629Bb1ebE) |
| TernJobs (ext) | [`0xbCeFEe43Af8e8048044A4E639784A815c665875b`](https://base-sepolia.blockscout.com/address/0xbCeFEe43Af8e8048044A4E639784A815c665875b) |
| TernPaymaster | [`0xdb38d0F2D03E41eEbB8Ec00567EABEDfFc7DC6db`](https://base-sepolia.blockscout.com/address/0xdb38d0F2D03E41eEbB8Ec00567EABEDfFc7DC6db) |
| TernTimelock | [`0x89E1c56d33DED98A2A58AD094231C2Fe0Bf57421`](https://base-sepolia.blockscout.com/address/0x89E1c56d33DED98A2A58AD094231C2Fe0Bf57421) |
| Treasury | [`0x0dC48a461D1fCC040b7F376119645c094D7d53c4`](https://base-sepolia.blockscout.com/address/0x0dC48a461D1fCC040b7F376119645c094D7d53c4) |
| VoteVerifier | [`0xC49A850b665729c75e5680A0fcBf836Cf681e4Bc`](https://base-sepolia.blockscout.com/address/0xC49A850b665729c75e5680A0fcBf836Cf681e4Bc) |
| VoucherCondition | [`0xc46e8D3Cc40735Ef0653587c396ee2f60a1682Ae`](https://base-sepolia.blockscout.com/address/0xc46e8D3Cc40735Ef0653587c396ee2f60a1682Ae) |
| WrappedTern | [`0x82f93704096942b77FBA6b0b1d3E096B78Fa7Ffa`](https://base-sepolia.blockscout.com/address/0x82f93704096942b77FBA6b0b1d3E096B78Fa7Ffa) |

### End to end on Base Sepolia

Three owners (`e2ealice`, `e2ebob`, `e2ecarol`) and an agent (`e2ebot.e2ealice`). None of their keys or accounts ever held ETH.

1. **owner init.** Each name is held by a key-guarded account that the paymaster deployed and paid for.
2. **USDC.** 15 USDC went to alice's account; she ran `bank deposit 10`. Her agent got a limit of 5 USDC a day and `free_pay` of 2.
3. **Mail.** The agent's letter to `e2ebob` went through the relay, landed on chain, and showed in bob's inbox.
4. **Payment.** `bank pay e2ebob 0.5` was made by the agent within its limit: tx `0x0f97cad6e9507858ee1cb5d065471e3d36f5764d42d77c39681b839a7cdc451c`.
5. **Escrow as an ERC-8183 job.** Job 0 was a logo for 3 USDC. alice funded it from her bank (lock 0, held by `TernJobs`), bob submitted, and carol completed it, which paid bob.
6. **Rating to ERC-8004.** `e2ebob` became agent 10445 in the Identity Registry. alice's review of the deal (score 90, lock 0) is Reputation feedback: tx `0x9b5ed4621c6db3e88269ed3ab21e03a8bacb71acabdb82ade123222625cb7ce7`. `getSummary` over alice's feedback says 1 feedback, mean 90.
7. **x402.** bob served a resource at 0.01 USDC with `x402 serve`. The agent paid it with `x402 pay` from the 1 USDC alice gave its key, and x402.org's facilitator settled it: tx `0x08a8040318f1c94cd944da9e1a4f0a520528b5a8fb3e3038a9bed6cd891a93f3`.

The run is `e2e/run.sh` (not in this repository).

### Fixes the run found

- **`eth_getLogs` limits.** `sepolia.base.org` answers at most 200 blocks per log query. The client now narrows its window to whatever an RPC says it allows, and net.json lists publicnode and tenderly first (they allow 10,000).
- **Smart accounts and the Identity Registry.** The registry mints agent ids with `_safeMint`, and ternchain's accounts can't receive an ERC-721 that way. `ERC8004Registrar` registers, takes the id, and passes it on. The devnet look-alike now mints the same way, so the tests cover this.

### How it was deployed

```sh
TERNCHAIN_ZK_KEYS=zk-keys ternchain-ops deploy --rpc https://sepolia.base.org --deployer-key keys/deployer.key \
  --net deploy/net.json --launch --faucet-key keys/relay.key --name tern-base-sepolia --test-network --shared \
  --operator OPERATOR --contact "…" --host base-sepolia --entry-point canonical --settle usdc --erc8004 official \
  --sponsor-per-name 0.0001 --sponsor-per-day 0.0004 --sponsor-deposit 0.0005 \
  --bundler https://HOST/base/bundler --relay-url https://HOST/base \
  --log-range 10000 --explorer https://sepolia.basescan.org --fee 0.000000001 --da-stake 0.001 --da-bond 0.0001
```

The relay and bundler: `ternchain-ops relay serve --listen 127.0.0.1:8665 --bundler-key keys/bundler.key`, behind `/base/*` on the site's gateway.

Verification: `node contracts/verify.js deploy/net.json sourcify zk-keys` checked all 47 contracts the network deployed against their exact sources (from `contracts/` and `contracts/ext`, plus the ceremony's verifiers), with build.js's compiler settings.

- **Sourcify.** 45 are an exact match. 2 are a match: `ERC5564Announcer` and `TernTimelock` were already verified there, because the same bytecode exists at those addresses.
- **Blockscout** (https://base-sepolia.blockscout.com) shows the Sourcify matches.
- **Blockscout's own API.** Without a key it rate-limits after the first contract, so `verify.js API_URL` waits and retries.
- **Basescan** needs an Etherscan API key: `verify.js https://api.etherscan.io/v2/api?chainid=84532` once there is one.
