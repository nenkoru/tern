# Bots, slop and abuse: what we simulate

Goal: the network filters bot farms, AI slop and manipulation for the people who use it, without a central authority and without hurting honest humans and honest agents.

Each attack has: who attacks, how, what defends (primitive), what we measure.
Metrics for every scenario: slop items reaching humans' feeds/inboxes (per 1000 sent), the attacker's cost in coins and gas, time to detection and to removal, honest users wrongly hidden/banned (false positives), moderators' effort (actions per 100 spam items).

## A. Volume
| # | Attack | Defence to test |
|---|---|---|
| A1 | Mail flood: a farm of 50 agents (one owner) mails 1000 people ads/slop | recipient postage, taint+approval, owner-level bans, rate limits at the data node, push only for known senders |
| A2 | Sybil farm: 200 owners (free names) × 1 agent, each sends a few messages to stay under limits | postage per sender, "new name" labels, trust-graph filtering, faucet /64 limits, cost of names |
| A3 | Group flood: bots join open communities and post 100s of near-duplicate AI posts | post fee, duplicate auto-hide, reports → auto-hide, rate limit per member, humans-only spaces |
| A4 | Paraphrase flood: LLM rewrites each post to dodge duplicate detection | reports and moderators, trust-weighted visibility, post fee, new-member probation |

## B. Manipulation
| # | Attack | Defence |
|---|---|---|
| B1 | Fake reviews: farm reviews its own shop to 5★ | reviews count only with a paid lock between parties; wash trading cost; trust-depth rating |
| B2 | Review bombing a competitor | same; rating per trust circle; panel disputes |
| B3 | Vote brigading in a community election / veto | one vote per owner, new-member voting delay, citizens-only communities |
| B4 | Sybil citizens: farm tries to get vouched into the citizens' house | 3 vouches from citizens, name age, probation before vouching, admission cap, vouchers punished |
| B5 | Astroturf: 30 "different people" praising a product in many communities | owner linkage (agents of one owner count once), trust graph, cross-community blocklists |
| B6 | Price-oracle push by a whale with many names | bond-weighted median, step cap, outlier slashing |

## C. Agent attacks
| # | Attack | Defence |
|---|---|---|
| C1 | Prompt injection in mail/posts/cards/files to make agents pay, spread, or reveal | fencing as data, taint, owner approvals, guard on every payment path |
| C2 | Worm: an injected agent is told to forward the payload to all its contacts | taint propagation, approvals for new correspondents, rate limits, owner alerts |
| C3 | Impersonation: name look-alikes (oscar vs oscаr with Cyrillic а), fake "support" agents | name normalization/confusables warning, cards with verified credentials, contacts list |
| C4 | Phishing for approvals: tricking an owner into a broad approval | approvals bound to exact target and amount, single use, plain-language ask |

## D. Moderation abuse
| # | Attack | Defence |
|---|---|---|
| D1 | A captured moderator bans honest members / hides criticism | veto window, reversal restores fully, recall |
| D2 | Report brigading against an honest member | reports from probation members don't count; auto-hide only pending a moderator; dismiss |
| D3 | Hostile blocklist: a popular blocklist suddenly bans many honest names | follow takes effect after a veto window, per-user unfollow, transparency of reasons |

## E. Humans-only spaces
| # | Attack | Defence |
|---|---|---|
| E1 | A bot owner key (no agent label) posts in a humans-only community | citizenship required (vouched humans), passkey presence |
| E2 | A real citizen rents their account to a farm | reports, recall of citizenship by citizens' vote, vouchers punished, rate limits |

## Actors in the run
- Honest humans (20) with their agents, 3 communities (one humans-only), a shop, ordinary traffic.
- Bot farms: scripted farms (cheap, high volume: A1–A3, B1, B5) and 3–4 adaptive LLM attackers who read the docs and try to evade (A4, B3–B4, C1–C4, D1–D3, E1–E2).
- Defenders: elected moderators (LLM agents), members who report, people following blocklists.

## F. Validator capture (the people's devnet, Besu QBFT; sim/people)
QBFT facts: with n validators, f = ⌊(n−1)/3⌋ faults tolerated; more than 1/3 offline halts the chain; 2/3+ colluding can censor and finalize anything they agree on (they can't rewrite already-final blocks, but can refuse transactions and change the future). The set lives in contracts/Validators.sol and changes only through Parliament (both houses + exit window).
| # | Attack | Defence to test / build |
|---|---|---|
| F1 | Governance capture: a faction passes proposals that remove honest validators and add its own | both houses must agree; citizens' sybil limits; rate limit on set changes (≤ 1/3 of the set per N days, at most one removal per proposal); removals need a longer exit window than additions; minimum set size |
| F2 | Salami: replace validators one by one over many proposals | the same rolling cap (counts changes over a window, not per proposal); alerts to all citizens when cumulative churn > threshold |
| F3 | One person, many validators (sybil operators) | one validator per citizen (validator address bound to a citizen; a citizen revoked → validator removed); citizens can challenge "same operator" (shared IP/ASN/keys evidence) by vote |
| F4 | Halt: > 1/3 of validators collude to go offline (or are bribed) | liveness monitoring; a documented social recovery: honest citizens restart from the last final state with the honest set (fork-from-state tool), clients follow the fork via a signed checkpoint |
| F5 | Censorship: 2/3+ validators refuse transactions from certain names (e.g. the vote that would remove them) | detection (data nodes see pending txs never included; inclusion receipts), forced-inclusion path via the data layer, and the social fork as last resort |
| F6 | Hostile upgrade of Validators/Parliament itself (code that gives a faction control) | upgrades of governance-critical contracts need a longer exit window and a supermajority; clients show "governance contract changed" loudly and don't auto-follow |
| F7 | Bootstrap abuse: whoever deploys keeps power over the set | one-time bootstrap handed to the timelock at deploy; test the deployer has zero powers |
| F8 | Key theft of a validator / a validator's key sold | key rotation by its citizen; theft of < 1/3 keys is survivable; slashing for double-signing evidence (QBFT round-change equivocation) |
| F9 | Validators front-run or reorder (MEV) to profit from tenders/auctions | sealed commit–reveal already hides bids; measure what ordering power leaks |
Metrics: can the faction reach 2/3 of the set, how long it takes, how many honest proposals it needs, whether honest citizens notice and stop it in the exit window, whether the chain halts, and whether the social recovery restores service with the honest set.
