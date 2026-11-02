# Governance: who can change the network

> **The public network (tern-base-sepolia-1) has no citizens and no coin house, so nothing on this page can pass there: its contracts are immutable.** This page describes the design and the L1 lab.
>
> **В публичной сети (tern-base-sepolia-1) нет граждан и палаты монет, поэтому ничего из этой страницы там пройти не может: её контракты неизменяемы.** Здесь описаны замысел и L1-лаборатория.

## Кратко (по-русски)

Сеть никому не принадлежит, в том числе автору. После запуска у того, кто её развернул, не остаётся никаких прав: это проверяет тест `TestDeployerHasNoPowers`.

Изменения принимают **две палаты**, как в Optimism:

- **Палата монет.** Голосуют обёрнутыми монетами (wTERN), по снимку через 2 дня после предложения (задержка голосования; на devnet 1 секунда). Кворум — 4% всех wTERN, но не меньше 10 000 wTERN на запуске (10 на devnet).
- **Палата граждан.** Один гражданин, один голос. Гражданин — это имя владельца, агенты гражданами не бывают. Гражданство нельзя купить, его зарабатывают: за имя должны поручиться K граждан (по умолчанию 3), а самому имени должно быть не меньше N дней (по умолчанию 30). Палата проходит при явке 20% граждан, но не меньше min(5, половина граждан). Поручитель рискует своим статусом. Если граждане решат голосованием, что имя было подставным (сибилом), все, кто за него поручился, теряют гражданство на срок наказания. Гражданство можно отозвать голосованием граждан, из граждан можно выйти.

Граждане голосуют **тайно**: голос — доказательство с нулевым разглашением (схема Semaphore v4), что голосующий был гражданином, когда предложение открыли, и остаётся им сейчас; один голос на гражданина, кто как голосовал, не видно. `--open` — голос под своим именем. Палата монет открыта. Подробно: `PRIVACY.md`.

Основатели перечислены при запуске, но они временные. До конца первого срока каждого из них должны заново подтвердить max(K, основатели/2) других граждан, иначе он выбывает (`gov lapse ИМЯ` может запустить кто угодно).

Предложение — это обновление (новый адрес для ключа в Directory вместе со спецификацией, аудитом и заметками о переезде), изменение параметра или грант из казны. Оно проходит, только если **обе палаты** в одном окне голосования набрали кворум и большинство (на запуске монеты голосуют 7 дней после 2-дневной задержки, граждане — 9 дней, от предложения до конца голосования монет). Любая палата может остановить его одна. После этого действует окно выхода (timelock: 7 дней на запуске, 10 минут на devnet), и затем исполнить предложение может кто угодно.

Старые контракты не меняются и продолжают работать. Directory хранит все версии каждого ключа. Клиент сообщает владельцу, что «Bank v2 уже текущий, а вы на v1», но сам деньги не переводит: владелец переходит командой `gov use`.

Казна (комиссии и пожертвования) платит только по решению обеих палат и только в замок Bank. Деньги из замка выпускают рецензенты, когда работа сдана, а после срока они возвращаются в казну.

Ручательство — не единственный путь к гражданству: обе палаты могут добавить другие источники доказательства личности (`Citizens.setSources`) с весами. Механизм цен тоже меняется голосованием: Inbox берёт текущий `FeeOracle` из Directory, а все числа оракула — управляемые параметры.

Изменения, от которых зависит само управление (обновление Validators, Parliament, Citizens, Directory, таймлока, палаты монет, FeeOracle; параметры таймлока, палат, FeeOracle и правил набора валидаторов), — **критические**: им нужно 2/3 поданных голосов в обеих палатах и тройное окно выхода. Эти числа (тройное окно, 2/3, порог явки 5) и правила приёма граждан (20% за срок, не меньше 3) задаются при развёртывании флагами `deploy` и потом не меняются для этих Parliament и Citizens. Клиент громко предупреждает о таком предложении и о смене управляющего контракта и сам на него не переходит. Набор валидаторов меняется медленно: не больше трети за окно, удаление ждёт дольше добавления, один валидатор на гражданина (`docs/abuse/VALIDATORS.md`).

Совета безопасности нет. Почему, сказано ниже.

## The two houses

