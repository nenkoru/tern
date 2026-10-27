# ops: running the ternchain L1

Operations files for the L1 on Besu QBFT (testnet chain id 83760, mainnet 8376). For the geth devnet see `RUNBOOK.md`.

The commands here (`validator`, `deploy`, `faucet serve`, `relay serve`, `fork-from-state`, `checkpoint`, `ceremony`) are in `ternchain-ops`: `ternchain-ops-VERSION-OS-ARCH` from the release, or `go build -tags ops -o ternchain-ops .`; put it beside `ternchain` as plain `ternchain-ops` (plain `ternchain` forwards them to it).

| Path | What |
|---|---|
| `besu/VERSION` | the pinned Besu image (26.9.0), how to get its digest, JDK 25 |
| `besu/UPGRADE.md` | canary upgrade: release hash, snapshot, canary, one validator, the rest, rollback |
| `besu/rpc-node.md` | a public RPC node: `--profile rpc`, Caddy, checks |
| `besu/rpc-node.toml` | the public RPC profile as a Besu config file (no ADMIN/DEBUG) |
| `besu/Caddyfile.rpc` | TLS proxy for an RPC node: body limit, per-IP rate limit (plugin; fallback in rpc-node.md) |
| `monitoring/README.md` | how to run the monitoring and wire alerts to Telegram or Matrix |
| `monitoring/docker-compose.yml` | Prometheus, Alertmanager, Grafana, node_exporter, blackbox_exporter |
| `monitoring/prometheus.yml` | scrape jobs: Besu 9545, node_exporter, data node and RPC probes |
| `monitoring/alerts.yml` | alert rules: stalled blocks, missing seals, rounds, peers, disk, data node, RPCs |
| `monitoring/alertmanager.yml` | alert routing (Telegram) |
| `monitoring/blackbox.yml` | probe modules (`/info`, `eth_chainId`) |
| `monitoring/seal-exporter.sh` | per-validator seal counts from `validator status`, for the textfile collector |
| `monitoring/grafana/` | data source and a small "ternchain L1" dashboard |
| `incident/INCIDENT-RESPONSE.md` | roles, severity, channels, playbooks, the fork-from-state drill, post-mortem template |
| `explorer/README.md` | Blockscout on the archive node: what it needs, how to start |
| `explorer/docker-compose.yml` | Blockscout backend, frontend, verifier, Postgres, Caddy |
| `explorer/Caddyfile.explorer` | one host name for the UI and the API |
| `explorer/explorer.env.example` | the explorer's settings |
| `backup/README.md` | what to back up, why keys are kept apart, install, restore |
| `backup/besu-backup.sh` | backup of a non-validator node (stop, tar+zstd or rsync, start, keep N, offsite) |
| `backup/besu-restore-test.sh` | restore into a temp folder, start a node, check it syncs |
| `backup/systemd/` | nightly backup and monthly restore-test units, settings file |
| `testnet/README.md` | the closed multi-machine testnet: roles, files, the allocation plan |
| `testnet/BOOTSTRAP-CHECKLIST.md` | the coordinator's ordered steps, drills and exit criteria |
| `testnet/VALIDATOR.md` | the validator's page: requirements, firewall, commands, monitoring, upgrades |
| `testnet/00-validator-init.sh` | each node: make the key, print address and enode |
| `testnet/10-make-genesis.sh` | coordinator: genesis with the allocation plan, keccak |
| `testnet/20-validator-run.sh` | each node: check the genesis keccak, start |
| `testnet/30-deploy.sh` | coordinator: `deploy --launch`, `net.json` with RPCs and bootnodes |
| `testnet/40-check.sh` | anyone: health check of the chain and its RPCs |
| `testnet/keccak256.py` | keccak of a file, plain Python |
| `testnet/lib.sh` | helpers for the testnet scripts |
