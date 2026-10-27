# Захват валидаторов: атаки F1–F9 на народной сети

## Кратко (по-русски)

Сеть — Hyperledger Besu 26.9, QBFT, набор валидаторов в контракте `Validators.sol`, меняется только двумя палатами (`sim/people`). Мы построили защиту от захвата набора и прогнали атаки из `THREATS.md` (раздел F) на живой сети из 7 валидаторов в Docker: `sim/people/capture.sh`. Цифры ниже — из прогона 25.10.2026 (окна devnet: голосование 60 с, выход 20 с, окно ограничителя 300 с, отсрочка удаления 40 с; на запуске — дни, см. «Параметры»).

### Что защищает

- **Скользящий ограничитель.** За окно (30 дней на запуске) — не больше max(1, ⌊n₀/3⌋) изменений набора (добавлений и удалений вместе), где n₀ — размер набора до изменений этого окна. Добавлять своих, чтобы поднять предел, не выходит: n₀ считается от начала окна.
- **Одно изменение на предложение.** Предложение — один вызов: `addValidator` или `removeValidator` одного адреса.
- **Удаление ждёт дольше добавления.** Добавление — сразу после окна выхода. Удаление после окна выхода только назначается и вступает в силу ещё через 14 дней (`applyRemoval`, может вызвать любой). За это время граждане успевают провести отмену (`validators cancel`).
- **Минимальный набор — 4.** Удаление, после которого осталось бы меньше, не применяется (ждёт, пока добавят нового).
- **Тревога оттока.** Когда изменений за окно больше четверти n₀ — событие `ChurnAlert`, его видит `watch` каждого клиента и `validator status`.
- **Один валидатор на гражданина.** Адрес привязан к имени гражданина (в генезисе и при добавлении). Второй — отказ (клиент — до голосования, контракт — при исполнении). Гражданин сам меняет ключ своего валидатора (`gov validator rotate`). Гражданин ушёл, отозван или выбыл — его валидатор снимается тем же ограниченным путём (`gov validator lapse`).
- **«Один оператор».** Гражданин подаёт жалобу на пару валидаторов с доказательством (общий IP/ASN, хостер, ключи; хэш и ссылка на файл). Граждане голосуют (обвиняемые не голосуют), 2/3 «за» при явке — второй валидатор снимается тем же ограниченным путём.
- **Управляющие контракты.** Обновление ключей Validators, Parliament, Citizens, CitizenGroup, Directory, TernTimelock, TernGovernor, WrappedTern, VoteVerifier и любые параметры таймлока, палат и правил набора требуют 2/3 поданных голосов в обеих палатах и тройного окна выхода. Клиент громко пишет «GOVERNANCE CONTRACT CHANGED» и не переходит сам; узлы Besu тоже не переходят (читают адрес контракта из генезиса).
- **У деплоера нет власти** над набором (проверяется и тестом, и на Besu: `validator powers`).
- **Цензура видна.** Наблюдатель (`validator watch`) замечает транзакцию, которая K блоков ждёт в пуле, хотя она следующая у отправителя, платит базовую цену и в блоках есть место. Отчёт с самой транзакцией и списком пропустивших её предлагающих гражданин кладёт в цепь (`gov validator report`).
- **Социальное восстановление.** Если больше трети валидаторов остановились или больше двух третей цензурируют: `ternchain fork-from-state` строит генезис из последнего окончательного состояния (сверено с корнем состояния блока) с честным набором, граждане подписывают checkpoint, клиенты переходят по `checkpoint verify --follow`.

### Атаки: результат

