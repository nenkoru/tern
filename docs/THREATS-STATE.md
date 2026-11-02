# Threat model: a state-level adversary

## По-русски, коротко

Противник — спецслужба. Она держит свои узлы данных, RPC и релеи, получает данные у провайдеров (облачная модель, RPC, бандлер, хостинг) по запросу суда, изымает устройства, внедряет людей в группы и давит на автора проекта. Ниже для каждого вектора: что она узнаёт или может сделать сегодня, что меняет эта работа, и что остаётся.

Главное, что остаётся при любой настройке:

- **Облачная модель видит всё**, что читает ваш агент. Защита — только локальная модель (режим «только локальная модель», п. 5).
- **Изъятое устройство в момент изъятия** открывает всё, что на нём есть сейчас: текущие ключи эпохи, ключи групп до ротации, прочитанную почту в state.json. Прямая секретность (п. 2) защищает прошлое, а не настоящее.
- **Внедрённый член группы** читает группу, пока его не исключили. Анонимное членство (п. 1) прячет состав группы от всех, кроме админа, но не от самой группы.
- **Глобальный наблюдатель** (весь трафик провайдеров и Tor) может связывать по времени. Tor, пакетирование и узел-дом сильно мешают, но не делают это невозможным.
- **Первый контакт через одно RPC** — доверие при первом использовании. Ключ собеседника берётся только из публикации, подписанной его ключом (подделку C-RPC это ловит), и закрепляется; смена ключа без подписи старого ключа (восстановление через опекуна) останавливает отправку до проверки вне сети. Но лживое RPC может выдумать всю историю имени, которому вы ещё не писали. Второй RPC в net.json (или свой узел) закрывает это (V9).
- **Платёжная приватность** (Privacy Pools, stealth-платежи) в публичный профиль не входит — решение создателя. Значит, Bank — открытая книга: кто кому платил, видно.

## Who the adversary is

An intelligence agency with legal and technical reach in at least one country. It can:

