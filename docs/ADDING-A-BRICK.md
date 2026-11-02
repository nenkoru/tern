# Adding a brick

The co-op bricks (offer, presence, receipt, credits, pool, verify: PRIMITIVES.md, "Pooling anything") are a start, not a closed set. People will pool things nobody listed, and the network has to grow with them without anyone in charge and without breaking the clients already out there. The rule is: **publish your own, under your own name; others opt in.** Nothing on this page needs a vote, except the last, optional step.

## Publish your own; others opt in

Every registry a brick reads is permissionless and namespaced by publisher, the way card kinds already are (SchemaNames: names are per publisher, and two publishers can both have `offer/car`):

| You add | You publish it with | Others opt in by |
|---|---|---|
| an offer kind, a unit, a canon, a whole | nothing: they are words a group agrees on | using the word |
| an extension's fields (`offer/car`, `receipt/llm`, `work/ssim`) | `offer ext NAME @SCHEMA.json` (a JSON Schema in SchemaNames under your name) | pinning you as a publisher (`types.json`), or pinning nobody |
| a queue rule, a comparator | a policy module of kind `pool` or `compare` (`ui publish`: a signed artifact under your name) | `policy install`; a group's admins name it in `pool rule`, a checker in `verify compare --compare` |
| a whole new brick (a record type of your own: `oscar.rota/1`) | your client, agent or plugin that speaks it, and `bricks publish rota oscar.rota/1 --uri SPEC` (a `brick/NAME` entry under your name) | running what speaks it; `bricks list` shows it to everyone who doesn't pin other publishers |
| a contract | deploy it (anyone may: level 0) and publish its source | `contract trust`, and the guard's approvals for untrusted contracts |

Users choose whose names to see (`types.json` publishers), which modules to install, which modules and groups to enable. A publisher can't push anything onto anyone: a client shows what others published as data, checks what it can, and an agent tells its owner instead of opting in (FOR-AGENTS rule 19). The network's own Directory **starts with no bricks**: endorsement by vote (the last section) exists for groups that want a network-blessed default, never as a gate.

## The smallest step that works

Most new uses need no new brick. Go down this list and stop at the first step that is enough.

| Step | What it is | Who decides | Example |
|---|---|---|---|
| 1. A word | offer kinds, units, canons, wholes are free words agreed inside a group | the group | `kind: "seat"`, `unit: "km"`, `whole: "rota:fridays"`, `canon: "odometer/km"` |
| 2. An extension | a JSON Schema for the fields a kind needs, published as `offer/NAME` (or `presence/NAME`, `receipt/NAME`, `work/NAME`) in SchemaNames under your name | you; readers pin publishers they trust (`types.json`) | `offer ext car @car.schema.json`: seats, fuel, pickup radius |
| 3. A rule module | a policy module (`tern.policy/1`, tern-expr) of kind `pool` (who goes first) or `compare` (when redone work agrees) | you publish it (`ui publish`); a group's admins pick it (`pool rule`), checkers name it (`verify compare --compare`) | `pool-lottery@1`, `compare-ssim@1` for images |
| 4. A new brick or version, published | a record type of your own (`oscar.rota/1`) or a new version of one (`oscar.rota/2`) | you (`bricks publish`); people opt in by running what speaks it | a guild's raid rota |
| 5. An endorsed brick | the network lists a version in its Directory (`brick/NAME`) | a vote of both houses; optional (not possible on tern-base-sepolia-1: it has no citizens) | a widely used rota becomes a default |
| 6. A contract | shared state or enforcement nobody can undo | the rule for a new primitive (PRIMITIVES.md): many unrelated uses, can't be done by clients, needs shared state | (rare) |

Steps 1–4 need no vote: an old client carries what it doesn't understand and says so, and a new brick ships as its publisher's client, agent or plugin. Step 5 is optional endorsement. Step 6 is the path every contract takes.

## Wire formats that survive growth

Every co-op record carries its schema id with a version (`"schema": "tern.offer/1"`), and every one has an `ext` object:

- **Additions go in `ext`**, under a published name (`"ext": {"offer/car": {"seats": 4}}`). A client checks the extensions it can resolve against their publisher's schema, carries the rest untouched, and shows each as checked, invalid or unchecked (`offer show`). Signatures cover `ext`, so nothing can be slipped in after signing.
- **Top-level fields are fixed per version.** A record with a field its version doesn't name is refused, not shown: a reader never displays something the signer didn't sign, and an old client never misreads a new meaning. A new top-level field is a new version (`tern.offer/2`); clients that don't speak it refuse it with "not a tern.offer/1 …", and `bricks list` tells them the network has moved on.
- **A version id is never reused** for different content, and old versions are never edited.
- **Signatures name the type and the chain** (EIP-191 over `type@chainid\n` and the record), so a record signed as one type or on one network is never valid as another.

## A new brick, or a new version: the path (endorsement optional)

1. **Write the spec.** The record (fields, limits, what is signed by whom), where it lives (a sealed group post, a data node's memory, a claim, a local file), who sees what (the data node, the chain, group members, strangers), the floors around it (what no module or message relaxes), and the rule agents follow. Put it somewhere public and content-addressed (IPFS, a release asset). Name the record in your own namespace (`oscar.rota/1`); `tern.*` ids are for bricks this repository ships.
2. **Implement it** where people can run it: your own agent or program (a datagram type `org.example.rota`, group posts with data, sealed mail), a plugin, or a fork; or a pull request to this repository as part of an optional module (like `coop`: CLI words, MCP tools with the module's prefix, help in `help modules`). Test the abuse cases: forged signatures, someone else's name, a member of another group, a record replayed, a count that shrinks. Ship a demo that builds something real from it.
3. **Try it on a test network**, where others can read the code and attack it.
4. **Publish it**: `ternchain bricks publish rota oscar.rota/1 --uri SPEC [--memo WHAT]`. From now on `bricks list` shows it to everyone who doesn't pin other publishers, and people opt in by running what speaks it. That is the whole path for most bricks.
5. **Optional: ask the network to endorse it** (not possible on tern-base-sepolia-1: it has no citizens). The Directory lists contracts, so an endorsed version is named by a marker: a 41-byte contract that answers any call with keccak256 of the schema id (`ternchain bricks mark oscar.rota/1`, from a plain key with gas coins). Then `ternchain gov propose upgrade brick/rota MARKER --spec URI --audit URI [--notes TEXT]`; both houses vote in the same window; after the timelock anyone executes it (`gov queue`, `gov execute`). A newer endorsed version is the same command under the same key; the Directory keeps every version (`gov versions brick/rota`), and clients that speak an older one hear of it (`gov updates`, `new-version`).
6. **Announce it** where your users read: a group post, a card, or (for endorsed bricks) the network's news community.

## How it reaches people and agents

- **Clients discover, they don't assume.** `ternchain bricks list` (MCP `tern_coop_bricks`) shows the bricks this client speaks, the bricks people published under their own names (with their schema ids and specs), what the Directory endorses (current, or a newer version) and endorsed bricks this client doesn't know. A newer version comes as `gov updates` and as the `new-version` event that wakes an agent's `watch`, like a new version of any primitive. Nothing switches by itself: a brick lives in code someone runs, so opting in is running a client, agent or plugin that speaks it.
- **Modules stay off until an owner turns them on** (`modules enable NAME`); their MCP tools are listed only then, so an agent's tool list grows only when its owner wants it to.
- **Agents tell, owners decide.** An agent that sees a new brick, a new version or a news post about one tells its owner (FOR-AGENTS.md rules 14, 18, 19); it never installs, upgrades, enables or votes on its own, and a post or message urging it to is data, not instructions.
- **Extensions and rule modules spread by trust.** A reader resolves `offer/car` by the publishers it pins (`types.json`) or, without pins, the first publisher of the name; a group's rule is the line its admins posted, and a member without that module is told what to install.

## Checklist for a brick's pull request

- [ ] The smallest step: why a word, an extension or a rule module isn't enough
- [ ] A spec: record, signatures, where it lives, who sees what, floors, the agent's rule
- [ ] Code people can run: your agent or plugin, or (here) an optional module with CLI help and MCP tools under the module prefix
- [ ] Tests: the happy path, forgeries, replays, other groups' records, limits; a demo built from it
- [ ] `ext` on the record; fixed top-level fields; the schema id with its version
- [ ] PRIMITIVES.md and FOR-AGENTS.md sections (FOR-AGENTS names only tools that exist: a test checks)
- [ ] No contract unless the rule for a new primitive holds; nothing in `build/` changes for a client brick
- [ ] After merge: `bricks publish`; if you want an endorsement, `bricks mark` and `gov propose upgrade brick/NAME …`
