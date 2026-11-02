# Bootstrap checklist (for the coordinator)

In order. Tick each box. Times are rough. Background: `L1-READINESS.md` (internal review, not yet published) §2 (phase 1), §3 (people), `ops/testnet/README.md`.

## A. Before inviting anyone

- [ ] The release: one version of `ternchain` and `ternchain-ops` (the operator binary: `validator`, `deploy`, `faucet serve`, `relay serve`; `scripts/release.sh` builds `ternchain-ops-VERSION-OS-ARCH` next to each `ternchain-…`) with `SHA256SUMS`, with the allocation plan, `--profile rpc`, `--metrics`, `--restart`, `rpcs`/`bootnodes`/`genesis_hash` in `net.json`.
- [ ] Besu pinned: `ops/besu/VERSION` (26.9.0 today) with its digest filled in.
- [ ] The lawyer's answer on sanctions (L3) before inviting residents of Russia. Until then: not resident in Russia.
- [ ] Testnet terms: "the coin is gas, no value promised, not for sale; testnet coins never become mainnet coins" (L2).
- [ ] The operator agreement, one page (L1-READINESS §3.3).
- [ ] The archive box rented (e.g. a rented dedicated server): archive node, Blockscout, monitoring, data node.
- [ ] Channels made: alert group, operators' group, backup channel (email list or Signal), announcements. This checklist's `INCIDENT-RESPONSE.md` pinned.

## B. People (2–3 weeks)

- [ ] 1. **Invite 10–12** people (invitation text: L1-READINESS §3.4) to get 7 validators.
- [ ] 2. **Vet each one** with the checklist (§3.5): independence, hosting (host, ASN, country; at most 2 per host and per country), one per citizen, key hygiene, experience, reachability (a test message answered within 12 h), law, motive (no promise of value, in writing), dry run.
- [ ] 3. **Infra people:** 3 bootnode operators (different people), 2 RPC operators (one can be you), the archive node (you).
- [ ] 4. **Citizens:** at least 2× the validators (15+ for 7). Every validator operator is also a founder.
- [ ] 5. Keep the **operator list**: name, role, country, ASN, host, time zone, two contacts. The public part (country, ASN, host) gets published.

## C. Collect (1 week)

- [ ] 6. **Founders:** each runs, on their own machine (not the server): `ternchain owner keygen --out ~/tern/founder.key --name NAME` and sends `NAME=0x…`. Put the lines into `founders.txt`.
- [ ] 7. **Validators:** each runs `ops/testnet/00-validator-init.sh NAME PUBLIC_IP` on their server and sends `validator: NAME=0x…` and `enode: enode://…`. Put `NAME=0x…` lines into `validators.txt`.
- [ ] 8. **Infra nodes:** each bootnode, the archive node and each RPC node runs `00-validator-init.sh NAME PUBLIC_IP ~/tern/ROLE`. Keep only their enodes; their addresses do **not** go into `validators.txt`. The 3 bootnode enodes go into `bootnodes.txt`.
- [ ] 9. Check by hand: no address twice, no node address equal to an owner address, every validator name in `founders.txt`. (`10-make-genesis.sh` checks this too.)

## D. The allocation table (founders agree)

- [ ] 10. Default plan (`10-make-genesis.sh`):

  | Who | Coins |
  |---|---|
  | deployer (fresh key, nonce 0) | 100 |
  | each founder | 1,000 |
  | each validator node | 10 |
  | faucet (testnet only) | 10,000,000 |
  | Treasury contract | the rest of 1,000,000,000 |

  No key but the Treasury above 1%. Change a number only with the founders' agreement (variables in the script header).
- [ ] 11. Send the table to the founders. Get "agreed" from each in writing.

## E. Genesis

- [ ] 12. On your laptop (not a server):
  ```bash
  ops/testnet/10-make-genesis.sh --validators validators.txt --founders founders.txt
  ```
  It makes `deployer.key` and `faucet.key` (if missing), runs `ternchain-ops validator genesis` with the plan flags (chain id 83760), prints the allocation table and the keccak, and writes `genesis.keccak`, `genesis.sha256`, `genesis.out.json`. It uses the default `--selection contract`: the Validators contract in the genesis, each validator bound to its founder. An operator-run testnet with no founders uses `--selection header` instead (the operator-run genesis script; README, "The validator set").