| # | Атака | Результат | Цифры (прогон) | Что защищает |
|---|---|---|---|---|
| F1 | Фракция с 80% палаты монет и 2 из 9 граждан предлагает снять честных валидаторов | **Отбито** за одно окно голосования | Оба предложения провалены за 64 с: 2 «за» / 5 «против»; без голосов честных — 2 из 9 при пороге явки 5. До 2/3 набора с 2 гражданами — никогда (модель ниже) | Обе палаты; порог явки граждан min(5, половина); один валидатор на гражданина |
| F2 | Салями: добавить своих двоих, затем по одному снимать честных | **Остановлено ограничителем** | 2 изменения из 7 → `ChurnAlert`; третье (удаление v7) — отказ «rolling cap»; честные отменили удаление за 85 с, на 174-й секунде окна в 300 с | ⌊n₀/3⌋ за окно, n₀ от начала окна; тревога > 1/4; отсрочка удаления; `validators cancel` |
| F3 | Один человек — несколько валидаторов | **Отбито** | Второй валидатор max: клиент отказал до голосования, контракт — при исполнении («one validator per citizen»). Пара x1/x2 одного оператора: жалоба 7 «за» / 0, x2 снят (после окна ограничителя) | Привязка к гражданину; жалоба «один оператор» (2/3, обвиняемые не голосуют) |
| F4 | Больше 1/3 уходят — сеть стоит | **Восстановлено** | Голова 991 → 991 за 15 с; `fork-from-state`: 63 аккаунта, 935 слотов, корень состояния совпал, 0,1 с; 5 из 9 граждан подписали; первые блоки форка через 19 с; балансы те же (ben 120, max 600), платёж прошёл за 2 с; 11 предложений сохранились | Наблюдатель (`halt`), архивный узел, `fork-from-state`, checkpoint с подписями граждан, `checkpoint verify --follow` |
| F5 | Картель 6 из 8 цензурирует голос ben за снятие | **Обнаружено** | Пока честные v1, v2 предлагают блоки — голос вошёл через 3 блока; без них — тревога через 20 с: 10 блоков с местом, все 6 предлагающих пропустили; отчёт в цепи | `validator watch`, `CensorshipReported`; против 2/3 — только форк (F4) |
| F6 | Враждебный Validators в Directory | **Медленно и громко** | 5 из 9 «за» (большинство, не 2/3) — провал; 7 из 9 — прошло, окно выхода 61 с вместо 20 с; клиент: «GOVERNANCE CONTRACT CHANGED… does NOT follow»; Besu продолжил читать 0x…7E40 | 2/3 в обеих палатах, 3 окна выхода, громкие тревоги, клиент и узлы не переходят сами |
| F7 | Деплоер сохраняет власть | **Нет власти** | 0 из 7 привилегированных вызовов | Одноразовая передача таймлоку (`setGovernance`) |
| F8 | Украден ключ валидатора | **Пережито** | Вор с тем же ключом (1 из 7): 20 блоков по 1,00 с, 0 смен раунда; ротация — одна транзакция (1,2 с), после неё вор не в наборе и не подписал ни одного из 15 блоков | Ротация ключа гражданином; < 1/3 ключей не ломает QBFT |
| F9 | Порядок транзакций / MEV на запечатанных ставках | **Качественно: утечка порядка есть** | Следующий предлагающий предсказан в 17 из 17 блоков (круговая очередь) | Commit–reveal скрывает суммы; длинные окна раскрытия (≥ 3n блоков) |

### Что осталось опасным

- **Палата граждан — главный рубеж.** Ограничитель замедляет, но не останавливает фракцию, у которой есть ≥ 3 граждан и которая проводит предложения: модель ниже — власть остановить сеть за 2 окна (60 дней), 2/3 набора за 4 окна (120 дней), если каждое удаление проходит обе палаты. Защита — в явке граждан и в тревогах; при апатии большинства граждан ограничитель даёт только время.
- **Подкуп операторов.** Ограничитель считает изменения набора, а не владельцев: если операторы уже стоящих валидаторов договорились (F5, F4), набор не меняется вовсе. Против этого — только обнаружение, удаление голосованием и, если голос цензурируют, форк.
- **Цензура обнаруживается, но не пресекается автоматически.** Принудительного включения нет: отчёт — доказательство, а не наказание. Против цензоров с 2/3 — только социальный форк.
- **Форк — дело людей.** Нужен архивный узел (FOREST), согласие на блок и набор, подписи больше половины граждан. Журналы событий старой цепи не переносятся (только состояние), номера блоков начинаются с 0.
- **Нет залога и наказания за двойную подпись.** QBFT без ставок: украденный ключ (< 1/3) не ломает сеть, его выключает ротация, но вор ничего не теряет.
- **Порядок в блоке.** Предлагающий известен заранее (круговая очередь) и сам решает порядок и включение в своём блоке; запечатанные ставки скрывают суммы, но не факт ставки и не время.
- **Одна реализация (Besu)** и малый набор: при 4–7 валидаторах «треть» — это 2–3 человека.

### Где взять первых доверенных валидаторов

