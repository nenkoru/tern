# Privacy: anonymous by default, reveal what you want

## По-русски

### Что скрыто по умолчанию

| Что | Как | Кто всё же видит |
|---|---|---|
| Кто кому пишет и что | Запечатанные конверты с метками (как было) | Размер, время, две метки |
| Кто платит кому, приватно | **Пул (Privacy Pools).** `private deposit` кладёт монеты в пул как «записку» (note); `private send TO AMOUNT` платит любую её часть с доказательством с нулевым разглашением: «одна из записок в дереве моя, вот её нуллификатор, её депозит входит в выбранный мной набор». Какой депозит заплатил, не видно | Сумма вывода, получатель (если это не stealth-адрес), выбранный набор (ASP) |
| Кому платят | **Stealth-адреса (ERC-5564/6538)** включаются в `owner init`: плательщик, который умеет, платит на новый одноразовый адрес. `private send` имени со stealth-ключами платит на такой адрес | Что какой-то адрес получил монеты |
| Кто платит газ | **Релеер** отправляет вывод из пула и тайный голос от своего адреса и берёт комиссию из суммы вывода (голоса бесплатно, с лимитом). Получателю газ заранее не нужен | Релеер видит ваш IP (см. `--proxy`) |
| Кто как голосовал в палате граждан | **Тайное голосование (Semaphore v4).** Голос — доказательство: «моя личность была в дереве граждан, когда предложение открыли, и есть в нём сейчас», с нуллификатором H(scope, secret): один голос на гражданина, кто как голосовал — не видно | Итог; что голос подал *какой-то* гражданин |
| Кто как голосовал в группе | **GroupTrees**: та же машина Semaphore (дерево Poseidon2 глубины 16, та же схема и верификатор), но дерево на каждую группу — набор в любом `ICommunity` (Sets, Commons, чужой контракт). Бюллетень открывает кто угодно; нуллификатор у каждого бюллетеня свой, поэтому голоса одной личности в разных бюллетенях не связать. Пока уровень контракта и клиента: команды CLI нет | Итог; кто в группе (чей лист чей — публично, как в Semaphore); что голос подал *какой-то* член |
| Кто в группе сказал | Кольцевые подписи (как было) | |
| Что вы доказываете о себе | SD-JWT, раскрытие по выбору (как было) | Только выбранный вами проверяющий |
| Ваш IP | `--proxy socks5://127.0.0.1:9050` (Tor) или `TERNCHAIN_PROXY`: RPC, релеи, узлы данных, кран идут через прокси | Выходной узел Tor |

Палата монет остаётся открытой (OpenZeppelin Governor: голос — транзакция с вашего адреса).

### Что видно всем

- Имена, гранты агентов, карточки, группы и их участники, публичные посты, отзывы, утверждения (EAS), замки Bank и платежи `pay` (Bank — это открытая книга).
- В пуле: каждый депозит (адрес, сумма, время), каждый вывод (сумма, получатель или stealth-адрес, комиссия, релеер, выбранный набор), размер дерева. **Суммы видны**: депозит 1,2345 и вывод 1,2345 сразу связываются. Кладите круглые суммы (0,1 / 1 / 10) и выводите частями.
- Состав дерева граждан: какой commitment у какого имени (как в Semaphore: группа известна, голоса нет).
- Время: депозит и вывод через минуту в маленьком пуле почти не прячут.

### Как раскрыть, шаг за шагом

1. **О себе:** `cred present N CLAIM[,CLAIM] VERIFIER` — только выбранные поля, только проверяющему (не на цепочку).
2. **Голос:** `gov vote ID yes|no --house citizens --open` — голос под вашим именем (тот же нуллификатор: дважды не проголосовать).
3. **Платёж:** `pay` / `bank pay` — открыто; `private send` — скрыто. Чтобы доказать получателю, что это вы заплатили приватно: дайте ему хэш транзакции вывода; полное доказательство «из какого депозита» — раскрыть нуллификатор и секрет потраченной записки (они в `pool.notes`); отдельной команды пока нет.
4. **Чистота денег:** выбор набора (`--asp NAME`) — это и есть выборочное раскрытие: «мой депозит среди этих N, которые такой-то считает чистыми». Чем строже набор, тем меньше толпа.

### Наборы (ASP) и компромиссы

