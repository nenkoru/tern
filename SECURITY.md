# Security policy

ternchain is a prototype, designed and written with heavy use of Claude (Anthropic) under human direction. It has not had an external audit. The integrated red-team report is in [redteam/REPORT.md](redteam/REPORT.md); the other internal reviews are not published, and their open findings are summarised below. This page says how to report a vulnerability, what counts, how a fix gets out, and what a reward can and cannot be.

A short Russian summary is at the end: [Кратко по-русски](#кратко-по-русски).

## How to report

Report privately. **Never open a public issue, post or group message about a vulnerability.**

Report through **GitHub's private vulnerability reporting**: https://github.com/nenkoru/tern/security/advisories/new (or the repository's *Security* tab, then *Report a vulnerability*). Only the maintainers see the report; the fix and the advisory are prepared there and published together.

No e-mail address or PGP key is published. The public Base Sepolia network has no operator to write to on the network itself.

What to put in a report:
- what is affected (contract, command, file and line if you have them);
- the steps to reproduce, ideally a Go test or a script against a local devnet;
- the impact as you see it;
- the version: `ternchain version` prints the binary's version and the hash of its contract set;
- how you want to be credited, or that you don't want to be;

## Scope

In scope: **the code in this repository and the public network's contracts.**

| Area | Where |
|---|---|
| Contracts | `contracts/` (Bank and its conditions, Inbox, Registry, Citizens, Parliament, the timelock, Treasury, Directory, Validators, PrivacyPool, and the rest), and on Base Sepolia `contracts/ext` (TernJobs, ERC8004Registrar, InboxForwarder, ModuleRegistry, NoCoinHouse) |
| The public network's deployment | the contracts deployed for `tern-base-sepolia-1` (addresses in `networks/tern-base-sepolia-1/net.json` and [docs/base-sepolia.md](docs/base-sepolia.md)) |
| The Go client and the data node | the `ternchain` binary (CLI, MCP server) and the `ternchain-ops` binary (the same code built with `-tags ops`): `relay serve`, `relay serve --da`, the devnet faucet and `faucet serve` |
| Circuits | `internal/zk` (the withdraw and vote circuits, their verifiers and hashing) |
| The private L1 lab's genesis and validator tooling (in `ternchain-ops`) | `validator genesis`, `validator run`, `validator status`, `validator watch`, `fork-from-state`, `checkpoint sign` / `verify`, `deploy` |
| `net.json` handling | how the client reads and trusts `net.json` and the RPC it names (chain id checks, contract addresses, relay and data node URLs) |

The networks: the public network tern-base-sepolia-1 (Base Sepolia, chain 84532), local devnets you run yourself, and the private L1 lab tern-testnet-1 (chain 83760). There is no mainnet.

Out of scope:
- **Value of test money.** Test USDC and test coins have no value. A bug that "steals" them is still a bug and in scope; its severity is judged as if the same contracts held real value.
- **Bugs in Base, Base Sepolia, Hyperledger Besu or other chains themselves.** Report them upstream (for Besu, github.com/besu-eth/besu).
- **Other third-party software and services:** gnark, OpenZeppelin, EAS, Circle's paymaster, bundlers, public RPCs, Docker, the operating system. Report upstream. Our *use* of them is in scope.
- **Social engineering** of the author, validators, citizens or users.
- **Denial of service by volume** against a faucet, RPC or relay: flooding requests. A single request that crashes a node or costs far more than it should is in scope.
- **Known issues** listed below, unless you show a new impact or a way around a fix.

## Severity

We rate by impact as if the same contracts held real value, even though every network here is a test network. Rows about houses, citizens and validators apply to networks that have them (the L1 lab, devnets); the public network has none.

