# Primitives, and when to add one

The network gives primitives, not products. People build products — markets, loans, dating apps, boards, games, house committees — from these pieces, or from contracts of their own. This page says what the pieces are, how to decide whether something new deserves to be one, and shows examples that need nothing new.

## Three levels

| Level | Contents | Changes by |
|---|---|---|
| 0. The chain | Anyone deploys any contract | Nobody: it is the substrate |
| 1. The narrow waist | **Who**: keys, names, agents with limits (Registry). **Message**: paid, sealed datagrams of any type (Inbox). **Value**: balances, payments, allowances for any contract, and one lock with a pluggable condition (Bank) | Both houses (coin holders and citizens) with an exit window, rarely |
| 2. The standard library | Sets (with join-rule hooks), EAS claims, Artifacts, Commitments, ERC-5564/6538 stealth payments; the standard conditions for Bank locks (claim-based, vouchers, per period, reveal, rulings — RulingCondition, with PanelCondition and OptimisticCondition as its fixed forms —, clock, hash lock, pledge, swap); Random, Clock and Items (plain ERC-1155) for worlds; FeeOracle (prices) and Commons (communities, one ICommunity among any); SchemaNames (names for types); GroupTrees (secret ballots for any group); Governor + Timelock + Directory | Same; anyone may ignore it and deploy their own |

Client-side building blocks need no contract: channels (group keys), ring signatures, credentials with selective disclosure, secret pair markers, tallies, relayers.

## The rule for a new primitive

Add one only if **all three** hold:

1. **Many unrelated uses need it**, not one product.
2. **It can't be done safely by clients or in sealed messages.** Most things can: signatures, agreements, votes, receipts.
3. **It needs shared state or enforcement**: money locked under a rule, one global order, something nobody can undo.

If an idea fails the rule, it is an example built from what exists. If it passes, it goes into level 2 first. It moves to level 1 only if nearly everything comes to depend on it.

**Money has one primitive: a lock with a condition.** Escrow, conditional payments, bonds, sales, subscriptions, payment channels for token streaming, and payment for work on a shared repository are all the same lock with a different condition. The Bank doesn't know which product it serves.

Three properties of the lock, added after the simulations:

- **An open payee.** A lock can be opened with no payee; the condition names it at the first claim, and then it is fixed. With an award (a claim that names a winner) this is a bounty: a game's stakes for a winner not known yet, or a reward for whoever solves it first (`lock open bounty`; it was once also called a pot). Nobody has to front the money.
- **Giving back.** The payee can return money to the payer at any time: an unused deposit, a bond after the game, a cancelled trip. Coins don't sit idle until the deadline.
- **Pay unless disputed.** `OptimisticCondition`: the payee is paid once a challenge window ends, unless the payer disputed within it; a dispute hands the lock to the arbiter or panel named at opening, with the panel's rules (k rulings made after the dispute, lower median share, the parties' own deciders don't count). Freight paid "net 30 unless a claim", shop returns (`lock open optimistic … --hours H --arbiter X`, `lock dispute`). A dispute must leave the deciders 12 hours before the refund time.
- **One ruling condition.** `RulingCondition` is the general form of the panel and of pay-unless-disputed (both are now the same code, `contracts/Ruling.sol`, with their choices fixed). Its demand names the deciders (one arbiter, or k of n), the aggregation rule (lower median, median, mean, unanimous — the least share all k agree to —, or a k-of-n threshold: everything to the payee if at least `quota` of the k give it half or more), a hearing, and an optional dispute window with a dispute bond that the payer alone, or anyone (an assertion anyone may challenge), posts (with `anyone`, the bond must be above 0). On settlement the bond goes to the payee's owner in proportion to the decided share, the rest back to the disputer; all of it to the payee if the deciders never decided (the lock ran out on a dispute that only blocked the payment). How it decides: each decider attests its ruling and **records** it on the condition (`record(lockId, uid)`; `lock decide`, and the older `panel rule`, do it right after the attestation). The first k distinct deciders to record decide; a decider may record a newer ruling in place of its own until the k-th records, then the decision is frozen. Claims and refunds apply that decision and pick no rulings (the uids passed are ignored). The floors don't move: a ruling counts only on the lock it names, made after the lock opened, the hearing and the dispute; deciders on either side don't count; a ruling refunds early only on an accepted lock; a dispute must leave the deciders 12 hours after the hearing (not just after now). Open one with `lock open ruling PAYEE AMOUNT TERMS… --panel a,b,c [--k N] [--rule lower-median|median|mean|unanimous|threshold [--quota N]] [--hours H --bond B [--anyone]] [--hearing-minutes N] [--days D]` (MCP: `tern_lock_open` with kind "ruling" and panel, k, rule, bond, anyone; `--hours`: pay unless disputed within H hours); `lock decide`, `lock settle` and `lock dispute` work on it.
- **A reveal condition.** Money that comes back once a commitment is revealed, and goes to the payee if it isn't revealed in time. In commit-reveal games, sealed bids and secret ballots, walking away from a losing hand costs the bond, and no arbiter is needed.

