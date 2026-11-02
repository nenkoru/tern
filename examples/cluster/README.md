# A cluster of friends

Friends pool their GPUs to run one model none of them could run alone: the model is cut into ordered positions (embedding, blocks, head), each machine holds a range, every friend gets their own chat, and when a laptop closes another machine picks up its range. [ClusterChat](https://clusterchat.io) does this in browsers. Tern doesn't run models; it gives the bricks around the engine (module `coop`, docs/PRIMITIVES.md "Pooling anything: co-ops"), the same bricks that pool an API key (`scripts/demo-quota.sh`) or family backups (`scripts/demo-backup.sh`):

| The engine needs | Tern brick |
|---|---|
| who is in the cluster, and the keys | a private group: a set plus a channel, admitted by its admins; a ban removes a member for everyone |
| what each machine offers | `offer set` (`tern.offer/1`: kind gpu, `whole` the model, `holds` its positions of `of`, amount, `protocol`, `region`, `ext` `offer/gpu`) |
| who is online now, with which positions, where its engine listens | `presence announce` / `presence list`: sealed to the group, minutes long, never on chain; a coverage map with the positions nobody holds and those held by fewer than `--min-copies` |
| what each machine did for whom | `receipt issue` / `receipt accept --observed N`: counters in any unit, countersigned from the consumer's own count |
| who goes first, without money | `credits post` and `pool rule` / `pool order`: credits first, turns, a daily quota, or first come — a module the group picks |
| or money, for a stranger's GPU | `receipt settle`: vouchers on a channel (no gas per payment) |
| did a machine really do the work | `verify statement` / `compare` (exact hashes, or a tolerance: `compare-cosine@1`, `compare-topk@1`) / `report`; `verify bond` + `escalate` to a panel |

## Run the demo

```bash
./scripts/demo-cluster.sh
```

It starts a devnet and a data node, makes three owners with an agent each (`box.oscar`, `box.nina`, `box.ada`), a private group `friends`, and three stand-in engines (`internal/stubengine`: 32 positions, each a deterministic step, so honest machines agree and a checker compares hashes; nina's skips the work on every second job). Then: two chats routed through the holders online, receipts countersigned and posted for credits, a check that outvotes nina's node, ada's laptop closing (her presence loop says "leaving" on SIGTERM; positions 22-31 go missing), oscar's machine picking them up, a third chat routed around nina, and the queue. `TestClusterOfFriends` (cluster_test.go) is the same story in one process, in CI.

## ClusterChat as the engine

ClusterChat (github.com/l-wn/ClusterChat) runs Qwen3.8-27B (8-bit, about 27 GiB, 66 blocks) in browsers with hand-written WebGPU kernels. Each browser holds a contiguous range of positions, ranges may overlap, and f16 hidden states move peer to peer over WebRTC data channels. A signalling server assigns ids and ranges, broadcasts the roster and relays offers and answers; private clusters go relay-only (TURN), with the invite code in the link's fragment and handshakes signed by a key derived from it. On a leave, the previous peer replays into another holder; each peer keeps one first-come queue, with speculative decoding of up to 7 tokens; a cluster holds up to 64 people. Its author measured 8.1 tok/s for one user on two RTX 3090s in homes 28 ms apart (about 20 tok/s with both in one machine; ~5 tok/s each with 12 users): the network is the main cost. Its stated limits: peers are trusted with no check that they ran honestly, the server sees membership, there are no incentives or fairness beyond an owner's VRAM limit, no API or tools yet.

It is **AGPL-3.0**, and its NOTICE asks for a visible credit in any deployment. Tern (Apache-2.0) never vendors or copies its code; it can sit beside it as a separate process, over a protocol or an adapter:

- **Membership instead of a link.** The invite goes sealed to a private tern group's members (or the roster comes straight from the group's set), so the signalling server learns nothing a member didn't send it, and a group ban removes someone at once.
- **Signalling over tern.** WebRTC offers and answers are small: sealed mail, or a presence extension (`presence/webrtc`) through the data nodes, signed by agent keys. No central server then sees who is in a cluster.
- **A coverage map with alerts.** Each member's offer names `whole: qwen3.8-27b-q8`, `holds: 0-21`, `of: 66`; `presence list --min-copies 2` shows ranges held by one browser only, so failover always has somewhere to go.
- **Locality.** `region` in the offer and measured round-trip times in `ext["offer/gpu"].rtt_ms` let an orchestrator form clusters of near peers (28 ms costs more than half the speed).
- **Rebalancing** — a runner on blocks 1-20 that never moves to 21-36 while they are thin — is an orchestrator agent's job, built on the coverage map: it reads `presence list`, asks idle members to load thin ranges, and they re-announce. Not the network's.
- **Honesty.** Different GPUs and kernels never agree bit for bit on activations, so exact hashes are wrong here: a checker reruns a random range on a second holder and compares a few logits with `compare-cosine@1` or the next token's top candidates with `compare-topk@1`, after calibrating the threshold on pairs it trusts (record the agreement of honest pairs; set the threshold below their worst case; the share of honest pairs below it is the false-positive rate, and should be published with the group's rules). A mismatch goes to a panel only with a majority; "undecided" means look closer.
- **Fairness.** Its first-come queue becomes the group's pool rule (credits first, turns, a quota), counted by receipts per position processed (`--unit position-token`).
- **Relays as a service.** A TURN relay is a capacity like any other: `offer` kind `relay`, paid per GB by channel voucher or in credits.
- **An OpenAI-compatible endpoint** in front of a cluster (its plan) makes it a LiteLLM backend, so agents can use a friends' cluster as their model; its tokens become receipts like the API-key demo's.
- **Browser peers.** The tern client is Go. A browser peer needs either a small WASM/JS client (keys, sealing, signatures, presence over HTTP) or a local bridge (`ternchain mcp` or a tiny HTTP service on the same machine) that the page talks to.

