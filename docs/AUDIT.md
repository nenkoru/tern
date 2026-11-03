# Contract audit: what was checked, what was found

tern's contracts have had **no professional audit**. Before the first public release (v0.6.0, November 2026) they went through every open-source check we could run, plus a manual review by an AI model following Trail of Bits' published audit methods. This page says what was run, what it found, and what protects you until a new contract version fixes it.

The deployed contracts are immutable: nobody can patch them. Findings are fixed in the **next contract version**, deployed as a new network that people opt into. Base mainnet (`tern-base-1`) is deployed only on that version.

## What was run

| Kind | Tools | Scope |
|---|---|---|
| Static analysis | Slither 0.11.6, Aderyn 0.6.8, Semgrep with Decurity's Solidity rules, Solhint 6.2.5, Mythril 0.24.8 | All contracts in `contracts/` and `contracts/ext/` |
| Property fuzzing | Foundry invariant tests (1024 runs × depth 128), Echidna 2.3.3, Medusa 1.5.1 (3.5M calls) | Bank and every lock condition, TernJobs, accounts, paymasters, forwarder, module registry |
| Symbolic checks | Halmos 0.3.3 | Lock payout, withdrawals, agent limits, job pays once |
| Manual review | Claude, following Trail of Bits' audit-context-building, entry-point and secure-contract checklists | Money core (Bank, conditions, accounts, paymaster, governance) and the Base, social and community contracts |

Every finding below marked *proven* has a failing test that demonstrates it.

## What held

- **The money core.** Across millions of fuzzed calls with adversarial conditions, re-entrant tokens and an independent model of every balance: the Bank never pays out more than it holds, a lock never pays more than its amount or pays twice, refunds go only to the payer after the deadline, and agents never spend past their limits. Halmos proves the same for the key properties.
- **Escrow jobs (ERC-8183).** A job pays once: to the provider on completion or back to the client, never both. Only the evaluator decides.
- **Accounts.** User operations are bound to the chain, the EntryPoint and the account; agent keys act only for their own names.
- **Governance.** With no citizens and a coin house that always reports zero, nobody can propose anything, and the deployer holds no role.
- **Mail.** Nobody can replace another name's keys or forge a sender.

## What was found

| Severity | Finding | Status on the live test network |
|---|---|---|
| High | **TernPaymaster under-charges gas** (`Accounts.sol` postOp): the pooled ETH deposit pays the gap, so a self-bundling attacker can drain other owners' deposits. *Proven.* | Holds 0 ETH. The client refuses to deposit into it and never routes through it; gas is paid in USDC through Circle Paymaster. `gas withdraw` takes any deposit out. |
| High | **The owner account's mail key passes ERC-1271 for any hash** (`Accounts.sol`): a stolen mail key could sign a USDC transfer from the account or an attestation as its owner. *Proven.* | Not fixable from the client. Keep little USDC in an owner account and guard the mail key like the account key. Fixed in the next version by binding mail signatures to their own domain. |
| Medium | Bank credits the requested amount for ERC-20 deposits, so fee-on-transfer tokens leave it short. *Proven.* | The client deals only in USDC. |
| Medium | A TernJobs provider can change the price right up to funding. *Proven.* | `job fund ID PRICE` refuses a price you didn't agree to. |
| Medium | Anyone can transfer an ERC-8004 agent id to a name's address and collect its reviews. *Proven.* | The client uses only ids minted for the name and checks their name tag. |
| Medium | In a community, a banned owner's agent keeps its elected powers; Sets bans don't cover a banned owner's other agent names; Sets moderators can remove admins. *Proven.* | Readers ignore moderation by banned owners' names, and admins don't send group keys to them. Channel keys published through a community need the contract fix. |
| Medium | Random can be steered by one participant (withheld reveals, draw timing) or blocked by spam. *Proven.* | Documented: don't let Random decide anything of value. |
| Medium | A data node can challenge its own root and take its stake back, so withholding mail costs it nothing. *Proven.* | Data nodes are optional; mail goes on chain directly by default. |
| Medium | Anyone with a large returnable bond can keep raising the Inbox fee through the FeeOracle. | Expensive and slow; fixed in the next version. |
| Medium | Secret-ballot keys come from a one-party setup. | Documented; a multi-party ceremony is required before anything valuable depends on them. |
| Low | About a dozen more: agent token limits outlive a re-grant, unbounded postOp penalty in SponsorPaymaster, pull-payment griefing, wrong field constants in two optional modules, unfenced game-move text (fixed in the client), and others. | See the reports. |

## The reports

The full reports, tests and harnesses are kept with the project's review files and are available on request through [SECURITY.md](../SECURITY.md). Re-running them needs only Foundry, Slither and the other tools listed above.

If you find something we missed, please report it privately ([SECURITY.md](../SECURITY.md)).
