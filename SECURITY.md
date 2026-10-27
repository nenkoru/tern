# Security policy

ternchain is a prototype. It has not had an external audit. The internal reviews are not published yet; their open findings are summarised below. This page says how to report a vulnerability, what counts, how a fix gets out, and what a reward can and cannot be.

A short Russian summary is at the end: [Кратко по-русски](#кратко-по-русски).

## How to report

Report privately. **Never open a public issue, post or group message about a vulnerability.**

Two private channels:

1. **Email:** `security@` (to be set before public release).
   - PGP key: not published yet. Until a key is published, send only a short note asking for a channel, without details.
2. **ternchain mail** to the operator's name. The name is in the `notice` field of the network's `net.json`.

   ```bash
   ternchain send --dir D --to OPERATOR-NAME --subject security --text-file report.txt
   ```

   The text is sealed, but anyone reading the chain sees the sender's and recipient's addresses, the time and the size. Use a fresh name if that matters to you.

What to put in a report:
- what is affected (contract, command, file and line if you have them);
- the steps to reproduce, ideally a Go test or a script against a local devnet;
- the impact as you see it;
- the version: `ternchain version` prints the binary's version and the hash of its contract set;
- how you want to be credited, or that you don't want to be;
- a ternchain name, if you want a reward proposed to you (see [Rewards](#rewards)).

## Scope

In scope:

| Area | Where |
|---|---|
| Contracts | `contracts/` (Bank and its conditions, Inbox, Registry, PrivacyPool, Citizens, Parliament, the timelock, Treasury, Directory, Validators, and the rest) |
| The Go client and the data node | the `ternchain` binary (CLI, MCP server) and the operator binary `ternchain-ops` (the same code built with `-tags ops`): `relay serve`, `relay serve --da`, the devnet faucet and `faucet serve` |
| Circuits | `internal/zk` (the withdraw and vote circuits, their verifiers and hashing) |
| The L1 genesis and validator tooling (in `ternchain-ops`) | `validator genesis`, `validator run`, `validator status`, `validator watch`, `fork-from-state`, `checkpoint sign` / `verify`, `deploy` |
| `net.json` handling | how the client reads and trusts `net.json` and the RPC it names (chain id checks, contract addresses, relay and data node URLs) |

The networks: local devnets you run yourself, the shared test network, and the L1 testnet (chain id 83760). The mainnet (chain id 8376) does not exist yet.

Out of scope:
- **Value of test coins.** Coins on any test network have no value. A bug that "steals" test coins is still a bug and in scope; its severity is judged as if the same bug hit mainnet.
- **Bugs in Hyperledger Besu itself.** Report them upstream to the Besu project (github.com/besu-eth/besu). Tell us too if the bug affects our network, so validators can prepare.
- **Other third-party software:** gnark, OpenZeppelin, EAS, Docker, the operating system. Report upstream. Our *use* of them is in scope.
- **Social engineering** of the operator, validators, citizens or users.
- **Denial of service by volume** against the public faucet, RPC or relay: flooding requests. A single request that crashes a node or costs far more than it should is in scope.
- **Known issues** listed below, unless you show a new impact or a way around a fix.

## Severity

We rate by impact on mainnet settings, even when the bug was found on a testnet.

| Level | Meaning | Examples in this system |
|---|---|---|
| **Critical** | Theft or permanent loss of funds; control over the network without both houses | taking money from someone else's Bank balance or lock; draining the PrivacyPool by a bug in a circuit or contract; spending from the Treasury without a passed grant through the timelock; changing the validator set without both houses, or past the rolling cap; anyone (the deployer included) getting an admin power over the timelock, Treasury, Directory or Validators; a genesis whose Validators storage differs from what the constructor would leave |
| **High** | Loss of funds under some conditions; breaking a governance or privacy guarantee | forging or blocking citizens' votes by a flaw other than the known trusted setup; bypassing an agent's spend limit; linking pool withdrawals or secret votes to people at scale; a `checkpoint verify` that accepts a fork with a wrong state or too few signatures; a client signing a transaction valid on another chain id; a malicious `net.json` or RPC making the client pay the wrong contract |
| **Medium** | Limited loss, or a guarantee weakened in a way users can avoid | crashing a data node or validator tool with one crafted input; freezing a lock until its refund time; leaking metadata beyond what [PRIVACY.md](PRIVACY.md) says is visible; `validator watch` missing a censorship pattern it claims to catch |
| **Low** | Hardening, small leaks, wrong claims in the docs | missing rate limits on a cheap endpoint; key files with loose permissions in an edge case; a doc saying something is hidden when it is not |

Not a vulnerability on its own: the limits the design states openly. More than 1/3 of validators can halt the chain, more than 2/3 can censor; validators have no stake and no slashing; a proposer knows its slots in advance ([docs/abuse/VALIDATORS.md](docs/abuse/VALIDATORS.md), "What stays dangerous"). A cheaper way to reach those powers is in scope.

## Known issues

These are public already. Reports that only repeat them are out of scope.

- **The single-party trusted setup.** The Groth16 keys in `internal/zk/keys/` come from a local setup run by one person. Whoever ran it can forge pool withdrawals and secret citizens' votes. This is known and out of scope until the multi-party ceremony replaces the keys ([PRIVACY.md](PRIVACY.md), "Trusted setup: status"; internal audit 5, ZK-4).
- **Open findings of the internal audits** (six internal security reviews so far; the reports are not published yet, and the fixed findings have regression tests: `audit4_test.go`, `audit5_test.go`, `final_a_test.go`, `final_b_test.go`).
- **The agent guard assumes an MCP-only agent.** An agent that also has a shell and write access to its own identity folder can edit its state file or run `modules enable` for itself; the guard (taint, approvals, scopes.to) protects agents that reach the network only through `ternchain mcp` (audit 6 CL-4). Keep the owner's folder out of the agent's reach (START.md).
- **Launch gaps** of the L1: one client implementation (Besu), no external audit, no security council.

## Rewards

There is no fixed bounty budget. A reward is a treasury grant, and only the network can decide it.

How it works:
1. After the fix, the operator (or any citizen) proposes a grant to your ternchain name:
   ```bash
   ternchain gov propose grant YOUR-NAME AMOUNT --panel a,b,c --k 2 --days 30 --why "security report: …"
   ```
2. Both houses vote: the coin house and the citizens' house ([GOVERNANCE.md](GOVERNANCE.md)). Either one can say no.
3. If it passes, it waits out the exit window, and then anyone runs `gov execute`.
4. The money goes into a Bank lock. The reviewers on the panel release it to you.

What this means:
- **We cannot promise a reward.** Payment is a decision of the two houses. The operator can propose a grant and vote, nothing more.
- **On a testnet**, a grant is in test coins without value. You also get credit in the advisory and in the list below, if you want it.
- **Any cash part** is the operator's personal choice, paid by them outside the network. It is not a promise of the network, and it may be zero.
- The Treasury holds the supply that was not given out at genesis. It pays only through the timelock, after both houses ([GOVERNANCE.md](GOVERNANCE.md), "The treasury").

## Safe harbour

If you follow these rules, the operator will treat your research as good-faith and will not report it to anyone as an attack:

- test on a local devnet you run yourself, or on a testnet;
- never take funds on mainnet; on a testnet, use the least you need to show the bug, and give test coins back;
- don't read other people's mail, deanonymise real people or keep personal data you came across; if you reach someone else's data, stop and report;
- don't degrade a shared network for others: no flooding, no long halts; ask first if a test needs to stop the testnet;
- report privately and give us time to fix (below).

This covers only the operator's own actions. The operator is one person and cannot speak for validators, other users or any authority. It is not legal advice.

## Timeline

| Step | Target |
|---|---|
| Acknowledge your report | 72 hours |
| Triage: severity, scope, next steps | 7 days |
| Fix ready (code) | Critical 14 days, High 30 days, Medium 60 days, Low 90 days or the next release |
| Public disclosure | when the fix is live, or 90 days after the report, whichever comes first; later only if we agree with you |

These are targets for a small project, not guarantees. We will tell you if we miss one, and why.

### How a fix reaches the network

**Off-chain fixes** (the Go client, the data node, validator tooling, circuits before deployment) ship as a new release with SHA256SUMS. No vote is needed. Operators and users upgrade themselves.

**On-chain fixes take days, even weeks.** Contracts are never changed in place. A fix is a new version, published in the Directory by a governance proposal. There is no security council and no pause ([GOVERNANCE.md](GOVERNANCE.md), "Why there is no security council"). At launch settings:

| | Normal fix | Governance-critical fix |
|---|---|---|
| Voting delay (before the coin vote opens) | 2 days | 2 days |
| Vote in both houses | 7 days, majority in each | 7 days, 2/3 of votes cast in each |
| Exit window (timelock) | 7 days | 21 days (3 exit windows) |
| **Total, at the earliest** | **about 16 days** | **about 30 days** |

Governance-critical means an upgrade of Validators, Parliament, Citizens, CitizenGroup, Directory, the timelock, the Governor, wTERN or VoteVerifier, or a change to their rules. A scheduled chain upgrade (`gov propose param validators upgrade`) is critical too. A test network may run shorter windows.

What this means for a live bug in a version people use:
- the fix cannot be forced on anyone; old versions keep working;
- clients announce the new version (`gov updates`, the `new-version` push event); users move their own funds;
- the proposal is public the moment it is made, so it partly discloses the bug; we will plan its wording and timing with you;
- a Besu patch that does not change consensus rules can be rolled out by validators without a vote ([ops/besu/UPGRADE.md](ops/besu/UPGRADE.md)); the incident steps are in [ops/incident/INCIDENT-RESPONSE.md](ops/incident/INCIDENT-RESPONSE.md).

## Thanks

People who reported issues and agreed to be named will be listed here.

- (none yet)

See also: [.well-known/security.txt](.well-known/security.txt) (RFC 9116).

---

## Кратко по-русски

- **Как сообщить.** Только закрыто: почта `security@` (адрес будет задан до публичного выпуска) (адрес и ключ PGP будут позже) или письмо ternchain на имя оператора (имя — в поле `notice` файла `net.json`). Никогда не в открытом issue, посте или группе.
- **Что входит:** контракты `contracts/`, клиент на Go и узел данных (`ternchain` и операторский `ternchain-ops`, включая кран `faucet serve`), схемы `internal/zk`, генезис L1 и инструменты валидатора, обработка `net.json`. Сети: свой devnet, общая тестовая сеть, L1-тестнет (chain id 83760). Основной сети (8376) пока нет.
- **Что не входит:** стоимость тестовых монет (её нет), ошибки самого Besu (сообщать в проект Besu), социальная инженерия, DoS объёмом запросов против публичного крана, известные проблемы.
- **Известное:** доверенная настройка одного участника (подделка тайных голосов и выводов из пула) — известна и вне рамок до церемонии; открытые находки внутренних аудитов (отчёты пока не опубликованы).
- **Уровни:** Critical — кража денег из Bank или пула, траты казны без обеих палат, захват набора валидаторов; High — подделка или блокировка голосов другим путём, обход лимитов агента, деанонимизация; Medium и Low — ограниченный вред и укрепление.
- **Награда** — грант из казны (`gov propose grant`), его решают обе палаты. Обещать его нельзя. На тестнете это тестовые монеты без стоимости и благодарность в списке. Денежная часть — личное решение оператора, не обещание сети.
- **Безопасная гавань:** исследование в добросовестности на своём devnet или тестнете, без кражи средств основной сети, без чтения чужой переписки и деанонимизации людей, без перегрузки общей сети.
- **Сроки:** подтверждение за 72 часа, разбор за 7 дней, исправление в коде — от 14 дней (Critical) до 90 (Low), раскрытие — после исправления или через 90 дней.
- **Исправление в цепи** — новая версия через голосование: задержка 2 дня, голосование 7 дней, окно выхода 7 дней (около 16 дней). Для критических изменений — 2/3 в обеих палатах и окно выхода 21 день (около 30 дней). Совета безопасности и паузы нет.
