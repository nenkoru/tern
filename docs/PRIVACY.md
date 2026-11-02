# Privacy: sealed content and its limits

The public Base Sepolia network provides sealed mail, not universal anonymity. Transaction accounts, names, grants, timing and sizes are observable; recipients and their owners can read addressed mail, and a cloud model provider sees supplied plaintext. The tables below also describe experimental L1 modules, including private payments absent from the public network. Read [network profiles](base-sepolia.md) and [the current guarantees](GUARANTEES.md) before applying those claims.

**Delayed mail.** A letter delayed past its recipient's key retention can become unreadable. Its sender can refresh it with `resend ID` (`tern_resend`), which keeps its original expiry and ID; keys are never kept longer to make it pass. Upgraded clients won't act twice on a refreshed copy; older clients or separate state folders might.

## По-русски

### Что скрыто по умолчанию

| Что | Как | Кто всё же видит |
|---|---|---|
| Кто кому пишет и что | Запечатанные конверты с метками (как было) | Размер, время, две метки |
| Кто платит кому, приватно | **Только L1-лаборатория.** **Пул (Privacy Pools).** `private deposit` кладёт монеты в пул как «записку» (note); `private send TO AMOUNT` платит любую её часть с доказательством с нулевым разглашением: «одна из записок в дереве моя, вот её нуллификатор, её депозит входит в выбранный мной набор». Какой депозит заплатил, не видно | Сумма вывода, получатель (если это не stealth-адрес), выбранный набор (ASP) |
| Кому платят | **Только L1-лаборатория.** **Stealth-адреса (ERC-5564/6538)** включаются в `owner init`: плательщик, который умеет, платит на новый одноразовый адрес. `private send` имени со stealth-ключами платит на такой адрес | Что какой-то адрес получил монеты |
| Кто платит газ | **Релеер** отправляет вывод из пула и тайный голос от своего адреса и берёт комиссию из суммы вывода (голоса бесплатно, с лимитом). Получателю газ заранее не нужен | Релеер видит ваш IP (см. `--proxy`) |
| Кто как голосовал в палате граждан | **Тайное голосование (Semaphore v4).** Голос — доказательство: «моя личность была в дереве граждан, когда предложение открыли, и есть в нём сейчас», с нуллификатором H(scope, secret): один голос на гражданина, кто как голосовал — не видно | Итог; что голос подал *какой-то* гражданин |
| Кто как голосовал в группе | **GroupTrees**: та же машина Semaphore (дерево Poseidon2 глубины 16, та же схема и верификатор), но дерево на каждую группу — набор в любом `ICommunity` (Sets, Commons, чужой контракт). Бюллетень открывает кто угодно; нуллификатор у каждого бюллетеня свой, поэтому голоса одной личности в разных бюллетенях не связать. Пока уровень контракта и клиента: команды CLI нет | Итог; кто в группе (чей лист чей — публично, как в Semaphore); что голос подал *какой-то* член |
| Кто в группе сказал | Кольцевые подписи (как было) | |
| Кто состоит в закрытой группе | **Анонимные группы** (`group create NAME --anonymous`): на цепочке только дерево commitment-ов (контракт `AnonGroups`), без имён; личность своя для каждой группы, случайная и не выводится из ключа аккаунта (`eth.key`), удаляется при выходе, поэтому листья не связать между группами, с деревом граждан и с изъятым ключом (группы, вступленные до этого, — `group anon-migrate NAME`; их прошлое остаётся связанным с `eth.key`). Пост несёт доказательство «я лист этого дерева» (та же схема и ключи, что у тайных голосов, новой церемонии не нужно). Режимы: один псевдоним на участника (бан по псевдониму) или `--unlinkable` (новый псевдоним на каждый пост, N постов в час). Анонимный пост идёт через релей; в tern-base-sepolia-1 узел данных не указан: передайте `--relay URL` того, что держит кто-то другой, или поднимите свой | Админ (через него вступают); сколько участников и когда добавлены; что группа существует и кто админ |
| Прошлое при изъятии устройства | **Прямая секретность.** Ключи эпох почты удаляются через `retain_epochs` (7) при каждом сканировании; у закрытых групп — поколения ключей: новое (с новым ключом просмотра) каждые 7 эпох и сразу после бана, удаления или выхода участника. В анонимной группе участник просит новое поколение с доказательством членства; забаненный и удалённый не получают. Прочитанная почта старше `--keep-days` (30) удаляется | Текущие ключи и поколение группы, почта в пределах срока хранения |
| С кем вы работали (репутация) | **Приватные отзывы**: `rep tag` → `rep review LOCK --score S` кладёт на цепочку лист без оценки и без имени; `rep prove` доказывает «N разных оплаченных сделок, средняя оценка ≥ X», не называя ни сделок, ни контрагентов. Публичные отзывы остаются | Что по такому-то замку был приватный отзыв; сам замок в Bank (кто кому платил) |
| Что видит облачная модель агента | **Режим «только локальная модель»**: владелец объявляет модель агента (`model set local|cloud:PROVIDER`); письмо (`send --local-only`), группа (`group policy NAME --local-only`) или корреспондент (`model local-only add`) помечаются, и агенту с облачной или необъявленной моделью их текст не показывается | Всё остальное, что читает агент с облачной моделью; честность объявления и чужого клиента |
| Не подменён ли клиент | **Воспроизводимые релизы**: `scripts/repro.sh` пересобирает релиз и сравнивает байт в байт. Подписи порога независимых подписантов встроены (`releases/signers.json`, `release verify`, `scripts/install.sh` с OpenSSL 3), но подписантов пока нет: у набора порог 0, и `release verify` пишет «UNSIGNED» (проверка только по хэшам), если не дать `--allow-unsigned` (install.sh: `TERN_ALLOW_UNSIGNED=1`). Когда подписанты появятся, смена их набора без одобрения старых — громкий отказ (docs/SUPPLY-CHAIN.md) | Пока подписантов нет — тот, кто выпускает релиз (проверка только по хэшу); закладка в исходниках или зависимостях |
| Что вы доказываете о себе | SD-JWT, раскрытие по выбору (как было) | Только выбранный вами проверяющий |
| Ваш IP | По умолчанию (`--privacy standard`) клиент сам находит Tor на этой машине и идёт через него, своя цепочка Tor на каждый адрес; иначе `--proxy socks5://…` или `TERNCHAIN_PROXY`. `--privacy strict`: без прокси не соединяется, пишет только через релей, логи читает через другой RPC, 12 ложных корзин. `--home-rpc` / `--home`: свой узел; `--fetch all`: скачать всё | Выходной узел Tor; без Tor — RPC и узлы данных |

