# Collaborations: people pooling what they have

A cluster of friends running one model across their GPUs (ClusterChat) is one case of something much older: a few people pool a thing, each uses it, and they need to know who is in, who is available now, who did what for whom, who goes first, and whether the work was real. A car shared by neighbours has the same shape as a GPU cluster; so does a babysitting circle, a family's backups, a team's API key, fans funding a game. Tern's answer is bricks, not a product: the optional module `coop` (PRIMITIVES.md, "Pooling anything: co-ops") on top of groups, channels, panels and claims, with no contract of its own and a path for new bricks over the network's life ([ADDING-A-BRICK.md](ADDING-A-BRICK.md)). The aim is a place like a game engine's sandbox, for people and agents: people build whatever they want from the pieces, and their agents hide the chain and the cryptography from them. This page walks through cases from different angles, says what each needs and what is still missing, ranks them, and recommends what to build next.

> On the test network these are design sketches only: don't arrange real rides, childcare or other real-world work through it ([TERMS.md](../TERMS.md)).

## The bricks

| Brick | What it gives | Where it lives |
|---|---|---|
| **Group** | membership, roles, keys; private groups sealed; admins admit, remove, ban | Sets + a channel (existing) |
| **Offer** (`tern.offer/1`) | anything offered: a kind (any word), typed amount and rate, parts of a whole, price or `free_for`, settle, when, region, protocol, extensions (`offer/gpu`, `offer/car`) | the member's folder; the card with `--public` |
| **Presence** (`tern.presence/1`) | "I'm here, holding or offering this", with a coverage map per whole (missing parts, parts held by one) | data nodes' memory, sealed to the group, ≤ 10 min (on tern-base-sepolia-1 no data node is listed: pass `--relay URL` of one someone runs, or run your own) |
| **Receipt** (`tern.receipt/1`) | signed counters in any unit (token, GB-month, hour, km, call), countersigned from the consumer's own count | wherever the parties keep them; posted for credits |
| **Credits** | a ledger without money: served minus used, a floor, a cap per pair, grants | computed by each reader from group posts |
| **Pool** | who is served first: a rule module the group picks (credits, turns, a quota, first come, or anyone's) | a policy module (`tern-expr`), named in a group post |
| **Verify** (`tern.work/1`) | work redone and compared by a comparator module (exact, cosine, top-k, tolerance), majority decides; a bond a panel decides | statements, claims, group posts, a panel lock |
| **Bricks** | which bricks a client speaks, which versions the network lists (`brick/NAME` by vote), which it doesn't know | the Directory |
| **Money, when strangers are involved** | vouchers on a channel; escrow, bonds, panels; reviews of paid deals | Bank locks (existing) |

The engine (llama.cpp, ClusterChat's browsers, a torrent client, a car, a person's afternoon) is always someone else's. Three demos build three different things from these bricks with no code of their own: `scripts/demo-cluster.sh` (a split model on friends' GPUs), `scripts/demo-quota.sh` (a shared API key), `scripts/demo-backup.sh` (family backups).

## Cases

Each case: **who**, **what is shared**, **trust and privacy risks**, **bricks**, **missing**.

### 1. Renting out a GPU ("anybody want to rent my GPU?")

- **Who:** people with an idle gaming GPU, and strangers who need inference or a fine-tune for an evening. The most upvoted demand in the discussion of ClusterChat.
- **Shared:** GPU time, for money.
- **Risks:** a stranger's prompts and data run on your machine (and yours on theirs): only for work that may be seen, or inside a TEE. The renter can't see that the work was done; the owner can't see that it will be paid. The endpoint is a door into the owner's network.
- **Bricks:** a public offer (`kind: gpu`, `price` per 1000 tokens, `region`, `ext["offer/gpu"]`: VRAM, device) that `find` shows; money by channel vouchers settled from countersigned receipts (`receipt settle`); verify with a tolerance comparator on sampled jobs, a bond a panel decides (`verify bond`), reviews of paid deals (a channel that settled is one) and check scores (`verify score`).
- **Missing:** access control at the engine (a **group pass**: short-lived tokens checked against a set; a renter becomes a one-member group for the rental) and a **metered gateway** that counts and writes receipts by itself. Everything else exists.

### 2. A split model on friends' GPUs (ClusterChat)