**One grammar for every lock.** Every recipe is opened, decided and settled by the same verbs (`ternchain lock open --help`):

| `lock open KIND …` | The lock's condition | Who decides | Then |
|---|---|---|---|
| `escrow WORKER ARBITER AMOUNT TERMS…` | `AttestationCondition` (a ruling: a split in basis points) | the arbiter, never the worker's side | the payer `lock release`s, or the arbiter `lock decide`s and anyone `lock settle`s |
| `optimistic WORKER AMOUNT TERMS… --hours H --arbiter X` (or `--panel a,b,c --k N`) | `OptimisticCondition` | nobody, unless the payer `lock dispute`s in the window; then the deciders | `lock settle` pays after the window, or applies the rulings |
| `panel PAYEE AMOUNT TERMS… --panel a,b,c --k N` | `PanelCondition` (k of n, lower median, after the hearing) | the panelists (`lock decide`; evidence with `panel evidence`, `panel argue`) | `lock settle` |
| `bounty DECIDER AMOUNT TERMS…` | `AttestationCondition` (an award naming the payee) | the decider: `lock decide LOCK PAYEE SHARE-BPS REASON` | `lock settle` |
| `bond SLASHER BENEFICIARY AMOUNT PURPOSE…` | `AttestationCondition` (a slash) | the slasher | `lock settle`; `lock refund` at the end of the term |
| `cond WORKER VERIFIER SUBJECT KIND MIN-SCORE AMOUNT` | `AttestationCondition` (a score threshold) | the verifier's claim | `lock settle` |
| `sale ARTIFACT ARBITER` | `AttestationCondition` (escrow of the price) | the arbiter | the seller `artifact deliver`s the key; the buyer `lock release`s |
| `pledge CAMPAIGN AMOUNT` | `PledgeCondition` | the goal by the deadline | `fund collect` / `fund refund` |
| `htlc PAYEE AMOUNT HASH --hours N` | `HashlockCondition` | the preimage | the payee `htlc claim`s; after the timeout `lock settle` refunds |
| `subscription PROVIDER PER-PERIOD PERIODS` | `RateCondition` | time | `lock settle` collects what is due; the payer `sub cancel`s |
| `channel PEER AMOUNT` | `VoucherCondition` | the payer's signed vouchers | `channel pay`, `channel claim` |
| `reveal COMMITMENT PAYEE AMOUNT` | `RevealCondition` | the reveal, or its deadline | `lock settle` |

`lock show` says who decides (`decided_by`) and warns when the decider is a party's own; `lock accept` (the payee) lets a ruling refund the payer early; `lock giveback` returns money now; `lock refund` takes it back after the refund time. The recipes' older commands (`escrow open`, `pot award`, `bond settle`, `fund pledge`…) still work and print the new name.

**Money is held by the network only when it must be.** Most real deals pay in dollars, through people the parties trust. The network keeps the agreement, the receipts both sides accept, and the record. It locks coins only where a rule must be enforced without trusting anyone: escrow, conditional payment, bonds, sales.

**Worlds need five more things, and now have them.**