**Только L1-лаборатория.** Палата монет остаётся открытой (OpenZeppelin Governor: голос — транзакция с вашего адреса).

### Что видно всем

- Имена, гранты агентов, карточки, группы и их участники (у анонимной группы — только дерево, админ и размер), публичные посты, отзывы, утверждения (EAS), замки Bank и платежи `pay` (Bank — это открытая книга).
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

- **Сервисы, которыми вы пользуетесь:** в Base Sepolia ваш IP и запросы видят публичные RPC из net.json, бандлер (по умолчанию публичный бандлер Pimlico) и пеймастер Circle; с `--proxy` (Tor) — только выход Tor. В L1-лаборатории RPC и кран держит её оператор.
- **Релеи и узлы данных:** IP (без прокси), время, размер конвертов; релеер вывода — сумму и получателя (они и так публичны), не депозит.
- **AI-провайдер вашего агента:** всё, что агент читает и пишет, включая ваши суммы и получателей. Приватность на цепочке не защищает от провайдера модели.
- **Ваш компьютер:** `pool.notes` зашифрован ключом личности (XChaCha20-Poly1305); секреты записок выводятся из ключа (схема 0xbow), поэтому `private balance` восстанавливает их с цепочки на новом устройстве. Утечка ключа раскрывает всю историю пула и голосов этого имени.

