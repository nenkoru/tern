# Runbook: the shared devnet, Season 3

For the operator of the hosted test network (`https://tern.example.org`; server ports: RPC 8755 and data node 8655, behind `deploy/apply-https.sh`'s proxy; admin 8756, local only, as in the unit below and `deploy/backup.sh`). Paths below are on the server, as the service user.

> **Note for this repository.** This runbook records how the first hosted test network was run. The `deploy/` scripts it names (the HTTPS proxy `apply-https.sh`, `backup.sh` and its systemd units) are operator-specific and are not part of this repository; `ops/backup/` covers backups for the Besu L1. Replace `tern.example.org` with your own host.

Operator commands (devnet, faucet, faucet serve, advance, deploy, validator, fork-from-state, checkpoint, ceremony, simulate, relay serve) are in `ternchain-ops`: `ternchain-ops-VERSION-OS-ARCH` from the release (`scripts/release.sh` builds it next to each `ternchain-…`), or the same code built with `go build -tags ops -o ternchain-ops .`. Put it beside `ternchain` in the bin directory as plain `ternchain-ops` (plain `ternchain` forwards those commands to it). The faucet's limits are flags (`--faucet-per-hour 100`, `--faucet-per-ip-day 5`); what clients of a shared network refuse is net.json's `refuse` list (`mint`, `random`, `pot`, `passkey`, `fiat-real` by default), editable there.

```
~/ternchain-net/bin/<version>/ternchain     binaries (ternchain and ternchain-ops side by side); ~/ternchain-net/bin/current -> <version>
~/ternchain-net/deploy/backup.sh            from this repo's deploy/
~/ternchain-net/s3/chain                    the chain's datadir (holds operator.json: the faucet key)
~/ternchain-net/s3/net.json                 public: hand it out    (net.operator.json beside it: never publish)
~/ternchain-net/s3/dastore                  the data node's batches
~/ternchain-net/s3/id/nodeco                the data node's identity (guardian in s3/guardian/nodeco)
~/ternchain-net/upload/                     what apply-https.sh publishes to /get/ (nothing secret, ever)
~/ternchain-net/s3/world/                   the populated world's keys and state (never published)
~/ternchain-net/s2-retired/                 Season 2, moved aside (archive in ~/ternchain-backup/season-2/)
```

## Why Season 2 is a reset

The Season 1 chain predates the hash locks, pledges, the two houses, the Directory, the Treasury and the pay-unless-disputed condition. `--datadir` reopens a chain and deploys nothing, so a new binary on the old datadir would run the old contracts (it now logs a WARNING when the contract sets differ). Season 2 starts on a new datadir. After it, new contracts can come by vote (`gov propose upgrade`), which needs citizens, which need founders: so the founders come first.

## Season 3 (2026-10-26): what runs now

Season 3 is the redeploy below done with v0.3.0 (`ternchain` and `ternchain-ops` in `bin/v0.3.0`, `current` points at it) on a new datadir `s3/` with an empty dastore; Season 2 is in `s2-retired/` and `~/ternchain-backup/season-2/`. The flags are Season 2's with `--network season-3` and `--trusted-proxy 127.0.0.1` on both the chain and the data node (units below). **Chain id 1337**, the devnet default (no `--chain-id`): it is in `zk.DevnetChainIDs` (1337, 31337), so `deploy` put the devnet zk verifiers on chain, net.json names no `"ceremony"`, and clients prove with the embedded devnet keys without refusing; the faucet works as on any devnet. No founders (`--genesis`) this season. The world of scripted bots (carriers, services, communities, ai.dima's quest) runs as `ternchain-world` and `ternchain-world-tick.timer` (its scripts are not part of this repository).

## Redeploy needed for the audit-5 fixes (done: Season 3)