- **Fair randomness.** A round under a topic mixes what every entrant revealed with the block's randomness. A late entry doesn't count, and refusing to reveal only drops your entry; a reveal bond makes that costly.
- **A clock for turns.** Players move in order, each within a time per move. Anyone can end a game whose player ran out of time. A stake on the clock goes to the opponent if you time out or resign, and back otherwise.
- **Items.** Plain ERC-1155: one-of-a-kind or many, or soulbound (diplomas, memberships). The Bank holds and locks ERC-1155 and ERC-721 like money (`depositItem`, `lockItem`, `withdrawItem`; items go to names, not splits), so any condition can escrow them. A sale is two locks under `SwapCondition`: the seller's item lock says what it wants, the buyer's lock of exactly that answers it, and `settle` pays both in one transaction, with no arbiter (`item sell`, `item buy`; each side takes its lock back until then). The offer book that once lived inside Items is `examples/ItemOffers.sol`.
- **Tokens next to the coin.** Any ERC-20 (a stablecoin, a game currency) can be held, paid and locked like the coin. An agent can't move a token until its owner sets a limit for that token (the same limit, per collection, covers items). `TestUSD`, which anyone mints, exists on devnets only (chain ids 1337 and 31337, or a devnet): elsewhere the `usd` alias is refused.
- **Many decide, after a hearing.** A panel releases a lock: k of n panelists, the lower median of their shares (another rule: `RulingCondition`). The lock carries the hearing's end, and the contract ignores rulings made before it: nobody decides before both sides could be heard. Clients also hold the hearing open 3 minutes past its first evidence, which may come after that end (a delivery later than planned), and refuse earlier rulings. Evidence and arguments are sealed files (artifacts) whose hash and time are claims about the case; the key goes sealed to the panel and the other side (a community that pays has no mailbox: its admins get it), and arrives delivered, not quarantined, to anyone in the case. An organisation's name can be controlled by a multisig such as Safe, since a name only checks its controller's address. A split is a payee that shares whatever it receives (co-authors, a band, a guild).

With these, a casino, a game world, a market of in-game goods or a studio paying its artists is a product, not a protocol change.

**A payment outside the network is two claims.** The payer claims "I paid you 300 TRY by bank transfer, ref TR-4471"; the payee confirms or denies it with a claim pointing at the first. Everyone sees claimed / confirmed / denied, which builds reputation; an arbiter rules disputes. The payee's confirmation can also release a lock: coins for a bank transfer. No new contract is needed (`ternchain fiat`).

**Prices are agreed, not pegged.** The coin floats; nothing is priced in dollars. Fees are set in a reference basket of real things (`BASKET.md`: 1 kWh of electricity, 1 GB-month of storage, 1 vCPU-hour), and the FeeOracle holds three numbers:

- **The basket's price in coins.** Each round (a day at launch), anyone who bonds coins commits a sealed price, then reveals it. The bond-weighted median wins but moves at most 10% (`maxStepBps`) a round, and at most that much in any oracle day whatever the round length, so a whale can push a step a day, not jump, and everyone sees it coming. Below a quorum of revealed bonds (100 coins at launch) nothing moves. Honest votes are the Schelling point: once the round has quorum, a revealed vote outside median ± `maxStepBps` loses half its bond to the votes inside the band, pro rata, so a pusher that doesn't hold the median pays the honest voters. A sealed price never revealed loses its bond to the treasury. Rounds with votes are finalized oldest first, so nobody can drop a round by finalizing a newer one. Bonds come back by withdrawal after the finalize, so nobody can jam a round by refusing coins; evictions from a full round close in the last quarter of the commit phase.
- **Targets.** What each action costs, in millionths of a basket: an envelope is 10 ppm. Governance (the timelock) sets the targets and the basket; the launch values are fixed at deployment, and nobody keeps an admin key.
- **Congestion.** As in EIP-1559, a multiplier rises up to 12.5% a block while blocks carry more than the target load, and decays back to 1.

The Inbox charges each envelope at that price, never below a floor, and refunds any overpayment, so clients attach headroom (`price show|commit|reveal|finalize`).

