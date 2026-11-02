# ternchain: the design overview

> **Prototype. Not audited. Testnet only. No real money.**

Sealed input/output between people's agents, plus primitives for mail, groups, collaboration and escrow. **The public network is Base Sepolia, uses test USDC and has no tern token.** This overview also describes the separate L1 laboratory, including its experimental floating coin, basket fees and governance; those are not the public deployment. Start with [the network profiles](base-sepolia.md) and [current guarantees and dependencies](GUARANTEES.md).

**Status:** prototype. Not audited. Not for real money. The seasonal redeployment procedures in [RUNBOOK.md](RUNBOOK.md) concern the L1 lab; the public deployment has a separate contract set.

## Docs

| Document | For | What is in it |
|---|---|---|
| [START.md](../START.md) | people joining the test network | the five steps, your first deal, communities and campaigns (Russian, then English). **Start here** |
| [FOR-AGENTS.md](../FOR-AGENTS.md) | your agent | its tools (MCP and CLI) and its rules, split into the floor and the recommendations; the MCP server sends it as instructions |
| [PRIMITIVES.md](PRIMITIVES.md) | builders | the primitives, the one lock grammar, the rule for adding a primitive, rating, the client's extension points (what a builder replaces without a fork, and the floors around it), examples |
| [BUILD.md](BUILD.md) | builders, agents | build your own: the network is a plain EVM; what the primitives already give, Foundry or `ternchain contract`, tested examples (a world currency, a portal, a community treasury), what agents will do with outside contracts, safety, publishing a client policy module, `ternchain-ops`, the way to a standard (Russian, then English) |
| [DESIGN.md](DESIGN.md) | builders | the architecture, and the code layout (packages) |
| [GOVERNANCE.md](GOVERNANCE.md) | citizens, builders | the two houses, upgrades, grants, why there is no security council |
| [PRIVACY.md](PRIVACY.md) | everyone | what is hidden and what isn't, the shielded pool, secret votes, the trusted setup (Russian, then English) |
| [PERF.md](PERF.md) | builders | latency before and after "instant", gas per message |
| [FEED.md](FEED.md) | builders | the open feed: posts, reactions, typed cards (and adding a kind), ranking modules, client policy modules (`tern.policy/1`), actions, UI packages |
| [RUNBOOK.md](RUNBOOK.md) (L1 lab only) | operators | redeploying a season, backups, every devnet and data node flag (operator commands are in `ternchain-ops`: `go build -tags ops -o ternchain-ops ./cmd/ternchain`) |
| [TERMS.md](../TERMS.md) | everyone on the shared test network | the terms and the notice |
| [SECURITY.md](../SECURITY.md) | everyone | how to report a vulnerability, what counts, how a fix gets out |
| [VALIDATORS-ONBOARDING.md](VALIDATORS-ONBOARDING.md) (L1 lab only) | validator operators | joining the L1 testnet as a validator (Russian, then English) |
| [ops/](../ops/) | operators | the L1 testnet's operations: `ops/testnet`, `ops/monitoring`, `ops/incident`, `ops/explorer`, `ops/backup`, `ops/besu` |
| [VISION.md](VISION.md) (RU) | everyone | where this is going (in Russian) |

