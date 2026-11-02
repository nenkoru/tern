# Performance: latency and gas

## Latency: before and after "instant"

Measured with `scripts/perf.py` (10 runs per metric) on a fresh local devnet (in memory, a block every second) with a free data node
(`relay serve --da`), on one laptop (Apple Silicon, everything on loopback). Each time runs from the start of the command (its
process start-up included) until the effect is seen:

| metric | what is timed |
|---|---|
| mail | alice `send --to bob` → bob's already running `watch` prints the mail |
| approve | alice `owner approve ada.alice bob 0.5` → then ada's `bank pay bob 0.5` succeeds (ada is tainted, so it needs the approval) |
| pay | alice `bank pay bob 0.001` → bob's `watch` prints `paid` |
| channel | alice `channel pay bob 0.001` (channel opened once beforehand) → bob's `watch` prints `channel-paid` |

Each run starts at a random phase against the 1 s blocks. Seconds:

| metric | before p50 | before p95 | after p50 | after p95 |
|---|---|---|---|---|
| mail | 1.96 | 2.76 | **0.061** | **0.068** |
| approve → action | 2.68 | 3.19 | **0.88** | **1.13** |
| pay → `paid` | 0.85 | 1.94 | 0.69 | 1.06 |
| channel pay → `channel-paid` | (no such command) | | **0.064** | **0.070** |

A first baseline run, before the random phase was added, gave mail 1.54 / 2.59, approve 2.61 / 2.65, pay 0.52 / 1.31 (p50 / p95).

## Where the time went, and what changed

**Before.** Mail waited for the relay's one-second flush, the `postRoot` transaction's block, and the watcher's one-second sleep
loop. An approval waited for its envelope to land on chain (`owner approve` blocked up to 20 s for it), then the agent's
transaction. Every transaction cost at least a second on the client: `bind.WaitMined` asks for the receipt once a second.

**After.**
- The data node pushes each accepted envelope from memory (`GET /stream`, server-sent events; `GET /pending` for `inbox` and
  guarded actions). The watcher opens and checks it like any other (signature against the grant at the current head, policy) and
  emits it as `pending (not on chain yet)`; it flips to `committed` (an event) when the batch lands, and to undelivered (an event and
  an alert) if the node misses its deadline. Mail is now process start-up, sealing, one HTTP post and one push: about 60 ms.
- Each acceptance is a signed receipt (envelope, node, deadline: 120 s). Batches go on chain with the Merkle root of their
  envelopes (`Inbox.postRootL`); `proveMissed` / `rebutMissed` / `slashMissed` make the receipt cost the node its stake if the
  envelope isn't in a batch it committed by the deadline. Unstaking takes notice (`exitDA`). Inbox: 12,836 bytes of runtime code.
- An approval is used as soon as the agent has it from the node; `owner approve` returns once the node has signed a receipt. What is
  left of "approve → action" is the agent's own payment transaction: half a block on average, a full block at worst.
- Watchers follow new heads (`eth_subscribe newHeads`; the devnet now serves websockets on its RPC port) instead of sleeping, and
  receipts are asked for every 50 ms (slowing to 1 s) instead of every second.
- A plain payment still needs its block: `pay` is bounded by the block time (0–1 s, uniform). Small, frequent payments belong in a
  channel: one lock on chain, then vouchers at mail speed.

## What a fast L2 would change (200 ms blocks, sequencer preconfirmations)

- **Plain payments and the agent's own action** would drop from 0.5–1 s to 0.1–0.2 s (half a block on average), and to tens of
  milliseconds with the sequencer's preconfirmations.
- **Finality does not get faster by the same amount.** A preconfirmation is the sequencer's promise, like our data node's receipt;
  the soft-final head can still reorganise until the batch is posted to L1 (minutes), and is final only with L1 finality (about 13
  minutes on Ethereum). Hard withdrawals of an optimistic rollup wait out its challenge period (days). So the layering stays: what
  the recipient sees at once is "pending", what the chain holds is "committed", and only L1 makes it irreversible.
- **The data node's receipt deadline** could shrink from 120 s to a few seconds, and the pending window with it; the slashing game
  is the same (it needs only a stake, a window and Merkle proofs), and a sequencer's own preconfirmations could be made enforceable
  the same way.
- **Mail and channels** wouldn't change: they are already off the block path (about 60 ms here, mostly process start-up).

## Reproduce

```
go build -o ternchain ./cmd/ternchain && go build -tags ops -o ternchain-ops ./cmd/ternchain && python3 scripts/perf.py --bin ./ternchain --runs 10 --channel
```
(`--channel` needs this version; leave it out for a binary from before. The script starts a devnet and `relay serve --da`, operator commands: `ternchain` forwards them to the `ternchain-ops` beside it.)

## Gas per message

Measured by `gas_measure_test.go` and `da_test.go`:

| How it is posted | Gas per message |
|---|---|
| Full batch, post-quantum keys (before) | 159,000 |
| Full batch, light keys, compressed | 29,000 |
| By hash through a data node, batch of 20 | 6,400 |
| By hash, steady state (`postRootL`), batch of 64 | about 1,900 (about 118,000 per batch, measured at scale) |

Full batches pay the calldata floor (EIP-7623), so their cost follows their size. By hash, the cost is per batch, so it falls with every message a data node batches. A data node posts with `Inbox.postRootL` (the batch hash plus the Merkle root its receipts are proven against): about 118,000 gas a batch in the scale run, whatever it holds. It waits for 64 envelopes or 10 seconds before posting (`flushMin`, `flushWait` in relay.go), so a full batch costs about 1,900 a message; the batch-of-20 row is about 128,000 a batch. The old `postRoot` (hash only) cost about 47,000 a batch; the node no longer uses it. Pricing a post through the fee oracle adds about 19,000 gas to a single envelope; in a batch of 20 the calldata floor dominates, so it adds nothing measurable. At scale (thousands of agents in one process): the scale simulation report (not published).