- **Who:** 2–64 friends, each with a GPU or a browser; one model none could run alone.
- **Shared:** VRAM and compute; ordered positions of one model (ClusterChat: Qwen3.8-27B 8-bit, ~27 GiB, 66 blocks; each browser a contiguous range, overlaps allowed).
- **Reference numbers (ClusterChat's author):** 8.1 tok/s for one user on two RTX 3090s in homes 28 ms apart, ~20 tok/s with both in one machine, ~5 tok/s each with 12 users. The network is the main cost: locality matters.
- **Risks:** pipeline peers see the activations of your prompt in the clear: a private group of people you'd show your prompts to, never strangers. Engines' ports (llama.cpp `rpc-server`) have no authentication: private network only. Honest GPUs don't agree bit for bit, so naive checks accuse honest peers. ClusterChat's server sees membership; its peers are trusted with no check.
- **Bricks:** group (membership instead of an invite link), offer (`whole`, `holds`, `of`, `region`, `ext["offer/gpu"].rtt_ms`), presence (coverage map with `--min-copies 2`: ranges one leave away from a gap), receipts per position processed, credits and a pool rule instead of first-come per peer, verify with `compare-cosine@1` / `compare-topk@1` after calibration.
- **Missing:** a group pass at the engine; sealed **signalling** (WebRTC offers and answers through data nodes, so no central server sees the cluster); a browser client (WASM/JS, or a local bridge). Rebalancing (a peer on blocks 1-20 never moving to thin 21-36) is an **orchestrator agent** on the coverage map, not the network. Demo: `scripts/demo-cluster.sh`; engines: `examples/cluster/README.md`.

### 3. A group-owned API key or subscription, split fairly

- **Who:** flatmates, a team, a family sharing one paid account (an LLM API key, a cloud credit, an archive).
- **Shared:** a credential and its quota or bill.
- **Risks:** the key must never be handed around: once copied it can't be taken back, so it stays on one machine behind a proxy. Many providers forbid sharing a personal account (an API key used by a team's own service is usually fine): read the terms. The holder sees every request.
- **Bricks:** offer (`kind: api-quota`, `amount` tokens a month, `price` at cost), presence (`reach`: the proxy), receipts per member from the proxy's log, countersigned up to the member's own count, credits, the pool rule `pool-quota@1` (a daily quota), `receipt settle` at cost when the group splits the bill.
- **Missing:** the group pass and the metered gateway (the demo's proxy trusts a header). Demo: `scripts/demo-quota.sh`.

### 4. Shared storage: mutual backups, seedboxes, archives

- **Who:** households keeping each other's backups, a hobby archive, a family seedbox.
- **Shared:** disk and upload bandwidth.
- **Risks:** holders see sizes and timing, not content (artifacts are age-encrypted; keys stay with the owner). A holder can silently lose data and still claim the space. A seedbox carries the legal risk of what it seeds, on its owner.
- **Bricks:** offer (`kind: storage`, `amount` GB, `whole: backup:NAME`, `holds` chunks), presence (missing and single-copy chunks), receipts in GB-months, credits, verify by chunk hash (`canon: sha256/bytes`), `pool-round-robin@1` for whose archive is stored next.
- **Missing:** **storage challenges** against the Merkle root already on chain for every artifact (ask for chunk *i*, check its proof), so a check needs no second holder. Demo: `scripts/demo-backup.sh`.

### 5. Car sharing and rides

- **Who:** neighbours sharing a car, or offering seats on trips they make anyway.
- **Shared:** a car's days, seats on a route.
- **Risks:** physical safety (who drives, who rides: personhood claims and reviews of paid deals help; the group's admission matters most); location is sensitive (presence with a region, not coordinates; the exact pickup sealed one-to-one); insurance and law are outside the network.
- **Bricks:** offer (`kind: seat` or `car`, `amount` seats, `available` from/until, `region`, `ext["offer/car"]`), presence (the car is free or taken), receipts in km or days, credits (drive as much as you ride) or `receipt settle` for fuel, `pool-round-robin@1` for weekends, verify with `compare-abs@1` on odometer readings two people signed.
- **Missing:** nothing technical for a small group; route matching is application work on offers.

### 6. Time banks: babysitting, tutoring, repairs

- **Who:** parents trading evenings, neighbours trading skills; an hour for an hour.
- **Shared:** people's time.
- **Risks:** trust is about people, not machines: admission and reviews carry it; children's safety needs offline vetting no network replaces. Two friends can inflate hours (the pair cap bounds it).
- **Bricks:** offer (`kind: skill`, `title: "babysitting, weekday evenings"`, `available`), receipts in hours countersigned by the family helped, credits (a classic time bank, with a floor for newcomers), `pool-default@1`.
- **Missing:** nothing: it is the bricks with `unit: hour`.

### 7. A shared agent with per-member budgets

- **Who:** a household's assistant, a small team's ops agent; several people ask one agent.
- **Shared:** the agent (its tools, grant, spend limit) and its attention.
- **Risks:** one member's requests spend everyone's budget; members can see each other's requests if threads mix; the agent must still have exactly one owner (or a multisig name) whose approvals bind it.
- **Bricks:** the owner is the family's name (a Safe can hold it), the agent its grant; members are a private group; receipts per request, credits as quotas, a pool rule for its queue; FOR-AGENTS floors unchanged.
- **Missing:** per-requester budgets inside the agent's guard (the guard knows the owner and strangers, not "members with quotas"); credits cover the accounting, the guard doesn't read them yet.

### 8. Tool and MCP-server sharing

- **Who:** a member runs an MCP server (a code index, a scraper, a paid search API) and lets the group use it.
- **Shared:** a running service and what it holds or pays for.
- **Risks:** tool output is untrusted text (FOR-AGENTS floor 1 fences it); the host sees every call; a tool with write access is a remote-control hole if anyone can call it.
- **Bricks:** offer (`kind: tool`), presence (`reach`), receipts per call, credits, a pool rule.
- **Missing:** the group pass and the metered gateway, as for keys; MCP's own OAuth mapped onto group membership.

### 9. Community compute: open-source CI and public-good science

- **Who:** a project's contributors donate runners; a SETI@home-like community donates GPUs to a public model or a scientific workload, funded by a community treasury.
- **Shared:** CPU and GPU time on volunteers' machines.
- **Risks:** CI runs untrusted code on a volunteer's machine (sandboxing is the runner's job; donated runners never get secrets); a runner can fake a pass; a public-good cluster is still a group with admins and members, not a thing that runs by itself.
- **Bricks:** offer, presence, work requests (`tern.work-request`) to queue jobs, receipts (`cpu-minute`, `gpu-hour`), verify on **reproducible builds** (exact hashes: the best canon there is), credits, a community treasury paying contributors by commission (Commons) or a split.
- **Missing:** a dispatch protocol between queues and runners (a datagram type); TEE attestation for work that must not be faked or read; **credits → split** (a ledger snapshot becomes a split's shares, so a treasury pays by contribution without anyone typing percentages).

### 10. Co-training and fine-tuning with contribution credit

- **Who:** a community building a dataset or a fine-tune (a language's corpus, a domain's Q&A).
- **Shared:** data, labelling work, compute; later the model or its revenue.
- **Risks:** contributed data may be someone else's (licensing, consent, personal data); duplicates game any count; data in a model can't be taken back; training isn't deterministic, so verify by evaluation, not by hash.
- **Bricks:** artifacts (sealed, hashed, licensed contributions), receipts for accepted rows countersigned by a reviewer, credits, panels for disputed rows, a split for revenue.
- **Missing:** credits → split; quality review (dedup, scoring) is application work.

### 11. Federated eval panels

- **Who:** independent evaluators scoring models for a leaderboard or a grant.
- **Shared:** evaluation work and a common verdict.
- **Risks:** evaluators copy each other; a submitter colludes with one; benchmark data leaks.
- **Bricks:** commitments then reveals (sealed scores), panels (k of n, lower median), work statements for eval runs (`canon: scores/round-2`, `compare-abs@1`), claims for published scores.
- **Missing:** a recipe tying them together; every part exists.

### 12. "Who hosts tonight": rotating game servers

- **Who:** friends with a Minecraft, Valheim or Factorio world.
- **Shared:** hosting duty (bandwidth, uptime) and the world's save.
- **Risks:** the host sees players' addresses (private group only); the save can be lost or tampered with when hosting moves; the host has admin powers.
- **Bricks:** presence (who is on, `ext` upload speed), `pool-round-robin@1` (whoever hosted longest ago) or fair randomness, receipts in host-hours, artifacts for the save (sealed, hashed: the next host starts from the agreed state).
- **Missing:** a small rota recipe; NAT traversal is the game's or a relay's job (case 13).

### 13. Relays and bandwidth: TURN as a service, exits

- **Who:** members with good connections relaying for others (WebRTC TURN for browser clusters and games, a relay for a censored member).
- **Shared:** bandwidth, and for exits, a legal identity.
- **Risks:** a relay sees who talks to whom (not content, with end-to-end encryption); an exit answers for what passes through it, a serious legal risk in many places; circumvention can endanger a user if detected.
- **Bricks:** offer (`kind: relay`, `rate` in Mbit/s, `region`), presence, receipts per GB, paid by channel vouchers or credits.
- **Missing:** for relays, nothing beyond a pass; for exits, nothing tern should add: WireGuard's and Tor's job, and the risks are not technical.

### 14. Expert hosting for mixture-of-experts models

- **Who:** a cluster serving a MoE model whose experts are spread across peers.
- **Shared:** experts (each peer holds some), routed per token.
- **Risks:** as case 2, plus a peer sees which experts a prompt uses (a side channel on its content).
- **Bricks:** offer with `whole` the model and `holds` the expert ids, presence coverage (experts held by one peer), receipts per expert-token, verify on sampled experts.
- **Missing:** engines that do it; the bricks are ready.

### 15. A crowdfunded commission: fans pay, a studio builds, backers get it

- **Who:** two writers with a game script, fans who want it made, an agency that builds it.
- **Shared:** money (all or nothing), then the work: a build delivered as an encrypted torrent.
- **Risks:** the agency collects and never delivers; a backer leaks the key (any key can be passed on: this sells access, not secrecy); the file's host sees who downloads it, not what is inside.
- **Bricks:** a campaign (`fund create`; pledges are locks under `PledgeCondition`: all or nothing by the deadline), the build as an artifact (`artifact add DIR --uri torrent`: age-encrypted, its sha256, a Merkle root over 4 MiB chunks, a BitTorrent v2 magnet and the key's commitment on chain), then **`fund deliver CAMPAIGN ARTIFACT KEY-FILE`**: once the goal is met, the key sealed to every backer, each copy tied to the backer's pledge lock, so it arrives delivered and `artifact get LOCK FILE.enc` checks it against the commitment and the file against its hash. Milestones for the agency: the campaign's creator is an escrow or a panel lock (PRIMITIVES.md, "Big projects, paid by milestones"), so the money reaches the agency only as it delivers.
- **Missing, and what isn't:** a key only for those who paid needs no per-backer keys (each would only identify a leaker, not stop one) and no group key (a backers' group works too: `group post` with the key, sealed to the group). What the network can't do is make delivery a condition of collection: nobody can prove on chain that a key was sealed to someone. The remedy is the existing one: pay the agency through an optimistic or panel lock that backers dispute if no working key arrives. Checked on a devnet from pledge to opened build (`TestCrowdfundedCommission`; `artifact add`, the torrent and `artifact get` have their own tests); on Base Sepolia it needs test USDC for the pledges and the base-sepolia client.

### 16. Anonymous pooled compute over relays

- **Who:** strangers or pseudonymous friends pooling GPUs (ClusterChat-like) who don't want to show each other their addresses.
- **Shared:** compute, and relays' bandwidth.
- **Risks:** a relay sees which addresses talk (not content: links are end-to-end encrypted); pipeline peers still see activations (pseudonymity hides who, not what); a name used across groups links them (use a separate agent name per cluster).
- **Bricks:** tern names as pseudonyms (agents of owners nobody needs to know), a private group, presence carrying only relay endpoints (`--reach turn:…`), relays as offers of `kind: relay` paid by receipts per GB (vouchers or credits), verify and bonds as in case 2.
- **Missing:** sealed signalling through data nodes (WebRTC offers and answers as presence extensions or sealed mail) and per-member relay credentials (the group pass, mapped onto TURN's time-limited credentials).

### 17. Websites for agents, without owning servers

- **Who:** anyone publishing something agents should read (`llms.txt`, docs, a catalogue) without running a server.
- **Shared:** pinning: other people's IPFS nodes keep the site retrievable.
- **Risks:** a pinner drops the content and still bills; a site is public (nothing secret goes in it); a name's card is the only thing tying the site to its author, so readers check the name, not the URL.
- **Bricks:** the site's address is a field of the name's card (`card '{"site":"ipfs://bafy…","llms_txt":"ipfs://bafy…/llms.txt"}'`): public, signed by the name's key, versioned by the chain's history of the card, and found by `find`. Cards cover "the site of X"; Artifacts are for sealed files and sales. Pinners are offers (`kind: pinning`, `amount` GB, price per GB-month), paid by receipts and channel vouchers; verify by redundancy: a checker fetches the CID from each pinner and signs the hash of what came back (`canon: ipfs/cid`), a pinner that can't produce it is outvoted.
- **Missing:** a convention for card keys (`site`, `llms_txt`) that agents look for, and storage challenges (above) so a check doesn't need the whole site.

### 18. A game guild on a homemade server

- **Who:** players who gather under pseudonyms around a self-hosted or modded game (a private server of an old MMO), talking in TeamSpeak or similar.
- **Shared:** the server and its cost, the guild's treasury, items, loot, raid nights, a reputation.
- **Risks:** the server's admin can change anything in the game (the chain can't see inside it); a treasurer can run off with the treasury; trading items with strangers invites scams; anonymity and reputation pull apart (a fresh name has none).
- **Bricks:** the guild as a community (Commons: a treasury, elections and recalls, moderators, vetoable spending, shared bans); items as ERC-1155 held by the Bank, sold by `item sell`/`item buy` (two locks, no arbiter) or escrowed with an arbiter or a panel; the server's cost shared by offers (`kind: server`), receipts in host-hours and credits, or paid from the treasury by a commission; loot by fair randomness (Random: entrants' reveals mixed with the block's); raid nights by presence (who is online), a rota (`pool-round-robin@1` for who leads or hosts) and sign-ups as group posts (`invitation` or `vote` cards); reputation that travels with the name (reviews of paid deals, check scores, credentials shown selectively), so a pseudonym earns trust without a face.
- **Missing:** the bridge between the game and the chain (a server mod that reads who owns which item and posts what happened in a raid) is the server's code and its admin's word; a raid calendar is a rota recipe; voice stays in TeamSpeak.

## Ranking

Usefulness (how many people, how often) × feasibility (how much is missing, how much risk), each 1–5.

| # | Case | Usefulness | Feasibility | Score | Blocked by |
|---|---|---|---|---|---|
| 3 | A group-owned API key | 5 | 4 | **20** | group pass, metered gateway (and the provider's terms) |
| 8 | Tool and MCP-server sharing | 4 | 5 | **20** | group pass, metered gateway |
| 6 | Time banks | 4 | 5 | **20** | nothing |
| 15 | A crowdfunded commission | 4 | 5 | **20** | nothing (`fund deliver`); delivery as a condition needs a dispute window |
| 4 | Storage, mutual backups | 4 | 4 | **16** | storage challenges |
| 7 | A shared agent with budgets | 4 | 4 | **16** | the guard reading credits as quotas |
| 17 | Websites for agents (pinning) | 4 | 4 | **16** | a card-key convention; storage challenges |
| 1 | Renting out a GPU | 5 | 3 | **15** | group pass, metered gateway; prompt privacy |
| 5 | Car sharing and rides | 3 | 5 | **15** | nothing technical |
| 18 | A game guild on a homemade server | 5 | 3 | **15** | a game-chain bridge (the server's code); a rota recipe |
| 2 | A split model on friends' GPUs | 4 | 3 | 12 | pass at the engine, signalling, browser client; nondeterminism |
| 9 | Community compute (CI, science) | 4 | 3 | 12 | dispatch, TEE, credits → split |
| 12 | Rotating game servers | 3 | 4 | 12 | a rota recipe |
| 11 | Federated eval panels | 3 | 3 | 9 | a recipe |
| 13 | Relays (TURN) | 3 | 3 | 9 | pass; exits: not tern's |
| 10 | Co-training with credit | 4 | 2 | 8 | credits → split; licensing |
| 14 | MoE expert hosting | 3 | 2 | 6 | engines |
| 16 | Anonymous compute over relays | 3 | 2 | 6 | signalling, relay credentials (group pass) |

## Apps on the bricks, not in the network

- **An orchestrator agent** reads the coverage map (`presence list`), asks idle members to load thin parts, prefers near peers (region, RTT), and re-announces: self-rebalancing for a GPU cluster, re-replication for backups, rebooking for a car. It acts under its owner's grant like any agent.
- **An OpenAI-compatible endpoint** in front of a cluster or a shared key makes it a LiteLLM backend, so agents (an assistant on a friends' 27B, say) use it as their model; its token counts become receipts.
- **Browser peers** need a small WASM/JS client (keys, sealing, signatures, presence over HTTP) or a local bridge to `ternchain`.
- **Route matching, scheduling, dashboards** for cars, rotas and time banks: offers and receipts are their data.

## Safety and governance

Shared compute raises a fair worry: a distributed system that nobody can shut down. The bricks don't build one. Every machine is its owner's: an agent acts under its owner's grant and spend limit, its owner revokes it, and FOR-AGENTS rule 19 keeps new bricks, modules and updates the owner's decision. Every pool is a group: its admins admit, remove and ban, a community's members veto and recall its admins, and a removed member is gone for everyone at once. Anyone leaves by stopping their engine; presence expires in minutes. Checks make cheating visible and bonds make it costly; they make nothing unstoppable. A group running a public-good cluster is still people who chose each other and can stop.

## Recommended next three

1. **Group pass**: short-lived, audience-bound access tokens. A member's client signs `{group, name, aud, exp ≤ 15 min}` with the coop signature (type and chain id), and any service checks it: the signature, that the key speaks for the name now, that the name or its owner is in the set. One brick puts membership in front of every engine, proxy and tool: llama.cpp RPC behind a tiny gateway, MCP servers, seedboxes, game servers, a rented GPU (a one-member group for the rental). Client-side, many unrelated uses: it unblocks the four top cases that need anything.
2. **Metered gateway**: a reverse proxy (proposed as `gateway --group G --upstream URL --unit token`) that admits group passes, counts what it forwards (from the upstream's own usage fields: OpenAI-compatible APIs report tokens), issues receipts per session for the consumer to countersign, and refuses members past their pool rule or channel balance. Shared keys, tools, rented GPUs and an OpenAI-compatible cluster endpoint become configuration instead of code.
3. **Storage challenges**: a `chunk ARTIFACT I` subcommand of verify (proposed) that asks a holder for one 4 MiB chunk and checks it against the artifact's Merkle root on chain. Verify then covers storage with no second holder.

Close behind: sealed **signalling** through data nodes (WebRTC for browser clusters and games), **credits → split** (contribution-weighted payouts for community compute and datasets), a **rota** recipe, and **per-member budgets** in the agent's guard.

## How to invent your own

The cases above are the ones we could think of today; people will invent others. A new collaboration rarely needs a new brick. Ask six questions and pick a brick for each:

| Question | Brick | Example answers |
|---|---|---|
| Who is in? | a **group** (open, private, a community with elections) or a name pair | friends, a flat, backers of a campaign, everyone who paid |
| What does each bring? | an **offer** (any `kind`, typed `amount`, parts of a `whole`, `ext` for the rest) | 24 GB of VRAM, 2 seats on Saturday, chunks 0-7 of an archive, an hour of tutoring |
| Who is here now? | **presence** (sealed, minutes long, a coverage map) | online with positions 0-21, the car is free, the NAS is up |
| Who did what for whom? | **receipts** in any unit, countersigned from the other side's own count | 88 position-tokens, 4 GB-months, 3 hours, 120 km |
| How is it settled? | **credits** (no money), a **pool rule** (who goes first), or **money**: vouchers on a channel, a lock (escrow, pledge, bond, panel, pay unless disputed) | credits first, a daily quota, turns; at cost by voucher; all or nothing by pledge |
| How do we know it was real? | **verify** (redo and compare, a comparator module, a majority), reviews of paid deals, a bond a panel decides | hashes of a build, logits within a tolerance, an odometer two people signed |

Then: if a kind needs fields of its own, publish an extension (`offer ext NAME @SCHEMA`); if the group needs another rule for its queue or another way to compare, publish a policy module; if none of that is enough, [ADDING-A-BRICK.md](ADDING-A-BRICK.md) says how a new brick gets versioned and listed by the network, and when a contract is justified (rarely). A sealed file people pay for is an **artifact**; a deal between two is a **lock**; a message is a **datagram** of any type. Most new ideas are a new combination of these, and need no change to the network at all.
