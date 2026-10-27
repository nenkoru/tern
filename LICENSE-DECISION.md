# Licence decision (pending)

**Status: proposed Apache-2.0, pending owner decision.**

The repository ships `LICENSE` with the Apache License 2.0 so that it is not
published without a licence. The owner has not confirmed it yet. Until they
do, treat the licence as provisional.

## Proposal

| Part | Proposed licence | Why |
|---|---|---|
| Go client (`*.go`, `internal/`) | Apache-2.0 | patent grant (crypto and zk code); compatible with gnark (Apache-2.0); can link go-ethereum's LGPL-3.0 libraries, given the NOTICE and the published source |
| Own contracts (`contracts/*.sol`) | Apache-2.0 | matches the existing SPDX headers; compatible with the MIT OpenZeppelin and EAS code |
| gnark-exported verifiers | MIT (as generated) | keep the generator's header |
| `examples/*.sol` | MIT | the most permissive, for builders to copy |
| Docs | repository licence (optionally CC-BY-4.0) | |

Alternative if the owner prefers copyleft parity with go-ethereum:
LGPL-3.0-or-later for the client. GPL-3.0 for the client is not recommended:
it would stop builders from embedding it, and nothing requires it.

## Open questions for the owner

1. Confirm Apache-2.0 (or choose another licence) and the copyright holder
   line in `NOTICE` ("the ternchain authors" for now).
2. The GPL-3.0 ERC-4337 EntryPoint: its bytecode is embedded in the binary
   (`build/EntryPoint_sol_EntryPoint.bin`). Options:
   a. keep it, ship the GPL-3.0 text and source, and call it an aggregate (current `NOTICE`);
   b. stop embedding it and use the canonical EntryPoint v0.8 at its well-known address, or predeploy it in genesis;
   c. switch to an MIT-licensed ERC-4337 implementation.
3. Contributions: DCO sign-off (proposed in `CONTRIBUTING.md`) or a CLA.
4. Whether to add SPDX headers to the Go files once the licence is final.
