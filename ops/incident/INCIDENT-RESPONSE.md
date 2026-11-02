# Incident response: ternchain L1

This plan is for the closed testnet (chain id 83760) and, later, mainnet (8376).

**There is no security council, by design.** Nobody can pause the chain, freeze an account or push a fix alone. No admin key exists. The levers are:
1. each operator's own node (stop, restart, upgrade, roll back);
2. governance votes of both houses (at launch: 7 days to vote, 7 days of exit window; critical changes need 2/3 of both houses and 21 days);
3. social recovery: a fork from the last final state, signed by citizens (`fork-from-state`, `checkpoint`).

So speed comes from people knowing this plan, not from powers.

## Roles

| Role | Who | Does |
|---|---|---|
| Coordinator | the testnet organiser | Keeps the channel and the contact list. Declares incidents, names the incident lead, writes the public notes. Has **no** power over the chain. |
| Incident lead | Whoever sees it first and says "I lead" in the channel; the coordinator can hand it on | Keeps the timeline, asks people to act, decides when the incident is over. One lead at a time. |
| Validator operators (4–7) | Each operator | Run, fix and upgrade their own node. Answer alerts within 12 hours (SEV1: as fast as they can). Never hand their node key to anyone. |
| Infra operators | Archive node, RPC nodes, bootnodes, data nodes, explorer | Keep their service up. The archive node operator runs `fork-from-state` when asked. |
| Citizens | Every citizen | Vote on removals and upgrades. Sign checkpoints. Report censorship. |
| Scribe | Anyone, named by the lead | Writes the timeline (UTC times, who did what) for the post-mortem. |

Rules for everyone:
- Act on your own node only. Ask; never demand another person's key or shell access.
- Write what you do in the channel **before** you do it ("restarting ana's node now").
- Times in UTC.

## Severity

| Level | What | First answer | Examples |
|---|---|---|---|
| **SEV1** | The chain stopped, or funds or the validator set are at risk | as fast as possible; the coordinator within 1 hour while awake, every operator within 12 hours | halt (>1/3 down), more than 1/3 of node keys stolen, a bad upgrade halted the chain, censorship by >2/3 |
| **SEV2** | The chain works but is degraded, or one key is at risk | within 12 hours | one validator not sealing, many round changes, one node key or founder key stolen, all public RPCs down, data node down |
| **SEV3** | One non-critical part fails | within 3 days | one RPC down, explorer down, disk over 80%, certificate expiring |

Alerts (`ops/monitoring/alerts.yml`): critical → SEV1 or SEV2, warning → SEV3, until the lead says otherwise.

## Channels

- **Alerts:** a Telegram (or Matrix) group fed by Alertmanager. Bots only.
- **Operators:** a separate group for people. Every validator, infra operator and the coordinator. Pinned: this plan, the contact list.
- **Backup channel:** an email list or a Signal group with the same people. Use it when the main one fails. **Do not rely on ternchain mail during an incident**: if the chain halts, mail stops too.
- **Public notes:** the announcements channel and a status line on the project site. Short facts, no guesses.
- **Contact list** (kept by the coordinator, not public): name, role, node, two contact ways, time zone, country.

Message template for the operators' group:

```
[SEV1] chain halted at block 123456 (since 10:42 UTC)
Lead: ana. Scribe: ben.
Known: 3 of 7 validators not sealing (cem, dina, eva).
Doing: contacting cem, dina, eva. Next update 11:15 UTC.
```

## Playbooks

### 1. Chain halt (more than 1/3 of validators down)

Signs: `BlockProductionStalled`, `ChainHeadOld`, `validator watch` prints `{"event":"halt",…}`. With 7 validators the chain halts when 3 are down (blocks need 5 seals).