- [ ] 13. Keep `deployer.key` offline until step 19. It is the most dangerous key until the deploy (see INCIDENT-RESPONSE, key theft, deployer).
- [ ] 14. **Publish** `genesis.json` and the keccak in the operators' group **and** a second place (site or git tag). Each validator confirms the keccak in the group.

## F. Start the nodes

- [ ] 15. **Bootnodes first.** The first bootnode with an empty bootnodes file; the next two with the first's enode:
  ```bash
  ops/testnet/20-validator-run.sh --dir ~/tern/boot1 --genesis genesis.json --keccak 0x… --p2p-host IP --bootnodes-file /dev/null
  ```
  Then publish the final `bootnodes.txt` (3 lines).
- [ ] 16. **Validators**, at an agreed time (each on their server):
  ```bash
  ops/testnet/20-validator-run.sh --genesis genesis.json --keccak 0x… --p2p-host IP --bootnodes-file bootnodes.txt
  ```
  Blocks start once more than 2/3 are up (5 of 7). Each sends back its block 0 hash; all must be equal.
- [ ] 17. **Archive node** (on the archive box; RPC on the Docker bridge for Blockscout):
  ```bash
  ops/testnet/20-validator-run.sh --dir ~/tern/archive --genesis genesis.json --keccak 0x… --p2p-host IP \
    --bootnodes-file bootnodes.txt -- --archive --rpc-host 172.17.0.1
  ```
- [ ] 18. **RPC nodes** (each with Caddy, `ops/besu/rpc-node.md`):
  ```bash
  ops/testnet/20-validator-run.sh --dir ~/tern/rpc1 --genesis genesis.json --keccak 0x… --p2p-host IP \
    --bootnodes-file bootnodes.txt -- --profile rpc --rpc-allow-hosts rpc1.example.org
  ```
  Check from outside: `eth_chainId` answers `0x14730`; `admin_peers` is refused.

## G. Deploy and net.json

- [ ] 19. **Deploy soon after the first blocks** (through the archive node, on the box or by `ssh -L 8545:172.17.0.1:8545 archive-box`):
  ```bash
  ops/testnet/30-deploy.sh --rpc http://127.0.0.1:8545 \
    --rpcs https://rpc1.example.org,https://rpc2.example.org --bootnodes-file bootnodes.txt
  ```
  It checks the chain id, the deployer's nonce (must be 0), each public RPC (chain id, block 0, no ADMIN), then runs `ternchain-ops deploy … --launch --faucet-key faucet.key --rpcs … --bootnodes …`. `net.json` gets `rpcs`, `bootnodes` and `genesis_hash`. The script doesn't pass the other net.json flags yet: run its deploy line by hand with `--faucet-url https://HOST/faucet --relay-url https://DATANODE --name tern-testnet-1 --operator NAME --contact HOW --shared` (README, "net.json from deploy"). With fewer than 4 founders deploy refuses to hand the set to Parliament unless `--keep-validators`; the handover is deploy's last step.
- [ ] 20. `validator powers --deployer-key` shows no powers. Then destroy the deployer key (`shred -u deployer.key`).
- [ ] 21. **The faucet:** `ternchain-ops faucet serve --key faucet.key --listen 127.0.0.1:8660 --net net.json --trusted-proxy PROXY_IP --datadir ~/tern/faucet` (`chmod 600 faucet.key` first: it refuses otherwise), behind Caddy at `https://HOST/faucet`, the URL deploy wrote as `faucet_url`. Check `GET https://HOST/` shows its terms. (README, "The faucet".)
- [ ] 21a. **Publish `net.json`** (public). Keep `net.operator.json` private (0600); it holds the faucet key. `deploy --faucet-key` puts it there: `owner init` on that machine funds new names from it. Everyone else gets coins from `faucet_url`; with no coins, `owner init` prints the address to send to you and can be run again (same folders, same address).
- [ ] 22. **Founders claim:** each runs `ternchain owner init --key ~/tern/founder.key --dir id/NAME --name NAME --guardian-dir …` and `ternchain gov claim --dir id/NAME`, then they vouch for each other (`gov vouch NAME`) before the first term ends.

## H. Services