- Набор (association set) — корень дерева меток депозитов, которые публикатор считает чистыми. Публикует **кто угодно** (`asp publish --name NAME --rule all|clean24h` или `--labels`), имя принадлежит первому публикатору, метки идут в событие, клиенты пересобирают дерево и не используют набор, чьи метки не дают его корень. Фиксированного ведомства нет; что принимать — решает получатель.
- Правило `clean24h`: депозиты старше 24 часов, на которые ни один гражданин не подал флаг (`asp flag LABEL REASON`, утверждение `pool.flag`). Его может держать любой: `relay serve --asp citizens-24h` переиздаёт его каждые 10 минут.
- Компромиссы. Строгий набор — меньше анонимность. Набор «все депозиты» — максимум анонимности, но получатель может такой не принять. Публикатор набора видит, какие наборы используют выводы (это публично), но не кто вывел. Злой публикатор может издать набор из одной метки — тогда вывод через него деанонимизирует; клиент показывает размер набора (`anonymity_set`), выбирайте большие. Заменённый корень действует ещё 1 час (выводы в пути). «Ragequit» из 0xbow (публичный выход депозитора, если ни один набор его не включает) не сделан: любой может опубликовать набор «все».

### Что всё ещё видят оператор, релеи и AI-провайдеры

- **Оператор тестовой сети:** RPC видит IP и все запросы (какие события вы читаете — то есть, чьи записки ищете: клиент сканирует все события, но запросы идут с вашего IP), кран видит адрес и IP. С `--proxy` (Tor) — только выход Tor.
- **Релеи и узлы данных:** IP (без прокси), время, размер конвертов; релеер вывода — сумму и получателя (они и так публичны), не депозит.
- **AI-провайдер вашего агента:** всё, что агент читает и пишет, включая ваши суммы и получателей. Приватность на цепочке не защищает от провайдера модели.
- **Ваш компьютер:** `pool.notes` зашифрован ключом личности (XChaCha20-Poly1305); секреты записок выводятся из ключа (схема 0xbow), поэтому `private balance` восстанавливает их с цепочки на новом устройстве. Утечка ключа раскрывает всю историю пула и голосов этого имени.

### Доверенная настройка (trusted setup): статус

Groth16 в gnark требует настройки на каждую схему. **Ключи в `internal/zk/keys/` сделаны локально одним участником (`TERN_ZK_SETUP=1 go test -run TestZKSetup`): тот, кто её сделал, может подделывать доказательства — выводить чужие деньги и голосовать без гражданства. Только для devnet, НЕ для production.**

**На L1-тестнете ключи zk оператор сделал один** (`ternchain-ops ceremony solo --keys DIR`; `deploy` принимает такие ключи только с `--test-network`, на chain id основной сети 8376 — никогда). Скрытые переводы и тайные голоса там работают, но от оператора они не скрыты. Об этом говорит notice в net.json. В основной сети будет многосторонняя церемония (`ternchain-ops ceremony`, порядок — в английской части).

Почему Groth16, а не PLONK: самая дешёвая проверка на цепочке (~231 000 газа против ~300 000+ у PLONK), доказательство 256 байт, и так устроены оба стандарта, которым мы следуем (0xbow Privacy Pools и Semaphore — Groth16). gnark умеет PLONK с публичным SRS (KZG; можно загрузить, например, Aztec Ignition), и тогда отдельной церемонии не нужно — это запасной путь. Для production: многосторонняя церемония Phase 2 (`gnark/backend/groth16/bn254/mpcsetup`) поверх публичной Phase 1 (Perpetual Powers of Tau), с независимыми участниками; достаточно одного честного.

### Цифры (этот ноутбук: Apple M3 Pro, 12 ядер; `TestPrivacyGas`, `TestSecretCitizenVote`)

| | |
|---|---|
| Доказательство вывода (9 870 ограничений) | ~47 мс |
| Доказательство голоса (9 674 ограничения) | ~52 мс |
| Ключи доказательства (встроены в бинарь) | 4,0 МБ + 3,7 МБ |
| `verifyProof` (вывод), газ | 230 931 |
| Poseidon2, одно сжатие (вызов контракта) | ~7 050 |
| Депозит: первый в пуле / обычный | 754 282 / 365 959 |
| Вывод через релеера: на stealth-адрес / в Bank / на адрес | 623 053 / 623 155 / 610 058 |
| Тайный голос через релеера | 288 475 |
| Принятие в граждане с листом в дереве | 502 119 |
| Публикация набора из 16 меток | 177 071 |
| Размеры контрактов (байт) | PrivacyPool 6 202, PoolVerifier 5 059, AssociationSets 2 410, CitizenGroup 4 093, VoteVerifier 4 854, Parliament 19 716, Citizens 11 985 (всё < 24 576) |