1. **Confirm** from two places: `ternchain-ops validator status --rpc http://127.0.0.1:8545 --blocks 60` on two nodes. Same head on both and `head_age_s` growing: it is a halt, not one node's problem.
2. **Find who is missing.** `by_validator`: the ones with old `last_sealed`. Look at `recent`. Declare SEV1.
3. **Call those operators** (both contact ways). Common causes: host down, disk full, a bad upgrade, the p2p port closed, a clock off (check `chronyc tracking`), a changed IP (`--p2p-host`).
4. **They restart:** `ternchain-ops validator run` with the same flags (it starts the same container on the same data). Do not use `--fresh`: it moves the chain data aside.
5. **Wait.** After the quorum is back, QBFT needs time: its round timer doubled while the chain was halted. In the one-machine demo the first new block came 33–66 s after the nodes returned. If no block after 5 minutes with enough validators up: check peers on each (`peers` in status); then restart the running validators one at a time.
6. **If the missing validators cannot come back** (machine lost, operator gone, key lost) the chain cannot vote them out: votes need blocks. Then it is social recovery (playbook 7). Decide it in the channel when:
   - more than 1/3 of the set is confirmed gone for good, or
   - the halt has lasted **24 hours** with no way to bring them back.
   Start preparing the dump earlier (step 3 of playbook 7) so it is ready.
7. Close when 60 blocks in a row have no round changes and every validator in the set seals.

### 2. One validator not sealing (SEV2)

Signs: `ValidatorNotSealing`, `ValidatorNotProposing`, `RoundChanges`.

1. Tell the operator. They check: `docker ps`, `docker logs --tail 200 tern-…`, disk, peers, clock.
2. They restart it with the same flags.
3. If it stays down for days: the coordinator asks the operator's plan. If the operator is gone, citizens propose its removal: `ternchain gov propose param validators remove ADDRESS`. Removal waits longer than an addition (the removal delay). Add a synced replacement **first** if possible: an absent validator counts against the 1/3 at once.

### 3. Censorship

Signs: `ternchain-ops validator watch --rpc PRIVATE_RPC --report-dir reports/` prints `{"event":"censorship",…}` and writes `reports/censorship-*.json`. The watcher needs the TXPOOL API: run it against a private node (the archive node), not a public RPC.

1. **Check it is real.** Read the report: `blocks_pending`, `blocks_with_room`, `blocks_by_proposer`. Send the raw transaction (`transaction` in the report) to another RPC. If it gets in at once, it was one node's pool, not censorship.
2. **Find who skips it.** The proposers of the blocks with room are in the report. One or two names again and again: those validators.
3. **Put it on chain.** A citizen whose transactions still get in: `ternchain gov validator report reports/censorship-XXXX.json`.
4. **Remove.** `ternchain gov propose param validators remove ADDRESS` for each censoring validator, then the normal vote.
5. **If more than 2/3 censor** (the removal votes do not get in): social recovery (playbook 7). Censored, signed transactions can be replayed on the fork with the same chain id; that is how the blocked votes get in.

Severity: SEV2 for one validator, SEV1 if more than 1/3 of proposers skip it.

### 4. Key theft

First, what can the key still do?
```bash
ternchain-ops validator powers --rpc RPC --address 0xSTOLEN
```

**a. A validator node key** (`DIR/key`). The thief can sign as that validator. With fewer than 1/3 of the keys they cannot make blocks alone. But two nodes with one key sign twice, and that is grounds for removal.
1. The operator stops their node and says so in the channel (SEV2).
2. On a clean machine: `ternchain-ops validator init --dir ~/tern/validator-new`.
3. The citizen bound to the validator rotates it: `ternchain gov validator rotate ~/tern/validator-new` (or the new address).
4. Start the new node with `validator run`. Check that `validator status` shows the new address in the set and that it seals.
5. Find out how it leaked (backup, shell access, cloud account). Write it into the post-mortem.
If more than 1/3 of node keys are stolen: SEV1, treat as playbook 7 with the honest set.

**b. A founder or owner key** (`owner keygen`). It is a citizen's vote and its coins. It can also rotate that citizen's validator binding.
1. SEV2. Tell the channel at once: citizens watch for strange proposals and votes from that name.
2. The owner's guardian starts recovery to a new key: `ternchain recovery start --dir GUARDIAN-DIR OWNER NEW-ADDRESS`, then `ternchain recovery finish --dir GUARDIAN-DIR OWNER` after the wait.
3. If the thief cancels the recovery (`owner cancel-recovery`) or uses the vote: citizens revoke the name by vote: `ternchain gov revoke NAME REASON…`.
4. If the key had agents: `ternchain owner revoke --dir D AGENT --compromised` from the recovered owner.