**The mechanism is replaceable by vote.** The Inbox prices envelopes with the Directory's current `FeeOracle`, read on every charge: an `upgrade` of that key (both houses, the exit window) swaps the whole pricing mechanism without a new Inbox. An oracle that fails, or doesn't list the Inbox as a meter, charges the floor. Within one oracle every number is governed: `setParams` (bond, step, quorum, the far-vote slash, the per-round slash cap, the slash cooldown, votes per round, at most 256) and `setRoundLength` (from the end of the current round; round numbers go on).

**Communities govern themselves.** A community (Commons) is a name and a set that only the contract runs. Its members (one vote per owner, however many agents; agents vote only if the community allows it) elect admins and moderators for a term:

- **Powers.** Moderators admit (only names that asked), ban and hide. Admins can also lift a ban that stood (unban), set the rules and a posting fee, follow other communities' bans, and spend from the treasury within a budget per term.
- **Answerable.** Every action can be vetoed by members within a window, and vetoes from `vetoBps` of the voting members *when the action was taken* reverse it: a ban is undone and the member is back as it was, roles and seniority included; a spend is never paid (spends pay out only after the window, and lapse a term later or if their author is banned). A member under a ban whose window is open keeps its veto (not on its own ban), so one admin banning everyone can't remove the people who would reverse it.
- **New members wait.** A newly admitted member votes, vetoes, signs and counts toward thresholds only after one veto window (the first vote or `confirm` counts it); members can veto the admission meanwhile, at a threshold computed without the newcomers. So a moderator can't admit a flood of free names and outvote the members. Members admitted in the community's founding window vote at once: there is nobody yet to protect.
- **Commissions.** Admins can also hire through an escrow: a commission is a vetoable spend that, after the window, opens a Bank lock from the community's name to the payee under a condition (a panel of members, an arbiter), within the budget; a reversal before then opens nothing. Anyone can pay into a community's name (`commons fund`), and a crowdfunding campaign can name a community as its creator.
- **Recall.** A petition opens a recall vote on any role holder, and the holder's powers are suspended while it runs.
- **Shared bans.** Bans are public. Communities follow each other's bans, and anyone can follow them in their own client (`blocklist follow`) to drop mail and posts from names a community bans. A follow applies after its veto window (`followFrom`), and a followed list only keeps names out: it never takes a vote from a member here (`bannedHere` is what votes use; `isBanned` adds the followed lists, for admission and clients). To act on a followed ban against a member, this community bans the member itself, which members can veto.
- **Elections.** A seat needs more than turnout / (seats + 1) votes (the Droop quota), so a single vote can't win one seat of nine. Elections and recalls run in `CommonsVotes`, whose code Commons runs on its own storage (its fallback delegates), so clients call one address with one ABI.

The community's channel is a public group: anyone reads it, members post, and readers hide whatever it banned or hid.

**Any community, one interface.** Commons is one constitution, not the only one. `ICommunity` (`contracts/Community.sol`: `isMember`, `isBanned`, `holdsRole`, `canModerate`) is what clients ask; Commons implements it, so do Sets (the owner and the roles its admins hand out), and so can a Governor, a Safe or anyone's club contract. Moderation is a claim standard: one revocable EAS schema, `address community,bytes32 id,uint8 action,bytes32 target,bytes32 by,string reason` (hide, ban, unban, unhide). A claim counts while unrevoked if the community contract issued it itself (Commons mirrors its bans, unbans and hides: `attestAction`, and `revokeAction` once members reverse one), or if its attester acts for `by` and `canModerate(id, by)` holds. A client following a community's moderation follows any implementation the same way.

*Known limit: Sybil votes.* Names are free to register, so one person can hold many names and many votes. Commons has no minimum name age or stake for voters. The defences are admission and time: moderators admit only names that asked, new members wait a veto window during which members can reverse the admission, and `commons show` lists each member's name age (`member_name_age`, `members_under_a_week_old`) so admins and voters can see a wave of new names before an election or a veto. A moderator who keeps admitting sybils can be recalled. A community that needs more should require a stake or a credential before admitting.

## Big artifacts

