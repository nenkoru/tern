# ternchain on a host chain (Base Sepolia)

`deploy.sh` puts ternchain's contracts on Base Sepolia from a fresh identity and writes everything a public network needs. Nothing in it names a person, a host or a domain: those are parameters.

1. **Run it once with nothing funded.**
   ```sh
   T=/path/to/ternchain-ops CONTRACTS=/path/to/checkout/contracts HOME_DIR=~/tern-base \
     PUBLIC_URL=https://HOST/base OPERATOR=NAME CONTACT=HOW ops/hostchain/deploy.sh
   ```
   It makes three new keys in `HOME_DIR/keys` (deployer, bundler, relay) and the zk keys (`ceremony solo`, then the verifiers). The deploy stops at the first transaction for lack of gas, and the error names the deployer's address.
2. **Fund the deployer** with Base Sepolia ETH. The contracts take about 82M gas: 0.0005 ETH at 0.006 gwei. Run `deploy.sh` again: it goes on where it stopped (`deploy/net.deploy.json`).
3. **Fund the relay and bundler keys** with a little ETH each, then register the relay's name, which is what senders pay their postage to:
   ```sh
   TERNCHAIN_NET=deploy/net.json $T owner init --key keys/relay.key --dir id/relay --name NAME-relay --guardian-dir g/relay
   ```
   Both keys get their money back:
   - The relay posts envelopes and is paid by the senders' vouchers, in USDC (`RELAY_PRICE` an envelope).
   - The bundler fronts user operations, and the EntryPoint repays it from whoever pays the operation's gas.
4. **Run the relay** (`deploy/tern-NAME-relay.service`, a systemd user unit) behind a reverse proxy at `PUBLIC_URL`.
5. **Publish** `publish/net-NAME.json`, `publish/zk-keys-NAME.tar.gz` and `publish/SHA256SUMS-NAME` next to the release.
6. **Verify the sources:**
   ```sh
   node contracts/verify.js deploy/net.json sourcify zk-keys
   ```

Who pays for gas is `GAS`:

| `GAS` | who pays | what a newcomer needs |
|---|---|---|
| `usdc` (default) | each user, in USDC, through Circle Paymaster | test USDC from Circle's faucet, nothing else |
| `eth` | each user, in Base Sepolia ETH, from their own key | ETH from a faucet |
| `sponsor` | the operator, through `SponsorPaymaster` with daily caps | nothing |

The operator pays only for its own transactions: the deploy, and the relay and bundler, which get their money back.

docs/base-sepolia.md has the design and the first deployment's report.

**Base mainnet** (`HOST=base`, chain 8453): `networks/tern-base-1/README.md` is the owner's runbook. `deploy.sh` then needs `CONFIRM_MAINNET=yes`, takes the public set only, no founders and no sponsor, writes net.json caps (`CAP_PER_PAYMENT` 5, `CAP_PER_NAME` 20 USDC by default) and the "unaudited, real money" notice, and accepts the solo zk keys only as `--solo-zk` (net.json `zk_solo`). `fork-dryrun.sh` rehearses the whole deploy and a newcomer's first steps on an anvil fork of Base mainnet, with no real funds.