**c. The deployer key.** The deployer gets only 100 coins of gas. The rest of the supply sits at the Treasury's predicted address (the deployer's nonce 3). So:
- **Before `deploy` ran:** the thief could send transactions from the deployer and take the Treasury's address with their own contract. This is SEV1. Stop. Do **not** deploy. Make a new deployer key and a new genesis (`ops/testnet/10-make-genesis.sh`); every node restarts on it with `--fresh`. `deploy` also refuses a deployer whose nonce is not 0, so check it: `eth_getTransactionCount` of the deployer must be `0x0`.
- **After `deploy`:** `ternchain-ops validator powers --rpc RPC --deployer-key FILE` must show no powers. The key holds only gas left over. SEV3: note it, destroy the file.

**d. The faucet key** (testnet). At most 1% of supply. Move nothing; tell the channel; ask for a new faucet funded by a treasury grant (`gov propose grant`). SEV3.

### 5. Bad upgrade

Signs: nodes crash after an upgrade, a node forks off at the upgrade block, the database migration fails, `fork-from-state` stops working on the archive node.

1. **Stop the rollout.** Nobody else upgrades. (That is why the canary goes first: `ops/besu/UPGRADE.md`.)
2. **Roll back the nodes that failed:**
   - migration not yet done: start the old image: `validator run … --image hyperledger/besu@sha256:OLD --recreate`;
   - after a one-way migration (Besu 26.10 Bonsai): stop, move `DIR/besu` aside, restore the pre-upgrade snapshot, start the old image. It syncs the missed blocks from peers.
   - no snapshot: restore a non-validator's backup (`ops/backup/`) or resync from block 0.
3. **A fork upgrade (new chain rules at BLOCK):**
   - before BLOCK: operators keep the old release and config. Propose a new `validators upgrade` entry with a later BLOCK, or none;
   - after BLOCK, with a split or a halt: everybody returns to the old release **and** the old genesis config, and restarts. QBFT blocks are final, so a halted chain has nothing to undo. If some validators made blocks under the new rules and others under the old, the larger side (more than 2/3) is the chain; the other side resyncs.