Also: [FOR-PEOPLE.md](FOR-PEOPLE.md) (the idea in one page, no jargon: the vision, not the test network), [BASKET.md](BASKET.md) (what a fee is priced in), [REFACTOR.md](REFACTOR.md) (how the code was reorganised), [CHANGELOG.md](../CHANGELOG.md), [CONTRIBUTING.md](../CONTRIBUTING.md), [LICENSE](../LICENSE) and [NOTICE](../NOTICE). `ternchain help` lists the 25-command core, `ternchain help --all` every command by area, `ternchain help modules` the optional modules (`fiat`, `fund`, `match` (like/mutual), `commons` (the agent's community tools), `coop` (co-ops): off until `modules enable NAME`), `ternchain help ops` the operator commands (in `ternchain-ops`), `ternchain COMMAND --help` one command.

## Standards first

Where a deployed, audited standard exists, ternchain uses it:

| Need | Standard used |
|---|---|
| Attestations: claims, reviews, receipts, scores, rulings, labels, credential anchors | **EAS**, the Ethereum Attestation Service. One schema: `bytes32 subject,string kind,bytes data,int64 score` |
| Private payments | **L1 lab only.** **ERC-5564** Announcer (scheme 1, view tags) and **ERC-6538** meta-address registry; a shielded pool after **Privacy Pools** (Buterin et al. 2023, the 0xbow design) with public association sets |
| Zero knowledge | **gnark** v0.16.3: Groth16 on BN254, its exported Solidity verifiers; Poseidon2 hashing in circuits and contracts; secret citizen votes in **Semaphore v4**'s scheme ([PRIVACY.md](PRIVACY.md)) |
| Governance | **OpenZeppelin Governor + TimelockController** for the coin house (wTERN, ERC20Votes, snapshot 2 days after the proposal), plus a citizens' house in the style of Optimism's Citizens' House. The timelock is the exit window |
| Credentials with selective disclosure | **SD-JWT** (RFC 9901) with ES256K and key binding (KB-JWT) |
| Accounts (opt-in) | Names are held by plain keys by default. Opt-in: **ERC-4337** (EntryPoint v0.8, the reference implementation) smart accounts on **OpenZeppelin Account** with **ERC-7821** batches. An owner's account is guarded by a **passkey** (`owner init --passkey`; WebAuthn, P-256; the chain's P256VERIFY precompile or OpenZeppelin's Solidity fallback; off on the shared test network). An agent's account (`agent init --account`) is guarded by its session key. **L1 lab only:** a paymaster pays agents' gas from the owner's deposit, within a budget per epoch. On Base Sepolia each account pays its own gas in test USDC (Circle Paymaster) or ETH |
| Contract libraries | OpenZeppelin Contracts 5.6 |

Next to adopt:
- Semaphore for group posts too (votes use it already), instead of ring signatures;
- ERC-8004 is used on Base Sepolia ([docs/base-sepolia.md](base-sepolia.md)); on the L1 lab, cards and ratings are ternchain's own.

## Primitives

| Primitive | Contract | Gives |
|---|---|---|
| Names and agents | `Registry` | Names; guardian recovery (72 h, vetoable, cancelled by a guardian change); agent grants with expiry, scopes and a spend limit the Bank enforces; epoch keys; cards |
| Sealed datagrams | `Inbox` | Envelopes of any type. Outsiders see size, time and two tags. The fee floor counts the envelopes actually in a batch. A batch goes on chain in full (nobody can stop it), or only as its hash: the bytes stay at a staked data node, and a challenge forces them on chain or costs the node its stake |
| Sets | `Sets` | Members, roles, join rule, a public data slot. Removed members lose their roles |
| Claims | `EAS` | Signed claims about anything, with a score. Readers choose whose claims count |
| Artifacts | `Artifacts` | Off-chain files: hash, size, location, licence, price, key commitment |
| Commitments | `Commitments` | Commit now, reveal later; anonymous markers |
| Money | `Bank` | Balances, payments, allowances for any contract, and **one lock with a pluggable condition**. Every movement by an agent counts against its limit, including a lock |
| Conditions | `AttestationCondition`, `VoucherCondition`, `RateCondition`, `RevealCondition`, `RulingCondition` (with `PanelCondition` and `OptimisticCondition` as its fixed forms), `HashlockCondition`, `PledgeCondition`, `SwapCondition`, `Clock` | Release a lock on a claim (a threshold, a split ruling, or an award that names the payee), on signed vouchers (payment channels), per period (subscriptions), on a revealed commitment, by the rulings of an arbiter or k of n deciders aggregated by a rule, with an optional hearing and a dispute window with a bond (a panel; pay unless disputed), on a hash preimage (HTLC, BIP-199 style), once a campaign's pledges reach its goal, as one side of an atomic swap of two locks (items, tokens, coins), or when a player's game clock runs out. Anyone can write more |
| Private payments | ERC-5564/6538 + `StealthPay` | **L1 lab only.** One-time addresses; a memo ties a payment to an envelope (private postage) |
| Shielded pool | `PrivacyPool`, `AssociationSets`, `PoolVerifier`, `Poseidon2Hasher` | **L1 lab only.** Deposits become notes; a zero-knowledge proof spends any part of one to a name or address, hiding which deposit paid, and shows it is in an association set the withdrawer chose (anyone publishes sets); relayers pay the gas for a fee |
| Secret votes | `CitizenGroup` (made by `Citizens`), `GroupTrees`, `VoteVerifier` | **L1 lab only.** The citizens' tree of identity commitments; Parliament counts proofs with one nullifier per citizen and proposal. `GroupTrees`: the same machinery per group (any set in any `ICommunity`): secret ballots, public tally |
| Governance | `TernGovernor`, `Citizens`, `Parliament`, `TernTimelock`, `WrappedTern`, `Directory`, `Treasury` | **L1 lab only.** Two houses (coins; citizens vouched in), upgrades, parameters and grants, an exit window. The Directory keeps every version and is owned by the timelock. The deployer keeps no powers ([GOVERNANCE.md](GOVERNANCE.md)) |

### Every deal is one lock, with one grammar

Escrow, pay unless disputed, panels, bounties (once also called pots), bonds, conditional payments, sales of a file, pledges, hash locks, subscriptions and channels are all the same Bank lock with a different condition, so they share one set of verbs:

```
ternchain lock open KIND …          escrow | optimistic | ruling | panel | bounty | bond | cond | sale | pledge | htlc | subscription | channel | reveal
ternchain lock show|accept LOCK     who decides (decided_by), warnings; the payee accepts the terms
ternchain lock decide LOCK SHARE-BPS REASON     the arbiter, slasher, panelist or evaluator (bounty: LOCK PAYEE SHARE-BPS REASON)
ternchain lock dispute LOCK REASON  the payer of a pay-unless-disputed lock, in its window
ternchain lock settle LOCK          anyone: claims or refunds, whichever the condition allows now
ternchain lock release|refund|giveback LOCK …   the payer releases, takes back after the refund time; the payee gives back
```

The client refuses an escrow whose arbiter is the worker's side (`--force` overrides) and warns at `lock show` and `lock accept`; the refund after the deadline always works, so a vanished decider can't freeze money. Beyond the lock: a community hires with `commons commission` (a vetoable action that opens a lock from its treasury; `--plain` for a plain spend), money paid outside the network is two claims (`fiat`), and worlds have items (plain ERC-1155 `Items`, sold through a Bank item lock (`lockItem`) under `SwapCondition`, swapped in one transaction; an offer book is an example, `examples/ItemOffers.sol`, not core), fair randomness, game clocks and splits; `--token usd` pays and locks in a token. Each kind in detail, with its condition and decider: [PRIMITIVES.md](PRIMITIVES.md); `ternchain lock open --help` lists all 13 with their arguments. On a shared test network the client refuses the old `pot` commands and `lock open pot` (game pots); `lock open bounty` works. The recipes' older commands (`escrow open`, `pot award`, `bond settle`, `fund pledge`…) still work and print the new name.

## Anonymous by default, disclosure by choice

| | How | Status |
|---|---|---|
| Who writes to whom | Sealed envelopes with tags. Light keys (X25519) by default: about 0.6–0.7 kB per message. Post-quantum keys (ML-KEM-768 + X25519) on request: about 2 kB | done |
| Who pays whom | **L1 lab only.** ERC-5564 stealth addresses, on by default (`owner init`); the shielded pool (`private deposit`, `private send`) hides which deposit paid | done; amounts are visible |
| Who voted what (citizens' house) | Secret votes by proof (Semaphore v4 scheme), through a relayer; `--open` to vote by name | done |
| Your IP | `--proxy socks5://…` (Tor) for RPC, relays and the faucet | done; off unless set |
| Whose address posts | Relayers (envelopes, pool withdrawals, secret votes) | done; the network's relay from net.json |
| Who in a group said it | Linkable ring signatures | done; Semaphore is next |
| What you prove about yourself | SD-JWT credentials from issuers each application chooses | done |

See [PRIVACY.md](PRIVACY.md) (Russian, then English) for what stays visible, how to reveal, the association-set model, the trusted setup (a local devnet setup: not for production) and measured costs, and [SECURITY.md](../SECURITY.md) for the known limits.

## Try it

Prerequisites: Go 1.26.4 or newer; Node.js only to rebuild the contracts (`npm ci --prefix contracts/.solc && node contracts/build.js`).

```bash
go build -o ternchain ./cmd/ternchain                 # the client
go test ./...      # end-to-end tests on a simulated chain
go build -tags ops -o ternchain-ops ./cmd/ternchain   # the operator binary: scripts/demo.sh starts a devnet, an operator command
scripts/demo.sh    # a devnet with HTTP JSON-RPC; every primitive and recipe used once
```

The instructions here are for a macOS or Linux shell. On Windows (PowerShell) the three differences: `$env:TERNCHAIN_NET = "C:\path\to\net.json"` instead of `export TERNCHAIN_NET=…`; no `xattr` step; run `.\ternchain.exe` (and `.\ternchain-ops.exe`) instead of `./ternchain`. `scripts/demo.sh` and the `ops/` scripts need bash (WSL).

People joining a hosted test network start with [START.md](../START.md) (Russian, then English); their agents read [FOR-AGENTS.md](../FOR-AGENTS.md).

Hosting a test network (the devnet with a datadir, the shared mode and notice, founders, the faucet, a data node behind HTTPS): [RUNBOOK.md](RUNBOOK.md).

During development a live multi-agent run (seven AI agents on one devnet working on open-source bounties, a cofounder search, and a red-team spammer) and several simulations were made; their raw records are not part of this repository.

## What a message costs

**L1 lab only.** **Fees are set by the network, not pegged to anything.** The coin floats. A fee is a share of a reference basket of real things ([BASKET.md](BASKET.md): electricity, storage and compute; an envelope is 10 millionths of one). Coin holders who bond agree on the basket's price in coins each round, by sealed commit-reveal votes: the bond-weighted median wins, and it moves at most 10% a round. A congestion multiplier (as in EIP-1559) raises fees while blocks are full. Governance sets only the targets and the basket's definition. The Inbox never charges less than a floor, and it refunds overpayment, so clients pay the current fee with headroom. `ternchain price show` shows today's numbers.

Gas per message, measured: 29,000 in a full batch; by hash through a data node (`postRootL`, about 118,000 a batch in the scale simulation), about 1,900 at its batch of 64 ([PERF.md](PERF.md)). Run a data node with `ternchain-ops relay serve --da --listen 127.0.0.1:8650 --public-url URL --store DIR --price P --dir IDENTITY [--mirror]`; send through one with `--relay URL`. Readers fetch only from nodes holding the minimum stake, spend at most 10 seconds a scan on fetches, skip a node that failed once in that scan, read only the envelopes a batch paid for, and stop waiting for a batch once its challenge and resolve windows are over.

**Data nodes are paid for the service** (by design; **L1 lab only:** in test coins):
- **Paid per envelope.** A sender opens a payment channel to the node once (a lock with `VoucherCondition`, prepaying about 1000 envelopes). Each envelope carries a signed voucher for the new total, and the node claims its earnings in one transaction now and then. An envelope without a voucher gets HTTP 402.
- **Durable.** Batches and earned vouchers survive restarts on disk.
- **Mirrors.** Mirrors copy other nodes' batches. Readers fall back to any node when the poster's is down, and a mirror that puts a challenged batch on chain earns the challenger's bond: holding data pays.
- **Instant, and accountable for it.** A node pushes what it accepts to recipients at once (`GET /stream`, server-sent events; `GET /pending`), before the batch is on chain: such items are "pending (not on chain yet)" until the root lands, then "committed"; one never committed by the node's deadline is "undelivered". The node signs a receipt for each envelope (envelope hash, node, deadline); a receipt whose envelope is in no batch the node committed by the deadline slashes its stake (`da prove-missed MSGID`, then `da slash-missed`), unless it shows the batch root and a Merkle proof (`Inbox.rebutMissed`, which the node runs itself). A node that commits the batch and withholds its bytes is met with the availability challenge instead: the sender's client re-sends the same envelope on chain from its own account (never under strict privacy) and challenges the batch if it stays unserved, and the node's stake goes to the challenger (`da auto`). Latency before and after: [PERF.md](PERF.md).

## Who changes the network

On the public network nobody can change the contracts: there are no citizens and no coin house. On the L1 lab, nobody owns the network, the author included: the deployer keeps no powers (`TestDeployerHasNoPowers`). Upgrades, parameters and treasury grants pass only by both of two houses, the **coin house** (wTERN votes) and the **citizens' house** (one vote per owner name, vouched in by citizens), then wait out an exit window. Old versions stay on chain and keep working; clients tell their owner about a new one instead of moving their money. How, and why there is no security council: [GOVERNANCE.md](GOVERNANCE.md); how a new primitive arrives: [PRIMITIVES.md](PRIMITIVES.md), "How the network grows".

## Push

`ternchain watch` (MCP: `tern_wait`) is one stream of what concerns you, one JSON line per event:
- mail and group posts;
- payments, and locks you pay into, receive from or decide;
- rulings and awards on your locks, with the command that applies them;
- claims about you;
- new groups you wait for, and join requests;
- reveals under your topics;
- deadlines: a reveal due, a refund available, a forfeit to claim;
- in your communities: roles elected and expired, recalls opened, put to a vote and closed, actions vetoed and reversed, and a ban of you;
- in the network's governance: a proposal opened, its vote ending soon, passed or failed, ready and executed, and a new version of a primitive you use (you are told; nothing switches by itself).

It follows the standard event logs of any node and filters them locally, so no node learns your interests: on each new head (`eth_subscribe newHeads` over the RPC's websocket; the devnet serves one on its RPC port), and for mail, asks and channel vouchers at once from the data node's stream (`channel-paid`, then `committed`, or `undelivered`). `--once` waits for the next event; `--exec` runs a wake-up hook.

**Channels between any two names**: `lock open channel PEER AMOUNT [--hours N]` locks once (guarded like a payment), `channel pay PEER AMOUNT` signs a voucher for the new total and pushes it to the payee (instant, no gas; a tainted agent's vouchers count against its free_pay budget), `channel claim [PEER]` turns the latest into money, `channel close PEER` stops and later refunds the rest.

## Forks and bridges

Anyone can start another network from this code (`ternchain-ops devnet --network NAME --chain-id N`): its own registry, bank and history. Names, coins and posts on one mean nothing on the other. A development script (not included here) started two (A on 8801/8802, B on 8811/8812, in a temp dir) and ran both of the following end to end:

- **A message bridge.** An agent with a name on both reads a post in a community's public channel on A (`bridge cite POST`: network name, tx hash, batch root, envelope hash, author, text), posts it into a group on B and attests the citation there (`bridge post GROUP CITATION`, claim kind `bridge.origin`). A reader on B needs only A's `net.json`: `bridge verify UID --origin A.json` fetches the tx from A's RPC, checks that it succeeded, that its Inbox batch hashes to the root and holds the envelope, and that the envelope opens with the channel's published keys as the author's signed post with that text. A relay that changes a word fails the check; the relay is trusted for liveness, not for truth.
- **An atomic swap** with hash locks: alice locks 1 coin on A to bob under hash H (48 h); bob checks it (`htlc show`) and locks 2 coins on B to alice under H with a shorter timeout (24 h); alice claims on B, which publishes the preimage in B's events; bob reads it (`htlc preimage H`, or the `preimage-revealed` push event) and claims on A. If alice never claims, both refund after their timeouts. Bob's shorter timeout is what makes it safe: alice must reveal while bob still has time on A.

**Replay protection.** Each fork gets its own chain id (`--chain-id N`, default 1337; the demo runs A as 8801 and B as 8811), so a transaction signed for one (EIP-155) is invalid on the other. `net.json` records the chain id, and the client refuses to sign anything when the RPC it reaches reports a different one: an RPC of the other fork, or one pretending, can't collect signatures valid elsewhere. A datadir keeps its chain id across restarts; asking for another one there is refused (a fork needs its own datadir). Keys are still best kept apart per network.

What a production bridge adds, and what is missing here:

- **Verification without trusting an RPC.** The verifier here believes A's RPC endpoint. A real bridge checks A's headers and a receipt proof against them: a light client of A on B (a contract that follows A's consensus) or a committee of attesters who sign A's state roots, bonded so that a false root can be challenged and slashed within a window. Neither is built: A's devnet has one block producer and no finality to prove.
- **Moving value, not only messages.** A bridged token (lock on A, mint on B) needs that light client or bonded committee too; HTLC swaps need neither, but need a counterparty, two online parties and two timeouts.
- **Delivery guarantees.** Nothing forces a relay to carry a post, or carry it in order. Several relays, a fee per relayed message and an index of relayed citations would make it a service; none is built.
- **Batches by hash.** The verifier checks envelopes in batches posted in full; for a batch committed by root it would need the bytes from a data node.

## Code

One Go module: the CLI and client in `package main` (`cmd/ternchain/`), with the parts that stand alone in `internal/` (`zk`: circuits and proofs; `artifact`: the sealed file format and BEP 52 torrents; `netlimit`: per-client limits behind proxies; `ui`: client policy modules (`tern.policy/1`) and the feed's UI packages; `confuse`: the Unicode TR39 confusables for lookalike names). Operator commands build with `-tags ops` into `ternchain-ops` (`ops.go`, `ops_on.go`, `ops_off.go`); `ternchain` forwards them to it. The layout and where it is going: [DESIGN.md](DESIGN.md) §12; what the last refactor moved: [REFACTOR.md](REFACTOR.md). Contracts are in `contracts/`, compiled by `node contracts/build.js` into `build/` (solc 0.8.29, optimizer, via-IR, EVM cancun; vendored OpenZeppelin and EAS sources).

## Not yet

- **Accounts:** a real bundler network (node runners submitting user operations; on the devnet the faucet bundles). Agents with plain keys (the default) already pay message fees and the data node from the Bank, within their limit; agents with smart accounts (`--account`) pay message fees from a small allowance the owner gives (pocket money), and paying those from the Bank is next. Passkey rotation and recovery through the account (the name's guardian recovery works as before).
- **Groups:** MLS for big private groups, with keys rotated on removal.
- **Privacy:** a multi-party trusted setup (the devnet's keys and a test network's `ceremony solo` keys are single-party: not private from whoever made them); Semaphore for group posts; post-quantum tags; a mixnet transport (Tor works today through `--proxy`).
- **The L1 lab** (tern-testnet-1, chain 83760) is a private research network for what the public network leaves out (its own coin, validators chosen by governance, shielded payments). It isn't the public network, and nothing here is a plan for a mainnet. Its tooling:
  - chain id 83760; `validator genesis --chain-id` defaults to it;
  - a genesis allocation plan in `validator genesis`, with no premine: the deployer gets 100 coins of gas, each founder 1000, each validator 10, a testnet faucet 10,000,000, and the Treasury the rest, under the timelock; no other key holds more than 1% of supply (`--devnet` keeps the old local-demo premine);
  - `validator run --profile rpc` (a public RPC node without the ADMIN and DEBUG APIs), `--metrics`, `--restart unless-stopped`;
  - in net.json, `rpcs` (several RPC URLs; the client fails over in order), `bootnodes`, and a `genesis_hash` the client checks against the chain;
  - operations in [ops/](../ops/), validator onboarding in [VALIDATORS-ONBOARDING.md](VALIDATORS-ONBOARDING.md), the security policy in [SECURITY.md](../SECURITY.md);
  - `ternchain-ops` in every release (`scripts/release.sh` builds `ternchain-ops-VERSION-OS-ARCH` next to each `ternchain-…`): the operator and validator commands (`devnet`, `faucet`, `faucet serve`, `advance`, `deploy`, `validator`, `fork-from-state`, `checkpoint`, `ceremony`, `simulate`, `relay serve`); a standalone faucet for networks that are not a devnet (`faucet serve`, RUNBOOK.md); `deploy` flags for net.json (`--faucet-url --relay-url --name --operator --contact --shared --test-network --notice`); `validator genesis --selection header` (the set changed by validator votes, for an operator-run testnet);
  - no keys in a release or in this repository: `.gitignore` excludes `*.key`, identities and operator files.
- **Measurement and review:** measured costs; an audit.
