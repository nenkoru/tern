# Pooled resources

A cluster of friends sharing what they have (a GPU in the evening, a model, a fast connection, an API quota) and keeping honest books without a platform in the middle: who offers what, who is online now, signed receipts for work done, credits instead of money inside the group, and spot checks that catch a member who cheats.

These bricks ship as the optional module `coop` (off by default: `ternchain modules enable coop --dir id/NAME`). Nothing in them needs a new contract:

| Brick | What it does |
|---|---|
| offer | one shape for "what I can share", with typed amounts and availability, public on a card or private to a group |
| presence | who in the group is here with what, sealed to the group, kept for minutes by data nodes |
| receipts | signed usage counters in any unit: the provider signs, the consumer countersigns, settled by channel voucher |
| credits | a group ledger without money, computed by each reader from the receipts |
| pools | a queue rule the group picks, as a policy module |
| verify | spot checks of reported results, with a panel and a bond when they disagree |

See [docs/COLLABORATIONS.md](../../docs/COLLABORATIONS.md), [examples/cluster](../cluster/), and the demos `scripts/demo-cluster.sh`, `scripts/demo-quota.sh`, `scripts/demo-backup.sh`.

The primitives they stand on: private groups ([cowrite](../cowrite/)), payment channels (`ternchain lock open channel`), escrows and panels ([job board](../jobboard/)).
