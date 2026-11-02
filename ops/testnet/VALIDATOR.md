# Running a validator on the ternchain testnet

Thank you for running a node. This page is all you need. Questions: the operators' group.

The testnet's coins have **no value**. They never become mainnet coins. There is no payment, no token and no promise of value.

## 1. Requirements

- A server: 4 vCPU, 8–16 GB RAM, 100+ GB NVMe SSD (it grows), a **fixed public IPv4**, a stable link. A VPS is fine (about €40 a month in 2026).
- Ubuntu 22.04 or 24.04.
- Docker (simplest). If you run Besu natively instead: Besu 26.6+ needs **JDK 25**.
- Time sync: `apt install chrony`; `chronyc tracking` must show a small offset.
- `python3`, `curl`, `zstd`.
- The `ternchain` and `ternchain-ops` binaries of the release the coordinator names (`ternchain-VERSION-OS-ARCH` and `ternchain-ops-VERSION-OS-ARCH`; or build the second with `go build -tags ops -o ternchain-ops ./cmd/ternchain`). Check them against `SHA256SUMS` and put them side by side as plain `ternchain` and `ternchain-ops`: the validator commands live in `ternchain-ops`, and plain `ternchain` forwards them to it (that is how the scripts below reach them).
- Your founder key (`ternchain owner keygen`) lives on **another** machine, not on this server.

## 2. Firewall

| Port | Open to |
|---|---|
| 30303 TCP and UDP | everyone (p2p) |
| 22 (SSH) | you only, keys only |
| 8545, 8546 (RPC), 9545 (metrics) | nobody from outside: localhost only |

```bash
ufw default deny incoming
ufw allow 22/tcp
ufw allow 30303/tcp
ufw allow 30303/udp
ufw enable
```

Note: Docker publishes ports past ufw. `ternchain-ops validator run` publishes the RPC on 127.0.0.1 only, so that is safe. Do not add `-p 8545:8545` by hand.

## 3. Make your node key (once)

```bash
git clone … && cd tern-chain        # or unpack the release with the ops/ folder
ops/testnet/00-validator-init.sh NAME YOUR.PUBLIC.IP
```

`NAME` is your citizen name (the one you sent with `owner keygen`). It prints two lines. **Send both to the coordinator**:

```
validator: ana=0x…
enode:     enode://…@YOUR.PUBLIC.IP:30303
```

Then back up the key once, encrypted, offline:

```bash
age -p -o validator-key.age ~/tern/validator/key     # keep it on two USB sticks, in two places
```

Never copy the key to another server: two nodes with one key sign twice, and that is grounds for removal. Never send the key to anybody, the coordinator included.

## 4. Start (when you get the genesis)

The coordinator sends `genesis.json`, its **keccak**, and `bootnodes.txt`. Take the keccak from a second place too (the channel and the project site), not only from the same message as the file.

```bash
ops/testnet/20-validator-run.sh --genesis genesis.json --keccak 0xKECCAK \
  --p2p-host YOUR.PUBLIC.IP --bootnodes-file bootnodes.txt
```

It:
- refuses to start if the file's keccak differs (check it by hand with `python3 -I ops/testnet/keccak256.py genesis.json`);
- checks the chain id is 83760;
- runs `ternchain-ops validator run --dir ~/tern/validator --genesis genesis.json --p2p-host IP --bootnodes … --docker --metrics --restart unless-stopped`;
- waits for the node, prints block 0's hash and a first status.

Once `net.json` exists, pass `--genesis-hash` (its `genesis_hash`) as well. All validators must see the same block 0 hash.

The three commands under the script, if you want them by hand:

```bash
ternchain-ops validator init   --dir ~/tern/validator
ternchain-ops validator run    --dir ~/tern/validator --genesis genesis.json --p2p-host IP \
  --bootnodes enode://…,enode://… --metrics --restart unless-stopped
ternchain-ops validator status --rpc http://127.0.0.1:8545
```

## 4a. Header voting (an operator-run testnet)