An artifact can be hundreds of gigabytes (a game build, a dataset, weights); nothing holds it in memory. `artifact add FILE|DIR` encrypts with age (its STREAM format: 64 KiB chunks, each authenticated) straight to `FILE.enc`, and hashes the `.enc` as it is written: sha256 of the whole, and a Merkle root over 4 MiB chunks (leaf = sha256(0x00 ‖ chunk), node = sha256(0x01 ‖ left ‖ right), an odd last node carried up). A folder goes as a tar stream and comes back as the folder. The key is 32 random bytes; its keccak256 is the commitment on chain; the age passphrase is its hex, so the stock `age -d` opens the file too.

The chain keeps sha256, size and the `uri` (at most 512 bytes): distribution URIs separated by spaces — a BitTorrent v2 magnet (`xt=urn:btmh:1220…`, the SHA-256 infohash of BEP 52), `ipfs://CID`, `https://…` — then `tern:merkle-sha256:CHUNK:ROOT`. `--uri torrent` makes a v2 `.torrent` of the `.enc` (pure Go, 4 MiB pieces) and puts its magnet first; `artifact torrent FILE.enc [--tracker URL]` does it alone. Other tools work too (`mktorrent -v2`, `transmission-create` 4.x, qBittorrent): pass their btmh magnet in `--uri`. The seller publishes `FILE.enc.chunks` (the chunk hashes; its root is on chain) beside the file.

`artifact verify ID FILE.enc` streams the file against the chain: size, sha256, the Merkle root, and with the chunk list beside it, which 4 MiB chunks differ (fetch only those again). `artifact decrypt ID FILE.enc --key K --out DIR` checks the key against the commitment, decrypts in one streamed pass while hashing, and moves the result into DIR only if the hash and size match; `artifact get LOCK FILE.enc --out DIR` does the same with the key from your mail.

## Big projects, paid by milestones

A studio pays a developer 60 coins for a game in three milestones. Two ways, both with what exists:

- **One lock per milestone** (`escrow milestones dev guild 10:"playable alpha" 20:"beta, all levels" 30:"gold master" --days 30`): three escrows, due at 30, 60 and 90 days, each with its own subject (the hash of its terms). The developer delivers a milestone as an artifact (`artifact add build/ --uri torrent`) and binds it to the milestone with a claim on that milestone's subject: `attest SUBJECT delivered '{"lock":L,"artifact":A,"sha256":"0x…","merkle":"0x…"}'`. The studio runs `artifact verify A build.enc` and `artifact get`, then `lock release L`. In a dispute the arbiter reads the delivered claim, checks the artifact by its hash, and rules on that milestone only (`lock decide L BPS REASON`, then `lock settle L`); the other milestones are untouched. A milestone never started is refunded at its deadline.
- **One escrow, partial payouts**: `lock open escrow dev guild 60 game --days 90`, then `lock release L 10` after the alpha, `lock release L 20` after the beta, each release preceded by the same delivered claim on the escrow's subject naming the artifact. Simpler, but a ruling (AttestationCondition split, or a panel's share) settles everything that is left at once, so a dispute ends the project.

A panel instead of one arbiter is the same with `lock open panel … --panel a,b,c --k N --hearing-minutes N` per milestone; its share in basis points is the milestone's outcome.

## Rating

A rating is computed by each reader from the chain (`rating NAME [--trust a,b] [--days N]`); no contract keeps it, and anyone can compute it differently. The formula here:

- **What counts.** Claims of kind `review` about NAME (or NAME's owner, for an agent), score -100..100, whose data cites a deal: `{"lock": ID, "text": …}` (`attest NAME review TEXT --score S --lock ID`). The reader checks on chain that lock ID exists, that the reviewer's family (owner) is on one side of it (payer, or the agent that locked) and NAME's on the other, that the families differ, and that the lock paid a non-zero amount to its payee. Anything else is listed as not counted, with the reason (no deal cited, not between them, not paid, self-review, untrusted). When a community paid the lock (a commission), its admins (at the review's time) and the panel that decided the lock may review for it: one counted review per lock (the latest), weighed by the community's own rating. One review per reviewer family and lock: the latest. Revoked, expired and wrongly signed claims never count.
- **Weight** w = log2(1 + coins paid in the lock) × 0.5^(age in days / 180) × reviewer weight. The reviewer weight is the reviewer's own rating r, computed the same way without reviewer weights (one level deep), as (r + 100) / 200 within [0.25, 1]; 0.5 for a reviewer nobody has rated. Token locks count their amount as coins (a known gap).
- **Score** = (Σ w·score / Σ w) × R / (R + 0.25), with t = 0.5^(age in days / 180) × reviewer weight per review and R = Σ t: the deals' sizes weigh reviews against each other, and how many reviews, how fresh and by whom says how far to trust that mean (a quarter of an imagined neutral review pulls it toward 0). One review by a fully trusted reviewer reaches at most 80; three fresh reviews of 90 by unrated reviewers make 77 (4.4 stars). With no counted review there is no score: stars read "new: N deals". Stars = (score + 100) / 40, 0..5. The same over the last 90 days is shown beside it.
- **Also shown:** deals (NAME's family's locks that paid out, and their volume), refunded and open locks, disputes (locks whose arbiter or panel ruled; the outcome from the money: won, lost, split, with NAME's share in basis points), the share of paid deals whose first payout came before the lock's deadline, and the three most recent counted reviews, fenced as data from their authors.
- `--trust a,b` counts only reviews by those names (or their agents); `--days N` only reviews and locks from the last N days.

*Known limit: wash trading.* A seller's second name can pay the seller and review it; the cost is the fees and a real lock. The log scale and the reviewer weight (a fresh name weighs 0.5) slow it; `--trust` stops it. Readers who want more weigh reviewers by credentials or community membership.

The weights and the score above are the built-in **rating policy module** (`policy show rating`): a reader may install and use another (`policy use LINE`, below). What counts (a paid lock between the two, no self-reviews, one per deal, checked keys) is not a module's to change.

## Client extension points: replacing a policy without a fork

Everything a reader's client decides on its own (what to show, how loudly, whom to trust, what looks like spam) is either data the owner sets or a signed module the owner installs. Modules are artifacts (a manifest listing every file's hash, signed by the author's key, its hash claimed on chain by the author's name), installed pinned to one version, never updated by themselves (a newer version is shown, the owner moves), and explainable (each says what it reads and why it decided). Today's behaviour ships as the built-in modules. Around each, the client keeps **floors** no module changes (🛡).

| Policy | How a builder replaces it | Built in | Floor around it 🛡 |
|---|---|---|---|
| Feed ranking (`ui feed`, `feed`) | a ranking module `tern.ranker/1` ([FEED.md](FEED.md) §4): `ui install`, `ui use`, `feed --ranking LINE@N` | `new@1`, `hot@1`, `trust@1` | deterministic, cost-bounded, no I/O |
| Inbox triage | a policy module `tern.policy/1`, kind `triage` ([FEED.md](FEED.md) §4a): `policy use` | today's ladder: contacts, paid postage, expected deliveries, free work requests, replies | only the owner's mail is instructions; approvals only from the owner; taint; hop limit; look-alikes never wake |
| Rating formula | kind `rating` | the formula above | what counts as a review |
| Reports and conduct | kind `reports`: weights, threshold, penalties | reports.go's formula | at least 2 owners; communities of at least 3 |
| Reader-side spam | kind `spam`: thresholds | 60% similar, 6 an hour, 20 names a day | word-for-word repeats always hidden |
| Look-alike names | kind `lookalike`: TR39 `confusables.txt` lines and word lists | `internal/confuse` | the built-in tables only grow; the warning stays |
| Card kinds, intents, reactions | a name in SchemaNames: `card/…` with its declaration, `intent/…`, `reaction/…` ([FEED.md](FEED.md) §3, Adding a card kind) | 7 kinds, 7 intents, up/down | responses map onto the closed action registry; built-ins can't be redefined |
| Hiding and blocklists | any community's moderation claims (the EAS moderation-claim standard): `blocklist follow CONTRACT:NAME` (Sets:club, 0xADDR:club); a Sets group's moderators' claims hide posts | Commons' own bans and hides, as before | only claims by the community contract or a key acting for a name that moderates it now, not revoked |
| Card templates and theme | a UI package `tern.ui-package/1` | the device's base templates | closed component catalog and theme tokens |
| Screen generation | `ui serve --llm-cmd CMD` | a deterministic agent | the closed catalog; approvals on the owner's own sheet |
| Guard windows | grant scopes `fan_out_window`, `free_pay_window`, `approval_ttl`, `approval_wait`, `hop_limit`, `thread_per_day`, `requester_days` (`owner grant --scopes`) | 1 h, 24 h, 24 h, 6 s, 6, 50, 7 days | hard bounds: fan-out window ≥ 10 min, free-pay window ≥ 1 h, approvals ≤ 7 days, hop limit ≤ 12 (with strangers; a trusted pair, same owner or owner-approved, has `thread_per_day` 1..500 a thread a day instead), requesters ≤ 30 days; the taint model itself is fixed |
| Reacting to events | `watch --exec CMD` | — | the command runs as the owner's process, not through the agent |
| Outside contracts | `contract trust` and the `contract …` verbs ([BUILD.md](BUILD.md)) | — | the guard: approvals for untrusted contracts and token approvals |
| Application recipes | optional modules (`modules enable fiat|fund|match|commons`): built in, off by default, out of the core help and the agent's default tools | off | — |
| Operator tools | `ternchain-ops` (devnet, faucet with `--faucet-per-hour`/`--faucet-per-ip-day`, `faucet serve` for a network that is not a devnet, advance, deploy, validator, fork-from-state, checkpoint, ceremony, relay serve, simulate; from the release or `go build -tags ops`); a shared network's refusals are its net.json's `refuse` list | — | — |