The Season 2 server devnet ran the contracts of an earlier build. `contracts_hash` changed with the audit-5 fixes (the zk verifiers' CEREMONY and devnet-only constructor, rulings bound to their lock, the two-step validator add, the Merkle node tags, the voucher digest, held pledges, and others), and `--datadir` reopens a chain without deploying anything, so the server needs a redeploy (a new datadir, as in "Why Season 2 is a reset") to get them. Compare `ternchain version`'s `contracts_hash` with the one in `s2/net.json`: they differ until then (the log says so on every start).

## Before an L1 deploy: the ceremony

A people's L1 or any network with value must not use the embedded zk keys: they come from a single-party setup, carry `CEREMONY = 0`, and `deploy` refuses them off a devnet chain id (the verifier contracts refuse too). So, before `ternchain-ops deploy`:

1. Run the ceremony with the contributors (PRIVACY.md, "Trusted setup": `ternchain-ops ceremony init`, each `contribute`, `init --phase2 --beacon`, each `contribute`, `verify`, `finalize --beacon HEX --keys DIR`). Publish every transcript file and its hash as it comes.
2. `node contracts/build-zk.js DIR` (writes `PoolVerifier.bin`, `VoteVerifier.bin`).
3. `TERNCHAIN_ZK_KEYS=DIR ternchain-ops deploy …`: net.json gets `"ceremony"`.
4. Hand out net.json with `DIR` beside it as `zk-keys/` (or net.json `"zk_keys"`, a path relative to it). Clients without the keys refuse to prove; clients with other keys refuse too.

Phase 1 is our own (no import of a public powers of tau), so say so when announcing. A devnet (chain ids in `zk.DevnetChainIDs`) needs none of this.

An operator-run test network (the L1 testnet) may skip the multi-party ceremony: `ternchain-ops ceremony solo --keys DIR` makes the keys in one go (the operator the only contributor), then `node contracts/build-zk.js DIR` and `TERNCHAIN_ZK_KEYS=DIR ternchain-ops deploy … --test-network`. `deploy` reads `DIR/transcript.mpc` and refuses fewer than 3 contributions per phase on the mainnet chain id (8376) and on any non-devnet chain deployed without `--test-network`. Say it in the notice (the default `--test-network` notice does): shielded transfers and secret votes on that network are not private from the operator.

**Distributing the test network's zk keys.** They are public (proving and verifying keys, `CEREMONY`, `transcript.mpc`, `SHA256SUMS`). Publish them with each release, beside `net.json` at `/get/`: `tar czf zk-keys.tar.gz zk-keys/` and its line in the release's `SHA256SUMS`. People unpack it next to their `net.json` (`tar xzf zk-keys.tar.gz` gives `zk-keys/`; the client looks there, or at net.json's `"zk_keys"` path, or `TERNCHAIN_ZK_KEYS`) and check `zk-keys/SHA256SUMS`. Only shielded transfers (`private …`) and secret votes need them; everything else works without. A client with no keys, or with keys of another `CEREMONY`, refuses to prove. Keys change only with a new deploy (a reset): publish the new archive with the new `net.json`.

## Before the day

1. **Things only you provide:** your name for `--operator`, a contact for `--contact` (an email or a Telegram handle people may use for notices), the founders (at least 4 people), and a short answer from a lawyer on sanctions before inviting anyone resident in Russia or another sanctioned place (TERMS.md §9).
2. **Founders.** Each founder, on their own machine, with the new binary:
   `ternchain owner keygen --out ~/tern/founder.key --name NAME`
   and sends you the printed line `NAME=0x…`. One person, one key: if you hold several founders' keys, you (with the faucet's coins) can pass proposals alone, and the citizens' house means nothing.
3. **Build** on your laptop: `scripts/release.sh v0.2.0` → `dist/v0.2.0/` with twelve binaries (`ternchain-…` and `ternchain-ops-…` for six platforms) and `SHA256SUMS`. `ternchain version` prints the version, commit, build date and `contracts_hash`.
4. **Announce 48 h ahead** in the friends' channel: the date, "Season 1 ends: names, coins and mail are deleted; get the new files from /get/ and redo START.md's 5 steps; founders: keep your founder.key".

## The redeploy

1. **Freeze:** `systemctl --user stop ternchain-net-node` (the data node first), then `systemctl --user stop ternchain-net`.
2. **Back up Season 1, stopped:** `mkdir -p ~/ternchain-backup/season-1 && tar czf ~/ternchain-backup/season-1/s1-$(date -u +%F).tar.gz -C ~/ternchain-net <the old datadir> <the old dastore> net.json net.operator.json id upload bin` then `sha256sum` it. Keep it until Season 2 has run a week.
3. **Move aside, delete nothing.** Rename the old datadir and dastore (e.g. `mv ~/ternchain-net/data ~/ternchain-net/s1-data`). **Season 2 gets an empty dastore**: a data node re-queues stored batches the chain doesn't know, so an old dastore would re-post Season 1 mail into Season 2. `mkdir -p ~/ternchain-net/s2/dastore`.
4. **Install the binaries:** copy `ternchain-v0.2.0-linux-amd64` to `~/ternchain-net/bin/v0.2.0/ternchain` and `ternchain-ops-v0.2.0-linux-amd64` to `~/ternchain-net/bin/v0.2.0/ternchain-ops`, `chmod +x`, `ln -sfn v0.2.0 ~/ternchain-net/bin/current`. Check `~/ternchain-net/bin/current/ternchain version`.
5. **Start the chain** (unit `ternchain-net`, below). The flags, once:
   ```
   ternchain-ops devnet --datadir ~/ternchain-net/s2/chain --net ~/ternchain-net/s2/net.json \
     --listen 127.0.0.1:8755 --admin 127.0.0.1:8756 \
     --public-url https://tern.example.org --relay-url https://tern.example.org/da \
     --network season-2 --shared --operator "YOUR NAME" --contact "YOUR CONTACT" \
     --trusted-proxy 127.0.0.1 \
     --genesis name1=0x…,name2=0x…,name3=0x…,name4=0x…
   ```
   Leave `--epoch` at its default (86400): agents' limits then reset at 00:00 UTC, as START.md says. Restarts may drop the flags: `--network`, `--relay-url`, `--public-url`, `--operator`, `--contact` and `--shared` are kept from the first start (the log says so); `--genesis` only matters on the first start. Check net.json: `"mode": "shared"`, the notice names you, `contracts_hash` equals `version`'s.
6. **The data node:**
   ```
   cd ~/ternchain-net/s2
   ternchain owner init --net net.json --dir id/nodeco --name nodeco --guardian-dir guardian/nodeco
   ternchain bank deposit --net net.json --dir id/nodeco 50
   ```
   then start unit `ternchain-net-node`: `ternchain-ops relay serve --da --listen 127.0.0.1:8655 --public-url https://tern.example.org/da --store ~/ternchain-net/s2/dastore --price 0.0001 --dir ~/ternchain-net/s2/id/nodeco --net ~/ternchain-net/s2/net.json --trusted-proxy 127.0.0.1`. Caddy's `handle_path /da/*` strips the prefix, so the node stakes the `/da` URL.
7. **Publish /get/:** put into `~/ternchain-net/upload/` the six `ternchain-…` binaries (and `ternchain-ops-…` for whoever runs a data node) and `SHA256SUMS` from `dist/v0.2.0/`, `START.md`, `FOR-AGENTS.md`, `TERMS.md`; copy `s2/net.json` to `~/ternchain-net/net.json` (what apply-https.sh publishes). Then `grep -l faucet_key ~/ternchain-net/upload/* ~/ternchain-net/net.json` must print nothing (the binaries contain the word: check only the non-binary files), and run `deploy/apply-https.sh`.
8. **Smoke test from outside** (a laptop with only the published files):
   - `curl -s -X POST -H 'content-type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' https://tern.example.org`
   - `curl -s https://tern.example.org/da/info`
   - START.md's 5 steps with a throwaway name, one `send`, then `read ID`: `data_node` is the public `/da` URL; `ternchain version` shows `matches_network: true`.
   - `TERNCHAIN_MODULES=fiat ternchain fiat paid x 1 EUR sepa --dir …` is refused (shared mode: only `TEST`). Without `TERNCHAIN_MODULES=fiat` it is refused because the module is off, which proves nothing about the network.
9. **Founders claim:** each runs `ternchain owner init --key ~/tern/founder.key --dir id/NAME --name NAME --guardian-dir …` (it says "a founding citizen") and `ternchain gov claim --dir id/NAME`. On a devnet the first term is 7 days: within it, every founder needs `gov vouch NAME` from K = 3 other citizens, or lapses.
10. **Backups on:** copy `deploy/` to `~/ternchain-net/deploy/`, install the timer (below), run `systemctl --user start ternchain-backup` once, then the restore test.
11. **Announce** "Season 2 is live" with the SHA256SUMS line of each platform, the season, date, version, `contracts_hash` and the founders.
12. **Rollback** if the smoke test fails: stop both units, `ln -sfn <old version> ~/ternchain-net/bin/current`, point the units back at the Season 1 paths, start. Nothing was deleted in step 3.

## Units (systemd --user)

`~/.config/systemd/user/ternchain-net.service`:
```
[Unit]
Description=ternchain devnet (season 3)
[Service]
ExecStart=%h/ternchain-net/bin/current/ternchain-ops devnet --datadir %h/ternchain-net/s3/chain --net %h/ternchain-net/s3/net.json --listen 127.0.0.1:8755 --admin 127.0.0.1:8756 --public-url https://tern.example.org --relay-url https://tern.example.org/da --network season-3 --shared --operator "Оскар" --contact "через того, кто вас пригласил" --trusted-proxy 127.0.0.1
Restart=on-failure
[Install]
WantedBy=default.target
```
`ternchain-net-node.service` is the same with the `ternchain-ops relay serve …` line of step 6 and `After=ternchain-net.service`. A restart with `--datadir` is safe: same chain, nothing deployed.

## Backups

- `deploy/backup.sh` snapshots without stopping anything: it pauses block production on the admin port (`/pause`, at most 5 minutes; reads keep working, transactions wait), copies the datadir until no file changed during the copy, resumes, adds the dastore, net.json, the operator file and `TERN_EXTRA` (the node's `id/`), and writes `~/ternchain-backup/tern-STAMP.tar.gz` with a `.sha256`. It keeps the newest 7 (TERMS.md promises no more).
- Nightly: `cp deploy/ternchain-backup.service deploy/ternchain-backup.timer ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user enable --now ternchain-backup.timer && loginctl enable-linger $USER`. Check with `systemctl --user list-timers` and `journalctl --user -u ternchain-backup`.
- The unit in the repo still names Season 2 (`TERN_SEASON_DIR=%h/ternchain-net/s2`, `TERN_EXTRA=%h/ternchain-net/s2/id`). In the installed copy set `TERN_SEASON_DIR=%h/ternchain-net/s3` and `TERN_EXTRA=%h/ternchain-net/s3/id %h/ternchain-net/s3/world` (space-separated; the world's keys), then `systemctl --user daemon-reload`.
- The archives hold the faucet key and the node's key: they stay on the server (or an encrypted disk), mode 0600.

### Restore test (once after setup, then monthly)

`TERN_BIN=~/ternchain-net/bin/current/ternchain ~/ternchain-net/deploy/backup.sh --restore-test ~/ternchain-backup/tern-STAMP.tar.gz` unpacks into a scratch folder, starts a devnet on it on ports 18755/18756, checks that blocks are served and the Registry has code, and stops it. To check a name and a balance by hand, start it the same way, then `ternchain bank balance NAME --net /tmp/…/restored.json --dir <a copy of an identity>`. A backup nobody has restored is a hope.

A real restore: stop both units, move the broken datadir and dastore aside, unpack the archive's `chain/` and `dastore/` into `s3/` (and `world/`, with `ternchain-world` stopped), start the chain, then the node.

## Watching it

- Block height advances (`eth_blockNumber` twice a minute apart); `/da/info` answers; the faucet's balance (`eth_getBalance` of the faucet address in net.operator.json) stays above 10%; disk under `s3/` below 80%; `relay:` errors in `journalctl --user -u ternchain-net-node`.
- journald `MaxRetentionSec=14day`; Caddy access logs at most 7 days, without bodies; Caddy must pass `X-Forwarded-For` **and** the devnet and data node must name it with `--trusted-proxy 127.0.0.1` (the default trusts no proxy: without the flag every friend shares the proxy's address and its 5 faucet grants a day).

## Any test network: the devnet and data node, flag by flag

The two commands of a hosted network (the Season 2 flags above are one instance):

```bash
ternchain-ops devnet --datadir /var/lib/tern/chain --net /var/lib/tern/net.json \
  --listen 127.0.0.1:8545 --admin 127.0.0.1:8546 --public-url https://rpc.example.org \
  --relay-url https://dn.example.org --network season-2 --shared --operator "NAME" --contact "HOW TO REACH YOU" \
  --trusted-proxy 127.0.0.1 \
  --genesis alice=0x…,bob=0x…,carol=0x…,dave=0x…
ternchain-ops relay serve --da --listen 127.0.0.1:8650 --public-url https://dn.example.org \
  --store /var/lib/tern/dastore --price 0.0001 --dir /var/lib/tern/id/nodeco --net /var/lib/tern/net.json \
  --trusted-proxy 127.0.0.1
```

- **Persistence.** With `--datadir` the chain is geth's database on disk (archive mode, hash scheme: every block's state is written as it is made) and the first start writes `DIR/operator.json` (the genesis and faucet key, the deployment). A restart opens the same database on the same genesis and deploys nothing: names, balances and locks stay. Without `--datadir` the chain lives in memory and a restart is a new network. A restart without `--network`, `--relay-url`, `--public-url`, `--operator`, `--contact` or `--shared` keeps what the first start wrote. `deploy/backup.sh` snapshots the datadir without stopping it (it pauses blocks for the copy) and restore-tests a snapshot; [RUNBOOK.md](RUNBOOK.md) has the season redeploy.
- **Shared mode and the notice.** `--shared` writes `"mode":"shared"` into net.json; clients then refuse real currencies in `fiat` (only `TEST`), minting, randomness rounds, the old `pot` commands and `lock open pot` (game pots), and passkeys. `lock open bounty` is allowed. Private payments (stealth addresses, the pool) are standard and allowed. `--operator` and `--contact` go into the notice that net.json, `owner init` and the faucet's reply carry. `ternchain version` prints the binary's version and the hash of its contract set; net.json records the set the network was deployed with.
- **Founders.** The citizens' house needs founders (more than K = 3), or no vote can ever pass. Each founder runs `ternchain owner keygen --out FILE --name NAME` offline and sends the `NAME=0x…` line; the operator starts the network with `--genesis`; each founder then runs `owner init --key FILE` and `gov claim`.
- **Two files.** `--net` is the public file to hand out (RPC URL, chain id, contracts, `faucet_url`, `relay`). Beside it, `NET.operator.json` (mode 0600) holds the faucet key and the admin URL; keep it on the server. `ternchain-ops faucet` and `advance` need it.
- **Private actions.** `relay serve` also relays pool withdrawals (`POST /pool/withdraw`, fee `--pool-fee`, default 0.001) and secret votes (`POST /gov/vote`); `--asp NAME` keeps an association set by the `clean24h` rule. The zk keys come from `TERN_ZK_SETUP=1 go test -run TestZKSetup` (then `node contracts/build.js`), a single-party devnet setup.
- **Faucet.** `POST https://rpc.example.org/faucet {"address":"0x…"}` gives `--faucet-coins` (20) test coins, once per address a day, at most 5 a day per IPv4 address or IPv6 /64 and 100 an hour in all (the log is kept in the datadir). `owner init` calls it when the machine has no faucet key (`--faucet URL` overrides). With `NET.operator.json` beside net.json (the operator's machine, or a local devnet), `owner init` pays from the faucet key instead: 100 coins, and 1 to the guardian, whatever `--faucet-coins` says.
- **Behind a proxy.** Put the RPC (8545) and the data node (8650) behind HTTPS (Caddy or nginx), pass `X-Forwarded-For` and name the proxy with `--trusted-proxy CIDR,…` on the devnet and the data node (rate limits believe the header only from those addresses; the default is none), and never expose the admin port (8546). `--public-url` is what the devnet writes into `net.json` and what the data node stakes as its URL; the client works with https URLs. A relay takes at most 120 envelopes a minute per client, queues at most 20,000, posts in chunks within the Inbox and transaction limits, and sets aside an envelope that keeps failing (`quarantine.bin`).

## A network that is not a devnet (the L1 testnet): net.json and the faucet

`ternchain-ops deploy` (contracts on any chain) writes net.json from its flags; nothing needs editing by hand:

| flag | net.json | what |
|---|---|---|
| `--faucet-url URL` | `faucet_url` | the public faucet, `https://HOST/faucet` (below) |
| `--relay-url URL` | `relay` | the default data node |
| `--name NAME` | `network` | e.g. `tern-testnet-1` |
| `--operator WHO --contact HOW` | `operator`, `contact` | who runs it and how to reach them |
| `--shared` | `mode: "shared"`, `refuse` (the default list) | an invited test network: clients refuse real-money features |
| `--test-network` | `test_network: true`, `notice` | an operator-run test network: zk keys from `ceremony solo` allowed; refused on the mainnet chain id 8376 |
| `--notice TEXT` | `notice` | replaces the default notice |

The default `--test-network` notice: test coins with no value, may be reset, run by OPERATOR, who made the zk keys alone, so shielded transfers and secret votes are not private from the operator. `owner init` and the faucet show it.

Off a devnet the faucet is its own service, with the genesis faucet key:

```bash
ternchain-ops faucet serve --key FAUCET.key --listen 127.0.0.1:8660 --net net.json \
  [--faucet-coins 5] [--gas 0.1] [--faucet-per-hour 100] [--faucet-per-ip-day 5] [--trusted-proxy CIDR] [--datadir DIR]
```

- `--net net.json`, or `--rpc URL` to sign through an RPC with no net.json (it needs no contracts); `--coins` is the same as `--faucet-coins`.
- It serves `POST /faucet {"address":"0x…"}`, the same as the devnet faucet. `GET /` shows its terms (coins, gas, caps, the notice) as JSON.
- A grant is `--faucet-coins` plus a little gas (`--gas`). One grant per address a day; `--faucet-per-ip-day` per IPv4 address or IPv6 /64 a day; `--faucet-per-hour` for everyone together.
- The key file must be mode 0600: it refuses otherwise (`chmod 600 FAUCET.key`).
- Its log (who got what, so the caps survive a restart): `DIR/faucet.json` with `--datadir`, else `FAUCET.key.faucet.json`.
- Put it behind the same TLS proxy as the RPC (Caddy: `handle /faucet` to 127.0.0.1:8660), pass `--trusted-proxy` with the proxy's address (or every caller is the proxy and shares its 5 grants a day), and deploy with `--faucet-url https://HOST/faucet`.
- Run it as a unit like the others (`ExecStart=…/ternchain-ops faucet serve …`, `Restart=on-failure`). Watch the faucet address's balance.

A newcomer with no coins (no faucet, a cap reached, the faucet down): `owner init` keeps the key and guardian folders, prints the address and says to send it to the operator. Send coins to it (`ternchain-ops faucet ADDRESS AMOUNT` with the operator file, or a plain transfer); the person then runs the same `owner init` command again: same folders, same address.