| | Coin house | Citizens' house |
|---|---|---|
| Who votes | anyone holding wTERN (the coin, wrapped 1:1) | citizens: owner names, never agents |
| Weight | coins at the snapshot, 2 days after the proposal (the voting delay; 1 s on a devnet) | one vote each; you must have been a citizen before the proposal opened |
| Counted by | `TernGovernor` (OpenZeppelin Governor; it only counts) | `Parliament` |
| Passes when | quorum (4% of all wTERN, never less than a floor of 10,000 wTERN at launch, 10 on a devnet) and more yes than no | turnout of 20% of citizens, never below min(5, half the citizens), and more yes than no |

**Citizens vote in secret** (Semaphore v4's scheme, [PRIVACY.md](PRIVACY.md)). Each owner name registers a voting identity (`owner init` does it; `gov identity`); while it is a citizen, its commitment is a leaf of the citizens' tree, which `Citizens` keeps in step on every admission, departure, lapse and revocation (`CitizenGroup`; `gov sync NAME` for a change by time). A vote is a proof that the voter's leaf was in the tree when the proposal opened and is in it now, with a nullifier per citizen and proposal; a relayer submits it, so neither name nor address shows. `--open` votes by name with the same nullifier. A citizen with no identity in the tree when a proposal opened votes by name the old way; nobody can vote both ways. The coin house stays public.

A proposal needs **both houses** within the same voting window. At launch the coin vote opens 2 days after the proposal and lasts 7 days; the citizens vote from the proposal to the coin deadline, 9 days. If either house says no or misses its quorum, the proposal fails. Coins can't outvote people, and a crowd of names can't outvote the people who carry the cost.

## Citizenship

An owner name becomes a citizen when all of these hold (`GovernanceCitizens.sol`):

1. **Personhood worth 10000 points.** K vouches from current citizens (K = 3) are worth `vouchWeight`, 10000 at launch, so they suffice alone. Each citizen may vouch for up to 5 names per 90 days. Both houses may add other proof-of-personhood sources and weigh them (`Citizens.setSources(sources, weights, vouchWeight)`, at most 8): a source is any `IPersonhood` contract answering how strongly it holds that a name is one person, in basis points, and adds weight × that / 10000. `ClaimPersonhood` is one: the `personhood` claims of one issuer (a passport-proof verifier, an in-person ceremony). None at launch. A source that fails counts nothing; a founder's confirmation is still by vouches only.
2. **Age.** The name was registered at least N days ago (N = 30). The Registry records when each name was registered.
3. **Deals** (optional): M settled deals with distinct counterparties. This counts only once a vote of both houses names a counter contract. At launch it is off.

The last vouch admits the name automatically. If the name is still too young at that point, or its points come from other sources, anyone can run `gov admit NAME` once it is eligible. The admission caps (per window, per vouch tree) hold whatever the source.

- **A sybil ring can't vouch itself in.** Only citizens vouch, so a thousand fresh names vouching for each other get nowhere.
- **Vouchers answer for whom they vouch for.** Any citizen can open a revocation vote (`gov revoke NAME [--sybil] REASON`). The vote lasts 7 days and passes with 2/3 yes on a 20% turnout, never below min(K + 2, half the electorate) votes cast, so a tiny set never decides alone. A revoked name is barred for the penalty period (180 days). With `--sybil`, everyone who vouched for that name is suspended for the same period. Afterwards they come back with `gov reinstate`, without needing new vouches.
- **Per-vouch-tree admission share (GV-1).** One tight ring cannot fill a term: a citizen may sit in the depth-2 vouch tree (its vouchers and the ones who vouched them in; founders are roots) of at most ceil(cap/2) admissions per window. A full group waits: `gov admit NAME` works in the next window. What remains: at least 2K independent colluders, or rings three generations deep.
- **Leaving:** `gov leave`.
- **Founders.** The deployment lists a few names, each paired with the key that may claim it (`--genesis ana=0x…,ben=0x…`). If someone else registers a listed name first, they get the name but not the citizenship. Each founder must be re-confirmed by max(K, founders / 2) other citizens within the first term (90 days). Otherwise they lapse: anyone runs `gov lapse NAME` after the term. Nobody is a founder forever.

## Proposals

A proposal is one call that the timelock will make. `Parliament` builds that call itself from the kind of proposal, so voters read exactly what will run:

| Kind | What runs | Requirements |
|---|---|---|
| `upgrade` | `Directory.publish(key, address, spec hash + URI, audit hash + URI, migration notes)` | the address holds code; a spec and an audit report are both given |
| `param` | one call to a governed contract: fee oracle targets and parameters (`setParams`, `setRoundLength`), citizenship rules and personhood sources, the houses' own settings, the Governor's settings, the timelock's delay | it may not target the Directory or the treasury, which have their own kinds |
| `grant` | `Treasury.grant`: treasury money into a Bank lock | the money is released by a condition (reviewers by default) and is refundable to the treasury |

Any citizen may propose. So may a coin holder with at least 1 wTERN of votes.

**Votes accept a one-hour root window.** A secret citizens' vote proves membership against the tree's root; the root of a tree that has since changed stays valid for 1 hour, so a vote prepared just before an admission or a departure still counts.

What happens to a proposal:

1. **Voting.** Both houses vote during the window.
2. **Passed.** Both houses said yes. Anyone runs `gov queue` (within the queue window: 14 days of the vote's end at launch, 1 day on a devnet).
3. **Queued.** The exit window runs. This is the time for anyone who disagrees to move their funds.
4. **Ready.** The window is over, and anyone runs `gov execute`.

Nobody can cancel a proposal once it has passed.

**Governance-critical proposals** need more: 2/3 of the votes cast in **both** houses (on top of each house's quorum) and three exit windows (`Parliament.exitWindow(id)`; 21 days at launch). They are:
- an `upgrade` of a governance key: Validators, Parliament, Citizens, CitizenGroup, Directory, TernTimelock, TernGovernor, WrappedTern, VoteVerifier, FeeOracle (it prices every message);
- a `param` call to the timelock (its delay), Parliament (its settings), Citizens or its tree, the coin house or wTERN, or any version of a governance key, FeeOracle included (`setParams`, `setRoundLength`, targets, basket), except Validators' capped set changes (`addValidator`, `removeValidator`, `cancelRemoval`): so `setRules` and `scheduleUpgrade` are critical.

The FeeOracle's own bounds hold whatever a vote says: `setRoundLength` 2 s to 30 days, `slashCooldown` at most 10,000 rounds, at most 256 votes a round, a step within (0, 100%). And an oracle a vote installs can't drain a name: `Inbox.postPaid` charges at most 1000× the floor fee per envelope (`postPaidUpTo` sets one's own cap).

`Parliament.critical(id)` and the `Critical` event mark them; `gov show` says so, `watch` raises `governance-critical`, and a new version of a governance key raises `governance-contract-changed` ("GOVERNANCE CONTRACT CHANGED … this client does NOT follow it"). A client proposes changes to governance contracts against the address it was given, never against whatever the Directory names now. The validator set's own limits (the rolling cap, removal delay, minimum set, one validator per citizen, same-operator challenges) are in `docs/abuse/VALIDATORS.md`.

**Adding and removing validators (a people's L1).**
- *Adding takes two steps, in this order.* The citizen first offers the node key it will run, with its own key (`gov validator offer ADDRESS|DIR`; offering the zero address withdraws the offer). Then governance adds exactly that key (`gov propose param validators add ADDRESS|DIR [CITIZEN]`). The client says before the vote if the citizen has not offered, and the contract refuses an add for a key that was not offered, so nobody is bound to a node they did not agree to run.
- *A same-operator challenge only records a finding.* Citizens challenge two validators as one operator (`gov validator challenge A B --why EVIDENCE`, `challenge-vote`, `challenge-close`); 2/3 yes on the turnout makes a finding. It removes no one. The removal needs a vote like any other (`gov propose param validators remove V`, citing the challenge): both houses, their quorums, the exit window and the rolling cap. Citizens alone, on evidence nobody checks, never take a validator out; the coin house can block a removal too, and the answer to a captured coin house is the social fork (`fork-from-state`).
- *Rotating a key* moves the citizen's validator; a removal vote or challenge that names the old key follows it to the current one.

**Old contracts are never changed in place.** An upgrade adds a version; it does not alter the old contract. The Directory keeps every version of every key (`versions(key)`, `versionOf(key, address)`). Clients follow it only for keys they don't use yet. For a key they already use, they report the new version (`gov updates`, and the `new-version` push event) and stay where their money is until their owner runs `gov use KEY`. An agent tells its owner; it never switches by itself.

## The treasury

Network fees go to `Treasury` unless a deployment names another recipient (the devnet sends them to its faucet). The Treasury has a Registry name of its own, `tern-treasury`, so it can pay into the Bank.

It pays only on a passed `grant` proposal, and only into a lock. `gov propose grant TO AMOUNT --panel a,b,c --k 2 --days 30 --why TERMS` (`--reviewers` is the same flag) works as follows:

- The money stays locked until k of the reviewers rule that the work was delivered (the panel flow: each reviewer `lock decide LOCK SHARE-BPS REASON`, then anyone `lock settle LOCK`).
- A ruling is bound to its lock: the attestation's recipient is `rulingFor(bank, lockId)`, and the ruling, panel, attestation and optimistic conditions check it, so a ruling made for one lock cannot be replayed on another. Clients name the lock (`lock decide LOCK …`); the old by-subject verbs resolve the one open lock.
- If the reviewers don't rule within two weeks after the work's deadline, the money goes back to the treasury.

Audits, nodes and new primitives are all paid for this way. `Treasury.reclaim(bank)` brings refunded money back from any Bank version.

## No admin keys

After deployment:

- **Timelock:** no admin, no canceller. Its only proposer is `Parliament`; anyone may execute.
- **Contracts owned by the timelock:** the Directory, the FeeOracle (targets, basket, congestion, meters, `setParams`, `setRoundLength`), `Citizens.setParams` and `Citizens.setSources`, `Parliament.setSettings`, the Governor's settings and `Treasury.grant`.
- **One-time setters:** the Inbox's `setBank` is used once at deployment and refuses any later call. The Inbox has no oracle setter: it prices envelopes with the Directory's current `FeeOracle`, so the pricing mechanism is replaced by an `upgrade` of that key (both houses, the exit window), never by a key.

`TestDeployerHasNoPowers` checks that the deployer holds no timelock role, that the Directory, FeeOracle, Citizens and Treasury answer to the timelock, and tries as the deployer: the Directory's `set`, `publish` and `transferOwnership`; the timelock's `schedule`, `grantRole` and `updateDelay`; `Inbox.setBank`; the FeeOracle's `setParams` and `setRoundLength` (its other setters, targets, basket, congestion and meters, have the same `onlyGovernance` check, untried); `Citizens.setSources` and `setParams`; `Parliament.setSettings`; the Governor's voting period and quorum floor; `Treasury.grant`. Every attempt is refused. It also shows that the coin house alone gets nowhere: a proposal passed only in `TernGovernor` "executes" and changes nothing.

## Why there is no security council

Many networks keep a small multisig that can pause the network or push a fix without the exit window. We don't have one, because:

- **It would be a fixed authority.** A handful of keys that can change anything is exactly the owner the network is meant not to have. Those keys are also the first thing to be stolen, coerced or subpoenaed.
- **It is rarely needed here.** Contracts are immutable and every change is a new version. A bug in Bank v2 doesn't touch money held in v1, and users are told about a new version but never moved into it. The usual reason for a fast lane, upgrading a proxy in place before an attacker drains it, doesn't apply.

What we give up without one:

- **No fast response.** A live bug in a version people already use can't be paused. The fix is a new version, which takes the voting delay, the vote and the exit window: about 16 days at launch. Users must move their own funds out in the meantime, and the push events and `gov updates` tell them to.
- **No fast patch for a broken parameter.** A wrong fee target stays until a vote fixes it.

If the network ever wants a council, it can add one by vote, in the same way. Done properly, it would cost:

- a contract that can only pause, never move funds;
- members elected by both houses for a term, and recallable;
- every pause expiring by itself after a few days.

## Settings

| | Launch | Devnet (`Short`) |
|---|---|---|
| Voting delay (proposal to coin snapshot) | 2 days | 1 s |
| Voting window | coins 7 days; citizens 9 (from the proposal to the coin deadline) | 10 min |
| Exit window (timelock) | 7 days | 10 min |
| K vouches / N days / M deals | 3 / 30 days / off | 3 / 10 min / off |
| Revocation vote, penalty | 7 days, 180 days | 10 min, 1 h |
| Founders' first term | 90 days | 7 days |
| Vouch term (5 vouches per term) | 90 days | 1 day |
| Queue window (queue a passed proposal) | 14 days | 1 day |
| Founders (`--genesis`) | more than K: at least 4, each claims with its own key (`owner keygen`, `owner init --key`, `gov claim`) | same |
| Personhood | vouches only (`vouchWeight` 10000, no sources) | same |
| Fee oracle rounds, bond, step, quorum | 1 day, 0.01, 10%, 100 coins | same |
| Fee oracle slashing | far vote 50%, round cap 20%, cooldown 3 rounds, 64 votes a round | same |

Every setting above changes only through a `param` proposal.

**Set at deploy, then fixed.** These are deploy parameters (`ternchain-ops deploy` flags), immutable in the Parliament and Citizens that deploy makes; changing one later means a new Parliament or Citizens and a migration (Known gaps):

| Constant | Launch | Flag | Bounds (the constructor refuses others) |
|---|---|---|---|
| Parliament `CRITICAL_DELAY_X`: exit windows a critical proposal waits | 3 | `--critical-delay-x N` | 1 to 10 |
| Parliament `MIN_TURNOUT`: the citizens' turnout floor, min(this, half) | 5 | `--min-turnout N` | at least 1 |
| Parliament `SUPER_NUM / SUPER_DEN`: the critical supermajority | 2/3 | `--super N/D` | above 1/2, at most 1 |
| Citizens `ADMIT_BPS`: new citizens per term, a share of the count | 2000 (20%) | `--admit-bps N` | at most 10000 |
| Citizens `ADMIT_FLOOR`: new citizens per term, at least | 3 | `--admit-floor N` | at least 1 |

## Fixed on purpose 🛡

Some rules are floors: they protect people, so no vote changes them (a new version could only add beside them):

- an agent's spend limits (Registry `spendLimit`, Bank `tokenLimit`, the paymaster's gas budget);
- 12 hours to dispute and to decide: Bank `MIN_DISPUTE` (a lock's refund time at least 12 h away to be disputed) and Ruling `MIN_DECIDE` (a dispute leaves the deciders 12 h after the hearing);
- nobody judges their own case (`fair()`; deciders on a party's side don't count);
- a campaign doesn't count its creator's own pledge;
- one validator per citizen (Validators; the rolling cap, removal delay and minimum set are governed, the binding to citizenship is not);
- the closed proposal kinds (`upgrade`, `param`, `grant`): what voters read is what runs;
- in the client: the agent's taint model and fencing of outside text (FOR-AGENTS), the closed action registry and UI catalogue, a report needs at least 2 owners, reviews only of paid deals, the lookalike-name warning.

## Commands

```
gov lock|unlock AMOUNT                      wrap coins: votes in the coin house
gov citizens | gov vouch|unvouch NAME | gov admit NAME | gov lapse NAME | gov claim | gov leave
gov revoke NAME [--sybil] REASON | gov revoke-vote ID yes|no | gov revoke-close ID | gov reinstate NAME
gov propose upgrade KEY ADDRESS --spec URI|FILE --audit URI|FILE [--spec-hash H --audit-hash H] [--notes TEXT]
gov propose param KEY METHOD ARGS… [--why TEXT]      e.g. FeeOracle setTarget envelope 12
gov propose grant TO AMOUNT --panel a,b,c [--k N] [--days D] --why TERMS
gov vote ID yes|no [--house coin|citizens] [--open] | gov show ID | gov proposals [--all]
gov identity | gov sync NAME
gov queue ID | gov execute ID
gov versions [KEY] | gov updates | gov use KEY [ADDRESS|vN|current] | gov current KEY | gov primitives
gov propose param validators add DIR [CITIZEN] | remove V | cancel V | upgrade BLOCK HASH URI     (a people's L1)
gov validator show | offer ADDRESS|DIR | rotate NEW | lapse V | apply V | challenge A B --why EVIDENCE | challenge-vote ID yes|no | challenge-close ID | report FILE
```

Push events (`watch`): `proposal-opened`, `vote-ending`, `proposal-passed`, `proposal-failed`, `proposal-ready`, `proposal-executed`, `new-version`, `governance-critical`, `governance-contract-changed`, and on a people's L1 `validator-added`, `validator-removal`, `validator-removed`, `validator-rotated`, `validator-churn`, `validator-challenge`, `censorship`, `chain-upgrade`.

## Known gaps

- **Selling a citizen name.** Handing a name's key to someone else hands over its citizenship. The remedy is a revocation vote, which also punishes the name's vouchers.
- **The deal counter doesn't exist yet**, so M is off.
- **Personhood sources are only as good as their issuer.** A source with weight 10000 admits on its own (within the admission caps); a vote that adds one trusts its issuer as much as K citizens.
- **Changing a deploy-time constant means a new contract.** The critical delay, the supermajority, the turnout floor and the admission caps are fixed in Parliament and Citizens (Settings, "Set at deploy").
- **Replacing the citizens' house means a new Parliament.** Parliament points at Citizens and cannot be repointed. Moving to a new pair takes a `param` proposal that gives the timelock's proposer role to the new Parliament and takes it from the old one.
- **The coin house is only meaningful once coins are widely held.** Until then the quorum floor and the citizens' house carry the weight.