| Level | Meaning | Examples in this system |
|---|---|---|
| **Critical** | Theft or permanent loss of funds; control over the network without both houses | taking money from someone else's Bank balance or lock; draining the PrivacyPool by a bug in a circuit or contract; spending from the Treasury without a passed grant through the timelock; changing the validator set without both houses, or past the rolling cap; anyone (the deployer included) getting an admin power over the timelock, Treasury, Directory or Validators; a genesis whose Validators storage differs from what the constructor would leave |
| **High** | Loss of funds under some conditions; breaking a governance or privacy guarantee | forging or blocking citizens' votes by a flaw other than the known trusted setup; bypassing an agent's spend limit; linking pool withdrawals or secret votes to people at scale; a `checkpoint verify` that accepts a fork with a wrong state or too few signatures; a client signing a transaction valid on another chain id; a malicious `net.json` or RPC making the client pay the wrong contract |
| **Medium** | Limited loss, or a guarantee weakened in a way users can avoid | crashing a data node or validator tool with one crafted input; freezing a lock until its refund time; leaking metadata beyond what [PRIVACY.md](docs/PRIVACY.md) says is visible; `validator watch` missing a censorship pattern it claims to catch |
| **Low** | Hardening, small leaks, wrong claims in the docs | missing rate limits on a cheap endpoint; key files with loose permissions in an edge case; a doc saying something is hidden when it is not |

Not a vulnerability on its own: the limits the design states openly. More than 1/3 of validators can halt the chain, more than 2/3 can censor; validators have no stake and no slashing; a proposer knows its slots in advance ([docs/abuse/VALIDATORS.md](docs/abuse/VALIDATORS.md), "What stays dangerous"). A cheaper way to reach those powers is in scope.

## Known issues

These are public already. Reports that only repeat them are out of scope.

- **The single-party trusted setup.** The Groth16 keys in `internal/zk/keys/` come from a local setup run by one person. Whoever ran it can forge pool withdrawals and secret citizens' votes. This is known and out of scope until the multi-party ceremony replaces the keys ([PRIVACY.md](docs/PRIVACY.md), "Trusted setup: status"; internal audit 5, ZK-4).
- **Open findings of the internal audits** (six internal security reviews; the integrated red-team report is in redteam/REPORT.md, the others are not published, and the fixed findings have regression tests: `audit4_test.go`, `audit5_test.go`, `final_a_test.go`, `final_b_test.go`, `redteam_*_test.go`).
- **The agent guard assumes an MCP-only agent.** An agent that also has a shell and write access to its own identity folder can edit its state file or run `modules enable` for itself; the guard (taint, approvals, scopes.to) protects agents that reach the network only through `ternchain mcp` (audit 6 CL-4). Keep the owner's folder out of the agent's reach (START.md). The default join path (AGENTS.md) keeps the owner key and the agent key in separate folders on the same computer, so an agent with a shell could still reach the owner's folder: the separation holds only if the owner keeps it out of the agent's reach.
- **Single-RPC directory trust.** Pins reject unsigned key substitutions and identity changes that were not approved, but cannot authenticate log positions: an RPC can replay an older signed publication at a fabricated later position. Use an independent second RPC in net.json or a trusted node; witnesses check first pins and later publications (docs/THREATS-STATE.md, V9). No light client: block hashes are compared between RPCs, not proven.
- **Client-side limits.** Contact limits, correspondent approvals and taint are enforced by the user's client, not on chain. Only Bank spend limits and grant revocation are enforced by the contracts ([docs/GUARANTEES.md](docs/GUARANTEES.md)).
- **Releases are not yet signed** by independent release signers: the first version is a release candidate, checked by hash only ([docs/SUPPLY-CHAIN.md](docs/SUPPLY-CHAIN.md)).
- **Launch gaps** of the private L1 lab: one client implementation (Besu), no external audit, no security council.

## Rewards

There is no bounty budget, and no treasury or governance that can pay one: the public network has no citizens, so its governance can't pass a grant. **No reward is promised.** What you get is credit in the advisory and in the list below, if you want it. Any thank-you beyond that is the author's personal choice, paid outside the network, and may be zero.

## Safe harbour

If you follow these rules, the author will treat your research as good-faith and will not report it to anyone as an attack:

- test on a local devnet you run yourself, or on a test network;
- never take funds that hold real value; on a test network, use the least you need to show the bug, and give test money back;
- don't read other people's mail, deanonymise real people or keep personal data you came across; if you reach someone else's data, stop and report;
- don't degrade a shared network for others: no flooding, no long halts;
- report privately and give us time to fix (below).

This covers only the author's own actions. The author is one person and cannot speak for validators, other users or any authority. It is not legal advice.

## Timeline