- **Основатели, но не одни.** Первые 7 — граждане-основатели из разных стран, хостеров и работодателей; не больше одного на семью/компанию/хостера (проверяется жалобой «один оператор»).
- **Те, кому уже доверяют вне сети:** операторы узлов Ethereum/Gnosis-валидаторов, университетские и библиотечные ИТ-отделы, хакерспейсы, некоммерческие хостеры (у одного облачного провайдера — не больше одного-двух; лучше свои серверы).
- **Гранты из казны за работу узла** (`gov propose grant`), с проверкой `validator status` рецензентами: валидатор получает деньги, пока подписывает блоки.
- **Каждый валидатор — гражданин** с поручителями: его можно отозвать, и поручители отвечают.
- **Публичный список** операторов (страна, ASN, хостер) в карточках граждан, чтобы жалобы «один оператор» было на чём проверять.

## The attacks (English)

Run: `sim/people/capture.sh` (Docker, Besu 26.9.0, 7 validators + a data node; about 22 minutes on one Mac). Transcript: `sim/people/capture-output.txt`; numbers: `sim/people/capture-run/numbers.txt` (each run writes it; it is not kept in the repo). Tests: `validators_capture_test.go`, `validators_test.go` (the same defences in contract terms, on a simulated chain).

| # | Attack | Result | Measured | Defence |
|---|---|---|---|---|
| F1 | A faction with 80% of the coin house and 2 of 9 citizens proposes removing honest validators | blocked in one voting window | both defeated in 64 s (2 yes / 5 no; with nobody else voting, 2 of 9 under a turnout floor of 5) | both houses, the citizens' turnout floor |
| F2 | Salami: two additions, then removals one by one | stopped by the cap | churn alert at 2 changes of 7; the third change refused ("rolling cap"); the honest cancel took 85 s, 174 s into the 300 s window | ⌊n₀/3⌋ per window, alert past 1/4, removal delay, cancel |
| F3 | One person, several validators | blocked | second validator for one citizen refused by the client and at execution; a same-operator pair challenged 7–0, the second removed after the window | citizen binding, same-operator challenge |
| F4 | More than 1/3 offline: halt | recovered | halted at 991; fork-from-state: 63 accounts, 935 slots, state root matched, 0.1 s; 5 of 9 citizens signed; the fork's first block 19 s after the honest nodes started; balances equal; a payment in 2 s | watcher, archive node, fork-from-state, signed checkpoint, verify --follow |
| F5 | A 6-of-8 cartel censors a removal vote | detected | included after 3 blocks while two honest proposers were up; with only the cartel proposing, alert after 20 s (10 blocks with room, all 6 proposers skipped it), report on chain | watcher, CensorshipReported, the fork |
| F6 | A hostile Validators in the Directory | slow and loud | 5/9 yes: defeated (needs 2/3); 7/9: passed, exit window 61 s instead of 20 s; client warns and doesn't follow; Besu keeps reading 0x…7E40 | supermajority, 3 × exit window, alarms, no auto-follow |
| F7 | Deployer keeps power | none | 0 of 7 privileged calls | one-time handover |
| F8 | A stolen validator key | survived | thief signing with the same key: 20 blocks at 1.00 s, 0 round changes; rotation one tx (1.2 s), then the thief sealed 0 of 15 | rotation by the citizen, QBFT's 1/3 |
| F9 | Ordering / MEV on sealed bids | qualitative | next proposer predicted 17 of 17 blocks | commit–reveal hides amounts; long reveal windows |

### F1: how long a faction needs to reach 1/3 or 2/3 (model)

Seven honest validators, a faction with *c* citizens (each at most one validator), launch rules (window 30 d, cap ⌊n₀/3⌋, minimum set 4), and the worst case: **every faction proposal passes both houses** (honest citizens asleep, or fooled). Each window the faction adds its own validators up to *c*, then removes honest ones.

| faction citizens | halt power (> 1/3) | 2/3 (finality, censorship) |
|---|---|---|
| 1 | never | never |
| 2 | 2 windows (60 days) | never (needs 3 of a 4-set) |
| 3–5 | 2 windows (60 days) | 4 windows (120 days) |

Each of those windows raises a churn alert; each removal waits 30 days from proposal to effect (2 d voting delay, 7 d vote, 7 d exit, 14 d delay), long enough for a cancel. On the live run, a coin-house majority with 2 of 9 citizens got nothing: its removals failed in one voting window (honest no, or the turnout floor when nobody else voted).