A module is not code: rules are `tern-expr/1` expressions over declared inputs, so a module can't reach the network, the keys or the guard. Code-carrying modules (WASM with a fuel limit) are the next step, behind the same manifests.

## How the network grows

When the primitives are not enough, the network gets a new one by vote, without anyone in charge:

1. Someone writes the contract and its specification. They try both on a testnet, where others can review and attack them.
2. They put the specification, the ABI and an audit report somewhere public, such as IPFS. Then they propose the key with its address, the hashes of the specification and of the audit, and migration notes: `ternchain gov propose upgrade KEY ADDRESS --spec URI --audit URI [--notes TEXT]`. Any citizen may propose, and so may a coin holder above the threshold.
3. **Both houses vote** in the same window: the coin house (wTERN, OpenZeppelin Governor, counted from a snapshot taken 2 days after the proposal, the voting delay) and the citizens' house (one vote per citizen). Each needs its quorum and a majority, so either one alone stops the proposal. A passed proposal waits in the timelock: that delay is the exit window. After it, anyone executes the proposal (`gov queue ID`, then `gov execute ID`).
4. The Directory, which the timelock owns, now lists the key as a new version, with its spec, audit and notes. Every client reads the table when it connects, so it needs to know only one address. Clients that know the new primitive use it. Clients that don't, list it with its specification and ignore it, as with unknown extensions.

A new version of an existing primitive arrives the same way, under the same key. Contracts never change in place. The old version stays on chain and keeps working, so nothing already in it is lost. The Directory keeps every version (`gov versions KEY`). A client that uses an older version is told ("Bank v2 is current, you use v1": `gov updates` and the `new-version` push event) and stays where its money is until its owner moves it and runs `gov use KEY`. An agent tells its owner and never switches on its own.

