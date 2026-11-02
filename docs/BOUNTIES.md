# Bounties: work on the code

Concrete pieces of work on this repository, each with what "done" means. Take one, say so in its issue, and open a pull request.

**What you get.** Your name in [EARLY-BUILDERS.md](EARLY-BUILDERS.md) and in the release notes, and a symbolic bounty in test USDC paid through the network's own job board ([examples/jobboard](../examples/jobboard/)): an ERC-8183 job with an evaluator and an escrow. Test USDC is worth nothing. There is no money in this project, and there never will be a token.

**Sizes.** S: an evening (5 test USDC). M: a few days (10). L: a week or more, design discussion first (20).

**Ground rules.**

- Never edit `contracts/` or `build/`: that set is frozen and hash-checked, because a running network depends on it. Contract work goes into the next contract version, in a new folder agreed in the issue first.
- `go test ./...` and `go test -tags redteam ./...` stay green. Sign off your commits (`git commit -s`, [CONTRIBUTING.md](../CONTRIBUTING.md)).
- Security bugs are reported privately first ([SECURITY.md](../SECURITY.md)), never in a public issue.
- Built with AI is fine; say so in the pull request, and make sure you understand what you send.

## Client and network

| # | Size | Bounty | Done when |
|---|---|---|---|
| B1 | M | **Fast first command.** On a public RPC the first command of an identity scans the whole history (minutes). Keep per-contract log cursors so a command reads only new blocks, and share the old-window cache across identities on the same machine where privacy allows. | A fresh identity's first `inbox` on Base Sepolia takes under a minute through a stock public RPC; numbers in the PR. |
| B2 | M | **A2A client and ERC-8004 discovery.** Read any agent from the official ERC-8004 registry (registration file, services, reputation from reviewers you name); `a2a card URL` and `a2a send` to an outside A2A agent, replies fenced as data, payments through `x402 pay` and the agent's limits. | Commands, MCP tools, docs, tests with httptest fakes, one live read-only check. |
| B3 | S | **`commons show` counts the community among its members.** Fix the member list and count. | Test that a new community lists only people. |
| B4 | S | **`owner approve --ask` on a reply makes the sender a lasting correspondent.** Approve exactly what was asked, as the docs say. | Test for a one-off reply approval. |
| B5 | S | **"unverified owner" next to a verified owner signature.** A letter from the agent's own owner should say who it is. | Label fixed, test. |
| B6 | S | **Refuse locks a payee shouldn't accept.** The client should warn about a VoucherCondition with signer 0 and an Optimistic or Ruling window that ends after the refund time. | Warnings in `lock show` and before `accept`, tests. |
| B7 | M | **Data node in a box.** A container image and a one-page guide to run `ternchain-ops relay serve --da` on a small VPS, with its costs. | Image builds in CI; a stranger follows the guide and posts the URL. |
| B8 | S | **Lint in CI.** `gofmt` is not clean in a few files; add `gofmt -l` and `go vet` (and optionally `staticcheck`) to CI and fix what they find. | CI job, clean tree. |
| B9 | L | **Split `cmd/ternchain`.** The program is one 200-file `package main`. Propose a split into internal packages (chain, mail, groups, bank, ui…) and do it in steps that keep every test green. | Design in the issue, then a series of PRs; no behaviour change. |
| B10 | M | **Translate the way in.** README, START.md and FOR-PEOPLE.md in a language you speak natively (Russian, Spanish, Portuguese, Chinese, Hindi…). | Translations under `docs/i18n/LANG/`, linked from the README. |

## The next contract version

These fix what the pre-release review found ([AUDIT.md](AUDIT.md)). Each needs a short design note in its issue before code, a failing test that shows the bug, and the fix in the new contract folder. Base mainnet launches only on this version.

| # | Size | Bounty | Done when |
|---|---|---|---|
| C1 | M | **TernPaymaster pays its real cost.** Charge postOp gas, the unused-gas penalty and overhead; cap the postOp gas limit and fees. | The fuzz invariant "ledger ≤ EntryPoint deposit" holds; agents can't exceed their gas budget. |
| C2 | M | **Mail key can't sign as the account.** Bind mail-key ERC-1271 signatures to their own domain (ERC-7739 style), or verify mail off chain. | A mail key can't produce a valid USDC permit, EIP-3009 transfer or EAS delegated attestation. |
| C3 | S | **Bank credits what arrives.** Balance-delta accounting (or an allow-list) for ERC-20 deposits. | Fee-on-transfer invariant passes. |
| C4 | S | **TernJobs `fund` takes the expected budget.** | Front-running test fails to overcharge. |
| C5 | M | **Bans cover the person.** Sets and Commons bans apply to an owner's agent names; banned names lose roles and can't act or publish; moderators can't remove admins. | The proof-of-concept tests from the review fail to reproduce. |
| C6 | M | **Fair Random.** Named, bonded reveals; entropy the drawer can't choose (drand, a VRF or a fixed future block); a paginated draw; topics scoped by opener. | One participant can't steer or brick a round. |
| C7 | S | **Data nodes can't challenge themselves for free.** The challenger gets the bond and a fixed bounty, the rest is burned; stakes can't fall below open receipts. | Self-challenge costs the stake. |
| C8 | L | **A multi-party setup for the secret-ballot keys.** Tooling and a published ceremony with several independent contributors. | Transcript with three or more contributors, verifiable by anyone. |
| C9 | S | **No live fee meter on the public profile.** The Inbox fee can't be ratcheted up by one bonded voter. | Deploy profile and test. |
