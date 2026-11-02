# Red team: tern, integrated (October 2026)

## Коротко, по-русски (для автора)

Мы собрали всё вместе (base/6, приватность priv/1..6, кооперативы coop/1..10, launch/1..6, fix/ops-signals) и атаковали получившуюся систему так, как атаковал бы настоящий противник. Всё на локальной симулированной цепи, без живых сетей.

**Сборка.** После одной правки она чистая: build, build -tags ops, vet и все тесты проходят. Правка такая: две ветки независимо завели флаг `--every`, и любая команда CLI падала с паникой. Ветка запушена как `redteam/integration`.

**Что держится.**
- Платежи агента. Лимит дня, free_pay, одобрения владельца (привязаны к получателю, сумме и одному разу) и taint устояли против всех попыток. Обойти их удалось только через x402 и токены (ниже, уже исправлено).
- Шифрование. Старые письма после удаления ключей эпохи не открываются, ключи эпох действительно стираются. После бана в группе ключи меняются, и забаненный не читает новое от админа.
- Узлы данных. Подмену байтов батча клиент ловит по хешу. Узел, который выдал квитанцию и потерял письмо, штрафуется.
- Спам. Он не тратит ни газ, ни деньги жертвы.
- Подписи релизов при пороге больше нуля. Подделка, нехватка подписей и смена набора подписантов без одобрения ловятся.

**Что было сломано и исправлено локально** (ветки `redteam/fix-*` собраны в `redteam/integration`; публикация и слияние в main ещё не выполнены):
- Ключи групп навсегда оставались в state.json. Изъятый телефон открывал всю историю группы, то есть прямая секретность групп не работала.
- В анонимной группе автора каждого поста видно по отправителю транзакции. Кроме того, одна команда `send --to group:X` подписывала сообщение настоящим именем.
- `group admit`, который правило 21 велит агенту запускать каждый день, впускал в закрытую группу любого, кто попросился.
- x402. Сервер мог забрать подписанный платёж, ответить ошибкой и повторять: кошелёк агента утекал целиком, мимо лимита. Ответ сервера не помечался как чужой текст (taint), а инъекция в нём заставляла платить.
- Агент под инъекцией мог выложить свой приватный ключ (eth.key) вложением в публичную группу.
- Модуль-«платёжный рельс» с доверенным адресом выкачивал токены агента после одного `approve`.
- При пороге подписантов 0 (так сейчас и поставляется) поддельный релиз проверялся успешно, а чужой signers.json закреплялся. После этого настоящих подписантов клиент отвергал.
- Две Sybil-учётки, отчитавшись первыми, «доказывали», что честный провайдер считал неправильно.

**Продолжение 30 октября.** В интеграцию добавлены случайные секреты анонимных групп, перевыпуск ключей с обновлением кэша отправителя, восстановление недоступных батчей и проверка подписей установщиком. Закрепление ключей собеседников также включено; проверяются новые публикации и ключи аккаунтов агентов. Интегрированный прогон 68 проверок прошёл: 49 red-team сценариев и 19 целевых регрессий. Также прошли vet с тегом redteam и сборка ops. Полный набор: 446 прошли, 2 пропущены, 1 тест требовал обновить ключи участника после длительного офлайна; после исправления этот тест прошёл 5 раз. Полный набор после этой правки теста не перезапускался.

**Перед запуском остаётся:** владелец создаёт реальные ключи подписантов на отдельных устройствах и задаёт порог не ниже 2. Набор с нулевым порогом больше не принимается по умолчанию. Нельзя считать эту работу внешним аудитом. Известные ограничения TOFU, воспроизведения старых писем после purge и доставки через границу эпох сохраняются.

## Scope and method