### Доверенная настройка (trusted setup): статус

Groth16 в gnark требует настройки на каждую схему. **Ключи в `internal/zk/keys/` сделаны локально одним участником (`TERN_ZK_SETUP=1 go test -run TestZKSetup ./cmd/ternchain`): тот, кто её сделал, может подделывать доказательства — выводить чужие деньги и голосовать без гражданства. Только для devnet, НЕ для production.**

**На L1-тестнете ключи zk оператор сделал один** (`ternchain-ops ceremony solo --keys DIR`; `deploy` принимает такие ключи только с `--test-network`, на chain id 8376, который клиент резервирует, — никогда). Скрытые переводы и тайные голоса там работают, но от оператора они не скрыты. Об этом говорит notice в net.json. Сети, где перевод должен быть по-настоящему скрытым, нужна многосторонняя церемония (`ternchain-ops ceremony`, порядок — в английской части).

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
| Who pays whom, privately | **L1 lab only.** **Shielded pool (Privacy Pools, the 0xbow design).** `private deposit` puts coins in the pool as a note; `private send TO AMOUNT` pays any part of it with a zero-knowledge proof: "one of the notes in the tree is mine, here is its nullifier, and its deposit is in the association set I chose". Which deposit paid is hidden; the change stays a note with the same label | The amount, the recipient (unless a stealth address), the chosen set |
| Who is paid | **L1 lab only.** **Stealth addresses (ERC-5564/6538)**, enabled by `owner init`: a payer that supports them pays a new one-time address each time. `private send` to a name with stealth keys pays such an address and announces it | That some address received coins |
| Who pays the gas | A **relayer** submits pool withdrawals and secret votes from its own address and takes its fee from the withdrawn amount (votes: free, rate limited). The recipient needs no gas beforehand | The relayer sees your IP (use `--proxy`) |
| Who voted what in the citizens' house | **Secret votes (Semaphore v4).** A vote is a proof that "my identity was in the citizens' tree when the proposal opened and is in it now", with nullifier H(scope, secret): one vote per citizen, nobody sees who voted what | The tally; that *some* citizen voted |
| Who voted what in a group | **GroupTrees**: the same Semaphore machinery (a depth-16 Poseidon2 tree, the same circuit and verifier) with one tree per group, a set in any `ICommunity` (Sets, Commons, anyone's contract). Anyone opens a ballot; each ballot has its own nullifier scope, so one identity's votes can't be linked across ballots. Contract and client level for now: no CLI command | The tally; who is in the group (which leaf is whose is public, as in Semaphore); that *some* member voted |
| Who in a group said it | Linkable ring signatures (unchanged) | |
| Who is in a private group | **Anonymous groups** (`group create NAME --anonymous`): the chain holds a tree of identity commitments (the `AnonGroups` contract), no names; the identity is a random one per group, not derived from the account key and deleted on leave, so leaves link neither across groups, nor to the citizens' tree, nor to a seized `eth.key` (groups joined before: `group anon-migrate NAME`; their past stays linkable to `eth.key`). A post carries a proof "I am a leaf of this tree" (the secret votes' circuit and keys: no new ceremony). One pseudonym per member (bans by pseudonym), or `--unlinkable`: a fresh pseudonym per post, N posts an hour | The admin (people join through it); how many members and when leaves came; that the group exists and who its admin is |
| What you prove about yourself | SD-JWT selective disclosure (unchanged) | Only the verifier you choose |
| Your IP | By default (`--privacy standard`) Tor when this machine runs it, a circuit per destination; else `--proxy socks5://…` or `TERNCHAIN_PROXY`: RPC (and its websocket), relays, data nodes and the faucet go through it; loopback is never proxied. `--privacy strict`: no proxy, no connection (below) | The Tor exit; without Tor, the RPC and data nodes |