### F5: forced inclusion (design)

How the run censored: the cartel's six Besu nodes ran with an account allowlist (`--permissions-accounts-config-file`) that left ben out. That filters only their own pools: a block an honest proposer made with ben's vote was sealed by the cartel too (3 blocks). To keep a transaction out while honest proposers still take turns, a cartel has to refuse to seal their blocks (a patched client): that shows as round changes on every honest turn, which `validator status` counts. With the honest proposers down, plain pool filtering was enough, and the watcher caught it in 10 blocks.

What is built: detection (`validator watch`), evidence on chain (`Validators.CensorshipReported`, by any citizen whose own transactions get in), the censored transaction in the report (anyone rebroadcasts it), and the social fork.

What a forced-inclusion path would add, in order of cost:
1. **Inclusion receipts.** Data nodes sign "I saw tx T at block B"; a report with receipts from several data nodes is stronger evidence than one watcher.
2. **An on-chain inclusion queue.** Citizens post the hash of a censored transaction to a contract (cheap, and from accounts the cartel doesn't know). After M blocks, every block whose proposer doesn't include a queued, valid transaction is evidence against that proposer; `Validators` could then schedule its removal automatically through the capped path, without a vote the cartel could censor. The cartel can censor the queue posts too, but only by censoring every citizen, which the watchers see.
3. **Validity rule in the client.** Besu would have to reject blocks that skip a queued transaction older than M blocks: a consensus change (a Besu plugin or fork), not built.

Against a cartel with 2/3 of the seals none of these is enough: they can refuse to seal any block that includes it. The last resort is the fork, which F4 shows working.

### F9: ordering power (qualitative)

Measured: in QBFT the next proposer is the validator after the last one in the sorted set (17 of 17 blocks predicted on the fork). So every validator knows its slots in advance, and decides alone what goes into its block and in which order. What that leaks against sealed bids (`Commitments`, commit–reveal):
- **Hidden:** the amounts (a commit is a hash).
- **Visible to the proposer:** that someone bids, from which address, with what gas, and when it reveals.
- **Within its power:** to delay or drop a commit or reveal in its own block (so a bid can miss its deadline), and to put its own reveal last. With n validators it owns 1 block in n; to keep a reveal out of a window of W blocks it needs every proposer in that window, that is, a cartel (F5) — and that is what the watcher detects.
- **Mitigations:** reveal windows of many blocks (≥ 3n), send reveals to several nodes, refunds instead of forfeits for a reveal that a watcher shows was pending in time.

### Parameters

| | Launch (`launchRules`) | Devnet (`--devnet`) | This run |
|---|---|---|---|
| Cap window | 30 days | 10 min | 300 s |
| Removal delay (after the exit window) | 14 days | 60 s | 40 s |
| Minimum set | 4 | 4 | 4 |
| Lapse grace after the handover | 90 days | 10 min | 60 s |
| Same-operator vote | 7 days, 20% turnout, 2/3 yes | 60 s | 40 s |
| Governance-critical | 2/3 of votes cast in both houses, 3 × exit window | same | 3 × 20 s |

### Changes (code)

- `contracts/Validators.sol`: the cap, scheduled removals, minimum set, churn alert, citizen binding, rotation, lapse, same-operator challenges, censorship reports; `setGovernance(timelock, registry, citizens)`.
- `contracts/GovernanceParliament.sol`: governance-critical proposals (supermajority in both houses, `exitWindow(id)` = 3 × the delay, `Critical` event).
- `validator.go`: genesis with names and rules, `--archive/--fresh/--recreate/--heap/--besu-args`, `validator status` (rules, churn, bindings, removals, challenges, alerts), `validator powers`. `validator_watch.go`: the watcher. `validator_fork.go`: `fork-from-state`, `checkpoint sign|verify --follow`.
- `governance.go`, `governance_validators.go`: `validators add DIR CITIZEN|remove|cancel` with early checks; governance contracts are proposed against the client's pinned address, never the Directory's; `gov validator …`; loud push events (`governance-critical`, `governance-contract-changed`, `validator-churn`, `validator-removal`, `censorship`, …).
- `internal/zk/zk.go`: a fix found by this run: the CLI never registered the vote circuit's `bits` solver hints (only compiling registers them, and the CLI loads the compiled circuit), so **every citizen vote from the CLI failed** with "solver missing hint(s)" (the test binary links the package and passed).
