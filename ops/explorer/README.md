# Explorer (Blockscout)

Blockscout shows blocks, transactions, accounts and contracts of the ternchain L1. Chain id 83760 (testnet).

| File | What |
|---|---|
| `docker-compose.yml` | Postgres, the Blockscout backend (`ETHEREUM_JSONRPC_VARIANT=besu`), the contract verifier, the frontend, Caddy (TLS) |
| `Caddyfile.explorer` | one host name: `/api` and `/socket` to the backend, the rest to the UI |
| `explorer.env.example` | the settings; copy to `.env` |

## It needs the archive node

Blockscout reads old state and traces. A normal (Bonsai) node or a public RPC cannot serve that. So:
- run it beside the **archive node**: `ternchain-ops validator run --dir ~/tern/archive --genesis genesis.json --archive …` (FOREST storage);
- point it at the archive node's **private** RPC. It needs DEBUG and TRACE methods. The public profile (`--profile rpc`) has neither, and it must not get them;
- never make that private RPC reachable from the internet.

Blockscout's own guidance: 4–8 cores, 8–32 GB RAM, Postgres 14+, an archive node. The plan in `L1-READINESS.md` (internal review, not yet published) §4 puts the archive node, Blockscout, Postgres, monitoring and the data node on one dedicated box (e.g. a rented dedicated server).

## The archive node's RPC, seen from the containers

`validator run` publishes the RPC on 127.0.0.1 by default. Containers cannot reach the host's 127.0.0.1. Bind it to the Docker bridge address instead (private to this machine):

```bash
ip -4 addr show docker0          # usually 172.17.0.1
ternchain-ops validator init --dir ~/tern/archive
ternchain-ops validator run --dir ~/tern/archive --genesis genesis.json --archive \
  --rpc-host 172.17.0.1 --p2p-host PUBLIC.IP --bootnodes "$(paste -sd, bootnodes.txt)" \
  --metrics --restart unless-stopped
```

`--rpc-host` is in `validator.go` (default 127.0.0.1). The compose file maps `host.docker.internal` to the bridge (`host-gateway`), so `http://host.docker.internal:8545` works. Check the firewall: 8545/8546 must stay closed from outside (a bridge address is not routed from the internet, but check `ss -ltnp`).

The archive node's API list comes from `besuArgs`: `ETH,NET,WEB3,QBFT,ADMIN,TXPOOL,DEBUG`. **TRACE is not in it.** Blockscout's Besu variant fetches internal transactions with `trace_replayBlockTransactions`. Check:

```bash
curl -s -X POST -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"trace_replayBlockTransactions","params":["latest",["trace"]]}' \
  http://172.17.0.1:8545
```

- If it answers with a result: keep `NO_TRACE=false`.
- If it says "Method not enabled": either set `NO_TRACE=true` in `.env` (no internal transactions in the explorer), or run the archive node with TRACE added to its HTTP API list (through `--besu-args`, after checking with `--print` that Besu takes the flag).

## Start

```bash
cd ops/explorer
cp explorer.env.example .env && chmod 600 .env
# fill in: EXPLORER_HOST, PUBLIC_RPC_URL, POSTGRES_PASSWORD (openssl rand -hex 24),
# SECRET_KEY_BASE (openssl rand -hex 64); check the image tags (below)
docker compose pull
docker compose up -d
docker compose logs -f backend      # first start: database migration, then indexing from block 0
```

Open `https://EXPLORER_HOST`. Ports 80 and 443 must be open (Let's Encrypt).

## Image versions

Certain: `postgres:17-bookworm`, `caddy:2.10.0`. **Not verified here** (written from memory, not pulled): `blockscout/blockscout:8.0.0`, `ghcr.io/blockscout/frontend:v2.0.0`, `ghcr.io/blockscout/smart-contract-verifier:v1.9.0`. Before the first start:
- look up the newest stable tags (https://hub.docker.com/r/blockscout/blockscout/tags, https://github.com/blockscout/frontend/releases, https://github.com/blockscout/blockscout-rs/releases);
- use a backend and a frontend from the same period (their API must match);
- set them in `.env`; after it works, pin by digest.

Blockscout renames environment variables between major releases. If the backend ignores a setting, check the env docs for that release (https://docs.blockscout.com/setup/env-variables).

## After a fork-from-state

Block numbers restart at 0 on a fork. Start a new database (`docker compose down`, remove the `db-data` volume or use a new project name) and let it index the fork from block 0. Keep the old one read-only if people still need the old chain's history.

## Backups

The explorer's database can always be rebuilt from the archive node (it takes time). Back up the archive node (`ops/backup/`), not the explorer.