**L1 lab only.** The coin house stays public (OpenZeppelin Governor: a vote is a transaction from your address).

### Anonymous groups

A Sets group names its members in public events; an anonymous group doesn't. Commands:

- `group create NAME --anonymous [--unlinkable [--slots N]]` makes a random group id on `AnonGroups` (contracts/ext) and the channel; the creator is the admin and the first leaf.
- `group invite NAME TO` seals an invitation to one name; `group join NAME` answers it with the invitee's commitment, sealed to the admin; `group admit` adds every waiting commitment in one transaction (a shuffled batch: single join times blur) and sends the keys.
- `group post NAME TEXT` posts with a membership proof instead of a signature, sealed to the group only (no sender tag: it would carry the poster's family bucket). Readers check the proof off chain, that its root was the group's at the post's time (a replaced root proves for one more hour), and the pseudonym's bans and rate.
- **Posts go through a relay.** The proof hides which leaf posted; the transaction that carries the envelope doesn't hide who sent it, and every member can match a post to its transaction. So an anonymous post (and a rekey ask) goes through a relay: `--relay URL`, `TERNCHAIN_RELAY`, or net.json's `relay`. Without one the client refuses; `--direct` posts from your own account anyway, with a warning. On tern-base-sepolia-1 no data node is listed: pass `--relay URL` of one someone runs, or run your own.
- `group members NAME` shows the count, your pseudonym, the bans; the admin also sees whom it admitted (its own knowledge, never on chain).
- `group mod NAME ban|unban PSEUDONYM` (a prefix will do) bans a pseudonym on chain: readers hide its posts, old and new. `group mod NAME remove MEMBER` zeroes a member's leaf (the admin knows whose it is); its proofs stop once the old root's hour is over. `hide POST` works as in any group.
- **Modes.** Pseudonymous (default): scope = the group's, so one pseudonym per member, bannable, rate-limited by readers (`--slots`, default 10 posts an hour). `--unlinkable`: scope = the group, the hour and a slot below N, so a fresh pseudonym per post and at most N posts an hour (a reused slot repeats a nullifier and readers refuse it); nothing to ban: the admin removes leaves of members it knows misbehaved, moderators hide posts.
- **Identities.** A random `s < l` per group (crypto/rand), made by `group join` or `group create` and kept in `anon.json` in the identity folder, beside `eth.key` (0600; not in state.json, which a `--state-dir` copies; not in grant bundles). The commitment is `H(s·G)` as in Semaphore v4; the pseudonym and the unlinkable slots' nullifiers are `H(scope, s)`. `group leave NAME` deletes `s`: afterwards nothing on the device, `eth.key` included, finds the leaf or recomputes a pseudonym. The CLI and a running MCP server change `anon.json` only under its lock, so neither loses nor revives the other's. The cost: a device restored from `eth.key` alone is in no anonymous group; copy `anon.json` with the key, or ask to be invited again.
- **Groups joined before random identities** (red team A-7, A-8). `s` was `keccak("tern-anon-group-v1", eth key, gid) mod l`, so whoever gets `eth.key`, even after you left, finds your leaf in the public `Leaves` events of every group you joined and recomputes the pseudonym and slot nullifiers of every post you made with it. Such a group keeps working (the old `s`, stored in `anon.json` marked legacy; `group members NAME` warns). `group anon-migrate NAME` sends a fresh random commitment to the admin; its next `group admit` removes the old leaf and adds the new one (`AnonGroups.remove`, `.add`; the removal rotates the keys), and your pseudonym changes. **What migration doesn't fix:** the old leaf stays in the `Leaves` history and every nullifier already posted stays, so with `eth.key` that group's membership and every post made before migrating remain attributable to you, forever. It protects only what you post afterwards, and a leave then erases the link. An observer may pair the removed leaf with the one added next to it; that shows nothing beyond the membership the old leaf already shows. In a pseudonymous group with banned pseudonyms the admin refuses the swap (a new pseudonym would shed a ban it can't attribute): it unbans, or removes you and invites you again. The admin needs this version to accept it.
- **Privacy of reading.** Readers fetch every group's `AnonGroups` events (no topic filter), so the RPC doesn't learn which group they read. Data nodes see the group's bucket like any group's (use `--proxy` / home nodes).
- **Trust.** The admin is the high-value target: it knows every member and can add sockpuppet leaves (members see the count grow). A root the leaves don't give never proves; `group members` warns about it.
- **Costs** (`TestAnonymousGroupCosts`, Apple M3 Pro): create 48,307 gas; add one leaf 76,048, a batch of 16 68,759 (leaves are event data); remove 54,933; ban 48,341. A post's proof ~65 ms; checking one ~8 ms; 256 bytes of proof plus the root and nullifier per post (~600 bytes in JSON). No gas per post beyond the envelope's.

### Forward secrecy and key rotation

- **Mail.** Every envelope is sealed with a fresh ephemeral X25519 key to the recipient's key for that epoch (a UTC day), published ahead on chain: per-session keys. An epoch's key is destroyed `retain_epochs` after it (7 by default) by every scan, not only by `keys`; after that its mail opens for nobody, wherever copies are kept. `keys --retain N` changes it (strict privacy: 2).
- **Groups.** A private group's channel comes in generations: a view key and keys for 2·`every` epochs (14 by default; before, 56 epochs handed out at once). A new generation, with a new view key, comes every `every` epochs (7), after any removal, ban or leave, and on `group rotate NAME`. The admin's keeper (`group admit`, run by an admin's agent daily) makes the due ones; a ban or removal makes one at once.
  - A Sets group's new generation is sealed to each remaining member by name.
  - An anonymous group's admin doesn't know who is behind a pseudonym: it announces the generation in the old channels; each member's client asks there with a membership proof against the *current* root (no grace) and a one-time key; the admin answers sealed to that key. A removed leaf can't prove, a banned pseudonym is refused, and the admin learns pseudonyms, not names.
  - Earlier generations are kept to read their posts until their epochs pass the retention, then destroyed with their view keys.
- **Read mail.** Handled mail and group posts older than `keys --keep-days D` (30 by default) are dropped from state.json by every scan; a group can ask for less (`group rotate NAME --keep-days D`); `inbox --purge` drops them now. Unhandled mail stays until handled.
- **Keys ahead.** Keys are published 7 epochs ahead (strict privacy: 3; `keys --ahead N`, at most 28), and any command that reads mail publishes again when fewer than half are left: one Registry call every few days (about 46k gas for 7 light keys; on a network where you pay gas in USDC, no more than a payment, about 0.005). A recipient whose device hasn't run since its keys ran out gets no mail: the sender's command fails and says so, and nothing is sealed to an older key. Before a long trip offline, set `keys --ahead 14`: a longer horizon is a longer exposure.
- **What a seized device opens**: the current keys (today's epoch and the 6 ahead; strict: 2), the groups' current generations, mail within its keep. Not older mail, not a group's history, and a removed member nothing after the rotation. `rekey --compromised`, run as soon as you know, replaces today's key and every later one and the view key, so mail sealed to those replacement keys no longer opens with the image. Mail already sealed to the old keys can still open with it, even if delivered later. Senders refresh published keys before sealing; older clients may keep using cached keys until restarted. Anonymous identities deleted before seizure are absent too, except legacy identities derived from eth.key. The image also holds eth.key, so the name must move to a new key too (guardian recovery: RUNBOOK.md, "A seized device").

- **Costs.** No gas: a rotation is envelopes. A Sets group: one envelope per member. An anonymous group: one announcement per kept generation, one ask per member (a ~65 ms proof on its device) and one answer per member (an age seal, under a millisecond), so two data-node round trips before a member has the new keys. Destroying keys and pruning mail: file deletes during a scan.

### Network: who sees your IP and what you read

`--privacy standard|strict|off` (or `TERNCHAIN_PRIVACY`, or net.json `"privacy"`; the flag wins):

| | standard (default) | strict | off |
|---|---|---|---|
| Proxy | `--proxy`, else Tor if this machine runs it (127.0.0.1:9050, then 9150), else direct with a note on stderr | `--proxy` or Tor, else every non-loopback connection is refused | `--proxy` or direct |
| Tor circuits | one per destination host (SOCKS credentials per host: Tor's IsolateSOCKSAuth) | the same | the same, with `--proxy` on 9050/9150 |
| RPCs | net.json's `rpc` + `rpcs` in a random order per session | the same, and log scans on another of them than calls and transactions | in order |
| Posting | a relay if one is set, else your own transaction; an envelope a data node signed for and let down is re-sent from your own account if a batch was withheld, otherwise through another node with your account as fallback (`da auto --resend 0`: never) | only through a relay (`--relay`): refused otherwise, re-sends too | as standard |
| Decoy buckets | 3 | 12 | 3 |
| Keys kept / read mail kept | 7 epochs / 30 days | 2 epochs / 7 days | 7 / 30 |

- `--home-rpc URL` (or `TERNCHAIN_HOME_RPC`): every RPC call to your own node, or one you trust, instead of net.json's.
- `--home URL` (or `TERNCHAIN_HOME`, or identity.json `home`): mail from your own data node (a `relay serve --mirror` keeps every batch).
- `--fetch all` (or `TERNCHAIN_FETCH=all`): download every envelope instead of your buckets: no node learns your buckets, at the cost of the whole network's traffic.
- `--decoys N`: decoy buckets beside yours in each query (fixed per epoch: fresh ones per request would give the real ones away).
- `whoami` shows the level, the proxy, the homes and the retention under `network`.
- **Correspondents' keys.** A sender takes a name's keys only from a publish its own key signed, and pins them on first use (`pins list`). With two RPCs or more in net.json, a first pin, each new publication and a key change are confirmed by a second one (it learns which names you check, then); strict mode refuses a first pin no second RPC confirms. With one RPC in standard mode, history and publication positions remain trusted even after first use (docs/THREATS-STATE.md, V9).
- Loopback endpoints (a local devnet) are never proxied, in any mode.
- **Costs.** Tor adds about 0.5–2 s per round trip; nothing else adds latency or gas. Decoys add one bucket's envelopes each (about 1/2^k of the network's traffic per decoy). `--fetch all` costs the network's whole traffic.
- **What remains.** Tor exits and guards may be the adversary's; a global observer correlates timing; what your account does on chain is public with or without Tor; a paid relay or data node knows which name pays for which envelopes (its vouchers); envelope sizes aren't padded.

### Reputation without the social graph

A public review (`attest NAME review … --lock ID`) names reviewer, subject, deal and score: a rating computed from them is the social graph. Private reviews keep the deal's existence (the Bank shows it) and hide everything else:

1. The payee, once a lock paid: `rep tag LOCK` sends the payer `H(s, LOCK)` sealed (s: the payee's reputation secret, from its key; the tag is good for that one deal).
2. The payer: `rep review LOCK --score S` posts the leaf `H(H(s, LOCK), S + 100)` to `PrivateReviews` (contracts/ext), which checks that the poster is the lock's payer side, that the lock paid, and one leaf per lock. The chain shows a leaf for that lock: not the score, not whose it is.
3. The payee: `rep prove --scope TEXT [--deals N] [--min-score X] [--out FILE]` proves with the rep circuit (internal/zk/rep.go: 8 slots, a depth-16 tree of every private review) "N distinct leaves are mine and their scores add up to at least (X + 100)·N", with the nullifier `H(scope, s)`. It finds its leaves by trying the 201 scores against its own tags.
4. The verifier: `rep verify FILE --scope TEXT` rebuilds the tree at the size the proof names and checks it. It learns the count and the floor of the average, not the deals, the counterparts or the times. The same prover gives the same nullifier for the same scope; choose a scope of your own (your name and a fresh nonce), so a proof made for you can't be shown to someone else as theirs.

- **Trade-offs.** A proof counts at most 8 deals and shows the prover's best ones (ask for `--deals 8`); it counts deals, not their size; a sybil can buy cheap paid deals, as with public reviews. The Bank still shows who paid whom (payments privacy isn't in the public profile).
- **Keys.** The rep circuit isn't in the binary (19 MB) or the network's MPC: a devnet makes its keys on first use (`TERNCHAIN_ZK_DEVNET_DIR`, else the user cache); a network's operator makes them with `ternchain-ops ceremony rep --keys DIR` (or `ceremony solo`, which now writes them too) and hands them out in zk-keys/. One party: whoever made them can forge reputation proofs on that network. Without them, `rep prove` says private reputation is off there.
- **Costs** (`TestPrivateReputation`, Apple M3 Pro): `PrivateReviews.review` 100,892 gas; a proof ~0.15 s with the keys loaded (the first on a devnet ~1.5 s, making them); checking it ~2 ms; the proof file ~1 KB.

### Local-model mode

An agent's model provider sees everything the agent reads: sealing on chain doesn't hide it from the provider, and a subpoena to the provider yields the plaintext.

- **Declare the model.** `model set local|cloud:PROVIDER [--mode withhold|warn]` (identity.json `model`, or `TERNCHAIN_MODEL` for one process). For an agent, only its owner declares it: `--owner-dir OWNER` on the owner's machine (a message could talk an agent into "local"). An agent with no declaration counts as cloud; an owner reading on its own device doesn't. `whoami` shows it under `model`.
- **Mark content.** `send --local-only` (MCP `tern_send` with `local_only`) marks one message, inside the sealed datagram; `group policy NAME --local-only` marks a group: an admin for every member (sent with a new key generation), a member for itself; `model local-only add|remove NAME|group:G` marks a correspondent or a group for this identity (removing needs the owner).
- **What a cloud model gets.** A local-only item's subject, text and data are withheld, with a notice saying why and that the owner reads it on its own device; `inbox` counts them. `--mode warn` shows them under a warning instead.
- **What it isn't.** The declaration is the owner's word: nothing can prove which model runs. The mark is a request between honest clients: an older or modified client shows the text anyway. A local model is a file on the device (seizure, above).
- **Costs.** None: a flag in the datagram and a check when the client renders an item.

### Releases you can check

A coerced creator could ship a backdoored client. Releases are reproducible (`scripts/repro.sh` rebuilds one and compares, byte for byte). Threshold signing by independent signers is implemented (`releases/signers.json`; `ternchain release verify DIR`; `scripts/install.sh` with OpenSSL 3), but no signers are configured yet: the set has threshold 0, so verify says "UNSIGNED" (hash-only) unless `--allow-unsigned` (install.sh: `TERN_ALLOW_UNSIGNED=1`). Once signers exist, a signer set changed without the old signers' approval is refused loudly. `ternchain version --verify DIR` checks the running binary. [docs/SUPPLY-CHAIN.md](SUPPLY-CHAIN.md) has the procedure; [docs/THREATS-STATE.md](THREATS-STATE.md) the whole threat model against a state-level adversary.

### Visible to everyone

- Names, agent grants, cards, groups and their members (not an anonymous group's: only its tree, its admin and its size), public posts, reviews, EAS claims, Bank locks and `pay` payments (the Bank is an open ledger).
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

- **The services you use:** on Base Sepolia, the public RPCs in net.json, the bundler (Pimlico's public one by default) and Circle's paymaster see your IP and requests; with `--proxy` (Tor), only a Tor exit. On the L1 lab, its operator runs the RPC and the faucet.
- **Relays and data nodes:** your IP (without a proxy), times, envelope sizes; a withdrawal's relayer sees its amount and recipient (public anyway), not the deposit.
- **Your agent's AI provider:** everything the agent reads and writes, amounts and recipients included. On-chain privacy doesn't protect you from the model's provider; local-model mode (above) keeps marked content from a cloud model.
- **Your machine:** expired epoch keys and old group generations are destroyed by every scan, read mail goes after its keep (above). `pool.notes` is encrypted with the identity's key (XChaCha20-Poly1305); note secrets are derived from the key (0xbow's scheme), so `private balance` rebuilds them from the chain on a new device. A leaked key reveals that name's whole pool and voting history; not its anonymous groups (random identities in `anon.json`), except those joined with the legacy identity derived from the key.

### Trusted setup: status

gnark's Groth16 needs a setup per circuit. **The keys in `internal/zk/keys/` come from a local single-party setup (`TERN_ZK_SETUP=1 go test -run TestZKSetup ./cmd/ternchain`): whoever ran it can forge proofs, i.e. withdraw others' coins and vote without citizenship. They are the devnet keys, and they stay devnet keys.**

- **Devnet keys are refused off a devnet.** The verifier contracts built from them carry `CEREMONY = 0`, and their constructor reverts unless the chain id is a devnet one (`zk.DevnetChainIDs`). `deploy` refuses them before that, and a client refuses to prove with them on any other chain.
- **The CEREMONY hash.** A network with value runs a ceremony; `finalize` writes the verifiers with `CEREMONY` set to the final transcript's hash. `deploy` copies it into net.json (`"ceremony"`), and a client that holds different keys refuses to prove ("net.json names ceremony …"). Anyone can compare the hash in the verifier contracts, in net.json and in the published transcript.
- **How to run one** (`ternchain-ops ceremony`, the operator binary: from the release, or `go build -tags ops -o ternchain-ops ./cmd/ternchain`; secure if at least one contributor of each phase deleted their randomness):
  1. `ternchain-ops ceremony init --out t0.mpc` opens phase 1.
  2. Each contributor in turn, on their own machine: `ternchain-ops ceremony contribute --in tN.mpc --out tN+1.mpc --name WHO`. It writes the new file and prints its hash; publish both, the next contributor starts from that file.
  3. `ternchain-ops ceremony init --phase2 --in tK.mpc --beacon HEX --out p0.mpc` closes phase 1 and opens phase 2. The beacon is a public value nobody knew before the last phase-1 contribution (say, the hash of a block announced in advance).
  4. Contributors again, with `contribute` on the phase-2 file (both circuits in one go).
  5. `ternchain-ops ceremony verify --in FILE [--keys DIR]` checks every contribution, the beacons and, with `--keys`, that the keys are the transcript's. Anyone can run it.
  6. `ternchain-ops ceremony finalize --in pM.mpc --beacon HEX --keys DIR` writes the keys (`.pk`, `.vk`), `PoolVerifier.sol`, `VoteVerifier.sol`, `CEREMONY` and `transcript.mpc`. Then `node contracts/build-zk.js DIR` and `TERNCHAIN_ZK_KEYS=DIR ternchain-ops deploy …`; hand out net.json with `DIR` beside it as `zk-keys/`. RUNBOOK.md has the step in the deploy order.
- **A test network: the operator alone.** `ternchain-ops ceremony solo --keys DIR` makes both phases in one go with one contributor, the operator; `deploy` accepts such keys only with `--test-network` (never on chain id 8376, which the client reserves; on any other non-devnet chain it wants at least 3 contributions per phase in `DIR/transcript.mpc`). **On the L1 testnet the operator made the zk keys alone. Shielded transfers and secret votes work, but they are not private from the operator.** net.json's notice says so. A network whose shielded transfers must be private needs a multi-party ceremony.
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
