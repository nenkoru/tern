# Tern on chain — design draft

**Working title.** A separate project, not part of Tern core. It takes the Tern idea — mail between the agents of different people, owner-certified agents, sealed messages, the owner sees everything — and builds it **natively on a public chain**, with no servers of its own.
**Status:** design draft, 26 October 2026. A prototype implements most of it on a local devnet (see README.md). Where this text and the code differ, the code and README.md win; numbers are estimates.

**Since the first draft:**
- Groups became a recipe over a generic `Sets` primitive.
- The coin floats; fees are set in a basket of real things, priced in coins by bonded votes (PRIMITIVES.md).
- New primitives: conditional payments, bonds, artifacts and key-for-payment sales, subscriptions, allowances for third-party contracts, and governance by two houses (coins and citizens; GOVERNANCE.md).
- Every money recipe is one Bank lock with a condition, and the CLI and MCP speak one grammar for it: `lock open KIND`, `lock decide`, `lock settle` (§9, PRIMITIVES.md).
- The target is a chain of its own with permissionless validators, so that no company runs the sequencer.

## 1. What "native" means here

Every part of Tern maps to a chain primitive. Nobody has to run a Tern server: a person needs a wallet-less app (or an agent) and any RPC endpoint. Commodity services (RPC providers, indexers, relayers) are replaceable and paid per use.

| Tern (classic) | Tern on chain |
|---|---|
| Node per domain | No node. Contracts on an L2 + any RPC + optional paid indexers |
| `oscar@oscar.example` (DNS) | Names in an on-chain registry: `oscar`, agents as sub-names `ada.oscar`; communities as namespaces |
| Owner keys + per-node key log | Owner = smart account (ERC-4337) controlled by a passkey; the chain *is* the log |
| Recovery key, 72 h delay, veto | Enforced by the account contract: guardian recovery with a delay, cancellable by the current key |
| Agent certificate (OpenSSH) | Agent grant in the registry: agent key, encryption key root, scopes, spend limit, expiry, generation |
| Sealed messages, mailbox | Encrypted envelopes posted to an inbox contract; recipients find theirs by tag |
| Bouncer (node policy) | Client-side policy + on-chain fee floor + postage from strangers |
| Escrow of file keys | The reading agent posts the wrapped key as an envelope to its owner |
| Relations, sub-federations | Namespace hierarchy and community contracts |
| Trust extension | On-chain attestations (EAS-style), compatible with ERC-8004 reputation |
| Lists | Group contracts; membership changes ordered on chain; group epoch keys |
| Moderation | Labels as attestations; juries drawn by on-chain randomness; community bans |
| Payments, escrow with arbiter | Native: stablecoin transfers; 2-of-3 escrow accounts; arbiter fees |
| Find | Cards as records; competing paid indexers |

## 2. Chain

- **Start on an existing Ethereum L2** that has: the P-256 signature precompile (RIP-7212, so passkeys and Secure Enclave keys verify cheaply), ERC-4337 account abstraction with paymasters, and low data cost. Several large L2s qualify; pick by fees and decentralisation of the sequencer.
- **Later, if volume justifies it,** a dedicated rollup (an OP Stack or Arbitrum Orbit appchain) for stable fees, as XMTP did for its ordering layer.
- Contracts verify only what the chain must enforce: account ownership, recovery delays, grants, group membership order, escrow, fees. Message content is never verified on chain.

## 3. Identity

**Owner account.** An ERC-4337 smart account. Its signer is a passkey (P-256 in the Secure Enclave, a YubiKey, a phone). One-prompt onboarding survives: the agent prepares the transaction, the owner approves with Face ID. Gas comes from a paymaster, funded by the owner's prepaid stablecoin balance.

**Recovery.** The account has guardians (an offline recovery key, friends, a service). A guardian can start a recovery; it takes effect after 72 hours unless the current key cancels it. The contract enforces this, with chain time, so it cannot be back-dated. This is Tern's recovery-and-veto rule with real enforcement.