---

## In English

### Hidden by default

| What | How | Who still sees |
|---|---|---|
| Who writes to whom, and what | Sealed envelopes with tags (unchanged) | Size, time, two tags |
| Who pays whom, privately | **Shielded pool (Privacy Pools, the 0xbow design).** `private deposit` puts coins in the pool as a note; `private send TO AMOUNT` pays any part of it with a zero-knowledge proof: "one of the notes in the tree is mine, here is its nullifier, and its deposit is in the association set I chose". Which deposit paid is hidden; the change stays a note with the same label | The amount, the recipient (unless a stealth address), the chosen set |
| Who is paid | **Stealth addresses (ERC-5564/6538)**, enabled by `owner init`: a payer that supports them pays a new one-time address each time. `private send` to a name with stealth keys pays such an address and announces it | That some address received coins |
| Who pays the gas | A **relayer** submits pool withdrawals and secret votes from its own address and takes its fee from the withdrawn amount (votes: free, rate limited). The recipient needs no gas beforehand | The relayer sees your IP (use `--proxy`) |
| Who voted what in the citizens' house | **Secret votes (Semaphore v4).** A vote is a proof that "my identity was in the citizens' tree when the proposal opened and is in it now", with nullifier H(scope, secret): one vote per citizen, nobody sees who voted what | The tally; that *some* citizen voted |
| Who voted what in a group | **GroupTrees**: the same Semaphore machinery (a depth-16 Poseidon2 tree, the same circuit and verifier) with one tree per group, a set in any `ICommunity` (Sets, Commons, anyone's contract). Anyone opens a ballot; each ballot has its own nullifier scope, so one identity's votes can't be linked across ballots. Contract and client level for now: no CLI command | The tally; who is in the group (which leaf is whose is public, as in Semaphore); that *some* member voted |
| Who in a group said it | Linkable ring signatures (unchanged) | |
| What you prove about yourself | SD-JWT selective disclosure (unchanged) | Only the verifier you choose |
| Your IP | `--proxy socks5://127.0.0.1:9050` (Tor) or `TERNCHAIN_PROXY`: RPC (and its websocket), relays, data nodes and the faucet go through it; loopback is never proxied | The Tor exit |

The coin house stays public (OpenZeppelin Governor: a vote is a transaction from your address).

### Visible to everyone

- Names, agent grants, cards, groups and their members, public posts, reviews, EAS claims, Bank locks and `pay` payments (the Bank is an open ledger).
- In the pool: every deposit (address, amount, time), every withdrawal (amount, recipient or stealth address, fee, relayer, chosen set), the tree's size. **Amounts show**: depositing 1.2345 and withdrawing 1.2345 links at once. Deposit round amounts (0.1 / 1 / 10) and send parts.
- The citizens' tree: which commitment belongs to which name (as in Semaphore: the group is known, the votes are not).
- Timing: a deposit followed by a withdrawal a minute later in a small pool hides little.

### Revealing, step by step

1. **About yourself:** `cred present N CLAIM[,CLAIM] VERIFIER`: only the chosen claims, only to that verifier (nothing on chain).
2. **A vote:** `gov vote ID yes|no --house citizens --open`: your name on it (the same nullifier: still one vote).
3. **A payment:** `pay` / `bank pay` are public; `private send` is not. To show a recipient that you paid privately, give them the withdrawal's transaction; full proof of which deposit paid means revealing the spent note's nullifier and secret (in `pool.notes`); there is no command for that yet.
4. **Clean money:** choosing a set (`--asp NAME`) *is* the selective disclosure: "my deposit is among these N that so-and-so considers clean". The stricter the set, the smaller the crowd.

### Association sets and their trade-offs

- A set is the root of a tree of deposit labels its publisher considers clean. **Anyone** publishes one (`asp publish --name NAME --rule all|clean24h` or `--labels`); the name belongs to its first publisher; the labels go into the event; clients rebuild the tree and ignore a set whose labels don't give its root. No fixed authority: the recipient decides which sets it accepts.
- Rule `clean24h`: deposits older than 24 hours that no citizen flagged (`asp flag LABEL REASON`, a `pool.flag` claim). Anyone can keep one: `relay serve --asp citizens-24h` republishes it every 10 minutes.
- Trade-offs. A strict set means a smaller anonymity set. "All deposits" gives the largest, but a recipient may refuse it. A set's publisher learns which withdrawals used its set (public anyway), not who withdrew. A malicious publisher can issue a one-label set: a withdrawal through it is deanonymised; the client reports the set's size (`anonymity_set`): choose big ones. A replaced root stays valid for an hour (withdrawals in flight). 0xbow's ragequit (a depositor's public exit when no set includes it) isn't built: anyone can publish an "all" set instead.

