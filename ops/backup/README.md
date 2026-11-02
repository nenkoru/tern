# Besu backups

The geth devnet has its own backup procedure (docs/RUNBOOK.md). This folder is for the Besu L1.

| File | What |
|---|---|
| `besu-backup.sh` | stop a non-validator node, archive its data (tar+zstd, or an rsync snapshot), start it, keep N, optional offsite copy |
| `besu-restore-test.sh` | unpack a backup into a temp folder, start a node on it with a new key, check it opens at the right block and syncs |
| `systemd/tern-besu-backup.{service,timer}` | nightly backup |
| `systemd/tern-besu-restore-test.{service,timer}` | monthly restore test |
| `systemd/backup.env.example` | settings for both units (`/etc/tern/backup.env`) |

## What to back up

- **The archive node** (FOREST). It is the only source of a full state dump for `fork-from-state`, and the explorer's source. Its loss means a sync from block 0. Back it up nightly.
- An RPC node or a bootnode: optional. A new one syncs from peers.
- **Not validators.** Do not run `besu-backup.sh` on a validator: it refuses when the node's address is in the set. Stopping a validator costs the chain a sealer. A validator's chain data is the same as any other node's of the same storage format; to restore a validator, restore a Bonsai non-validator's backup (an RPC node or a bootnode) and put the validator's own key beside it.

Why stop the node: Besu keeps its RocksDB database open. A copy of a running node can be inconsistent. The script stops the container (`docker stop -t 120`), copies, and starts it again, also on error. With the archive node down for some minutes, the chain goes on; only the explorer and `validator watch` pause.

## Keys are not data

The archive never contains the node key (`DIR/key`). The script lists only `besu/`, `genesis.json` and a `meta.json`, and excludes `key` too. A restored node gets a new key (`validator init`).

Why the key must not go into the same archive:
- data archives are many, large, kept on servers, copied offsite and handled by scripts. A key inside them leaks with any of these copies;
- a validator key in the wrong place is a stolen validator: anyone with it can sign as that validator. Two nodes with one key sign twice, and that is grounds for removal;
- a data restore must never start a second node with a live validator's key by accident.

So node keys are backed up **apart**: once, when made, encrypted, offline. For example:

```bash
age -p -o validator-key.age ~/tern/validator/key      # or: gpg -c
# copy validator-key.age to two USB sticks / paper (age -p asks for a passphrase); keep them in two places
```

Founder keys (`owner keygen`) and the deployer key never live on a node at all.

## Install

As root, on the archive node's machine:

```bash
apt install zstd rsync python3 curl
install -d -m 0755 /opt/tern /etc/tern
install -m 0755 ops/backup/besu-backup.sh ops/backup/besu-restore-test.sh /opt/tern/
install -m 0600 ops/backup/systemd/backup.env.example /etc/tern/backup.env    # edit: node folder, RPC, offsite
cp bootnodes.txt /etc/tern/bootnodes.txt                                      # one enode per line
cp ops/backup/systemd/tern-besu-*.service ops/backup/systemd/tern-besu-*.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now tern-besu-backup.timer tern-besu-restore-test.timer
systemctl start tern-besu-backup.service          # one backup now
journalctl -u tern-besu-backup -n 20
```

The first restore test right after the first backup:

```bash
set -a; . /etc/tern/backup.env; set +a
/opt/tern/besu-restore-test.sh "$(ls -1t /var/backups/tern-besu/besu-*.tar.zst | head -1)"
# restore-test: OK: … restores and syncs
```

A backup nobody has restored is a hope.

## Settings

See the header of each script. The main ones:

| Variable | Default | What |
|---|---|---|
| `TERN_NODE_DIR` | (required) | the node folder made by `validator init` |
| `TERN_METHOD` | `tar` | `tar`: one `.tar.zst` + `.sha256` per run; `rsync`: snapshot folders with hard links (small daily growth, fast restore) |
| `TERN_KEEP` | 7 | archives or snapshots kept |
| `TERN_OFFSITE` | — | `rsync` target for a copy, e.g. a Storage Box |
| `TERN_UNIT` | — | for a native Besu under systemd instead of Docker |
| `TERN_BOOTNODES_FILE` | — | for the restore test: it must sync from peers |
| `TERN_REF_RPC` | — | for the restore test: compare a block hash with a live node |

Space: an archive node's data grows. Keep at least 2× the data size free on the backup disk for `tar` (one old, one new), and the data size free beside it for the restore test (`TERN_SCRATCH`). The `DiskFilling` alert watches it.

## A real restore

1. Stop the broken node: `docker stop tern-…`.
2. Move its data aside: `mv DIR/besu DIR/besu.broken-$(date +%F)` (delete nothing yet).
3. Unpack: `zstd -dc besu-STAMP.tar.zst | tar -xf - -C DIR besu` (keep the node's own `key` and `validator.json`).
4. Start: `docker start tern-…` (or `ternchain-ops validator run` with the same flags). For a FOREST backup, the node must run with `--archive`.
5. Check: `ternchain-ops validator status --rpc http://127.0.0.1:8545`: the head moves up to the network's head.

To restore a **validator** after a disk loss: restore a backup of a Bonsai node (an RPC node or a bootnode) into `DIR/besu`, put the validator's key (from its offline backup) into `DIR/key`, start. A FOREST (archive) backup does not fit: a validator runs Bonsai. The simplest path for a validator is a fresh sync from peers; at 1 s blocks and light load it is not slow while the chain is young.
