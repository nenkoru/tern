# Валидаторы L1-тестнета / Validators of the L1 testnet

[Русский](#русский) · [English](#english)

---

## Русский

Это набор для человека, которого пригласили держать валидатор закрытой тестовой сети ternchain (chain id 83760). Основной сети (chain id 8376) пока нет. Откуда этот набор и что ещё не готово: `L1-READINESS.md` (internal review, not yet published) §3. Пошаговая настройка сервера: [ops/testnet/VALIDATOR.md](../ops/testnet/VALIDATOR.md).

### Кого мы ищем

- Людей, которые держат сервер 24/7 и отвечают на тревоги.
- Независимых друг от друга: разные страны, хостеры и работодатели. Один человек — один валидатор.
- Тех, кому интересна сама технология и репутация, а не будущие награды. Стоимости мы не обещаем.
- Опыт с узлами Ethereum, Cosmos и подобных сетей полезен, но не обязателен.

### Требования

- Сервер: 4 vCPU, 8–16 ГБ памяти, NVMe SSD от 100 ГБ (объём растёт), фиксированный публичный IP, стабильная связь.
- Ubuntu 22.04 или 24.04. Docker (проще всего) или JDK 25 — он нужен Besu с версии 26.6.
- Синхронизация времени (chrony).
- Порт 30303 TCP и UDP открыт всем. Порты 8545 и 8546 — только localhost.
- Деньги: реально около €40 в месяц на VPS, или своё железо. Цены облачных VPS в 2026 году выросли, поэтому старая оценка «€20–40» — это нижняя граница. Для сравнения: Vultr 4 vCPU / 8 ГБ — $40, DigitalOcean — $48.

### Три команды

Команды валидатора живут в отдельной программе `ternchain-ops`. Возьмите `ternchain-ops-ВЕРСИЯ-ОС-АРХ` из релиза (он лежит рядом с `ternchain-…`) или соберите сами: `go build -tags ops -o ternchain-ops ./cmd/ternchain`. Положите её рядом с `ternchain` под именем `ternchain-ops`: обычный `ternchain` сам передаёт ей команды оператора.

```bash
ternchain-ops validator init --dir ~/tern/validator
#   ключ узла (файл 0600), адрес, enode: отправьте адрес и enode координатору

ternchain-ops validator run --dir ~/tern/validator --genesis genesis.json \
  --p2p-host ВАШ.IP --bootnodes enode://…@…:30303,enode://…@…:30303 \
  --metrics --restart unless-stopped
#   --metrics: метрики Besu на 127.0.0.1:9545; --restart unless-stopped: контейнер поднимается сам после сбоя и перезагрузки

ternchain-ops validator status --rpc http://127.0.0.1:8545
#   голова, набор, ваши подписи, пиры, график обновлений
```

**Перед запуском сверьте генезис.** У всех валидаторов должен быть один и тот же `genesis.json`, байт в байт. Координатор публикует `keccak`, который напечатала `validator genesis`. Каждый оператор выкладывает в общий канал `sha256sum genesis.json` своей копии. Если хоть одна сумма не совпала — не запускайтесь.

Подробно, с systemd, файрволом и проверками: [ops/testnet/VALIDATOR.md](../ops/testnet/VALIDATOR.md). Публичный RPC держат на отдельном узле-невалидаторе с `--profile rpc` (без ADMIN и DEBUG). RPC валидатора наружу не открывайте.

### Как растёт набор на тестнете: с 1 до 4 за один присест

Генезис тестнета собран с `validator genesis --selection header`: набор валидаторов записан в заголовке генезиса и меняется голосами самих валидаторов (`qbft_proposeValidatorVote`), без контракта Validators и без палат. Тестнет стартует с одним валидатором оператора. Контрактный набор (`--selection contract`, по умолчанию) — для основной сети: там набором управляет Parliament.

1. Каждый новый оператор: `ternchain-ops validator init --dir v2` (и v3, v4), отправляет адрес координатору, запускает узел с генезисом и bootnodes сети (`validator run --dir v2 --genesis genesis.json --bootnodes …`) и ждёт синхронизации (`validator status --rpc http://v2:8545` показывает голову).
2. Сначала все три новых узла синхронизированы и видят пиров. Потом координатор на RPC валидатора 1 (пока n = 1, голосует только он):
   `curl -s -X POST -H 'content-type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"qbft_proposeValidatorVote","params":["0xV2",true]}' http://127.0.0.1:8545`
   При n = 1 одного голоса хватает: валидатор 2 входит со следующего блока.
3. Сразу же, уже на валидаторах 1 **и** 2 (большинство из 2 — это 2): предложить `0xV3` на обоих. Потом на 1, 2 и 3 (большинство из 3 — это 2, но голосуйте на всех трёх): предложить `0xV4`.
4. Проверить, что `qbft_getValidatorsByBlockNumber` с `"latest"` даёт 4. Затем `qbft_discardValidatorVote` для оставшихся голосов.

Никогда не оставляйте сеть на n = 2 (падение любого узла останавливает цепь) и не останавливайтесь на n = 3 (нет запаса на отказ). Удаление — те же голоса с `false`, никогда ниже 4. RPC должен включать API QBFT (`--rpc-http-api=…,QBFT`); `validator run --profile validator` так и делает. Тот же текст: `ternchain-ops help validator`.

На тестнете с `--selection header` пункты ниже про гражданство («один валидатор на гражданина», `gov validator rotate`, монеты основателя) не действуют: смену ключа делают так же, голосами — новый адрес `true`, старый `false`. Они описывают основную сеть.

### Мониторинг

- `validator status` по cron каждые 5 минут:
  ```
  */5 * * * * ternchain-ops validator status --rpc http://127.0.0.1:8545 >> ~/tern/status.log 2>&1
  ```
- Тревога, если `last_sealed` вашего валидатора отстаёт больше чем на 2n блоков (n — размер набора) или появились смены раунда.
- `validator watch --rpc http://127.0.0.1:8545` — остановки сети и цензура.
- Метрики Besu (`--metrics`, 127.0.0.1:9545) для Prometheus. Готовые конфигурации, панели и правила тревог: [ops/monitoring/](../ops/monitoring/).
- Общий канал тревог (Telegram или Matrix). Ссылку даёт координатор.
- Что делать при сбое: [ops/incident/INCIDENT-RESPONSE.md](../ops/incident/INCIDENT-RESPONSE.md). Обновления Besu: [ops/besu/UPGRADE.md](../ops/besu/UPGRADE.md).

### Чего мы ждём

- аптайм ≥99%;
- ответ на тревогу в течение 12 часов;
- обновление до объявленного блока (не меньше 7 дней на подготовку);
- участие в учениях (раз в 2 недели в фазе 1);
- быть гражданином и голосовать в палате граждан;
- опубликовать страну, ASN и хостера;
- не держать второй валидатор;
- сообщить, если ключ мог утечь;
- предупредить за 30 дней, если уходишь.

### Ключи

- Ключ узла не копируйте на другие машины. Два узла с одним ключом — это двойная подпись и повод для удаления.
- Держите зашифрованную резервную копию ключа офлайн.
- Ключ основателя (`owner keygen`) — на другой машине, не на сервере валидатора.
- Если ключ мог утечь: сразу скажите в канале и смените его (`gov validator rotate`).
- В генезисе у валидатора 10 монет на газ, у основателя — 1000. Это тестовые монеты, их хватает на голосования и транзакции. Остальной запас лежит в казне, его тратит только таймлок по решению обеих палат. У деплоера 100 монет на газ и никаких прав.

### Что получаешь

- место в публичном списке валидаторов-основателей;
- отзывы и репутацию в сети (`attest`);
- гранты из казны за работу узла (`gov propose grant`), только по решению обеих палат. На тестнете это тестовые монеты без стоимости;
- возможно, возмещение хостинга на время тестнета, если координатор лично так решит. Это его личный выбор, а не обещание сети;
- голос в развитии сети.

### Чего не получаешь

- никаких обещаний стоимости, токенов, долей или будущих раздач (airdrop);
- монеты тестнета никогда не станут монетами основной сети.

### Соглашение оператора (кратко)

Одна страница, текст согласуется с юристом. Суть:
- участие добровольное;
- трудовых отношений нет;
- без гарантий и без обязательств платить;
- соблюдать условия сети ([TERMS.md](../TERMS.md)) и санкционные правила своей страны;
- уйти можно в любой момент, с уведомлением.

### Чек-лист отбора

- [ ] **Независимость.** Не работает с другим валидатором в одной компании, не родственник, не на одной зарплате. Не управляет чужим валидатором.
- [ ] **Хостинг.** Хостер, ASN и страна записаны. Не больше 2 на хостера и на страну. Не больше одного на «облако одного аккаунта».
- [ ] **Один на гражданина.** Есть имя-гражданин (основатель или с поручителями); контракт не даст второй.
- [ ] **Гигиена ключей.** Ключ создан на сервере `validator init`, права 0600, офлайн-копия зашифрована. Ключ основателя на другой машине. Ключи не хранятся у координатора или у агента.
- [ ] **Опыт.** Держал узел или сервер 24/7 хотя бы 3 месяца, или прошёл тестнет-0 без замечаний.
- [ ] **Связь.** Два канала связи, часовой пояс записан, ответ на тестовую тревогу пришёл за 12 часов.
- [ ] **Право.** Подтвердил, что участие законно там, где он находится. Не в санкционном списке. Пока нет ответа юриста — не резидент РФ (L3).
- [ ] **Мотив.** Понимает «без обещаний стоимости» и принимает его письменно.
- [ ] **Пробный запуск.** Синхронизировался как невалидатор и показал `validator status` с пирами.

### Текст приглашения

> Привет! Я делаю ternchain — открытую сеть, где люди и их ИИ-агенты переписываются, договариваются и платят друг другу. У сети нет владельца: изменения принимают две палаты (держатели монет и граждане — люди с поручителями), даже у меня нет особых прав.
>
> Сейчас я собираю первых 7 валидаторов для закрытой тестовой сети на Hyperledger Besu (QBFT, блок в 1 с, окончательность сразу). Нужен сервер на 4 vCPU, 8–16 ГБ, SSD 100+ ГБ, открытый порт 30303, три команды и около часа на настройку. Потом — следить за тревогами и обновляться к объявленному блоку.
>
> Что честно могу предложить: место в списке валидаторов-основателей, голос в управлении и гранты казны за работу узла (на тестнете это тестовые монеты без стоимости). Денег, токенов и обещаний стоимости нет и не будет: монета — это газ сети, а не инвестиция.
>
> Мне важно, чтобы валидаторы были независимы: разные страны, хостеры и работодатели, один человек — один валидатор. Если интересно, ответь, и я пришлю короткий набор: требования, команды, чего ждём, — а также 15-минутный созвон.

### Санкции и право

Это не юридическая консультация. Пока юрист не ответил на вопрос о санкциях (Регламент ЕС 833/2014, ст. 5b), мы не приглашаем резидентов России и не ставим валидаторы на серверах в России. Участвуйте, только если это законно там, где вы находитесь, и вы не в санкционном списке. Монета — газ сети, она не продаётся.

---

## English

This is the kit for a person invited to run a validator on the closed ternchain test network (chain id 83760). The mainnet (chain id 8376) does not exist yet. Where this kit comes from, and what is not ready yet: `L1-READINESS.md` (internal review, not yet published) §3. Server setup step by step: [ops/testnet/VALIDATOR.md](../ops/testnet/VALIDATOR.md).

### Who we look for

- People who keep a server up 24/7 and answer alerts.
- People independent of each other: different countries, hosts and employers. One person, one validator.
- People who care about the technology and their reputation, not future rewards. We promise no value.
- Experience with Ethereum, Cosmos or similar nodes helps, but is not required.

### Requirements

- A server: 4 vCPU, 8–16 GB RAM, 100+ GB NVMe SSD (it grows), a fixed public IP, a stable link.
- Ubuntu 22.04 or 24.04. Docker (simplest), or JDK 25, which Besu needs since 26.6.
- Time sync (chrony).
- Port 30303 TCP and UDP open to all. Ports 8545 and 8546 on localhost only.
- Money: realistically about €40 a month for a VPS, or your own hardware. Cloud VPS prices rose in 2026, so the old "€20–40" estimate is the low end. For comparison: Vultr 4 vCPU / 8 GB is $40, DigitalOcean $48.

### The three commands

Validator commands live in a separate program, `ternchain-ops`. Take `ternchain-ops-VERSION-OS-ARCH` from the release (it sits next to `ternchain-…`), or build it: `go build -tags ops -o ternchain-ops ./cmd/ternchain`. Put it beside `ternchain` as plain `ternchain-ops`: plain `ternchain` forwards operator commands to it.

```bash
ternchain-ops validator init --dir ~/tern/validator
#   node key (file mode 0600), address, enode: send the address and enode to the coordinator

ternchain-ops validator run --dir ~/tern/validator --genesis genesis.json \
  --p2p-host YOUR.IP --bootnodes enode://…@…:30303,enode://…@…:30303 \
  --metrics --restart unless-stopped
#   --metrics: Besu metrics on 127.0.0.1:9545; --restart unless-stopped: the container comes back after a crash or reboot

ternchain-ops validator status --rpc http://127.0.0.1:8545
#   head, set, your seals, peers, upgrade schedule
```

**Check the genesis before you start.** Every validator must run the same `genesis.json`, byte for byte. The coordinator publishes the `keccak` that `validator genesis` printed. Each operator posts `sha256sum genesis.json` of their copy in the shared channel. If any sum differs, don't start.

The full steps, with systemd, the firewall and checks: [ops/testnet/VALIDATOR.md](../ops/testnet/VALIDATOR.md). A public RPC runs on a separate non-validator node with `--profile rpc` (no ADMIN, no DEBUG). Never expose your validator's RPC.

### How the testnet set grows: 1 to 4 in one sitting

The testnet genesis is made with `validator genesis --selection header`: the validator set is in the genesis header and changes by the validators' own votes (`qbft_proposeValidatorVote`), with no Validators contract and no houses. The testnet starts with one operator validator. The contract set (`--selection contract`, the default) is for the mainnet, where Parliament governs the set.

1. Each new operator: `ternchain-ops validator init --dir v2` (and v3, v4), sends the coordinator the address, starts the node with the network's genesis and bootnodes (`validator run --dir v2 --genesis genesis.json --bootnodes …`) and waits until it is synced (`validator status --rpc http://v2:8545` shows the head).
2. All three new nodes are synced and peered first. Then the coordinator, on validator 1's RPC (only validator 1 votes while n = 1):
   `curl -s -X POST -H 'content-type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"qbft_proposeValidatorVote","params":["0xV2",true]}' http://127.0.0.1:8545`
   At n = 1 one vote is a majority: validator 2 joins at the next block.
3. Immediately, now on validators 1 **and** 2 (a majority of 2 is 2): propose `0xV3` on both. Then on 1, 2 and 3 (a majority of 3 is 2; vote on all three anyway): propose `0xV4`.
4. Check that `qbft_getValidatorsByBlockNumber` with `"latest"` lists 4. Then `qbft_discardValidatorVote` the pending votes.

Never leave the network at n = 2 (either node down halts the chain), and don't stop at n = 3 (no fault tolerance). Removals: the same votes with `false`, never below 4. The RPC needs the QBFT API (`--rpc-http-api=…,QBFT`); `validator run --profile validator` turns it on. The same text: `ternchain-ops help validator`.

On a `--selection header` testnet the citizen items below (one validator per citizen, `gov validator rotate`, founder coins) don't apply: a key is replaced by votes too, the new address `true`, the old one `false`. They describe the mainnet.

### Monitoring

- `validator status` from cron every 5 minutes:
  ```
  */5 * * * * ternchain-ops validator status --rpc http://127.0.0.1:8545 >> ~/tern/status.log 2>&1
  ```
- Alert when your validator's `last_sealed` lags by more than 2n blocks (n is the set size), or when round changes appear.
- `validator watch --rpc http://127.0.0.1:8545` for halts and censorship.
- Besu metrics (`--metrics`, 127.0.0.1:9545) for Prometheus. Ready configs, dashboards and alert rules: [ops/monitoring/](../ops/monitoring/).
- A shared alert channel (Telegram or Matrix). The coordinator gives you the link.
- What to do when something breaks: [ops/incident/INCIDENT-RESPONSE.md](../ops/incident/INCIDENT-RESPONSE.md). Besu upgrades: [ops/besu/UPGRADE.md](../ops/besu/UPGRADE.md).

### What we expect

- ≥99% uptime;
- answer an alert within 12 hours;
- upgrade before the announced block (at least 7 days' notice);
- take part in drills (every 2 weeks in phase 1);
- be a citizen and vote in the citizens' house;
- publish your country, ASN and host;
- no second validator;
- report a possibly leaked key;
- give 30 days' notice before leaving.

### Keys

- Never copy the node key to another machine. Two nodes with one key means double signing and grounds for removal.
- Keep an encrypted offline backup of the key.
- The founder key (`owner keygen`) lives on another machine, not on the validator server.
- If the key may have leaked: say so in the channel at once and rotate it (`gov validator rotate`).
- At genesis each validator gets 10 coins for gas, each founder 1000. These are test coins, enough for votes and transactions. The rest of the supply sits in the Treasury, which only the timelock spends, after both houses. The deployer gets 100 coins of gas and no powers.

### What you get

- a place on the public list of founding validators;
- reviews and reputation in the network (`attest`);
- treasury grants for node work (`gov propose grant`), only by decision of both houses. On the testnet these are test coins without value;
- possibly, hosting reimbursed during the testnet, if the coordinator personally chooses to. It is their personal choice, not a promise of the network;
- a voice in the network's development.

### What you don't get

- no promise of value, tokens, shares or future airdrops;
- testnet coins never become mainnet coins.

### Operator agreement (summary)

One page; the wording goes past a lawyer. In short:
- taking part is voluntary;
- no employment;
- no warranty and no obligation to pay;
- follow the network's terms ([TERMS.md](../TERMS.md)) and your own country's sanctions rules;
- leave at any time, with notice.

### Vetting checklist

- [ ] **Independence.** Doesn't work at the same company as another validator, isn't family, isn't on the same payroll. Doesn't operate anyone else's validator.
- [ ] **Hosting.** Host, ASN and country recorded. At most 2 per host and per country. No two in one cloud account.
- [ ] **One per citizen.** Has a citizen name (a founder, or vouched in); the contract refuses a second validator.
- [ ] **Key hygiene.** Key made on the server by `validator init`, mode 0600, an encrypted offline copy. The founder key on another machine. No keys held by the coordinator or by an agent.
- [ ] **Experience.** Has run a node or server 24/7 for 3+ months, or passed testnet-0 cleanly.
- [ ] **Reachability.** Two contact channels, time zone recorded, answered a test alert within 12 hours.
- [ ] **Law.** Confirms taking part is lawful where they are. Not on a sanctions list. Until the lawyer answers, not resident in Russia (L3).
- [ ] **Motive.** Understands "no promise of value" and accepts it in writing.
- [ ] **Dry run.** Synced as a non-validator and showed `validator status` with peers.

### Invitation text

> Hi! I'm building ternchain, an open network where people and their AI agents write to each other, make deals and pay each other. Nobody owns it: changes pass two houses (coin holders and citizens, i.e. people vouched for by others), and I have no special powers either.
>
> I'm gathering the first 7 validators for a closed test network on Hyperledger Besu (QBFT, 1-second blocks, instant finality). You'd need a server with 4 vCPU, 8–16 GB RAM and 100+ GB SSD, port 30303 open, three commands and about an hour to set up. After that: watch the alerts and upgrade before an announced block.
>
> What I can honestly offer: a place on the list of founding validators, a voice in governance, and treasury grants for running the node (on the testnet these are test coins without value). There is no money, no token and no promise of value, now or later: the coin is the network's gas, not an investment.
>
> I care that the validators are independent: different countries, hosts and employers, one person, one validator. If you're interested, reply and I'll send a short kit (requirements, commands, what we expect) and suggest a 15-minute call.

### Sanctions and the law

This is not legal advice. Until a lawyer answers the sanctions question (EU Regulation 833/2014, Art. 5b), we don't invite residents of Russia and don't place validators on servers in Russia. Take part only where that is lawful for you, and only if you are not on a sanctions list. The coin is the network's gas; it is not for sale.