### What the operator, relays and AI providers still see

- **The test network's operator:** its RPC sees your IP and every query (the client scans all events, but from your IP); the faucet sees your address and IP. With `--proxy` (Tor), only a Tor exit.
- **Relays and data nodes:** your IP (without a proxy), times, envelope sizes; a withdrawal's relayer sees its amount and recipient (public anyway), not the deposit.
- **Your agent's AI provider:** everything the agent reads and writes, amounts and recipients included. On-chain privacy doesn't protect you from the model's provider.
- **Your machine:** `pool.notes` is encrypted with the identity's key (XChaCha20-Poly1305); note secrets are derived from the key (0xbow's scheme), so `private balance` rebuilds them from the chain on a new device. A leaked key reveals that name's whole pool and voting history.

### Trusted setup: status

gnark's Groth16 needs a setup per circuit. **The keys in `internal/zk/keys/` come from a local single-party setup (`TERN_ZK_SETUP=1 go test -run TestZKSetup`): whoever ran it can forge proofs, i.e. withdraw others' coins and vote without citizenship. They are the devnet keys, and they stay devnet keys.**

- **Devnet keys are refused off a devnet.** The verifier contracts built from them carry `CEREMONY = 0`, and their constructor reverts unless the chain id is a devnet one (`zk.DevnetChainIDs`). `deploy` refuses them before that, and a client refuses to prove with them on any other chain.
- **The CEREMONY hash.** A network with value runs a ceremony; `finalize` writes the verifiers with `CEREMONY` set to the final transcript's hash. `deploy` copies it into net.json (`"ceremony"`), and a client that holds different keys refuses to prove ("net.json names ceremony …"). Anyone can compare the hash in the verifier contracts, in net.json and in the published transcript.
- **How to run one** (`ternchain-ops ceremony`, the operator binary: from the release, or `go build -tags ops -o ternchain-ops .`; secure if at least one contributor of each phase deleted their randomness):
  1. `ternchain-ops ceremony init --out t0.mpc` opens phase 1.
  2. Each contributor in turn, on their own machine: `ternchain-ops ceremony contribute --in tN.mpc --out tN+1.mpc --name WHO`. It writes the new file and prints its hash; publish both, the next contributor starts from that file.
  3. `ternchain-ops ceremony init --phase2 --in tK.mpc --beacon HEX --out p0.mpc` closes phase 1 and opens phase 2. The beacon is a public value nobody knew before the last phase-1 contribution (say, the hash of a block announced in advance).
  4. Contributors again, with `contribute` on the phase-2 file (both circuits in one go).
  5. `ternchain-ops ceremony verify --in FILE [--keys DIR]` checks every contribution, the beacons and, with `--keys`, that the keys are the transcript's. Anyone can run it.
  6. `ternchain-ops ceremony finalize --in pM.mpc --beacon HEX --keys DIR` writes the keys (`.pk`, `.vk`), `PoolVerifier.sol`, `VoteVerifier.sol`, `CEREMONY` and `transcript.mpc`. Then `node contracts/build-zk.js DIR` and `TERNCHAIN_ZK_KEYS=DIR ternchain-ops deploy …`; hand out net.json with `DIR` beside it as `zk-keys/`. RUNBOOK.md has the step in the deploy order.