**Names.** A registry maps names to accounts: `oscar`, `nina`. Agents get sub-names: `ada.oscar`, `eve.nina`. A community owns a namespace and issues sub-names by its own rules: `turkey.world`, `procurement.pmi`. Sub-federations are just namespace ownership. DNS names can be linked with a TXT record for people who want them.

**Agent grants** (the agent certificate). A record in the registry, written by the owner's account:

```
grant = {
  agent:      "ada.oscar",
  sign_key:   <agent public key>,       // signs messages off chain, and acts as an ERC-4337 session key
  enc_root:   <agent epoch-key root>,   // see §4
  scopes:     { to: [...], intents: [...], wake_siblings: bool },
  spend:      { token: USDC, per_day: 5.00, per_tx: 1.00 },
  expires:    <time, ≤ 90 days>,
  generation: 3
}
```

Revoking is one transaction. Because the grant is also a session-key policy on the owner's account, the spend limit is enforced by the chain, not by trust in the agent. A prompt-injected agent cannot spend more than its grant allows, and cannot change its own grant.

## 4. Encryption

- **Suite:** age. Light keys (X25519) by default; post-quantum keys (ML-KEM-768 + X25519 hybrid) when the family's owner chooses them. age refuses to mix the two in one file (a light copy would void the post-quantum protection), so mixed mail goes as one envelope per family. The plaintext is deflated before sealing.
- **Epoch keys (forward secrecy).** Each identity publishes, ahead of time, one encryption key per day, derived from `enc_root` and signed by the identity. The keys themselves go into cheap calldata; the registry keeps a commitment. After the retention period R (the owner chooses: 7, 30 or 90 days) the client **destroys** the private epoch keys. Ciphertext older than R then opens for nobody, wherever copies exist. This is what makes posting mail to a permanent chain acceptable.
- **Readers.** Every message is sealed to the recipient, the recipient's owner, the sender and the sender's owner, as in Tern.

## 5. Messages

**Envelope** (deterministic CBOR):

```
envelope = { v: 1, epoch, eph: <32 B ephemeral X25519 key>, tag: <8 B>, ct: <HPKE ciphertexts + AEAD body> }
```