| Step | Target |
|---|---|
| Acknowledge your report | 72 hours |
| Triage: severity, scope, next steps | 7 days |
| Fix ready (code) | Critical 14 days, High 30 days, Medium 60 days, Low 90 days or the next release |
| Public disclosure | when the fix is live, or 90 days after the report, whichever comes first; later only if we agree with you |

These are targets for a small project, not guarantees. We will tell you if we miss one, and why.

### How a fix reaches the network

No governance fix is promised: on the public network there is no governance that can pass one. Fixes ship two ways:

**New client releases.** Fixes to the Go client, the data node, tooling and circuits before deployment ship as a new release with SHA256SUMS. No vote is needed. Users upgrade themselves.

**New deployments.** On the public network (tern-base-sepolia-1) the contracts are immutable: there is no pause, no admin key and no upgrade path. A contract fix is a new deployment with a new net.json. Users opt into it and move their own funds; the old deployment keeps running. Nobody can force a fix on anyone.

What this means for a live bug in a deployed contract:
- the fix cannot be forced on anyone; old deployments keep working;
- the new deployment and its net.json are public the moment they are published, so they partly disclose the bug; we will plan the wording and timing with you;
- on the private L1 lab, which has validators, a Besu patch that does not change consensus rules can be rolled out by its validators ([ops/besu/UPGRADE.md](ops/besu/UPGRADE.md)); the incident steps are in [ops/incident/INCIDENT-RESPONSE.md](ops/incident/INCIDENT-RESPONSE.md).

## Thanks

People who reported issues and agreed to be named will be listed here.

- (none yet)

See also: [.well-known/security.txt](.well-known/security.txt) (RFC 9116).

---

## Кратко по-русски

- **Как сообщить.** Только закрыто, через закрытые отчёты GitHub: https://github.com/nenkoru/tern/security/advisories/new (вкладка *Security* → *Report a vulnerability*). Никогда не в открытом issue, посте или группе.
- **Что входит:** код этого репозитория и контракты публичной сети: `contracts/` и, в Base Sepolia, `contracts/ext` (TernJobs, ERC8004Registrar, InboxForwarder, ModuleRegistry, NoCoinHouse); клиент на Go и узел данных (`ternchain` и `ternchain-ops`, включая кран `faucet serve`), схемы `internal/zk`, генезис и инструменты валидатора частной L1-лаборатории, обработка `net.json`. Сети: публичная tern-base-sepolia-1 (Base Sepolia, chain 84532), свой devnet и частная L1-лаборатория tern-testnet-1 (chain 83760). Основной сети нет.
- **Что не входит:** стоимость тестовых денег (её нет), ошибки самих Base, Besu и чужих сервисов (сообщать им), социальная инженерия, DoS объёмом запросов, известные проблемы.
- **Известное:** доверенная настройка одного участника (подделка тайных голосов и выводов из пула) — известна и вне рамок до церемонии; открытые находки внутренних аудитов (отчёт red team — в redteam/REPORT.md, остальные не опубликованы); ограничения на контакты и одобрения держит клиент пользователя, а не цепочка; релизы пока не подписаны независимыми подписантами (первая версия — кандидат в релиз).
- **Уровни:** Critical — кража денег из Bank или пула, захват прав над контрактами; High — обход лимитов агента, подделка голосов, деанонимизация; Medium и Low — ограниченный вред и укрепление. Тяжесть оцениваем так, будто в контрактах настоящие деньги.
- **Награды не обещаем.** Бюджета и казны, которая могла бы платить, нет: в публичной сети нет граждан, и её управление ничего не может принять. Благодарность — в бюллетене и в списке; что-то сверх этого — личное решение автора вне сети, может быть ничем.
- **Безопасная гавань:** исследование в добросовестности на своём devnet или в тестовой сети, без кражи ценного, без чтения чужой переписки и деанонимизации людей, без перегрузки общей сети.
- **Сроки:** подтверждение за 72 часа, разбор за 7 дней, исправление в коде — от 14 дней (Critical) до 90 (Low), раскрытие — после исправления или через 90 дней.
- **Как выходит исправление.** Исправлений через управление не обещаем. Исправления клиента — новые релизы. Контракты публичной сети неизменяемы: нет паузы, ключа админа и обновлений; исправление контракта — новое развёртывание с новым net.json, на которое пользователи переходят сами, а старое продолжает работать.