A genesis made with `validator genesis --selection header`  has the set in its header: it changes by the validators' own votes (`qbft_proposeValidatorVote`), not by the Validators contract or Parliament, and there are no citizens. Such a testnet starts with one validator. Grow it from 1 to 4 **in one sitting**:

1. Each new operator: `ternchain-ops validator init --dir v2` (and v3, v4); send the coordinator the address; start the node with the network's genesis and bootnodes (`validator run --dir v2 --genesis genesis.json --bootnodes …`) and wait until it is synced (`validator status --rpc http://v2:8545` shows the head).
2. All three new nodes synced and peered first. Then the coordinator, on validator 1's RPC (only validator 1 votes while n = 1):
   ```bash
   curl -s -X POST -H 'content-type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"qbft_proposeValidatorVote","params":["0xV2",true]}' http://127.0.0.1:8545
   ```
   At n = 1 one vote is a majority: validator 2 joins at the next block.
3. Immediately, now on validators 1 **and** 2 (a majority of 2 is 2): propose `0xV3` on both. Then on 1, 2 and 3 (a majority of 3 is 2; vote on all three anyway): propose `0xV4`.
4. Check that `qbft_getValidatorsByBlockNumber` with `"latest"` lists 4. Then `qbft_discardValidatorVote` the pending votes.

Never leave the network at n = 2 (either node down halts the chain), and don't stop at n = 3 (no fault tolerance). Removals: the same votes with `false`, never below 4. The RPC needs the QBFT API (`--rpc-http-api=…,QBFT`); `validator run --profile validator` turns it on. The same text: `ternchain-ops help validator`. On such a testnet the citizen items (sections 1, 3 and 8: founder key, citizen name, "be a citizen and vote") don't apply.

## 5. Check

```bash
ops/testnet/40-check.sh --rpc http://127.0.0.1:8545 --net net.json
```

Look for: your validator `PASS` (sealing), peers ≥ 2, chain id 83760, block 0 matches. `ternchain-ops validator status` shows the head, the set, your seals (`by_validator`: `sealed`, `last_sealed`), peers and the upgrade schedule.

## 6. Monitoring

Minimum: `40-check.sh` from cron every 5 minutes, with mail or a message on failure:

```
*/5 * * * * cd ~/tern-chain && ops/testnet/40-check.sh --rpc http://127.0.0.1:8545 --net ~/tern/net.json >/tmp/tern-check.log 2>&1 || mail -s "tern check failed" you@example.org </tmp/tern-check.log
```

Better: the stack in `ops/monitoring/` on your box, or let the central Prometheus read your metrics (`ops/monitoring/README.md`, "Remote nodes"). Join the alert group (Telegram or Matrix) the coordinator names.

Answer alerts within **12 hours**. If you will be away longer, say so in the group.

## 7. Upgrades

- Upgrades are scheduled on chain: `ternchain-ops validator status` → `upgrades` (block, release hash, URI). Nodes do not follow them by themselves: you act.
- Procedure: `ops/besu/UPGRADE.md`. The canary node goes first, then one validator, then the rest, one at a time, all at least 48 hours before the block.
- Snapshot before an upgrade: Besu 26.10 migrates the database **one-way**.
- Check the release file: `sha256sum` of the file at URI must equal the release hash.

## 8. What we expect

- ≥ 99% uptime (seals); answer alerts within 12 hours;
- upgrade before the announced block (at least 7 days' notice);
- take part in drills (every 2 weeks in phase 1; `ops/incident/INCIDENT-RESPONSE.md`);
- be a citizen and vote;
- publish your country, ASN and host;
- no second validator;
- report a possibly leaked key at once (playbook "Key theft");
- 30 days' notice before you leave.

## 9. What to send the coordinator

| When | What |
|---|---|
| Before the genesis | `validator: NAME=0x…`, `enode: enode://…`, your founder line `NAME=0x…` (from `owner keygen`), country, ASN, host, time zone, two contact ways |
| After the start | `block 0 hash` from `20-validator-run.sh`, and "running" |
| After each upgrade | "upgraded to X at HH:MM UTC" |
| Any time | a leaked key, a planned downtime, leaving |

Never send: the node key, the founder key, a shell login.