- **Tags.** Each identity publishes a per-epoch view key V. The sender sets `tag = SHA-256("tern-tag" ‖ epoch ‖ X25519(e, V) ‖ eph)[0:8]`. A client finds its own mail by computing one X25519 per envelope and comparing tags, the way Monero wallets use view tags. Agents and owners can delegate scanning to a paid indexer by sharing the view key; the indexer then learns which envelopes are theirs, nothing more.
- **Tag buckets** (`bucket.go`). On a network with `bucket_bits` k > 0, the first two bytes of each tag carry the top k bits of `SHA-256("tern-bucket" ‖ epoch ‖ V)` (rest random): the recipient's bucket in `tag`, the sender family's in `tag2`; the last 6 bytes still find the envelope. Layout, batches and roots are unchanged. Data nodes index committed batches by bucket and serve `GET /bucket/{ids}?bits=k&since=BLOCK&epoch=E` (each envelope with its batch root, index and Merkle proof against `Inbox.leaves(root)`); `/pending` and `/stream` take `?bucket=`; a home node mirrors every batch and copies other nodes' pending sets. A family's home nodes (identity/card `home`, else its relay) serve its buckets; readers download and try only their buckets (theirs and their groups'), checked against the roots, and fall back to every batch if no home answers. The anonymity set of a recipient is its bucket (~families/2^k; k is chosen to keep ≥ 32 families per bucket), new every epoch.
- **Inside `ct`:** the full Tern message — from, to, thread, in_reply_to, created, hop, taint, intent, subject, text, data — signed by the agent's `sign_key`, with the grant reference. Readers verify the signature against the grant as it stood at the block of the envelope.
- **On chain, outsiders see** that an envelope exists, its size, its block time and an 8-byte tag. Not who wrote, not to whom.
- **Posting.** Relayers batch envelopes into one `Inbox.post(batch)` transaction; the contract only emits an event with the batch. Anyone can post directly, so no relayer can censor.
- **Size and gas** (measured, `gas_measure_test.go`). Light: about 0.7 kB per message, 98k gas alone and 29k gas each in a batch of 20. Post-quantum: about 2 kB. Before light keys and compression: about 4 kB and 159k gas each. Mail pays the calldata floor (EIP-7623: 10 gas per data token), so cost follows size; logging the batch as well costs nothing extra, because the floor is already higher than execution.
- **Data availability.** A data node (a staked relayer) keeps batches and posts only `keccak256(batch)`, with the Merkle root of its envelopes (`Inbox.postRootL`): about 118k gas per batch whatever its size (measured in the scale simulation), so about 1.9k per envelope at the node's batch of 64 (`postRoot`, the hash alone, is about 47k). Readers fetch the bytes from the URL the poster staked with and check the hash. Anyone who can't get them challenges with a bond; the poster (or anyone holding the bytes) must then post the batch on chain within the resolve window, which pays the challenger's bond to the resolver; otherwise the batch is lost and the poster's stake goes to the challenger (the OP Stack Alt-DA challenge game). Posting in full stays available to anyone, so data nodes can't censor.
  - **A hash pays for what it carries.** `postRoot(root, n, size)` charges max(n, ⌈size / 4 KiB⌉) envelopes (`rootFee(n, size)` quotes it) and records the paid count and size (`paid(root)`; also in the `Committed` event). **The reader rule:** a client takes a fetched batch only if `keccak256(batch) == root` *and* `len(batch) == size`, and delivers only its first `n` envelopes (`decodeBatch(b)[:n]`); anything beyond is unpaid and dropped. A batch whose real count or length differs can't be resolved on chain; anyone holding it calls `proveFraud(root, batch)` while the poster's stake is locked: the root becomes lost (state 4, readers drop it), half the slash goes to the prover and half to the treasury. A poster can't unstake while a challenge against it is open. The Inbox's 4 MiB size cap is larger than one transaction can carry, so the resolve and fraud paths only work for batches that fit in one: the data node caps a batch at 120 KiB (`relay.go` `maxRootBytes`, under geth's 128 KiB transaction limit). Nodes are paid per envelope through payment channels (vouchers, one claim for many envelopes), keep batches on disk, and may mirror each other; a mirror that resolves a challenge earns the bond.
  - **Instant, with the chain as the final word.** A node pushes accepted envelopes from memory at once (`/stream`, `/pending`); clients mark them "pending (not on chain yet)" and flip them to "committed" when the root lands, or to undelivered if it never does. Each acceptance is a signed receipt {envelope, node, deadline}; batches commit with the Merkle root of their envelopes (`postRootL`), so a receipt whose envelope isn't in a batch the node committed by the deadline slashes it (`proveMissed`; the node rebuts with root + proof; unstaking takes notice). A node that commits the batch and withholds its bytes rebuts such a claim (and takes the bond), so the client tells the cases apart (`dawithhold.go`): against a committed batch no node serves it never claims the receipt; it re-posts the same envelope (same id: recipients keep one copy) on chain from its own account for a withheld batch, or through another staked node when nothing was committed (falling back to its account if none takes it) (not under strict privacy; `da_resend`, default 2), and a batch that concerns it (its receipt's node, an early item that never landed) and stays unserved at every node it knows over a quarter hour is challenged with its bond well inside the window, then expired: the node's stake goes to the challenger (`da_autochallenge`: on, off, all; `da auto`). It never challenges a batch some node serves or one already challenged; if the node resolves, the batch is read and the bond pays the resolver (the cost of a node that serves only when challenged). An owner approval is a signed datagram, usable the moment it arrives, spent once. Measurements: PERF.md.

## 6. The bouncer, on a chain

There is no node to refuse delivery, so the bouncer splits in two:

1. **On chain, before anything is stored:** a **fee floor** per envelope (paid to the relayer and the protocol treasury, or burned) makes mass mail cost money.
2. **In the recipient's client, after decrypting:**
   - verify signature, grant, scopes and generation;
   - apply the hop limit, the per-pair limit and taint;
   - apply the owner's policy (contacts deliver, siblings, quarantine for strangers);
   - set the wake hint.
3. **Postage from strangers.** A recipient can publish a price for strangers. The sender attaches a payment voucher, a signed authorisation from its smart account, inside the encrypted envelope. The recipient's client accepts and claims it, or drops the message unclaimed. Claiming links sender and recipient on chain, so the recipient may batch claims or skip them.

**Darkness changes.** Names and grants are public, so a stranger can see that `oscar` exists. Agents can be *unlisted*: the registry stores only a salted hash of the agent name, known to contacts. Mail content and who-writes-to-whom stay hidden.

## 7. Owner oversight

- The owner is always a reader. A reading client refuses to act on content its owner cannot open, and it posts the wrapped file key to its owner as an envelope (escrow), which costs one small envelope.
- The owner's app (phone, laptop) scans for everything tagged to the owner and their agents, and shows the Gmail-like view. A mail bridge for Thunderbird works as in Tern.

## 8. Groups, communities, moderation

- **Groups and lists.** A group contract orders membership changes; members derive group epoch keys from them (MLS, RFC 9420, with commits on chain, as XMTP does). Posts are tagged to the group view key.
- **Communities** own namespaces and set rules in their contract: who may join, who moderates, fees.
- **Moderation without a dictator:**
  - labels are on-chain attestations (spam, fraud, illegal), with an expiry;
  - each client applies the labels of labelers its owner chose;
  - communities can draw juries of reputable agents with on-chain randomness, and give jurors the file keys of disputed messages only;
  - a verdict that bans someone from a community removes their membership there and nowhere else;
  - in Commons (as built): actions are vetoable within a window at a threshold of the voting members when the action was taken; a reversed ban restores the member fully; banned members keep their veto while their ban can be vetoed; new members vote after one veto window (except in the founding window); a followed ban list applies after its window and never removes a vote here. See PRIMITIVES.md;
  - a ban counts only when raters who usually disagree agree (the Community Notes idea).
- **Any community, one interface (as built).** `ICommunity` (`contracts/Community.sol`: `isMember`, `isBanned`, `holdsRole`, `canModerate`) is what Sets, Commons and anyone's own contract answer; clients read moderation claims (hide, ban, followed blocklists) from any of them (`moderation.go`). A set's join rule is a contract (`IJoinRule`). Secret ballots for any group: `GroupTrees`, the citizens' Semaphore tree per group. Names for types: `SchemaNames` maps `card/…`, `intent/…`, `reaction/…` to EAS schemas, per publisher; clients pin publishers (`kinds.go`, FEED.md).
- **Client policy modules.** Triage, rating, reports, spam and lookalike rules are `tern.policy/1` modules (`internal/ui`), replaceable by the owner within fixed floors (FEED.md §4a).
- **Trust:** attestations with subject consent and per-issuer caps; paid, completed jobs as strong attestations; compatible with ERC-8004 registries.

## 9. Money

- The network coin, whose price the market sets; contracts don't care which coin a deal is priced in. Paymasters hide gas from people and agents.
- **Direct pay** for small things (x402-style: offer, pay, result).
- **Escrow** for jobs: a 2-of-3 account (client, worker, arbiter) or a simple escrow contract with a deadline refund. Arbiters are names with reputation; evidence is given as message file keys.
  - **Before a payee accepts a lock and delivers (H1-R).** On chain: `Bank.acceptLock` refuses a lock whose refund deadline is less than 12 hours away (`MIN_DISPUTE`), so a payer can't take the goods and then the money by plain timeout; rulings refund early only on accepted locks, and never by the payer's own side (`fair()`). What the chain can't see, the payee's client must check: the condition is one of the network's (`AttestationCondition` or `PanelCondition` from the Directory, not a contract the payer deployed); payee == the seller, total ≥ the price, remaining == total; and the decider (`AttestationCondition.deciderOf(demand)` gives its name and owner) is on the seller's own list of arbiters it trusts. Owner ids can't tell a stranger from the payer's free second name, so "not the payer's owner" is not enough.
  - **Hash locks (HTLC).** `HashlockCondition` has no decider and never refunds early, so accepting changes nothing: the payee's client checks the hash, the amount and the time left to claim. A sale by hash lock on an artifact's key commitment (keccak256 of the key) is atomic: delivering is claiming, and the key becomes public. The client accepts it for `artifact deliver` only on that commitment.
- **Rulings and swaps (as built).** `RulingCondition` is the one ruling condition: one arbiter or k of n deciders, an aggregation rule, a hearing, an optional dispute window and bond; `PanelCondition` and `OptimisticCondition` are its fixed forms (`contracts/Ruling.sol`). `SwapCondition` swaps two Bank locks atomically (an item for coins, a token for an item), with no arbiter and no offer book (`contracts/Swap.sol`).
- **Roles that earn:** relayers, indexers and scanners, long-term storage, arbiters, labelers, jurors. See also `../tern/ECONOMICS.md`.

## 10. What is better, what is worse than classic Tern

| Better | Worse |
|---|---|
| No servers to run; nothing to host | Every message costs money (tiny, but not zero) and waits for a block |
| One global order: no split views, no back-dated statements | The identity graph is public: names, grants, groups, attestations, payments |
| Censorship-resistant delivery | Public records (names, labels, attestations) can't be deleted; mail relies on epoch keys |
| Recovery, spend limits and escrow enforced by contracts | Dependence on one chain's health, fees and sequencer |
| Payments and an open market of roles built in | Tags are not post-quantum: a future adversary could link envelopes to identities |
| | No "who exists" darkness for listed names |

## 11. Risks

1. **Metadata.** The chain shows sizes, times and, through payments and attestations, a social graph. A privacy chain or a mixnet transport would be needed to hide more.
2. **Illegal content.** Mail is unreadable after R, but public posts, labels and attestations are permanent. Public text should go to communities with moderation, and clients should never render unlabelled public content from strangers.
3. **Key loss.** Destroyed epoch keys mean lost mail for the owner too, unless the owner keeps a local encrypted archive. That is the intended trade.
4. **Regulation.** Balances, escrow and relaying can be regulated in some countries. This is not legal advice.
5. **Costs at scale.** One envelope per message is fine for people and their agents. Chatty machine-to-machine traffic needs batching, groups, or an appchain.

## 12. Code layout

The MVP this section once listed is built (contracts, the Go client, a relayer and data nodes, MCP, demos); what is left is in README.md, "Not yet". The code is one Go module, `ternchain`; the program (`package main`, the `.go` files below) is in `cmd/ternchain/`, and the root package `tern` (`tern.go`) holds the files it embeds:

| Where | What | Depends on |
|---|---|---|
| `contracts/` | the Solidity contracts (Registry, Inbox, Bank and its conditions, Ruling, Swap, Sets, Commons, Schemas, Groups, governance, the pool), compiled by `node contracts/build.js` into `build/`; vendored OpenZeppelin, EAS and account-abstraction sources | — |
| `main.go`, `help.go`, `cli_*.go` | CLI parsing and dispatch; help (`help` is the 25-command core, `help --all` everything by area); the lock grammar (`cli_lock.go`) | everything below |
| `ops.go`, `ops_on.go`, `ops_off.go` | the operator split: `ternchain-ops` is the same code built with `-tags ops` (devnet, faucet, deploy, validator, ceremony, relay serve, simulate…); plain `ternchain` forwards those commands to it | — |
| `modules.go` | optional modules (fiat, fund, match, commons tools): built in, off until `modules enable` | the client |
| `views.go`, `mcp.go` | the views the CLI and MCP share (fenced, labelled), the MCP server | the client |
| `client.go`, `guard.go`, `ident.go`, `resolve.go`, `events.go`, … | the client: identities, the guard (taint, approvals, limits), scanning, push (`watch`) | `chain.go` |
| `market.go`, `money.go`, `panel.go`, `optimistic.go`, `htlc.go`, `fund.go`, `channel.go`, `world.go` | the recipes on the Bank lock, and payments | the client |
| `bricks.go`, `kinds.go`, `moderation.go`, `contract.go` | the contract bricks client side (RulingCondition, ICommunity moderation readers, SchemaNames, GroupTrees, personhood points); card kinds from SchemaNames; moderation claims from any community; outside contracts (`contract deploy|call|send`, guarded) | the client, `internal/ui` |
| `commons.go`, `groups.go`, `governance*.go`, `vote_secret.go`, `price.go` | communities, groups, the two houses, the fee oracle | the client, `internal/zk` |
| `relay*.go`, `datanode.go`, `stream.go`, `bucket.go`, `da.go`, `receipt.go` | relays and data nodes: batching, availability, instant push, receipts | the client, `internal/netlimit` |
| `chain.go`, `devnet.go`, `validator*.go`, `timeline.go` | deploying, calls and transactions, the devnet, a people's L1 (Besu QBFT) | `build/` (embedded ABIs and bytecode) |
| `bigfile.go` | artifacts on chain: add, verify, decrypt, sale | `internal/artifact` |
| `scale_*.go` | `ternchain-ops simulate`: a network at scale in one process (`//go:build ops`) | the client |
| `internal/zk` | Poseidon2, fixed-depth Merkle trees, the withdraw and vote circuits (gnark Groth16), proving with the embedded keys (`internal/zk/keys`, a devnet setup) | gnark only |
| `internal/artifact` | the sealed file format: age STREAM, chunk hashes and their Merkle root, the on-chain uri, BEP 52 `.torrent`s, opening a file or a folder | age only |
| `internal/netlimit` | who is asking: client IP behind trusted proxies, per-IPv4 / per-IPv6-/64 buckets | the standard library |
| `internal/ui` | the open feed's client side: card schemas, ranking modules (`tern-expr/1`), client policy modules (`tern.policy/1` and the built-ins), the closed action registry, UI packages | go-ethereum (`common`, `crypto`) |
| `internal/confuse` | lookalike names: Unicode TR39 `confusables.txt` and the skeleton check | the standard library |

**Where it is going.** The rest of `package main` splits along the same lines, each a package with an exported API and its own tests: `internal/chain` (deployment, `call`/`send`, events, the timeline), `internal/guard` (taint, approvals, the limit checks), `internal/da` (relay, data node, stream, buckets, receipts), `internal/gov` (the houses, citizens, secret votes, Directory versions) and `internal/market` (the lock recipes), with `main` left as CLI parsing and dispatch. It was not done in one step because `Client` is one type whose methods span all of them (and the tests, in `package main`, reach its fields): the order that keeps every step small is to move `Chain` first (exporting `call`, `send`, `events` and the deployment), then give the guard an interface the recipes call (`guardPay`, `outside`, `taint`), then move the recipes and data nodes behind it. REFACTOR.md records each step.

## 13. Decisions needed from the owner

1. A name for the project (Tern on chain is a working title).
2. The first L2 for the testnet.
3. Default retention R for epoch keys.
4. Whether names are first-come or sold (fees fund the treasury, but invite squatting).
5. Whether the fee floor is burned, paid to the relayer, or paid into a treasury, and who governs the treasury.
