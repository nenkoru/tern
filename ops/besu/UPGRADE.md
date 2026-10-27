# Upgrading Besu (canary procedure)

Every ternchain L1 node runs Besu. There is no second QBFT client: a bad Besu release can stop the whole network. So we upgrade in stages, with a snapshot before each step and a way back.

The pinned release is in `ops/besu/VERSION` and in `const besuImage` in `validator.go`. Today: `hyperledger/besu:26.9.0`. The next candidate: 26.10.1 (22 Oct 2026).

## What is special about 26.10

- **One-way database migration.** On its first start, 26.10 migrates a Bonsai database. After that, 26.9 cannot open it. Take a snapshot **before** the first start. Without a snapshot, the only way back is a full resync.
- **JDK 25.** Besu 26.6 and later need Java 25. This matters only for a native install. The Docker image has its own Java.
- **FOREST will go away.** Our archive node uses FOREST (`validator run --archive`). `fork-from-state` needs it (`debug_accountRange`, `debug_storageRangeAt`). Besu plans to replace FOREST with Bonsai Archive. On every upgrade, test `fork-from-state` on the canary archive node (step 3).
- **Repository moved.** Releases are now at https://github.com/besu-eth/besu/releases. Check that the Docker image name has not moved too.

## Two kinds of upgrade

1. **A client-only upgrade** (same chain rules, e.g. 26.9.0 → 26.10.1 with no new fork in the genesis config). Nodes can upgrade one at a time. Never have more than one validator down at a time. With 7 validators the chain survives 2 down, but keep the second as a margin.
2. **A fork upgrade** (new chain rules at a block, e.g. a new fork time added to the genesis config). Every node must run the new release **and** the new config before the block. A node without it splits off at the block. If more than 1/3 of validators miss it, the chain halts at the block.

Both kinds are scheduled on chain, so that everybody sees the plan:

```bash
ternchain gov propose param validators upgrade BLOCK HASH URI
```

Both houses vote; then the exit window runs. `ternchain-ops validator status` shows the entry under `upgrades` with `in_blocks`. Nodes do **not** follow the schedule by themselves: each operator acts. Pick BLOCK at least "exit window + 7 days" ahead (at launch: 7 d vote + 7 d exit, so more than 21 days). At 1 s blocks, one day is 86,400 blocks.

## Step 0: record the release (HASH and URI)

HASH is 32 bytes. We use the sha256 of a small release manifest, so anyone can check it.

1. Pull the image and read its digest (see `VERSION`).
2. Write `release-26.10.1.json`:
   ```json
   {
     "besu": "26.10.1",
     "image": "hyperledger/besu@sha256:<digest>",
     "upstream": "https://github.com/besu-eth/besu/releases/tag/26.10.1",
     "genesis_config_change": "none",
     "ternchain": "<version and SHA256SUMS line, if the client changes too>",
     "notes": "Bonsai migrates one-way on first start: snapshot first"
   }
   ```
   For a fork upgrade, put the exact config change (the new keys and values) into `genesis_config_change`.
3. `sha256sum release-26.10.1.json` → HASH is `0x` + that hex.
4. Publish the file at a stable URL (the project site, a git tag, or `/get/`). That URL is URI.
5. Propose: `ternchain gov propose param validators upgrade BLOCK 0x<sha256> https://…/release-26.10.1.json`.

Operators check the file before they install: `sha256sum` of the downloaded file must equal the HASH in `validator status`.

## Step 1: pin

- Change `ops/besu/VERSION` (image and digest).
- Change `const besuImage` in `validator.go` in the same commit. Build and release the client.
- Write the change into the channel: version, digest, the reason, the planned BLOCK.

## Step 2: snapshot

On every node, before its first start on the new release:

- Non-validator nodes (archive, RPC, bootnodes): `ops/backup/besu-backup.sh` (it stops the node, archives the data folder, starts it again).
- Validators: stop the node, copy `DIR/besu` aside, start it again. Do it one validator at a time. Do **not** put the node key (`DIR/key`) into the same archive (see `ops/backup/README.md`).

```bash
docker stop -t 120 tern-xxxxxxxx
tar -C ~/tern/validator -I 'zstd -T0' -cf ~/besu-before-26.10.tar.zst besu genesis.json
docker start tern-xxxxxxxx
```

## Step 3: canary (a non-validator node first)

Use a node that does not seal: an RPC node or a spare full node. Best: also the archive node, after its own snapshot.

```bash
ternchain-ops validator run --dir ~/tern/canary --genesis genesis.json \
  --image hyperledger/besu@sha256:<new digest> --recreate \
  --p2p-host PUBLIC.IP --bootnodes "$(paste -sd, bootnodes.txt)" --metrics --restart unless-stopped
```

`--image` and `--recreate` are in `validator.go` (the container is made again with the same data). Watch it for **at least 24 hours**:

- it starts, migrates, and follows the head (`ternchain-ops validator status --rpc http://127.0.0.1:8545`: `head_age_s` small, `peers` > 0);
- no errors in `docker logs`;
- RPC answers the same as an old node (`eth_getBlockByNumber` for a few blocks, `eth_call` on a contract);
- on the archive canary: run a `fork-from-state` dry run into a scratch folder (see `ops/incident/INCIDENT-RESPONSE.md`, the drill). It must print `"state_root_matches": true`.

If anything fails: stop. Do not go on. Roll back the canary (below) and report upstream.

## Step 4: one validator

Pick one validator whose operator is online. Snapshot, then upgrade it the same way. Watch for **24 hours**:

- it still seals: `validator status --blocks 60` shows its `last_sealed` near the head, and it proposes its share;
- no new round changes;
- memory and disk are stable (Grafana).

## Step 5: the rest, before BLOCK

One validator at a time, at least 1 hour apart, each one checked with `validator status` before the next. All done at least **48 hours** before BLOCK. The coordinator keeps a table in the channel: operator, upgraded (yes/no), time.

For a fork upgrade, also replace the genesis config on every node with the new one before BLOCK (see the release manifest), then restart.

## Rollback

- **Before the database migrated** (the new release failed at start): run the old image again: `validator run … --image hyperledger/besu@sha256:<old digest> --recreate`.
- **After a one-way migration** (26.10 Bonsai): the old release cannot open the new database. Stop the node, move `DIR/besu` aside, restore the snapshot from step 2, start with the old image. The node then syncs the missed blocks from its peers.
- **No snapshot:** stop, move `DIR/besu` aside, start the old image on an empty data folder. It syncs from block 0 (slow for an archive node). Or restore a snapshot of another non-validator node (the data folder holds no key).
- **A fork upgrade that turns out bad before BLOCK:** operators keep the old release and the old config. A new proposal moves or cancels the upgrade. If BLOCK has passed and the chain halted, everybody returns to the old release and config and restarts (see the "bad upgrade" playbook in `ops/incident/INCIDENT-RESPONSE.md`).

Keep the old image pulled (`docker image ls`) until the upgrade has run a week.