- [ ] 23. **Explorer:** `ops/explorer/` on the archive box (check the image tags first; check TRACE).
- [ ] 24. **Monitoring:** `ops/monitoring/` on the archive box; the seal exporter in cron; Telegram (or Matrix) alerts; each validator joins the alert group. Fire a test alert.
- [ ] 25. **Backups:** `ops/backup/` on the archive box: nightly timer, the first backup, the first restore test (must print `OK`).
- [ ] 26. **The data node:** `ternchain-ops relay serve --da …` (docs/RUNBOOK.md "Any test network"), behind Caddy; a second one by another person. The blackbox probe in `prometheus.yml` watches `/info`.
- [ ] 27. **Check everything:** `ops/testnet/40-check.sh --net net.json` → `RESULT: ok`. Ask two validators to run it too.
- [ ] 28. Publish the operator list (country, ASN, host per validator).

## I. Drills (phase 1: one every 2 weeks; announce each 48 h ahead)

Log each in `ops/incident/drills/YYYY-MM-DD-NAME.md` (the post-mortem template). Times in UTC.

- [ ] D1. **Upgrade at a block:** Besu 26.9.0 → 26.10.x. Release manifest and hash (`ops/besu/UPGRADE.md` step 0); `ternchain gov propose param validators upgrade BLOCK 0xHASH URI`; vote; canary, one validator, the rest; **snapshot first** (one-way Bonsai migration). Target: all upgraded 48 h before BLOCK, no missed seals at BLOCK.
- [ ] D2. **3 of 7 down, then back:** three operators `ternchain-ops validator stop --dir ~/tern/validator` at HH:MM; the halt alert must fire within 1 minute; after 10 minutes they run `validator run` again with the same flags. Measure: time from the third restart to the first block (one-machine demo: 33–66 s).
- [ ] D3. **Key rotation:** one validator makes a new key on the same server (`validator init --dir ~/tern/validator-new`), starts it as a non-sealing node on spare ports (`--p2p-port 30304 --rpc-port 8555 --name tern-new`), then rotates (`ternchain gov validator rotate ~/tern/validator-new`), checks the new address seals, and stops the old node. Target: no halt, the new key seals within minutes of the rotation. Later, move the new node to the usual ports (`validator run --dir ~/tern/validator-new … --recreate`) and keep the old key's offline backup marked "retired".
- [ ] D4. **An 8th validator by vote:** the new person runs a synced node first; then `ternchain gov propose param validators add ADDRESS NAME`; both houses vote. With launch windows this takes about 2+ weeks (7 d vote, 7 d exit, plus the voting delay): start it in week 1.
- [ ] D5. **Same-operator challenge (staged):** two volunteers; a citizen runs `ternchain gov validator challenge A B --why evidence.txt`; citizens vote (`challenge-vote ID no`, it is staged); `challenge-close ID`. Check `validator status` shows it under `challenges`, and nothing is removed.
- [ ] D6. **Censorship alert:** one validator volunteers to censor a test sender (Besu account permissioning through `--besu-args`, as in the capture simulation, F5); run `ternchain-ops validator watch --rpc ARCHIVE_PRIVATE_RPC --report-dir reports/`; the test sender sends; the `censorship` event must appear; `ternchain gov validator report reports/censorship-*.json`. Then the volunteer removes the rule.
- [ ] D7. **fork-from-state** (V9, S6): the full drill in `ops/incident/INCIDENT-RESPONSE.md`, playbook 7, with its timing targets, in "copy" mode (the old chain resumes after).

Also measure on the WAN (L1-READINESS C4): average block time, round changes per day, and whether 1 s blocks hold for 7 nodes on several continents (else plan 2 s).

## J. Exit criteria (phase 1 → phase 2)

- [ ] 30 days in a row with no halt longer than 5 minutes.
- [ ] Every validator ≥ 99% seals over those 30 days (from Prometheus: `ternchain_validator_blocks_since_seal`, and `validator status --blocks …`).
- [ ] All 6 phase-1 drills (D1–D6) passed and logged; D7 (fork) done at least once.
- [ ] Backups restored at least twice.
- [ ] A post-mortem for every SEV1 and SEV2, with its actions done.

Then phase 2 (public testnet) needs more: the ceremony, audit 5, a bug bounty (L1-READINESS §2).
