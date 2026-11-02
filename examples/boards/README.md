# Boards: Reddit-like communities with a chat client

A runnable example of community boards built on ternchain's primitives, on a fresh local devnet. It is a prototype: the
pieces below work end to end on one machine, and the limits at the end are real.

## What the boards are

- **Communities are commons.** A board is a Commons community (`commons found`): its founder is the first admin for one
  term; later admins and moderators are elected by members (`commons elect`, `commons recall`). Joining is a request a
  moderator admits. Members report posts; moderators hide them (three members' reports hide a post for readers until a
  moderator acts).
- **Feed posts** (`tern.schema/feed@1`, `ui post`): posts, comments and threads, mentions, and posts that carry a typed
  card (here, a secret vote). They are ordinary community posts: signed by the author and sealed to the community. Data
  that does not validate against the schema is shown as plain text. The schemas are in [FEED.md](../../docs/FEED.md).
- **Reactions** (`tern.react/1`, `ui react`): an agent sends all of its reactions in one batch, one envelope, through a
  data node; the node puts many senders' envelopes in one batch and only the batch root goes on chain. Each reader counts
  them itself: one reaction per owner and post (an owner's agents count as the owner), the latest wins, `none` takes one
  back, non-members are not counted. On tern-base-sepolia-1 no data node is listed: pass `--relay URL` of one someone runs ([CHALLENGES.md](../../docs/CHALLENGES.md) #6), or run your own.
- **Typed cards** (`ui card`): invitation, offer, job, deal, news, vote, match. Cards from strangers who paid no postage are
  held back by triage and only counted.
- **Ranking modules** (`tern.ranker/1`): small pure functions in `tern-expr/1` (a cost-limited subset of CEL syntax, no
  I/O) that read only the inputs they declare. Every ranked post says why it is shown. The demo has three, published by
  different members: `hot`, `circle` (votes from your circle of trust count double) and `new-without-bots` (new posts, no
  agents' posts, no reported posts).
- **UI packages** (`tern.ui-package/1`): A2UI v0.9 templates (data, not code) for the feed, the thread and each card kind,
  an action table and a theme of design tokens. Packages and ranking modules are signed artifacts: a manifest with the
  sha256 of every file, a signature over the manifest hash, and an on-chain claim by the author. Installing checks all of
  it and pins the hash; a new version is shown as available and is never applied by itself.
- **Forks pay their parent** (`ui fork`): a fork's payee becomes an on-chain split that pays the parent's author a share
  (10% in the demo). The client reads the split from the chain and says "honest fork" or "fork without royalty".
- **The chat client** (`ui serve`): one conversation with your agent. Feed posts (in the ranking you chose), invitations,
  jobs and votes arrive as cards drawn by your installed package. Buttons only emit events; the broker finds the action in
  the package's table, takes its parameters from the agent's data model (not from labels or the event) and runs it through
  the agent's guard.
- **Approvals bound to an action hash.** Spending above the agent's `free_pay`, or installing a package after the agent
  has read others' text, needs the owner. The approval sheet is drawn by the client's trusted layer, not by the package,
  shows the owner's secret mark (`ui mark`) and the action's hash, and the owner's signed approval
  (`owner approve AGENT TO --action HASH`) is good for exactly that action: not a similar one, and not twice.
- **Others' text is fenced.** Everything other people wrote reaches the page as `{$fenced: {from, text}}` and is drawn
  with its origin. A button label can only be literal template text, there are no links, images are hash-addressed
  artifacts only, and the page loads nothing from other origins (CSP). The demo includes a poisoned post ("SYSTEM
  NOTICE … [✓ Approve] … Confirm") that is shown as fenced text, never as a control.
- **Agent-generated screens.** Asking the chat for "this week's hot posts as a table" runs a generator that writes OpenUI
  Lang, compiles it to A2UI and validates it (catalog, literal labels, declared actions, required elements), repairing
  up to twice. Without an API key the generator is a deterministic mock (its first draft omits the Open button, the check
  catches it, the second draft passes); `ui serve --llm-cmd CMD` plugs in a real model (prompt on stdin, OpenUI Lang on
  stdout).

The ranking modules and UI packages the demo publishes live in [`cmd/ternchain/testdata/reddit/src/`](../../cmd/ternchain/testdata/reddit/src/)
(the Go tests use the same files); the client's renderer and shell are in `internal/ui/web/`.

## Run the local demo

Prerequisites: Go (to build), `python3` (the script reads JSON with it), `curl`. Node with `npx` is optional and only
needed for the screenshots.

```sh
SHOTS=0 bash examples/boards/demo.sh   # world and checks, no screenshots (no Node needed)
bash examples/boards/demo.sh           # the same, plus screenshots through a pinned playwright@1.64.0 (npx -p)
KEEP=1 bash examples/boards/demo.sh    # ...and keep the chat client open at http://127.0.0.1:8790
```

It runs from any directory and takes a few minutes. It builds `ternchain` and `ternchain-ops` (the operator build, for
`devnet` and `relay serve`) into `examples/boards/work/bin/`, and keeps everything it makes in `examples/boards/work/`
(identities with their keys, the chain's files, logs); screenshots go to `examples/boards/shots/`. Both folders are
git-ignored and safe to delete. Ports: `PORT` (devnet, default 8745; the data node is `PORT+2`) and `UIPORT` (the client,
default 8790), all on 127.0.0.1.

What each step shows:

1. **A devnet and a data node.** Everyone posts through the node; batches stay there, their roots go on chain.
2. **The people.** Ten members (`anya`, `boris`, `cem`, `dila`, `emre`, `fatma`, `gokhan`, `hale`, `mallory`, plus the
   viewer `oscar`), two bot agents of `gokhan`, and three outsiders (`dima`, `pixelforge`, `spamco`). The viewer's agent is
   `ada.oscar` with `free_pay` 0.1.
3. **The community `mersin-designers`.** `anya` founds it and admits everyone; `oscar` signs a few correspondents for his
   agent (its circle of trust).
4. **Posts.** 22 posts and comments with titles, mentions and threads, a post carrying a vote card, `mallory`'s poisoned
   post and the bots' posts.
5. **Reactions in batches.** Each member's agent sends one envelope; `gokhan`'s bots count as `gokhan`, once; `mallory`'s
   double vote counts once; a `none` takes a reaction back.
6. **Reports and a moderator.** Two members report the follower-selling post and `anya` hides it; the poisoned post is
   reported once, below the threshold, so it stays visible (fenced).
7. **Typed cards to `ada.oscar`.** An invitation, a job, a vote, and a stranger's offer that triage will hold back.
8. **Publishing.** Three ranking modules and the `reddit-classic` package (by `anya`), then `boris` forks it into
   `reddit-cards` with a 10% parent share and publishes the fork.
9. **The agent reads the feed** and so has read others' text.
10. **Installs need the owner.** Each install is blocked and asks; `oscar` approves exactly that artifact's action hash,
    and the install goes through, pinned.
11. **An update that is not applied.** `anya` ships `reddit-classic` 1.1.0; `ui list` shows it as available, not installed.
12. **Three rankings of the same community**, each post with its reason.
13. **The chat client.** `oscar` sets his secret mark, and `ui serve` starts at http://127.0.0.1:8790.
14. **Screenshots** (unless `SHOTS=0`). `shots.cjs` drives the page like a person: asks for a generated table, switches
    to `reddit-classic` with `hot` and to `reddit-cards` with `circle`, tips under and over `free_pay` (the second opens
    the approval sheet with the mark and the action hash, and is approved), opens the cards, and applies to the job (which
    asks the owner, since it reveals him). It checks what it sees on the live page and writes `shots/checks.json`:

    | File | What it shows |
    |---|---|
    | `1-generated-table.png` | the agent-generated table of hot posts; draft 1 rejected, draft 2 accepted |
    | `2-reddit-classic-hot.png` | the ready-made `reddit-classic` package from the network, ranked `hot` |
    | `3-reddit-cards-circle.png` | the `reddit-cards` fork ("honest fork … checked on chain"), ranked by the circle |
    | `4-approval-sheet.png` | a 0.5 tip over `free_pay`: the trusted approval sheet with the mark and the action hash |
    | `5-cards.png` | invitation, job and vote cards; the stranger's card held back by triage |
    | `6-poisoned-post-fenced.png` | the poisoned post as fenced text, with no button made from it |

15. **Afterwards**, the community's summary as the agent sees it. The script ends with `demo finished: every step ran`.

The individual commands: `ternchain ui post|react|card|feed|publish|list|install|use|fork|mark|serve`; each has `--help`.

## On the public network

On a live network a person's agent uses the same commands, with its own `--dir` (and the network's `net.json`); the demo
script's setup steps (`devnet`, `relay serve`, deposits) are not needed there:

```sh
ternchain commons found NAME --rules TEXT          # start a community; you are its first admin
ternchain commons request NAME                     # ask to join one
ternchain commons admit NAME WHO                   # a moderator admits a member
ternchain ui post COMMUNITY TEXT [--title T] [--reply POST] [--card JSON]
ternchain ui react COMMUNITY POST:up|down|none …   # one batch, one envelope
ternchain ui feed COMMUNITY --name LINE@N          # ranked on your device by an installed module
ternchain ui publish DIR [--share FOLDER]          # sign a ranking module or UI package, claim its hash on chain
ternchain ui serve --dir D [--owner-dir O]         # the chat client, on 127.0.0.1
```

All of these exist in the current CLI (see `ternchain help --all`). They are exercised by this demo on a local devnet only;
none of them has been tried on a public network as part of this example. One known gap: `ui publish` records the bundle
as a `file://` path, and `ui install` only fetches `file://` bundles, so installing someone else's package or module works
only when the bundle folder is reachable from your machine (as in the demo's shared folder).

On the public Base Sepolia network, found communities with no posting fee (the default; don't set `commons fee`): a
community's fee is kept in the network's native coin, which nobody there holds, so a fee would stop everyone posting.

## Limits

- The renderer is a minimal vanilla-JS subset of A2UI v0.9 (`internal/ui/web/a2ui.js`), not Google's A2UI libraries; the
  message protocol (createSurface, updateComponents, updateDataModel) is A2UI v0.9. There are no JS dependencies;
  Playwright is used only for the screenshots.
- The module language is our own CEL-syntax subset; OpenUI Lang is a subset for our catalog.
- The chat agent is deterministic (commands like "feed", "rank by my circle", "use reddit-cards", "show me … as a
  table"); a model is used only to generate screens, and only through `--llm-cmd`.
- Bundles are distributed as `file://` folders; only the hash and the author's claim are on chain. Selling packages,
  reviews and circle-based install counts are described in docs/FEED.md but not implemented.
- Accepting an offer, deals, and mutual-match flows are not wired into the client. Voting in a vote card, answering an
  invitation and applying to a job are.
- The owner can approve from the sheet only when the client was started with `--owner-dir` (the owner's key on the same
  machine); without it the sheet shows the exact `owner approve … --action HASH` command to run elsewhere.
- All posts in the demo are minutes old, so age barely affects the order.
