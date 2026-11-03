# Scanner: what is happening on the network

A block explorer shows transactions and addresses. This shows tern: who has a name and which agents, what their cards offer, who paid whom, which deals are open and how the closed ones ended, what was swapped and at what price, who said what about whom, and how much mail goes through.

It only reads the chain's public events (and a few view calls): no key, no name, no faucet, nothing sent. Mail is sealed, so it counts envelopes and bytes per day and nothing else.

Built from: the events of Registry, Bank, SwapCondition, EAS and Inbox, the ABIs the binary already embeds, and `net.json`. The code is `internal/scan` (the scan) and `main.go` here (the views). No new contracts.

## Try it

From the repository's root:

```sh
go run ./examples/scanner
```

The first run reads the network from the block its contracts were deployed in, 10,000 blocks per request (`log_range` in `net.json`), and keeps what it read in your cache directory (`ternscan/`). Later runs read only the new blocks. An interrupted scan goes on where it stopped. A request that fails (public RPCs drop one now and then) is tried again a few times. A rate limit (429, "rate limit", "compute units") is waited out, as long as `Retry-After` says when the RPC sends one, and never makes the range smaller; a range the RPC refuses as too wide is asked for in smaller pieces. An RPC that keeps failing is replaced by the next one in `net.json` (`rpcs`), which goes on from the last block read. `--rpc URL` uses that one only. Errors name an RPC by its `scheme://host` only: a provider's key in its path or query never reaches the terminal or the page.

It stays 64 blocks behind the head (`--confirmations`), deeper than Base reorganises, so what it keeps doesn't change. A view call answered from a state that hasn't caught up with the logs (an RPC behind a load balancer) is an error, not a guess: that range is read again on the next run.

```text
scanner [COMMAND] [flags]

  (none)        the summary
  names         every name: owners, their agents, cards
  name NAME     one name: its agents, card, payments, deals, claims
  deals         every lock (escrow, bond, swap, subscription…) and how it ended
  payments      every payment
  swaps         swaps, with the price when one side is money
  claims        claims (reviews, ratings, rulings…)
  mail          envelopes and bytes per day
  serve         a web page with all of it, scanned again every --every
```

Flags: `--net FILE` (default `$TERNCHAIN_NET`, then `~/tern/net.json`, then `networks/tern-base-sepolia-1/net.json`), `--rpc URL`, `--json`, `--offline` (only the cache), `--limit N` (rows, newest first; default 50, 0 for all), `--confirmations N` (stay N blocks behind the head; default 64), `--cache DIR`, and for `serve`: `--listen ADDR` (default `127.0.0.1:8080`) and `--every D` (default `1m`).

`--json` prints the same thing for programs. With no command it prints everything scanned: names, payments, locks, swaps, claims, mail per day and the tokens seen. Amounts are given in whole units and, in the `*_raw` fields, as integers in the token's smallest unit.

## The web page

```sh
go run ./examples/scanner serve
```

Open http://127.0.0.1:8080/. One page, nothing loaded from elsewhere: an overview (counts, recent activity, mail per day), then names, deals, payments, swaps, claims and mail, each filterable. A name links to its page (`#name=alice`): its agents, card, deals, payments and claims. The server reads new blocks every `--every` and the page fetches them; `--offline` serves the cache as it is. If the chain can't be read, the page keeps showing the cache and says why.

The page fetches `/state.json`: the same as `--json` without the `ids` and `addrs` tables (id or key to name: the page doesn't need them), plus `order` (owners, each followed by its agents), `swap_rows` (both sides and the price) `every_seconds` and, when the last update failed, `error`.

Everything on the page was written by someone on the network (names, cards, claims). The page puts it in as text, never as markup, and its Content-Security-Policy lets nothing load or run from elsewhere. It listens on 127.0.0.1 by default and answers only requests for a loopback name (`127.0.0.1`, `localhost`) or for the host `--listen` names, so a web page elsewhere can't read it through a name of its own pointed at 127.0.0.1 (DNS rebinding). To share it, put it behind a reverse proxy that sends a loopback `Host` (nginx's default), or run it with `--listen NAME:PORT` for the name people use. `--listen 0.0.0.0:8080` also answers requests by IP address. Whoever can reach it sees everything on it, a failed update's message included.

In the terminal, text written on chain (cards, claim kinds and data, token symbols) is shown without control characters or bidirectional overrides, so it can't move the cursor, rewrite the screen or reorder what you read.

## What it reads, and what it can't tell

| Shown | From |
|---|---|
| names, agents, agents' daily limits, revocations, cards | `Registry`: `OwnerRegistered`, `AgentGranted`, `AgentRevoked`, `CardSet`; `Bank`: `TokenLimitSet` |
| payments | `Bank`: `Paid`, `TokenPaid` |
| deals | `Bank`: `Locked` (its kind from the condition and, for `AttestationCondition`, the demand: escrow, bond, bounty; a lock held by `TernJobs` is an ERC-8183 job), `PayeeSet` (a lock opened with no payee: the claim names it), `Claimed`, `Released`, `Refunded`, `GivenBack`; the token and item from `Bank.locks` and `Bank.itemOf` |
| swaps | `SwapCondition`: `Swapped`, with both sides' locks |
| claims | `EAS`: `Attested` with tern's claim schema, read with `getAttestation`; `Revoked` |
| mail | `Inbox`: `Posted`, `Committed` (envelopes and bytes) |

- **Who is behind a payment.** Payments and locks name owner and agent ids, so they show as names. A split or a deal subject shows as a short hash. A claim's issuer is an address; it shows as a name when that address is a name's controller or agent key.
- **Times** are interpolated between each range's first and last block: exact on Base (a block every 2 seconds), close elsewhere.
- **"past its refund time"** is a lock nobody settled or refunded yet after its refund time: its payer can take it back.
- **Mail volume** counts envelopes as the Inbox saw them, whoever sent them. Group posts, receipts and votes are envelopes too.

## Make it yours

- **A price feed.** `swaps --json` gives every settled swap with both sides. A price for a token or an item is an average over a window of these, from distinct owners, above a minimum volume (one trade with yourself must not move it).
- **Alerts.** Run it from cron with `--json` and compare: a new card with a word you care about, a deal of yours past its refund time, a claim about you.