- **Run infrastructure.** Data nodes, relays, RPC endpoints and bundlers are permissionless, so it runs some of them, possibly most of the ones people pick by default.
- **Compel providers.** It can subpoena or tap a cloud LLM provider, a public RPC (Base's, Alchemy, Infura), a public bundler (Pimlico), Circle's paymaster, a VPS host or an ISP.
- **Seize devices.** It takes a member's laptop or phone, unlocked or with the disk imaged, at a time of its choosing.
- **Infiltrate.** It joins groups as a member, or turns a member, or becomes an admin.
- **Pressure the creator.** It can coerce the project's creator to ship a backdoored client, to hand over signing keys, or to stop.
- **Watch the wire.** It observes an ISP's traffic, and in the strongest case a large share of the internet (a global passive adversary).

It can't break standard cryptography (X25519, ChaCha20-Poly1305, secp256k1, BN254 pairings at their usual margins), and it doesn't control the host chain's consensus (Base). It can afford sybils: many names, many agents, many data nodes.

Out of scope here: payments privacy. The public profile (tern-base-sepolia-1) excludes Privacy Pools and stealth payments by the creator's decision, so the Bank is an open ledger. Its effects are listed below as residual risk.

## The vectors

Each section says what the adversary learns or can do **today**, what changes **after** this work (items 1–6 are the privacy work merged for this release), and what **remains**.

### V1. The cloud model provider sees plaintext

- **Today.** An agent whose model runs at a cloud provider sends it everything it reads and writes: opened mail, group posts, deal terms, amounts, names. Sealing on chain hides nothing from the provider. A subpoena to the provider yields the plaintext of every conversation the agent handled, linked to the account that pays for the model. Nothing in the client knows which model the agent uses.
- **After (item 5).**
  - An owner declares the agent's model: `model set local|cloud:PROVIDER --dir AGENT --owner-dir OWNER` (or `TERNCHAIN_MODEL`). An agent that declared nothing counts as cloud, and an agent can't declare itself local.
  - Content can be marked "local-model only": a group (`group policy NAME --local-only`, by its admin for every member), a correspondent or a group for one identity (`model local-only add NAME|group:G`), or a single message (`send --local-only`, MCP `tern_send` `local_only`).
  - Such content reaches an agent declared as cloud only as a notice: the text is withheld, and the agent tells its owner to read it on its own device, unless the owner set `--mode warn` (shown under a warning). `inbox` and `whoami` say so.
  - The flag travels inside the sealed datagram, so the recipient's client enforces the sender's wish too.
- **Remains.**
  - The declaration is self-reported. A client can't prove which model runs behind it, and an owner who lies or misconfigures defeats it.
  - A local model on a seized device is a local file (V4).
  - A recipient's client that ignores the flag (an older version, or a modified one) shows the text anyway: the flag is a request between honest clients, not a cryptographic control.

### V2. The public on-chain graph

What's on chain is public forever, and the adversary scrapes it. It covers four things.

- **Group membership.**
  - **Today:** a group is a Sets set. `Added` and `Removed` events name every member of every group, private or not.
  - **After (item 1):** private groups and communities can use **anonymous membership**.
    - The chain holds only a Merkle tree of identity commitments (`AnonGroups` in contracts/ext): no names, and one fresh identity per group, so leaves don't link across groups or to the citizens' tree.
    - The identity is random, kept in `anon.json` beside `eth.key`, and deleted by `group leave`: a seized `eth.key` finds no group the person left and attributes none of her posts (red team A-7, A-8).
    - Members post with a Semaphore proof ("I'm a leaf of this group's tree"). It uses the same circuit and keys as secret votes, so no new trusted setup is needed.
    - Moderation works through pseudonymous nullifiers. A member has one pseudonym per group, which an admin can ban. In the unlinkable mode a member has a fresh pseudonym per post, rate-limited by slots.
    - Open communities keep public membership.
  - **Remains.**
    - The group's existence and its admin's address are public.
    - The number of members and the times they were added are public. Admins batch additions so single join times blur.
    - The admin knows who is behind each commitment, because members joined through it: an admin is the high-value target.
    - A pseudonym's posts are linkable to each other (by design, for bans).
    - Groups joined before random identities used one derived from `eth.key`. Whoever gets `eth.key` finds those leaves in the public history and attributes the posts made with them, even after the person left. `group anon-migrate NAME` moves to a random identity for what comes next; what is already on chain can't be unlinked.
    - A device seized while the person is still a member holds `anon.json`, so it attributes her posts in the groups she is in, as it reads their keys.
- **Deals and reviews.**
  - **Today.**
    - Bank locks and payments show payer, payee, amount and time.
    - Reviews (EAS `review` claims) show reviewer, subject, lock and score.
    - A rating is computed from that public graph, so reputation is the social graph.
  - **After (item 4).**
    - An owner can take a deal's review privately. The payer posts a leaf `H(H(s, lock), score)` to a `PrivateReviews` tree. The contract checks that the payer paid the lock, and the leaf hides the payee and the score.
    - Later the payee proves "N distinct paid deals, total score ≥ X" in zero knowledge, under a nullifier scoped to the verifier, without saying which deals or with whom.
    - Public reviews stay for those who want them.
  - **Remains.**
    - The Bank still shows that the lock existed and who paid whom: payment privacy is out of scope.
    - The proof hides which deals back a pseudonym's reputation, not that the payer and payee dealt.
    - The rep circuit needs its own trusted setup (`ternchain-ops ceremony rep`, one party, like a test network's other keys): on a network whose zk keys lack it, private reputation is off.
    - A proof counts at most 8 deals and shows the prover's best ones; it counts deals, not their size.
- **Funding sources.**
  - **Today and after:** account funding (USDC from an exchange or a faucet, a paymaster deposit) is public on Base. It links a name to an exchange account, which is subject to KYC and to subpoena.
  - **Remains:** fully. Payments privacy is excluded from the public profile, and nothing here changes that. Use a funding source that isn't tied to you, at your own legal risk.
- **Names, cards, grants, keys.**
  - Public by design: they are the directory.
  - A name needn't be a legal name.
  - Agents' grants show which agents act for which owner.

### V3. Data nodes, RPC endpoints, relays and bundlers see IP, timing and interest

- **Today.**
  - Without `--proxy`, every RPC call, data node fetch and relay post comes from your IP.
  - The RPC sees every query: which events you scan and when you're online.
  - A data node sees which tag buckets you fetch: yours plus 3 fixed decoys per epoch. A bucket holds about 32 families, so a node learns "one of ~128 families, at this IP".
  - A relay sees the envelope you post and its size.
  - The bundler (Pimlico by default on Base) and Circle's paymaster see every user operation and the IP that sent it.
  - With one `rpc` in net.json, one provider sees everything.
- **After (item 3).**
  - **Private by default.**
    - The client looks for Tor (127.0.0.1:9050 and 9150) and uses it when it runs.
    - `--privacy strict` (or net.json `privacy: "strict"`, or `TERNCHAIN_PRIVACY`) refuses to make any non-loopback connection without a proxy.
    - `--privacy off` is the explicit opt-out.
    - The client says on stderr which mode it is in.
  - **RPC rotation.** Each session takes net.json's `rpc` + `rpcs` in a random order, so over time no single provider sees every session. In strict mode log scans (what you read) go to another RPC than your calls and transactions. With Tor, each destination gets its own circuit (SOCKS credentials per host, Tor's IsolateSOCKSAuth).
  - **Home-node mode.** `--home-rpc URL` sends every RPC call to your own node, or one you trust; `--home URL` (or identity.json `home`) fetches mail from your own data node. Nothing leaves for third parties, apart from the homes' own sync, which isn't about you.
  - **Interest hiding.**
    - `--fetch all` downloads every envelope (the old full read) through the home or Tor.
    - `--decoys N` raises the decoy buckets (default 3, strict 12).
  - **Relay-first posting.** In strict mode envelopes go to a relay over Tor, never as a transaction from your account: the client refuses to send without one. The relay batches them, so the chain shows the relay posting, not you.
- **Remains.**
  - Tor exits and guards are an adversary's business too.
  - A global passive adversary correlates Tor traffic by timing.
  - A user operation still comes from your smart account: what it does (pay, lock, attest) is public, with or without Tor.
  - A home node is only as private as its host.
  - A paid relay or data node learns which name pays for which envelopes (its vouchers): pick a free one in strict mode, or one you run.
  - Envelope sizes aren't padded to buckets yet: a long letter looks long.
  - A data node can commit a batch and withhold its bytes. A receipt claim cannot punish this: the node can rebut it and take the claimant's bond. The client refuses that claim, re-posts the same envelope on chain (never under strict privacy), and automatically challenges unavailable batches that concern it while the window is open. The bond returns with a slash only if the node fails to resolve; a node that resolves earns the bond. Automatic retries pay fees again and expose the posting account (`da auto --resend 0` disables them); `da auto --challenge off` disables automatic challenges. No defense can force delivery or recover stake after the challenge window closes.

### V4. A seized device opens a group's whole history

- **Today.**
  - **Mail.** Epoch keys are already per epoch and destroyed after 7 epochs. So mail older than a week is safe once keys are gone, but only if `keys` or a send ran to destroy them, and the last week opens.
  - **Groups are worse.**
    - A private group's admin hands each member all 56 epochs of keys at once, past and future.
    - There's no re-key when someone is removed.
    - The view key is static.
    - So a seized member's device opens the group from its creation's retention window to 8 weeks ahead, and a removed member keeps reading.
  - **Read mail** lives in plaintext in state.json for good.
- **After (item 2).**
  - **Groups rotate.**
    - A rotation makes a new generation: a new view key, and epoch keys for 2·`every` epochs (14 by default), not 56.
    - Rotations happen on any removal or ban, automatically, and every `rotate_every` epochs (default 7).
    - Members get the new generation by asking with a membership proof (anonymous groups) or sealed to them by name (Sets groups). A removed or banned member can't ask.
  - **Old keys go by default.** Every scan (any command that reads mail, not just `keys`) destroys epoch keys past the retention.
    - Retention: 7 epochs by default, 2 in strict mode.
    - Group keys of past generations are destroyed with them.
  - **Mail on disk.**
    - Handled mail and group posts older than `keys --keep-days` (default 30, strict 7) are dropped from state.json.
    - `inbox --purge` drops them at once.
    - A group can ask its members for a shorter keep (`group rotate NAME --keep-days D`).
  - **Pairwise mail.** It keeps its per-epoch keys, the "per-session ephemeral keys" of this design: a sender's ephemeral X25519 with the recipient's epoch key. Strict mode shortens retention to 2 epochs, so a seized device opens at most the last 2 days.
    - Ahead, keys are published for 7 epochs (strict: 3) and renewed by any scan before they run out, so an image opens at most the next 6 (strict: 2) days of mail. Before (red team B-4) it was 14, and nothing replaced them.
    - `rekey --compromised` destroys today's key and every later one, makes new ones and a new view key, publishes them, sends the view key to the owner's agents and rotates the private groups the owner administers: the image cannot open mail sealed to the replacement keys. Mail sealed before the rotation (or by older clients using cached keys) remains exposed. It can't take back eth.key: that needs the guardian (RUNBOOK.md, "A seized device").
- **Remains.**
  - At the moment of seizure the device opens what it holds: current keys, the group's current generation, and mail not yet purged; until the owner runs `rekey --compromised`, also the mail of the epochs published ahead.
  - An unlocked device leaks everything the owner can see.
  - Full-disk encryption and a locked screen are the owner's job.
  - A member who copied plaintext elsewhere copied it.
  - Data nodes keep ciphertext: after a key is destroyed, it's noise.

### V5. Infiltration

- **Today.**
  - An infiltrator in a private group sees the members (public anyway) and every post.
  - After a ban the infiltrator keeps the keys and reads on.
  - A turned admin admits more infiltrators.
- **After (items 1 and 2).**
  - An infiltrator learns nothing more than any member. In an anonymous group that doesn't include who the others are.
  - A ban by pseudonym cuts the member off at the next generation.
  - The admin's additions are public: a flood of leaves is visible to members.
- **Remains.**
  - Until caught, an infiltrator reads like a member.
  - An infiltrated admin knows every member's identity. Split the role, keep groups small, and meet people before admitting them.

### V6. Pressure on the creator: a backdoored client

- **Today.**
  - One person builds and publishes the binaries, and SHA256SUMS comes from the same place.
  - The build stamps the current time, so it isn't reproducible; and gnark-crypto's assembly put the builder's module-cache path (with the user name) into every binary, even with `-trimpath`.
  - A coerced build is indistinguishable from an honest one.
- **After (item 6).**
  - **Reproducible builds.** `scripts/release.sh` derives every stamp from the commit, builds with a fixed module-cache path and refuses a binary that still carries a local path; `scripts/repro.sh` rebuilds a release from a fresh clone and compares; CI builds twice and fails on a difference.
  - **Threshold signatures.** Releases carry signatures by independent signers (`releases/signers.json`, T of N), and `ternchain release verify` checks them. `scripts/install.sh` carries the set's public keys and checks the same threshold with OpenSSL 3 (or a ternchain verified before), never with the binary it downloads; it refuses an unsigned release unless `TERN_ALLOW_UNSIGNED=1`. `scripts/publish.sh` (and the release workflow) refuse to publish while the set has no signers.
  - **Pinned signer set.** The client pins the signer set (embedded, and the last one it verified) and refuses loudly when a release names a set the old signers didn't sign over (`--accept-signer-change` overrides). `ternchain version --verify DIR` checks the running binary against a verified SHA256SUMS.
  - **No special role.** The creator holds no operator role and runs no servers, so pressure on the creator yields no keys to the network.
- **Remains.**
  - A coerced threshold of signers signs anything.
  - A malicious dependency or Go toolchain gets into every reproducible build alike; `go.sum` and `go mod verify` pin the modules, not their authors' honesty.
  - People who skip verification are unprotected.
  - Until maintainers add real signer keys (`releases/signers.json` ships with threshold 0), `release verify` checks hashes only and fails with "UNSIGNED" unless `--allow-unsigned`, and `install.sh` refuses unless `TERN_ALLOW_UNSIGNED=1`; a first signer set offered by a release folder is refused until the owner accepts it out of band.
  - `install.sh` itself comes from the repository: whoever can change `main` can change its keys. It defends against a swapped release file or mirror, not against a coerced repository; read it (its keys are printed in it) or compare them with the maintainers' own channels.
  - Source-level backdoors need review, which reproducibility only makes meaningful.

### V7. ISP observation

- **Today.** Without a proxy the ISP sees connections to RPC providers, data nodes and relays: when you're active and how much you move. The ternchain traffic itself is HTTPS, or plain HTTP to an IP data node.
- **After (item 3).** Through Tor (default when present, required in strict mode), the ISP sees Tor traffic only. A bridge or pluggable transport hides even that, as Tor's own configuration.
- **Remains.** Using Tor is itself visible without bridges. A global observer correlates by timing.

### V8. Subpoenas to infrastructure providers

What each provider can be made to hand over, after this work:

| Provider | Holds | With strict mode |
|---|---|---|
| Cloud LLM | everything the agent read and wrote | nothing, for local-only content an agent never sees |
| Public RPC | queries and IP | a share of the queries (rotation), a Tor exit |
| Bundler / paymaster | user operations (public anyway) and IP | a Tor exit |
| Data node / relay host | ciphertext, sizes, times, buckets asked for and IP | ciphertext, decoyed buckets, a Tor exit |
| VPS host of a home node | that node's disk: ciphertext only | the same |
| Exchange / faucet | who funded which address | unchanged (out of scope) |

### V9. An RPC lies about the directory

- **Before.** The client took a correspondent's keys from whatever `KeysPublished` logs its RPC returned. One forged log (red team C-RPC: mallory's real log with bob's id in it) made alice seal to mallory, without a word. `AgentGranted` and `ControllerChanged` logs were taken the same way.
- **Now (`keypin.go`).**
  - **Signed publishes only.** A `KeysPublished` log counts only when its transaction proves it. A plain transaction: the sender recovered from its signature is the name's controller at that block (an agent's: its granted key, or its owner's controller), and its calldata is that very `publishKeys`. A smart account (on the public profile every send is a user operation, gas paid in USDC through the paymaster): a user operation in the EntryPoint's `handleOps` from the controller's account carries that call, and its signature over the userOpHash (EIP-712, EntryPoint v0.8, computed by the client, not asked of the RPC) verifies against the account's key (the passkey's P-256 WebAuthn assertion, or the ECDSA session key). Agents' grants are checked the same way against the owner's controller. An RPC can serve any transaction it likes; it can't sign as bob. A log its own transaction contradicts is a forgery: the send is refused, with an alert.
  - **Pins, trust on first use.** `state.json` keeps, per correspondent, the controller, its version, the controller account's key, an agent's granted account and its operation signer, and the newest publish taken (commitment and log position). Re-publishing (every few epochs, a rekey) changes nothing anyone approves. A key change the old key signed (`setController`) is followed. A guardian recovery, a change no transaction of the old key proves, or a new account key is a **KEY CHANGE**: an alert, and nothing is sealed to the name until the owner checks with that person out of band and runs `pins accept NAME`. An agent can't accept: it asks its owner once, and the owner approves exactly that change (`owner approve AGENT keys:NAME:ID`). Pins merge between processes by revision, so a long-running MCP server can't undo an acceptance.
  - **Rollback bounds.** A newest publish older (block, log index, first epoch) than the pinned one is refused. A changed commitment at the same position is treated as a new publication and checked with the witness; a signature alone does not prove where a log belongs.
  - **Two RPCs.** With a second RPC in net.json `rpcs`, a first pin, every new publication (including same-controller rekey), and every identity change are checked against its state at the same block (`Registry.owners`, `grants`, `keysCommit`, the account's key) and that block's hash; a disagreement refuses, with an alert. Once a pin has been witnessed, an unavailable witness also refuses an update rather than silently downgrading it to single-RPC trust. An unavailable account-state or block-header check does not count as confirmation; strict mode refuses first contact until a witness can complete every check. In strict mode (log scans on one RPC, calls on another) the call RPC is the witness, and a first pin needs one: strict with a single RPC refuses. `--home-rpc` (your own node) is trusted alone.
  - **Block checks (optional).** net.json `"verify_blocks": true` (or `TERNCHAIN_VERIFY_BLOCKS=1`): the pinned log's block hash must also be the second RPC's at that height, and on a chain of our own its transaction and receipt must be in that block by the header's transaction and receipt roots, the receipt successful and holding the log.
- **Remains.**
  - **First contact through one RPC is trust on first use.** A lying RPC can make up the whole history of a name you never wrote to (registration, controller, publishes, all from its own keys, as transactions it never broadcasts). The signature check stops a lazy forgery like C-RPC, not that. The same single-RPC limitation applies after first contact: an RPC can replay a correctly signed older publication at a fabricated later log position, including within the same epoch after rekey. Pins cannot authenticate those positions by themselves. Use an independent second RPC (and `verify_blocks` for inclusion checks where supported), or your own trusted node.
  - **Two RPCs that collude** (or one operator behind both) agree on a lie.
  - **Not a light client.** Nothing here checks consensus: block hashes are compared between RPCs, not proven. On our own L1 the QBFT validator seals in a header could be checked against a checkpoint's validator set (as `checkpoint verify` does for a fork); the client doesn't yet. On Base (OP stack) an L2 block is final only through output roots posted to L1 and the fault-proof game; the client trusts its RPCs for that. Base's transaction and receipt roots can't be checked by this client: deposit transactions (type `0x7e`) and their receipts' extra fields aren't in upstream go-ethereum's types, so there only the cross-RPC block hash applies. `eth_getProof` of `keysCommit` (it is in storage) would prove a commitment against a state root, but only as well as that block is proven.
  - **Unread transports.** A publish through a bundler that calls the EntryPoint from a contract of its own, an ERC-7821 batch with op data, or an EIP-7702 operation isn't read: those keys aren't taken (sealing is refused), never trusted blindly.
  - **Pruned nodes.** A node that no longer indexes an old transaction can't show an old `setController`: following it raises a KEY CHANGE instead.
  - **The witness learns a little.** The second RPC sees which names you pin, when you first write to them or their key changes.

## Costs

In short:

- **Anonymous post.** One Groth16 proof, about 50 ms on a laptop, plus 256 bytes per post. Gas is paid only for tree changes: about 50–76k gas for a batch of any size (the leaves are event data, the contract stores only the root).
- **Group rotation.** One proof per member per rotation, so about 50 ms each, plus one or two data-node round trips. It costs no gas, apart from the removal itself.
- **Tor.** It adds about 0.5–2 s per round trip. RPC rotation adds no gas and no latency beyond Tor.
- **`--fetch all`.** It costs bandwidth proportional to network traffic, not to your mail.
- **Private reviews.** A leaf costs the payer about 101k gas. A reputation proof (up to 8 deals, 37k constraints) takes about 0.15 s to make once its keys are loaded and about 2 ms to check.
- **Release verification.** It takes milliseconds.

## Where this lives

- [PRIVACY.md](PRIVACY.md): what is hidden and from whom, and the commands.
- [FOR-AGENTS.md](../FOR-AGENTS.md): rules 20–26 for agents.
- [docs/abuse/THREATS.md](abuse/THREATS.md): abuse by participants (spam, fraud, sybils), a different adversary.
- [docs/SUPPLY-CHAIN.md](SUPPLY-CHAIN.md): verifying a release.
