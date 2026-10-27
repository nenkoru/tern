# A public RPC node

A public RPC node lets wallets, agents and the explorer's users read the chain and send transactions. It is a normal Besu node that does **not** seal blocks. Its key is not in the validator set.

Rules:
- **Never** serve a public RPC from a validator. The validator profile turns on ADMIN, DEBUG, TXPOOL and QBFT, with CORS `*` and host allowlist `*` (`besuArgs` in `validator.go`). That must never face the internet.
- Run at least **2** public RPC nodes, on different machines, by different people if possible. List both in `net.json` (`rpcs`). The client tries them in order and moves to the next one on a connection error or a wrong chain id. It also refuses an RPC whose block 0 differs from `genesis_hash`.
- Besu listens on 127.0.0.1. Only Caddy (TLS, limits) faces the internet.

Machine: 4–8 vCPU, 16 GB RAM, 100+ GB NVMe. Ports: 30303 TCP+UDP open, 443 (and 80 for the certificate) open, 8545/8546/9545 closed from outside.

## Run it with ternchain

```bash
ternchain-ops validator init --dir ~/tern/rpc1        # a new node key; never a validator's key
ternchain-ops validator run --dir ~/tern/rpc1 --genesis genesis.json \
  --profile rpc --rpc-allow-hosts rpc1.example.org \
  --p2p-host PUBLIC.IP --bootnodes "$(paste -sd, bootnodes.txt)" \
  --metrics --restart unless-stopped
```

`--profile rpc` gives:
- only `ETH,NET,WEB3` over HTTP and WebSocket;
- no `ADMIN`, `DEBUG`, `TXPOOL`, `QBFT`;
- CORS off;
- `--host-allowlist` from `--rpc-allow-hosts`.

`--metrics` adds `--metrics-enabled --metrics-host=127.0.0.1 --metrics-port=9545`. `--restart unless-stopped` makes Docker start the node again after a crash or a reboot.

Add `--print` first to see the exact Besu command without running it.

## Run it with Besu directly

`rpc-node.toml` in this folder is the same profile as a Besu config file:

```bash
besu --config-file=/etc/besu/rpc-node.toml
```

Native Besu 26.6+ needs JDK 25. Use the release in `ops/besu/VERSION`.

## Caddy in front

`Caddyfile.rpc` in this folder:
- TLS for `rpc1.example.org` (change the name);
- request body at most 256 KB;
- per-IP rate limit: 20 requests a second and 600 a minute;
- POST and WebSocket only;
- JSON access logs, kept 7 days, never with bodies.

The rate limit needs the plugin `github.com/mholt/caddy-ratelimit`. Build:

```bash
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
xcaddy build --with github.com/mholt/caddy-ratelimit
sudo install -m 0755 caddy /usr/bin/caddy
caddy list-modules | grep rate_limit     # must print http.handlers.rate_limit
sudo cp ops/besu/Caddyfile.rpc /etc/caddy/Caddyfile && sudo systemctl reload caddy
```

### Fallback without the plugin

Delete the `order rate_limit …` line and the `rate_limit { … }` block from the Caddyfile. Then limit new connections per IP in the kernel with nftables:

```
# /etc/nftables.d/rpc-limit.nft  (include it from /etc/nftables.conf)
table inet rpclimit {
  set rpc_flood { type ipv4_addr; flags dynamic; timeout 1m; }
  set rpc_conns { type ipv4_addr; flags dynamic; }
  chain input {
    type filter hook input priority 0; policy accept;
    tcp dport 443 ct state new add @rpc_flood { ip saddr limit rate over 30/second burst 60 packets } drop
    tcp dport 443 ct state new add @rpc_conns { ip saddr ct count over 50 } drop
  }
}
```

(IPv4 only; copy the two lines with `ip6 saddr` and `ipv6_addr` sets for IPv6. Test with `nft -c -f FILE` before loading.)

This limits connections, not requests. A client that keeps one connection open can still send many requests on it. So it is weaker. Use the plugin when you can.

## Check it

From another machine:

```bash
curl -s -X POST -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' https://rpc1.example.org
# {"jsonrpc":"2.0","id":1,"result":"0x14730"}      (83760; mainnet 8376 is 0x20b8)

curl -s -X POST -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"admin_peers","params":[]}' https://rpc1.example.org
# must be an error: "Method not enabled" (or similar). If it answers, the profile is wrong: stop the node.

curl -s -X POST -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x0",false]}' https://rpc1.example.org
# "hash" must equal genesis_hash in net.json
```

Then `ops/testnet/40-check.sh` checks all RPCs in `net.json` the same way.

## What a public RPC cannot do

- `ternchain-ops validator watch` needs the TXPOOL API. Run it against a private node (the archive node, on localhost), not a public RPC.
- Blockscout needs DEBUG/TRACE and an archive node. It uses the archive node's private RPC, never this one.