## With llama.cpp RPC

llama.cpp splits a model's layers across machines running `rpc-server`; one `llama-server` drives them. Only the engine and its unit change.

1. Each friend runs the backend on a private network (WireGuard, Tailscale): `rpc-server` has **no authentication and no encryption** (llama.cpp's own docs say never to expose it on an open network), so it listens only on the private one.
   ```bash
   rpc-server --host 10.8.0.2 --port 50052
   ```
2. Each agent offers it and announces it to the group only:
   ```bash
   ternchain offer set --dir agent/box.nina '{"schema":"tern.offer/1","kind":"gpu","title":"24 GB for qwen 32B q4","whole":"qwen2.5-32b-q4_k_m","amount":{"value":24,"unit":"gb-vram"},"free_for":["friends"],"settle":["credits"],"protocol":"llama.cpp-rpc/1","region":"eu-central"}'
   ternchain presence announce --dir agent/box.nina friends --every 30 --ttl 90 --reach tcp://10.8.0.2:50052
   ```
   llama.cpp places layers by memory itself, so this offer names an amount rather than `holds`.
3. Whoever chats builds the backend list from presence and starts its own server:
   ```bash
   RPC=$(ternchain presence list --dir agent/box.oscar friends | python3 -c 'import json,sys; print(",".join(m["reach"].removeprefix("tcp://") for m in json.load(sys.stdin)["online"] if m.get("reach","").startswith("tcp://")))')
   llama-server -m qwen2.5-32b-q4_k_m.gguf --rpc "$RPC" -ngl 99
   ```
   When someone leaves, `presence list` no longer shows them: restart with the new list (llama.cpp re-splits the layers).
4. Receipts: `llama-server` reports `usage.completion_tokens` per request; each backend's share is its share of the layers (the server's log prints the split). `--unit token`, countersigned with `--observed` from the chatting side's own count.
5. Checks: a sampled prompt with `--temp 0 --seed 1` on two other backend sets; hash the token ids (`--canon tokens/greedy`, `compare-default@1`), or compare a few logits (`--vec`, `compare-cosine@1`).

## Who sees what, and who decides

- **The data node** sees a random topic (the hash of the group's view key), slots only members can replace, and sealed bytes, for at most 10 minutes. Not names, positions or endpoints. On tern-base-sepolia-1 no data node is listed: pass `--relay URL` of one someone runs ([CHALLENGES.md](../../docs/CHALLENGES.md) #6), or run your own.
- **The chain** sees the group's posts (receipts, rules, check results) as sealed envelopes of the group's channel, like any group post.
- **Pipeline peers see what passes through them**: activations of your prompt, in the clear, on their GPU. A split model is a private-group thing among people who trust each other with their prompts; never route a private prompt through strangers' machines.
- **People stay in charge.** Shared compute that "can't be shut down" is not a goal: every machine is its owner's (an agent acts under its owner's grant and limits, and its owner revokes it), every cluster is a group whose admins admit, remove and ban, a community's members veto and recall its admins, and anyone leaves by stopping their engine. The bricks make cooperation easy among people who chose each other; they add no way around any of that.