- **A test network: the operator alone.** `ternchain-ops ceremony solo --keys DIR` makes both phases in one go with one contributor, the operator; `deploy` accepts such keys only with `--test-network` (never on the mainnet chain id 8376; on any other non-devnet chain it wants at least 3 contributions per phase in `DIR/transcript.mpc`). **On the L1 testnet the operator made the zk keys alone. Shielded transfers and secret votes work, but they are not private from the operator.** net.json's notice says so. The mainnet will run a multi-party ceremony.
- **Limitation: phase 1 is our own.** gnark's `mpcsetup` keeps phase 1's parameters unexported, so a public powers of tau (e.g. the PPoT `.ptau` files) can't be imported and verified through its API: there is no public-powers-of-tau import. Our phase 1 is small (2^14) and its soundness rests on the contributors we gather, not on a public ceremony with thousands of them. Importing one would mean parsing the `.ptau` into `mpcsetup.SrsCommons` ourselves and checking it with pairings.

Why Groth16 and not PLONK: the cheapest on-chain verification (~231,000 gas against ~300,000+ for PLONK), 256-byte proofs, and both standards we follow use it (0xbow's Privacy Pools and Semaphore are Groth16). gnark supports PLONK with a public KZG SRS (one can load e.g. Aztec's Ignition), which needs no ceremony of its own: that is the fallback. Pinned: gnark v0.16.3 (24 August 2026), gnark-crypto v0.21.0.

### Hashing

Poseidon2 over BN254 (t = 2, 6 full and 50 partial rounds, x⁵; gnark-crypto's parameters). `compress(l, r) = perm(l, r)[1] + r`; `H(x1..xn)` is Merkle–Damgård over it with the arity as IV; tree nodes are `compress(left, right)`, empty leaves 0. The circuits use gnark's `std/permutation/poseidon2`; the contracts call `Poseidon2Hasher` (generated from the same round keys, legacy codegen); `TestPoseidonMatchesContract` checks they agree. Notes: `H(value, label, H(nullifier, secret))`, nullifier hash `H(nullifier)`, label `keccak(scope, nonce) mod p`, context `keccak(withdrawal, scope) mod p`, as in 0xbow. Identities: a secret s < l, the Baby Jubjub key s·G, commitment `H(pk.x, pk.y)`, nullifier `H(scope, s)`, as in Semaphore v4 (the circuit checks s < l: otherwise s + l would give a second nullifier for one key). Trees are fixed-depth (pool 20, sets 20, citizens 16), not LeanIMT.

### Numbers (this laptop: Apple M3 Pro, 12 cores; `TestPrivacyGas`, `TestSecretCitizenVote`)

| | |
|---|---|
| Withdrawal proof (9,870 constraints) | ~47 ms |
| Vote proof (9,674 constraints) | ~52 ms |
| Proving keys (embedded in the binary) | 4.0 MB + 3.7 MB |
| `verifyProof` (withdrawal), gas | 230,931 |
| Poseidon2, one compression (a contract call) | ~7,050 |
| Deposit: first in the pool / steady | 754,282 / 365,959 |
| Relayed withdrawal: to a stealth address / a Bank balance / an address | 623,053 / 623,155 / 610,058 |
| Secret vote, relayed | 288,475 |
| Admission as a citizen, with the tree leaf | 502,119 |
| Publishing a set of 16 labels | 177,071 |
| Contract sizes (bytes) | PrivacyPool 6,202, PoolVerifier 5,059, AssociationSets 2,410, CitizenGroup 4,093, VoteVerifier 4,854, Parliament 19,716, Citizens 11,985 (all under 24,576) |

Most of a deposit is the tree (20 compressions, about 160,000) and new storage slots; most of a withdrawal is the pairing check and the change note's insertion.

### Not production-ready

- The single-party devnet keys (above) are not for value; a network with value runs its own ceremony, and its phase 1 is our own, not a public powers of tau.
- No audit of the circuits or contracts; the Poseidon2 Yul is generated and only tested for agreement.
- Amounts are public; no fixed denominations are enforced.
- No ragequit; association sets carry their labels in calldata (fine for thousands, not millions).
- A citizen whose status changes by time (a founder past the first term, a name whose key moved) stays in the tree until someone runs `gov sync NAME` (or `Citizens.lapse`).
- Secret votes are free at the relayer (rate limited per IP); relayers are trusted for liveness only.