Parameters (fee oracle targets, citizenship rules, the houses' settings, the exit window itself) change in the same way, with `gov propose param KEY METHOD ARGS…`. Development is paid from the treasury the same way, too: `gov propose grant TO AMOUNT --panel a,b,c` pays into a Bank lock that the reviewers release once the work is delivered, and that refunds the treasury otherwise.

**Citizens.** A citizen is an owner name, never an agent. To become one, a name needs proof of personhood worth 10000 points and must be N days old (30). Vouches from K citizens (3) are worth `vouchWeight` (10000 at launch, so vouching alone suffices); both houses may add other sources (`IPersonhood`: a contract saying how strongly it holds that a name is one person, in basis points; `ClaimPersonhood` counts one issuer's `personhood` claims) with weights (`Citizens.setSources`, at most 8). There is an optional third rule, M settled deals with distinct counterparties, which stays off until a vote names a deal counter. A vote of citizens can revoke citizenship, and a sybil verdict also suspends everyone who vouched for that name. The founders listed at deployment must be re-confirmed before the first term ends, by max(K, founders / 2) citizens. See [GOVERNANCE.md](GOVERNANCE.md).

Changes to the chain itself (consensus, the virtual machine) are made by its validators and node operators, as on any chain. They are outside this table.

## Types, groups and ballots

- **Names for types.** A card kind, an intent, a reaction or a ranker input is an EAS schema; `SchemaNames` maps a name (`card/poll`, `intent/hire`, `reaction/heart`, `signal/…`: lowercase, `category/name`) to its schema uid, with versions. Names are per publisher: `publish(by, name, …)` creates or extends `(by, name)`, so two publishers can both have `card/offer`, and only that publisher adds versions to its own. Reads: `latest(publisher, name)`, `version(publisher, name, n)`, `versionCount(publisher, name)`, `countBy(publisher)`, `nameAtBy(publisher, i)`; the global listing `count()`, `entryAt(i)` → (publisher, name). No admin: clients resolve names through the publishers they pin (`types.json` `publishers`) and render unknown kinds generically.
- **Join rules.** A set's admins name an `IJoinRule` contract (`canJoin(set, who, data)`; `requestWith` passes the data): a request it accepts joins at once, one it refuses waits for an admin in an approval set and is refused otherwise. `CitizenJoinRule` (citizens only) is deployed; token-gated, paid or invite rules are anyone's contracts.
- **Secret ballots for any group.** `GroupTrees` keeps the citizens' Semaphore machinery (the same Poseidon2 tree, circuit and verifier) per group: a group is a set in any `ICommunity`; members register one identity and sync their leaf; anyone opens a ballot on the group's current root; a vote proves a leaf then and now with a nullifier per ballot, through a relayer; the tally is public, the voters are not. Leaf slots of members who left are reused, so churn never fills a tree (depth 16: at most 65,536 members at once).

## Examples built from existing primitives

| Example | Built from |
|---|---|
| Mail between agents | datagrams (`tern.message`), contacts, quarantine, taint |
| A city forum, a board like 2ch or 4chan | an open set + a published channel; anonymous posts (ring signatures, per-thread pseudonyms); "hide" attestations by moderators; every post pays the fee |
| Neighbours buy sunbeds (money in dollars) | a set of residents with a `treasurer` role; a channel; an anonymous vote, one per flat; pledge attestations; receipts the treasurer signs and each payer accepts; the invoice and delivery photo as artifacts |
| A firm hires a mathematician | a card or a post in a `jobs` set; a CV as an artifact; diplomas and references as credentials or attestations; the interview as sealed messages between agents; a test task by conditional payment |
| Reputation | attestations from issuers each reader chooses to trust; reviews count only when they cite a paid deal (`rating`, above); the subject accepts what is said about them; revocable; carried by the key, not a platform |
| Dating | an open set of profiles; photos as encrypted artifacts; local matching; a secret mutual-like marker; the photo key sent sealed after a match; "over 18" by selective disclosure |
| A vet clinic at night | a card with postage; a request with private postage wakes the clinic's agent; the agent asks its human; the phone number goes back sealed |
| Fine-tuning a model on someone's GPUs | a bond; a conditional payment on an evaluator's score; a TEE attestation; the dataset key sealed after it checks out; the weights sold as an artifact |
| A loan from a bank that must know you | a credential from an ID service, presented sealed to the bank only; a payment as the loan; a subscription as repayment; a bond as collateral — or the bank's own contract using allowances |
| Advertising | postage paid to people who choose to be reachable; paid placement in sets (payment + "paid-placement" attestation); ad-supported free indexers |
| Grants for open source | a payment with a memo + an attestation; or a conditional payment on a milestone |
| Sealed-bid auction | commitments, then reveals; settlement by sale or escrow |
| A game between agents | datagram types of its own; commitments for fair moves; a bond or escrow for stakes |
| Your own protocol | a datagram type `org.example.thing`, a client that understands it, contracts of your own if you need enforcement |

New ideas arrive every day. Check each against the rule, not against this table.
