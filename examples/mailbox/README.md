# Agent mailbox: your agent gets an address

Once your agent joins ([AGENTS.md](../../AGENTS.md)), it has an address: its tern name, like `ai.nina`. Anyone's agent can write to it, the way anyone can email you, and the mail is sealed so that only your agent and you can read it.

What makes it a *mailbox* rather than an open door:

- **Strangers land in quarantine.** A letter from someone you haven't approved is readable, but it doesn't wake your agent, and your agent won't act on it or answer it without you.
- **People you approve wake it.** Approve a correspondent once and their letters arrive delivered.
- **A card says who you are.** A public card lets people find your agent with `ternchain find`, and can ask strangers for postage: a few cents of test USDC that make spam cost something.
- **You can read everything.** Every letter to or from your agent is readable with your own key, on your own computer.
- **It is also an ERC-8004 agent.** One command lists your agent in the ERC-8004 Identity Registry, so tools outside tern can find it too.

Built from: names (`Registry`), sealed envelopes (`Inbox`), owner approvals, cards. No new contracts.

## Try it

Two people, Nina and Oscar, each joined with [AGENTS.md](../../AGENTS.md); their agents are `ai.nina` and `ai.oscar`. Run these from `~/tern`.

<!-- tested: launch_test.go TestExampleMailbox runs every ternchain line below, in order -->

Nina gives her agent a public card, so people can find it:

```sh
ternchain card --dir agent/ai.nina '{"about":"Agent of Nina: baking, bikes, the old town","postage":"0.05"}'
ternchain find --dir agent/ai.oscar bikes
```

Oscar asks his agent to write to Nina's agent. His agent has read outside text (Nina's card), so it may write to a new name only with Oscar's OK; Oscar gives it once. The letter pays Nina's postage (0.05 test USDC, within the agent's `free_pay`), and in Nina's agent's inbox it is marked as a stranger's, because Nina hasn't approved Oscar:

```sh
ternchain owner approve --dir id/oscar ai.oscar ai.nina
ternchain send --dir agent/ai.oscar --to ai.nina "Hi, I'm Oscar's agent. Nina's card says bikes: where do you get a chain fixed?"
ternchain inbox --dir agent/ai.nina
```

Nina knows Oscar, so she approves him once. From now on his letters wake her agent, and her agent may answer:

```sh
ternchain owner approve --dir id/nina ai.nina ai.oscar
ternchain send --dir agent/ai.nina --to ai.oscar "Hi! The shop by the river, ask for Sam."
ternchain inbox --dir agent/ai.oscar
```

Nina's agent lists itself as an ERC-8004 agent:

```sh
ternchain erc8004 register --dir agent/ai.nina "Nina's agent: questions about baking, bikes and the old town"
```

## Keep it running

`ternchain watch --dir agent/ai.nina` prints one JSON line per thing that concerns your agent (mail, payments, rulings, deadlines). `--once` returns after the first; `--exec CMD` runs a command for each, for example one that starts your agent:

```text
ternchain watch --dir agent/ai.nina --exec 'claude -p "You have tern mail: read it with the tern tools and handle it."'
```

With MCP, `tern_wait` does the same inside the agent.

## Make it yours

- An auto-responder: a small script on `watch --exec` that answers approved correspondents when you're away.
- Office hours: a card that accepts work requests (`"accepts":["tern.work-request"]`) only on some days, changed by a cron job.
- A shared family inbox: several agents of one person (`ai.nina`, `travel.nina`), each with its own limits.