4. Write upstream (https://github.com/besu-eth/besu/issues) with logs.

### 6. Data node down

Signs: `DataNodeDown`, `DataNodeServerError`. Mail and batches stop going through that node; clients use the other data node if there is one.

1. The operator checks the service: `systemctl --user status ternchain-net-node` (or their unit), `journalctl --user -u … | grep relay:`.
2. Disk (`--store` folder), the stake, the proxy (Caddy) and its certificate.
3. Restart `ternchain-ops relay serve --da …` with the same flags. Check `curl -s https://dn.example.org/info`.
4. If down for more than a day: tell users in the public notes. Run a second data node (another person) if there is only one.

### 7. Social recovery: the fork-from-state drill

The real procedure and the drill are the same steps. In phase 1 of the testnet we run the drill; in a real incident, the "drill" lines do not apply. Background: `docs/abuse/VALIDATORS.md`.

Rules:
- Fork only when the normal path is blocked (the chain halted for good, or the removal votes are censored).
- Pick BLOCK = the last block every honest node has. QBFT blocks are final: there is no reorg to fear.
- The archive node (FOREST, `validator run --archive`) is the only source of the full state. Without it there is no fork. Keep it running from day one.

Targets (testnet drill; the one-machine run did the fork itself in 19 s):

| Step | Target |
|---|---|
| Alert to "halt confirmed" | 5 min |
| Decision to fork (drill: given) | — (real: 24 h rule, playbook 1) |
| Dump and new genesis | 15 min |
| Three people agree on `genesis_keccak` and `from_hash` | 30 min |
| Checkpoint signatures from more than half of the citizens | 4 h |
| Honest validators make blocks on the fork | 30 min after the signatures |
| RPCs, explorer, clients on the fork | 2 h after the first block |

Steps:

1. **Drill only: announce and make the halt.** Write "[DRILL] fork-from-state at HH:MM UTC" 48 h before. Then three of seven operators stop their nodes:
   ```bash
   ternchain-ops validator stop --dir ~/tern/validator
   ```
   The alert `BlockProductionStalled` must fire. Note the time.
2. **Pick BLOCK.** On the archive node:
   ```bash
   ternchain-ops validator status --rpc http://127.0.0.1:8545 --blocks 5      # "head"
   ```
   Two other operators read the hash of that block on their own nodes:
   ```bash
   curl -s -X POST -H 'content-type: application/json' \
     --data '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0xBLOCKHEX",false]}' http://127.0.0.1:8545
   ```
3. **Dump and make the fork genesis** (archive node operator):
   ```bash
   ternchain-ops fork-from-state --rpc http://127.0.0.1:8545 --at BLOCK \
     --validators ana=0xA…,ben=0xB…,fay=0xF…,gus=0xG… \
     --genesis genesis.json --net net.json --out fork/ --why "drill: 3 of 7 stopped"
   ```
   `--validators`: the honest set (name=address of each citizen's validator; at least 4). It must print `"state_root_matches": true`. It writes `fork/genesis.json` and `fork/checkpoint.json`, and prints `genesis_keccak` and `from_hash`.
4. **Publish and compare.** Put `fork/` where everybody can fetch it. At least three people check:
   ```bash
   python3 -I ops/testnet/keccak256.py fork/genesis.json     # = genesis_keccak
   ```
   and that `from_hash` equals the block hash they read in step 2.
5. **Citizens sign** the checkpoint, one after another (the folder is passed around; signatures go into `fork/checkpoint.sigs.json`):
   ```bash
   ternchain-ops checkpoint sign --checkpoint fork/checkpoint.json --dir ~/tern/id/NAME
   ```
6. **Honest validators start on the fork.**
   - Real incident: on the same folder: `ternchain-ops validator run --dir ~/tern/validator --genesis fork/genesis.json --fresh --p2p-host IP --bootnodes … --metrics --restart unless-stopped` (`--fresh` moves the old chain data aside; it deletes nothing).
   - **Drill:** keep the old chain's data. Make a fork folder with only the key (the same key on another chain is fine for a drill, on the same machine only):
     ```bash
     mkdir -m 700 ~/tern/validator-fork
     cp -p ~/tern/validator/key ~/tern/validator/validator.json ~/tern/validator-fork/
     ternchain-ops validator run --dir ~/tern/validator-fork --genesis fork/genesis.json \
       --p2p-host IP --p2p-port 30304 --rpc-port 8555 --name tern-fork --bootnodes ENODE_OF_FIRST_FORK_NODE
     ```
     Open port 30304 TCP+UDP for the drill. The first fork node starts without `--bootnodes`; its `enode` (printed) is the others' bootnode.
   - Real incident: bootnodes also restart on the fork genesis. With 4 or more validators blocks come at once.
7. **Clients follow** (each person, for their identities):
   ```bash
   ternchain-ops checkpoint verify --checkpoint fork/checkpoint.json --rpc FORK_RPC \
     --genesis fork/genesis.json --follow --net net.json --dirs ~/tern/id/NAME
   ```
   It checks the signatures against the fork's own Registry and Citizens, needs more than half of the citizens (`--min N` for more), and points `net.json` at the fork. Drill: `cp net.json net-drill.json` and use `--net net-drill.json` (and copies of the identity folders).
8. **Infra.** RPC nodes and the archive node restart on the fork genesis (`--fresh`). The explorer needs a new database: block numbers restart at 0. Publish a new `net.json` (`rpcs`, `bootnodes`, the new `genesis_hash`).
9. **After.** Block numbers restart at 0: schedule again anything keyed to a block number (`validators upgrade`). Event logs stay on the old chain.
10. **Drill only: go back.** Stop the fork nodes (`docker rm -f tern-fork`; `rm -rf ~/tern/validator-fork` after the drill log is written). Restart the three stopped validators with their usual `validator run`. The old chain resumes. Do not run `checkpoint verify --follow` against your real `net.json` in a drill: use a copy (`--net net-drill.json`).

## After every SEV1 and SEV2: post-mortem

Within 7 days. Blameless: we fix systems, not people. Publish it to the operators; publish a short version publicly.

```markdown
# Post-mortem: TITLE (SEV?, YYYY-MM-DD)

## Summary
Two or three sentences: what happened, how long, who was affected.

## Impact
- Chain: halted from HH:MM to HH:MM UTC (N blocks lost: none / …)
- Users: what failed for them
- Funds or keys at risk: yes / no

## Timeline (UTC)
- HH:MM  alert X fired
- HH:MM  ana leads; contacts …
- HH:MM  …
- HH:MM  resolved

## Root cause
What broke, and why it could break.

## What went well
## What went badly
## Where we were lucky

## Actions
| Action | Owner | Due | Done |
|---|---|---|---|
|  |  |  |  |

## Numbers for the drill log
Detection time, time to quorum, time to first block, fork timings (if any).
```

Keep the drill log in `ops/incident/drills/` (one file per drill, same template).