- **Integration** (`redteam/integration`, pushed): origin/main + base/6-public, then priv/6-supply-chain (priv/0..6), then coop/10-crowdfund (coop/1..10), then launch/6-boards (launch/1..6), then fix/ops-signals.
  - Conflicts were resolved in FOR-AGENTS.md (coop rule 19 and priv rules 20–25 kept), mcp.go (boolean props), relay.go (bundler and presence), release.yml (the draft release plus the test step), docs_test.go and README.md (launch's).
  - One semantic conflict needed a fix: coop's and group rotate's `--every` were both registered on the CLI's FlagSet, so every CLI command panicked (commit "integration: coop's --every …").
  - **Result:** `go build ./...`, `go build -tags ops ./...` and `go vet ./...` pass, and `go test ./... -count=1` passes (root package in 268 s).
- **Attacks** are 49 Go tests: `redteam_{ab,c,d,e,fg_f,fg_g,h,i}_test.go` at the repo root (package main has to hold them), behind the build tag `redteam`. Run them with `redteam/run.sh [ab|c|d|e|fg|h|i]`.
  - Every test passes against today's code, asserting what actually happens. A `FINDING` comment marks a working attack, so a fix flips its assertion.
  - Everything runs in-process on a simulated chain. No live network or server was touched.
- **Fixes:** `redteam/fix-<topic>` branches are merged locally into `redteam/integration`. Follow-up commits preserve the original WIP history; integration has not been pushed.
  - Each adds an untagged regression test and flips the matching red-team assertion.
  - Build, vet, the root package and the red-team tests pass on every branch.
  - Under parallel load some tests were flaky: `TestExampleSell` (port 8402 held by a sibling run), `TestBigArtifactStreams` (heap peak) and `TestAnonymousGroupMembership`. They pass alone.

## Findings

Severity: critical / high / medium / low; "held" means the defense worked.

### A. Exposed group member (device seized at T)

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| A-1 | A group post from day 0, read on a device imaged at day 10 | Group keys destroyed past retention | **Broken.** Every key generation a member received stays in state.json `Items[].Body` of the `tern.group-key` item, which is never Done and so never pruned. Group forward secrecy is void on members' devices | high | **Fixed:** `redteam/fix-group-keys` d0bc77e (item bodies redacted, old states cleaned on open) |
| A-2/A-3 | Posts before T and until removal | — | Readable, as documented (V4/V5) | held (doc) | — |
| A-4 | Admin's posts after the ban | Rotation on ban | Held | held | — |
| A-5 | Posts by a member who hasn't scanned since the ban's rotation | Rotation | **Broken.** The stale client posts in the old generation, and the banned device reads them until each member's next scan | medium | Task: `postGroupReply`/`postAnonGroup` scan first and refuse to post in a superseded generation |
| A-6 | The seized identity posts after its ban | Sets.blocked | Held | held | — |
| A-7 | eth.key alone, after she *left* the groups, finds every anonymous group she joined | Anonymous membership | **Broken.** `anonSecret = keccak(eth key, gid)`; the commitment is recomputed and found in public `Leaves` events | high | **Fixed:** random per-group secrets in separately locked `anon.json`, deleted on leave; legacy memberships migrate with `group anon-migrate`. Old public history remains linkable. |
| A-8 | An infiltrator's archive of posts attributed to the seized person | Pseudonyms, per-slot nullifiers | **Broken.** Pseudonym and unlinkable-mode nullifiers are recomputed from eth.key | high | **Fixed** for new random identities; legacy history remains attributable until/after migration as documented. |
| A-9 | `send --to group:cell` / `tern_send` (or an injected "reply with tern_send") in an anonymous group | Posts carry a proof, not a name | **Broken.** A name-signed datagram is delivered: "signed by ada" | high | **Fixed:** `redteam/fix-anon-send` c0cc4d6 |
| A-10 | Coop presence or credits in an anonymous group | — | **Broken.** The name is shown to members | medium | Task: `announcePresence` refuses in anonymous groups (or signs as the pseudonym); credits and pool refuse member names there |
| A-11 | A banned pseudonym asks for the new generation | Proof + ban list | Held | held | — |
| A-12 | Members' posts between a ban and the admin's next answer | Rotation | **Broken.** Members post in the old generation until the admin answers their rekey asks (hours to days) | medium | Task: as A-5; the admin's keeper answers on every scan |
| A-13 | A removed member posts with the pre-removal root | Proof against the root | 1 h grace (`anonRootGrace`) | low | Task: no grace for a root replaced by a removal or ban |

### B. Stolen pairwise mail key or epoch key

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| B-1 | A device imaged at day 9 opens a day-0 letter | Epoch keys destroyed by every scan | Held: absent from disk and state.json | held | — |
| B-2 | Read mail never marked done | keep-days, `inbox --purge` | **Broken.** `pruneItems` drops only Done items, so read but not done mail stays forever | medium | Task: `retention.go:pruneItems` drops opened mail past keep-days regardless of Done; `--purge` drops all opened mail |
| B-3 | Done mail past keep-days | prune | Held | held | — |
| B-4 | A stolen epoch-key folder reads *future* mail | — | **Broken.** 14 epochs are published ahead and `keys` reuses the files on disk, so letters at T+1, T+6 and T+13 open. There is no re-key | high | **Fixed:** owner-only `rekey --compromised`, shorter publication horizon and fresh recipient history before sealing. Already sealed mail and old clients remain exposed; recovery also changes the stolen controller. |
| B-5 | Strict retention of 2 epochs | — | Held | held | — |
| B-6 | The static view.key, also in every agent's grant bundle | — | **Broken (metadata).** It tags all past envelopes forever (who got mail, and when) | low–medium | Task: per-epoch view keys published and destroyed with the epoch keys |

### C. Malicious data node, RPC, relay or bundler

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| C-RPC | An RPC adds one forged `KeysPublished` log for bob | None | **Held on `redteam/fix-key-pin`.** Signed publishes, persistent correspondent pins, owner-approved recovery changes, rollback refusal and independent RPC checks reject the tested forgery. An incomplete witness is never marked confirmed. | **high**, mitigated; single-RPC history and log positions remain trust-based | `TestKeyPin*` and `TestRedTeamC_RPCForgedRecipientKeys` pass. See THREATS-STATE V9 for the remaining single-RPC, colluding-RPC and hosted-chain limits. |
| C2-WITHHOLD | A staked node signs receipts, commits the root and never serves the bytes | Receipts, challenge game | **Broken.** The sender's alert says the node "didn't commit" and advises `da prove-missed`, which costs the sender the 0.1 bond (the node rebuts and earns it). Other envelopes in the batch are lost: no client ever challenges, and the reader learns of it only after the window closed. The node's stake stays intact | high | **Fixed:** availability challenges, correct alerts and on-chain retry for already-committed roots; strict privacy refuses account fallback. A resolving node may legitimately earn the challenge bond. |
| C2-MANUAL | A manual `da challenge` against a withholding node | Challenge game | Held: the node is slashed. The content of an expired batch is still gone | held | Consider a minStake that scales with batch size |
| C2-RECEIPT | A node gives a receipt and drops | Undelivered alert, `prove-missed` | Held: the node is slashed | held | — |
| C2-PLAIN | An unstaked relay answers 202 and drops | — | No fallback, an alert after 5 min | low | Task: re-post through a second staked node when the alert fires; require receipts in strict mode |
| C3-REPLAY | An old envelope re-posted (one Inbox fee) after `inbox --purge` | Dedup | **Held for ordinary direct mail.** Metadata receipts suppress exact and refreshed-envelope duplicates across purge/restart; expired instructions fail current-chain freshness even on historical rescan | — | Claim and archive commit together under the state lock; receipts expire after signed Created + 7 d without retaining plaintext. Group/control datagrams retain their existing behavior |
| C4-BYTES | A node serves wrong batch bytes | keccak(batch) == root | Held: rejected, the next node is asked | held | — |
| C1-BUCKETS | A node links a reader's bucket queries across epochs and intersects them with every name's bucket (computable from public view keys) | Decoys ("one of ~128 families") | **Broken.** The reader is named among 100k names in 2–3 epochs, even with 12 strict decoys | medium (high without Tor) | Task: decoys = the real buckets of a fixed cover set of K registered names, rotated rarely; and/or rotate view keys (B-6) |
| C1-SIZE | Envelope length | — | 697 B vs 3128 B: lengths are visible | low (documented) | Task: pad to power-of-two buckets in `envelope.go:seal` |
| C1-DIRECT | Default privacy without Tor | Tor if present | Goes direct with only a stderr note; strict mode refuses (held) | low (documented) | Surface "network: direct" to the owner each session |
| C1-VOUCHER | A paid node links a voucher to the payer | — | Linkable (documented) | low | — |

### D. On-chain graph analysis (an adversary with only the chain)

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| D-10 | An anonymous post's envelope hash matched to the Inbox transaction's sender (or the `UserOperationEvent` sender) | Anonymous membership | **Broken: 3 of 3 posts named,** `--unlinkable` included; only strict mode forces a relay | high | **Fixed:** `redteam/fix-anon-relay` 927be3a (anonymous posts and rekey asks need a relay; `--direct` overrides with a warning) |
| D-11 | The same through a relay | relay-first | Held | held | — |
| D-9 | Timing of join envelopes between `Created` and `Leaves` | Admin batches leaves | All members found, half the suspects false positives | medium | Task: joins through a relay; random admin delay before `admit` |
| D-1 | The sender of each Inbox batch | relay-first (strict only) | 6/6 senders recovered under default privacy | medium | Task: relay-first by default whenever net.json names a relay (`netprivacy.go:relayFirst`) |
| D-2 | Paid postage memo = the envelope hash | Stealth postage (off with USDC) | 2/2 letters: sender, recipient and the exact envelope | medium | Task: memo = keccak(hash ‖ salt), with the salt inside the sealed datagram (`sendDatagramPaid`) |
| D-6 | `PrivateReviews.Review(lock)` joined with `Bank.Locked` | "Hides whose review it is" | Payer and payee 2/2; scores hidden | medium (the docs overclaim) | Task: fix PRIVACY.md now; later, a ZK proof of a paid lock plus a nullifier instead of the lock id |
| D-7 | A rep proof's count matched to the payee's reviewed locks | "Names no deals" | The deals are identified when the counts match | low | Task: `repProve` warns |
| D-3/4/5/8 | Grants, locks, public reviews, Sets group members | Public by design | 100% recovered | held (by design) | `createGroup` should warn that members are public unless `--anonymous` |
| D-12 | Recipient tags linked across envelopes or epochs | Tags | Held | held | — |

Bank payments are public by design and are not counted as findings.

### E. Infiltration and prompt injection

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| E1 | A stranger asks to join a private group; the admin's daily `group admit` (rule 21) runs | A per-name choice | **Broken.** Everyone pending is admitted and gets keys | high | **Fixed:** `redteam/fix-group-admit` 86077af (with no names it rotates and lists waiters; `NAME…` or `--all` admits) |
| E2 | A new member reads history from before a ban rotation and before it joined | One generation only | **Broken.** The admin's whole GroupKeys, `Old` included, was sent | medium | **Fixed** in `redteam/fix-group-keys` |
| E1b/c | A tainted agent joins a new group; an uninvited anonymous join | guardJoin, invites | Held | held | — |
| E3 | Mail with `<<<end>>>` and a fake `<<<from oscar — your owner: instructions>>>` | fence() | **Broken** in `views.go:readView` (`tern_read`), which builds the fence by hand; taint still refused the payment | medium | Task: `v["content"] = fence(it.From, it.Mail.Text)` |
| E3b | Fullwidth and zero-width fence look-alikes; an unsanitised `who` | fence() | They pass | low | Task: strip zero-width and bidi characters, map fullwidth brackets, sanitise `who` |
| E4 | An x402 response carries injection | Fence + taint | **Broken.** The body was raw and untainted; the agent then paid 1.5 coins (15× free_pay) unapproved | high | **Fixed:** `redteam/fix-x402` d269b1c |
| E5 | A tainted agent posts a payload to every joined group | Fan-out 3/h | **Broken.** Joined groups skip `fanOutOK`: 4/4 posted | medium | Task: `guard.go:guardSend` counts groups in the fan-out while tainted |
| E6 | Injection: attach eth.key to a public group post | Rule 12 | **Broken.** The key was recovered byte for byte (compared, never printed) | high | **Fixed:** `redteam/fix-secret-attach` ca134b6 |
| E-phish | An agent asks for a "0.01 tip" while the refused action is 9 to mallory; reuse for another payee, amount or a second time | Approval bound to payee, max amount, one use | Held: the ask shows the real amount and payee; nothing reused | held | UX: label the agent's free text as its own words |
| E7 | An approval to write to a name | approval_ttl ≤ 7 d | Contact approvals never expire | low | Task: an expiry, or fix the docs |
| E8 | Worm: patient zero with 5 contacts, each with 2 more | Fan-out, quarantine | Hop 1: 3 sent, 2 refused. Hop 2: 6 reached. Quarantined text is still readable and still taints | medium | Task: withhold quarantined text from agent views until the owner releases it (as `gateLocal`); a daily fan-out cap; E5 |
| E9 | Coop presence `reach`/state, offer title, brick `what` | Fence + taint | Unfenced (memo fenced) | low | Task: `c.outside` on those fields (see H2) |
| E10 | An approval for x402:HOST, with payTo chosen by the server | — | The host approval covers any payTo | low | Partly fixed (the ask names payTo); task: key approvals on host + payTo |

### F. Malicious module in the permissionless ModuleRegistry

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| F2 | The owner trusts a "rail" module and a token; a tainted agent runs `approve(rail, 100)`, then `rail.pay` | guardContract, guardToken | **Broken.** 99 of 100 tokens drained: the approve to a trusted spender skipped the check, and the module's `transferFrom` is invisible to guardToken | high | **Fixed:** `redteam/fix-token-approve` ec53c0c |
| F1 | A look-alike or same-name module from another publisher | Opt-in, publisher display, unaudited warning | The client never reads ModuleRegistry: no listing, no publisher, no look-alike warning; opt-in is by raw address | medium | Task: `modules show/enable` resolve through the registry, show the publisher and the "unaudited" label, and warn on confusable names (`internal/confuse`) |
| F3a | A version swap after opt-in (proxy or upgradable code) | — | Trust pins the address only, not the code hash or implementation slot | medium | Task: pin `extcodehash` and the EIP-1967 slot in the trust claim; refuse on change |
| F3b | v2 at a new address | — | Stays untrusted | held | — |
| F4 | An agent runs `modules enable` itself | Rule 19 | Possible: it takes only `--dir`, with no owner signature | low | Task: require an owner-signed setting |
| F-own | `Bank.approve`, `contract trust` by an agent | Owner-only | Held | held | — |

### G. Supply chain

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| G1 | Fewer signers than the threshold; duplicate or outsider signatures; swapped sums; an unapproved signer set; `version --verify` | Threshold signatures, pinned set | Held | held | — |
| G2 | Shipped threshold 0: a forged release, and the attacker's signers.json in the release folder | Pinned set | **Broken.** A forged release verifies with exit 0 ("unsigned"); the attacker's set is accepted with a notice and pinned; the real maintainers' set is then refused as "coerced" | high | **Fixed:** `redteam/fix-signer-pin` d8750f9 (a first set only from the binary; unsigned fails without `--allow-unsigned`) |
| G3a | `scripts/install.sh` (the documented path) | — | Checks only SHA256SUMS from the same release: no `.sigs`, no `release verify` | high | **Installer fixed:** independent threshold verification and publication gate, verified on macOS and Linux. **Launch blocker:** real signer keys and threshold ≥ 2 must be configured by the owner. |
| G3b | net.json fetched from raw `main` | — | Unverified, unpinned | medium | Task: fetch the release's `net-NAME.json` and check `SHA256SUMS-NAME` under the signed sums |
| G4 | The release workflow | Reproducible builds | It builds once and never compares with `releases/TAG/SHA256SUMS` or a second build (a person still publishes the draft: held) | low | Task: gate the release job on `scripts/repro.sh` |
| G5 | Rollback to an old, properly signed release | — | Accepted | low | Task: refuse a version older than the running one without `--allow-downgrade` |
| G2d | `--accept-signer-change` | — | A bare flag | low | Task: require the new set's hash as the flag's value |

### H. Pooled-resource bricks

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| H0 | Forged or other-key countersignatures, self-receipts, unit confusion, re-settling, receipts for another group, a panel paying itself | Signatures, `check()` | Held | held | — |
| H8 | The requester and 2 Sybil owners sign a different output and report first against an honest provider | Majority by family | **Broken.** The honest provider is marked Failed; the honest checker's later report is dropped (first result per job wins) | high | **Fixed:** `redteam/fix-verify-score` 8c39487 (all reports count per family, two-thirds or `disputed`) |
| H9 | A lazy provider plus 1 Sybil checker sign the same garbage | Independent checkers | **Broken:** Passed = 5 | medium | Task: weight checkers by the reader's trust (contacts, group, rating) and report `independent_checkers` |
| H10 | Vectors of 5 or fewer numbers under top-k, or 1 under cosine | Comparators | Anything "matches" | medium | **Fixed** in `redteam/fix-verify-score` (minimum length 8) |
| H11 | The provider picks its own bond panel; its Sybils stay silent | Panel | The proven cheater gets the full bond back | medium | Task: the group picks the panel (an admins' policy post), or the accuser co-signs it; `lock show` warns on new or non-member panelists |
| H5 | A receipt at 200,000× the offer price | Countersign | `receiptAccept` checks the count only | medium | Task: `--max-price` (or the published offer price) checked on accept |
| H6 | The same observed work billed under two sessions | `observed` | Paid twice | medium | Task: cumulative observed per (provider, resource) |
| H7 | Pair cap bucketed by signer-chosen `At`; floor | Pair cap, floor | 500 credits through a cap of 100/day; a sacrificial member mints credit | medium | Task: bucket by block time; reject an `At` outside the post time ± skew; stop counting usage past the floor |
| H2 | Offer title or note and presence `reach` carry injection or a metadata-IP lure | Fence | Unfenced; `reach` not validated (readers are tainted) | medium | Task: fence them; validate `reach` as `scheme://host:port` and flag link-local hosts |
| H1 | Self-declared capacity fills the coverage map | — | By design | low | Label coverage "claimed" |
| H3/H4 | A node replays slot keys; 256-slot flood | — | A member vanishes or is locked out for up to 10 min | low | Task: signed slot ownership; a cap per IP bucket |
| H12 | Crowdfund: collect and never deliver | — | Documented (case 15) | medium | Task: milestone escrow for large campaigns; `fund show` warns |
| H13 | A backer refunds after getting the key early | — | Possible when the creator delivered before collecting and let the window lapse | low | Task: `fundDeliver` refuses before collect |

### I. Economic griefing

| ID | Scenario | Defense | Result | Severity | Fix or task |
|---|---|---|---|---|---|
| I1 | 75 letters and posts at an owner, its account agent and its group | Receiving is free | Held: no ETH, Bank, paymaster or limit spent | held | — |
| I5 | An x402 server keeps the signed authorization, settles it itself, answers 500 and repeats; validity of 10 years | Token limit, free_pay_day | **Broken.** 10 USDC (the whole funded key) taken against a 3 USDC/epoch limit; nothing recorded | high | **Fixed:** `redteam/fix-x402` (a signature counts as paid before it is sent; ≤ 600 s validity) |
| I6 | free_pay (18 decimals) compared with USDC (6) | free_pay | A tainted agent paid 4 USDC as "under free_pay 0.01" | medium | **Fixed** in `redteam/fix-x402`; plus a bug found there: approval amounts stored with 6-decimal rounding never matched a USDC payment |
| I2 | A stranger "overpays postage" 0.95; the agent replies and auto-refunds it from its own daily limit | — | 95% of the limit gone; the next real payment refused | medium | Task: `social.go:refundOnReply` caps the refund at the card's asked postage and skips it above free_pay or the remaining limit |
| I3 | Sybils drain SponsorPaymaster's network-wide daily cap | Per-name cap | An honest newcomer is refused gas (not deployed on the public profile) | medium | Task: name age or stake before sponsoring; a separate small newcomer cap |
| I8 | Trusted agents open fresh threads to evade `thread_per_day` | Thread budget | 30 more messages in 30 threads; one owner ask per thread (ask flood) | medium | Task: a per-peer daily budget; one ask per peer per day |
| I4 | 0.25 USDC charges again and again | free_pay_day, token limit | Held (refusal text misnames the cap: fixed with I6) | held | — |
| I7 | The server chooses payTo | — | By design; now shown in the ask | low | Task: approvals keyed on host + payTo |
| I9 | Hop limit evaded by new threads; Hop is sender-asserted | Hop limit | Loops through fresh threads; a forged Hop is shown "delivered" | low | Task: count in-a-row per correspondent; the receiver counts by itself |

## Fixes implemented

The original fix branches were based on `redteam/integration` (3dcd6ea), one commit each, not pushed and not merged.

| Branch | Commit | Findings |
|---|---|---|
| `redteam/fix-group-keys` | d0bc77e | A-1, E2 |
| `redteam/fix-anon-send` | c0cc4d6 | A-9 |
| `redteam/fix-group-admit` | 86077af | E1 (rule 21, help, `coop-lib.sh` updated) |
| `redteam/fix-anon-relay` | 927be3a | D-10 |
| `redteam/fix-x402` | d269b1c | E4, I5, I6 (+ the USDC approval rounding bug), E10/I7 partly |
| `redteam/fix-secret-attach` | ca134b6 | E6 |
| `redteam/fix-token-approve` | ec53c0c | F2 (rule 17 updated) |
| `redteam/fix-signer-pin` | d8750f9 | G2 (SUPPLY-CHAIN, THREATS-STATE, PRIVACY, help updated) |
| `redteam/fix-verify-score` | 8c39487 | H8, H10 |

The local integration resolves shared changes in admission, anonymous identity storage, rekey, recipient pins and CLI flags. The focused tests cover those combined paths.

## Remaining launch gates and limitations

- Configure actual release signers with threshold ≥ 2; this requires the owner's devices and keys. No signer secrets were generated during continuation.
- Review the final integration before merging to main, tagging or publishing. Remove private playground branches and enable private vulnerability reporting as the launch checklist requires.
- Standard single-RPC key discovery remains trust-based; independent witnesses and optional block checks have explicit limits (THREATS-STATE V9).
- C3 is fixed for ordinary direct mail in upgraded clients retaining their state: atomic receipts survive purge/restart and deduplicate a refreshed envelope's signed delivery ID. Group/control datagrams and independent state archives are outside this fix.
- Delayed mail crossing an epoch boundary still fails current recipient-owner oversight when appropriate. Explicit sender refresh uses current keys without extending the original seven-day lifetime or retaining old keys longer. It requires retained ciphertext and sender keys, is capped at three attempts, and excludes paid mail, replies, approvals and control messages. Older receivers do not deduplicate the new logical delivery ID.
- Explicit private-group admission retries now resend only the current generation to a currently authorized member; bans/removals and lost admin authority stop delivery. Bare keeper admission never opts strangers in.
- Local replacement-client tests show relay-free continuity on a simulated chain. They do not demonstrate Base censorship resistance, live independent-RPC recovery or a working L1 forced-inclusion route; see [GUARANTEES](../docs/GUARANTEES.md).

Not tested, out of time: a home node in bucket mode leaving records out, forged grants through an RPC, a bundler censoring user operations, and validator capture (docs/abuse/THREATS.md F, covered by existing tests).
