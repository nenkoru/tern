# Monitoring

Prometheus, Alertmanager and Grafana for a ternchain L1, plus two small inputs:
- `seal-exporter.sh`: who seals, round changes and head age, from `ternchain-ops validator status` (cron, every minute);
- blackbox probes: the data node's `/info` and each public RPC's `eth_chainId`.

| File | What |
|---|---|
| `docker-compose.yml` | Prometheus, Alertmanager, Grafana, node_exporter, blackbox_exporter (pinned tags, host network, all on 127.0.0.1) |
| `prometheus.yml` | scrape jobs: Besu (9545), node_exporter, blackbox probes |
| `alerts.yml` | the alert rules |
| `alertmanager.yml` | where alerts go (Telegram by default) |
| `blackbox.yml` | probe modules (`http_2xx`, `rpc_chainid`) |
| `seal-exporter.sh` | per-validator seal counts for node_exporter's textfile collector |
| `grafana/provisioning/` | the Prometheus data source and a small dashboard "ternchain L1" |

## Where to run it

On the archive box (it also runs Blockscout and the data node; see `ops/testnet/README.md`). One central stack is enough for the chain-wide alerts: seals are in the block headers, so one seal exporter sees every validator.

Each operator can also run the same stack on their own validator box, for their own disk and peers. Or they expose their metrics to the central box (below, "Remote nodes").

## Start

Linux, Docker with the compose plugin.

```bash
cd ops/monitoring
mkdir -p secrets && chmod 755 secrets
openssl rand -base64 24 > secrets/grafana_admin_password
echo 'BOT_TOKEN_FROM_BOTFATHER' > secrets/telegram_bot_token
openssl rand -base64 24 > secrets/metrics_password          # only for remote nodes
# Each file readable only by the container user that needs it:
# Prometheus and Alertmanager run as uid 65534 (nobody), Grafana as 472.
sudo chown 65534 secrets/telegram_bot_token secrets/metrics_password
sudo chown 472 secrets/grafana_admin_password
sudo chmod 400 secrets/*
sudo mkdir -p /var/lib/node_exporter/textfile
# edit prometheus.yml (hosts) and alertmanager.yml (chat_id)
docker compose up -d
docker compose ps
```

Each Besu node must run with `--metrics` (`ternchain-ops validator run … --metrics`). That adds `--metrics-enabled --metrics-host=127.0.0.1 --metrics-port=9545`.

Check:
```bash
curl -s 127.0.0.1:9545/metrics | grep -E '^ethereum_(blockchain_height|peer_count)'
curl -s 127.0.0.1:9090/api/v1/targets | python3 -m json.tool | grep -E '"health"|"scrapeUrl"'
```

Grafana: `ssh -L 3000:127.0.0.1:3000 BOX`, then http://127.0.0.1:3000 (user `admin`, password in `secrets/grafana_admin_password`). The dashboard "ternchain L1" is in the folder "ternchain". For Besu's own detail, import the community dashboard "Besu Overview" (grafana.com id 10273; check it still fits the release).

## The seal exporter

```bash
sudo install -m 0755 ops/monitoring/seal-exporter.sh /opt/tern/seal-exporter.sh
crontab -e
# * * * * * TERN_RPC=http://127.0.0.1:8545 TERNCHAIN=/usr/local/bin/ternchain /opt/tern/seal-exporter.sh 2>>/tmp/tern-seal-exporter.log
```

It writes `/var/lib/node_exporter/textfile/ternchain_seals.prom`. Look at it once:
```bash
cat /var/lib/node_exporter/textfile/ternchain_seals.prom | grep -E 'blocks_since_seal|round_changes'
```

The cron user needs write access to the textfile folder. Use a private RPC (the archive node on 127.0.0.1). A public RPC also works: `validator status` reads headers and the contract.

## The alerts

| Alert | When | Severity |
|---|---|---|
| BlockProductionStalled | a node's height has not changed for 30 s | critical |
| ChainHeadOld | head block older than 30 s (seal exporter) | critical |
| ValidatorNotSealing | a validator in the set has no seal for more than 2n blocks | critical |
| ValidatorNotProposing | a validator proposed nothing in 60 blocks | warning |
| RoundChanges / RoundChangesMany | any round change for 10 min / 10+ in 60 blocks | warning / critical |
| SealsAtQuorumEdge | a block with fewer seals than the quorum | critical |
| PeerCountLow / PeerCountZero | fewer than 3 peers / none | warning / critical |
| DiskFilling / DiskAlmostFull | disk over 80% / 90% | warning / critical |
| DataNodeDown / DataNodeServerError | `/info` probe fails / answers 5xx | critical |
| PublicRPCDown / AllPublicRPCsDown | one RPC fails `eth_chainId` (or wrong chain) / all do | warning / critical |
| BesuDown | Besu metrics do not answer | critical |
| UpgradeSoon | a scheduled upgrade less than 7 days away | warning |
| ValidatorContractAlert | new alert lines from the Validators contract | warning |
| SealExporterDown, TLSCertExpiring | the inputs themselves | warning |

What to do for each: `ops/incident/INCIDENT-RESPONSE.md`.

Test a rule file before a reload:
```bash
docker run --rm -v "$PWD":/w --entrypoint promtool prom/prometheus:v3.5.0 check rules /w/alerts.yml
docker run --rm -v "$PWD":/w --entrypoint promtool prom/prometheus:v3.5.0 check config /w/prometheus.yml
docker compose restart prometheus
```

## Telegram

1. Talk to @BotFather: `/newbot`. Put the token into `secrets/telegram_bot_token`.
2. Make a group "tern-alerts". Add the bot. Send one message in the group.
3. Find the chat id: `curl -s https://api.telegram.org/bot<TOKEN>/getUpdates` and read `"chat":{"id":-100…}`.
4. Put it into `alertmanager.yml` (`chat_id`), then `docker compose restart alertmanager`.
5. Test: `docker compose exec alertmanager amtool alert add test severity=warning --annotation=summary="test alert" --alertmanager.url=http://127.0.0.1:9093`.

Use a separate group for alerts and for people talking. People should not have to read through alerts to find each other.

## Matrix

Alertmanager has no built-in Matrix receiver (as far as we know). Use a webhook bridge, for example `matrix-alertmanager-receiver` (not checked by us: read its code and pin a version). Run it on 127.0.0.1:9088 with a bot account and the room id. Then in `alertmanager.yml` set `receiver: matrix` and uncomment the `matrix` receiver.

## Remote nodes

Besu metrics listen on 127.0.0.1. To let the central Prometheus read a remote node, the operator runs a small Caddy site on that node, open only to the monitoring box's IP:

```
# on the remote node: /etc/caddy/Caddyfile (add to it)
val-ana.example.org:9443 {
	@monitor remote_ip 203.0.113.50          # the monitoring box
	handle @monitor {
		basic_auth {
			prometheus <hash from: caddy hash-password>
		}
		handle /metrics {
			reverse_proxy 127.0.0.1:9545
		}
		handle_path /node/* {
			reverse_proxy 127.0.0.1:9100       # node_exporter, if they run one
		}
	}
	respond 403
}
```

Open port 9443 in the node's firewall only for 203.0.113.50. Then add the node to `besu-remote` and `node-remote` in `prometheus.yml`.

An SSH tunnel or WireGuard works too. Never open 9545 itself to the internet.
