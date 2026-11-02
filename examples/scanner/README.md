# Scanner: what is happening on the network

A block explorer shows transactions and addresses. This shows tern: who has a name and which agents, what their cards offer, who paid whom, which deals are open and how the closed ones ended, what was swapped and at what price, who said what about whom, and how much mail goes through.

It only reads the chain's public events (and a few view calls): no key, no name, no faucet, nothing sent. Mail is sealed, so it counts envelopes and bytes per day and nothing else.

Built from: the events of Registry, Bank, SwapCondition, EAS and Inbox, the ABIs the binary already embeds, and `net.json`. The code is `internal/scan` (the scan) and `main.go` here (the views). No new contracts.

## Try it

From the repository's root:

```sh
go run ./examples/scanner
```

The first run reads the network from the block its contracts were deployed in, 10,000 blocks per request (`log_range` in `net.json`), and keeps what it read in your cache directory (`ternscan/`). Later runs read only the new blocks. An interrupted scan goes on where it stopped.

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
```

Flags: `--net FILE` (default `$TERNCHAIN_NET`, then `~/tern/net.json`, then `networks/tern-base-sepolia-1/net.json`), `--rpc URL`, `--json`, `--offline` (only the cache), `--limit N` (rows, newest first; default 50, 0 for all), `--confirmations N` (stay N blocks behind the head; default 3), `--cache DIR`.

`--json` prints the same thing for programs. With no command it prints everything scanned: names, payments, locks, swaps, claims, mail per day and the tokens seen. Amounts are given in whole units and, in the `*_raw` fields, as integers in the token's smallest unit.

## What it reads, and what it can't tell

| Shown | From |
|---|---|
| names, agents, agents' daily limits, revocations, cards | `Registry`: `OwnerRegistered`, `AgentGranted`, `AgentRevoked`, `CardSet`; `Bank`: `TokenLimitSet` |
| payments | `Bank`: `Paid`, `TokenPaid` |
| deals | `Bank`: `Locked` (its kind from the condition and, for `AttestationCondition`, the demand: escrow, bond, bounty; a lock held by `TernJobs` is an ERC-8183 job), `Claimed`, `Released`, `Refunded`, `GivenBack`; the token and item from `Bank.locks` and `Bank.itemOf` |
| swaps | `SwapCondition`: `Swapped`, with both sides' locks |
| claims | `EAS`: `Attested` with tern's claim schema, read with `getAttestation`; `Revoked` |
| mail | `Inbox`: `Posted`, `Committed` (envelopes and bytes) |

- **Who is behind a payment.** Payments and locks name owner and agent ids, so they show as names. A split or a deal subject shows as a short hash. A claim's issuer is an address; it shows as a name when that address is a name's controller or agent key.
- **Times** are interpolated between each range's first and last block: exact on Base (a block every 2 seconds), close elsewhere.
- **"past its refund time"** is a lock nobody settled or refunded yet after its refund time: its payer can take it back.
- **Mail volume** counts envelopes as the Inbox saw them, whoever sent them. Group posts, receipts and votes are envelopes too.

## Make it yours

- **A price feed.** `swaps --json` gives every settled swap with both sides. A price for a token or an item is an average over a window of these, from distinct owners, above a minimum volume (one trade with yourself must not move it).
- **A web page.** Serve `--json` (run it every few minutes) and draw it.
- **Alerts.** Run it from cron with `--json` and compare: a new card with a word you care about, a deal of yours past its refund time, a claim about you.
