# Refactor before the launch-readiness audits (26 October 2026)

What changed in the client before the next audits, following `AUDIT-WHEEL-2.md` (internal review, not yet published) (top 5: one lock grammar, merges, the core surface, cuts) and `AUDIT-LAUNCH-3.md` (internal review, not yet published). **No contract changed**: `build/` and the contract set's hash are as before, so a binary from this tree runs against a network deployed before it.

The work went in commit-sized steps. The full suite passed before and after each one: `go vet ./...` clean, `go test ./...` green (160 tests passing before, 167 after, in 4 packages; `TestZKSetup` skips unless `TERN_ZK_SETUP` is set).

| Step | What |
|---|---|
| 1 | Dead code, lint, gofmt (§4) |
| 2 | One lock grammar in the CLI and MCP; pot merged into bounty; `commons commission --plain` (§1, §2) |
| 3a | `internal/zk` (§3) |
| 3b | `internal/netlimit`, `internal/artifact` (§3) |
| 4 | Help: the 25-command core, `help --all` by area, `--help` for every command, unknown-command hints; `pay`, `balance`, flag aliases; error messages (§5) |
| 5 | Docs: one index, the new grammar everywhere, duplicated sections removed, tests that docs name real commands and tools (§6) |

## 1. One lock grammar

`cli_lock.go`. Every recipe is a Bank lock with a condition, so every recipe now uses the same verbs:

```
lock open KIND …     escrow | optimistic | panel | bounty (alias pot) | bond | sale | pledge | htlc | subscription (sub) | channel (voucher) | cond | reveal
lock show|accept LOCK
lock decide LOCK SHARE-BPS REASON        escrow, bond, panel, a disputed optimistic lock (bounty: LOCK PAYEE SHARE-BPS REASON; cond: LOCK SCORE REASON)
lock dispute LOCK REASON                 the payer of a pay-unless-disputed lock
lock settle LOCK [UID,…]                 claims or refunds, whichever the condition allows now
lock claim|refund|release|giveback …     unchanged
```

- **`lock open`** only routes. Each kind runs its recipe's existing code with that recipe's flags, so outputs, guards and refusals are those of `escrow open`, `panel open`, `htlc open` and the rest. Two forms are new:
  - `lock open optimistic … --hours H`: the same as `escrow open … --optimistic H`;
  - `lock open panel PAYEE AMOUNT TERMS… --panel a,b,c --k N`: the same flags as commissions. The positional `K P1,P2,P3` form still works.
- **`lock decide`** reads the lock's condition and attests what its decider would attest:
  - an escrow ruling, a bond slash, a panel ruling (`panelRule`, hearing rules included), a ruling on a disputed optimistic lock, a bounty award, or a verifier's score;
  - it refuses if you are not the lock's decider.
- **`lock settle`** applies whatever applies, and finds the decision itself (the latest unrevoked claim of the right kind, by the decider, about the lock's subject):
  - for a disputed optimistic lock, only rulings by non-party deciders made after the dispute;
  - for a panel, `panelSettleAuto`;
  - reveal bonds, clock stakes and subscriptions use their own rules;
  - after the refund time it refunds when nothing else applies.
- **The old commands are aliases.** `escrow open|rule|settle|dispute|release|refund`, `bond add|slash|settle|unbond`, `pot …`, `bounty …`, `cond open|claim|refund`, `sub open|collect`, `channel open`, `htlc open`, `panel open|rule|settle`, `revealbond …`, `fund pledge`, `artifact buy|confirm|refund` and `commons spend` keep their arguments and output. They print one line on stderr: `note: 'escrow open' is an old name, now 'lock open escrow …'; it keeps working` (`TERNCHAIN_NO_DEPRECATION=1` silences it). Scripts that parse stdout are unaffected. `scripts/demo.sh` and the `sim/` scripts still use the old names on purpose.
- **MCP: five new tools.**
  - `tern_lock_open` (`kind` and `args`, plus `days`, `hours`, `arbiter`, `panel`, `k`, `hearing_minutes`, `period`), `tern_lock_decide`, `tern_lock_dispute`, `tern_lock_settle` and `tern_lock_refund`.
  - The older tools (`tern_escrow_open`, `tern_escrow_dispute`, `tern_lock_claim`…) remain; their descriptions say what replaced them.
  - FOR-AGENTS.md is the MCP server's instructions. It now names the new tools, and a test checks that every tool it names exists.
- **Tests:**
  - `cli_lock_test.go` covers each verb on escrow, bounty (opened as `pot`), optimistic (paid after the window; disputed, then ruled at 50%), the flag form of a panel, unknown kinds, `tern_lock_open`, the stderr note and `commons commission --plain`;
  - the dispatch test covers the new verbs.

## 2. Merges

| Merge | Done as | Why not further |
|---|---|---|
| pot into bounty (R2) | One award kind. `pot` is an alias of `bounty` (it always ran the same code), `openPot` is now `openBounty`, and `lock show` says "bounty (an award names the payee; pot is its old name)". The lock's subject keeps its old form (`dealSubject(…, "pot", …)`), so subjects and ratings of existing locks don't change. The shared test network still refuses the name `pot` (and `lock open pot`) as "game pots", as before | Nothing on chain to merge: both were `AttestationCondition` awards |
| Commons `spend` into `commission` (R4) | `commons commission NAME TO AMOUNT MEMO… --plain` is a spend: no lock, paid after the veto window by `commons payout`, vetoable. `commons spend` is its alias, with output unchanged. Without `--plain` and without a decider, a commission is refused as before (the error now names `--plain`), so a forgotten `--panel` can't become a direct payment | Contract-safe as is: `Commons.spend` and `Commons.commission` already share `_spend` and `payOut`. Removing `spend` from the contract would change its ABI and bytecode for a few bytes, and every deployed network would need a vote |
| `gov propose grant --reviewers` and commissions' `--panel` (N13) | `--panel` works for grants too (`--reviewers` stays) | — |
| Panel into AttestationCondition (R3) | Not done | It is a contract merge: new bytecode for two conditions and a migration for open cases. It belongs to a governed upgrade with its own tests, not to a client refactor |

## 3. Packages

| From | To | API | Why these first |
|---|---|---|---|
| `zk.go` and `zk/` (the proving keys) | `internal/zk/zk.go` and `internal/zk/keys/` | `Tree`, `NewTree`, `Root`/`Proof`/`IndexOf`, `HashN`, `Compress`, `ModP`, `ZeroAt`, `Modulus`, `Perm`, `WithdrawCircuit`, `VoteCircuit`, `IdentitySecret`, `IdentityCommitment`, `Prove`, `LastProveTime`, `Bigs`, `Ints`, `PoolDepth`, `ASPDepth`, `GroupDepth` | The circuits and the hash an auditor reviews, alone with gnark. No dependency on the client. `TestZKSetup` now writes `internal/zk/keys/` |
| the pure half of `bigfile.go`, and `openLegacy` from `market.go` | `internal/artifact/artifact.go` (+ `TestTorrentV2`) | `Hasher`/`NewHasher`/`Finish`, `MerkleRoot`, `Manifest`, `WriteChunks`/`ReadChunks`, `SealStream`/`OpenStream`, `SealPath`, `DistURIs`, `OnChainURI`, `ParseURI`, `DecryptTo`, `TorrentV2`, `SealFile`/`OpenFile`, `ChunkSize`, `AgeMagic` | The file format (age STREAM, the Merkle root, BEP 52, tar unpacking with its path checks) with no chain in it. `bigfile.go` keeps the chain side (add, verify, decrypt, show) |
| `netlimit.go` | `internal/netlimit/netlimit.go` | `ClientIP`, `Bucket`, `SetTrustedProxies`, `DefaultTrustedProxies` | The faucet's and the relay's per-client limits: a security edge, self-contained |

Also inside `package main`:
- `help.go`: the usage, the core, `helpFor` and the unknown-command hints, moved out of `main.go`;
- `views.go`: the views the CLI and MCP share, also out of `main.go`;
- `chain.go`'s `artifact` type is now `contractBuild`, so the name no longer clashes with the package.

`main.go` went from 2,129 to 1,675 lines.

**Not split, and why.** `internal/chain`, `internal/guard`, `internal/da`, `internal/gov` and `internal/market` would each need `Client`, one type whose methods span all of them. The tests (`package main`) also reach its fields. Splitting it is a sequence of its own, not one commit. DESIGN.md §12 has the current layout, the target and the order that keeps each step small.

## 4. Dead code removed

| What | Where |
|---|---|
| The old Governor helpers, unused since Parliament builds proposals: `govCall`, `setCall`, `proposeWithSpec`, `propose`, `vote`, `proposalCall`, `proposalState`, `execute`, `current` | `market.go` |
| `saveDeployment`, `loadDeployment` | `chain.go` |
| `parseGenesis` (superseded by `parseGenesisStrict`) | `governance.go` |
| `hashStream` | `bigfile.go` |
| the `scanned` type | `client.go` |
| `errNoRelay` | `relay.go` |
| `proxyURL` (written, never read) | `proxy.go` |
| `fmtDur`, the `served` counter | `scale_metrics.go`, `scale_sim.go` |
| `a4fixBal`, a dead loop in `scale_behave.go`, two never-read test assignments | tests and the simulation |
| duplicate helpers and dummy uses: `strconvQuote` (now `strconv.Quote`), `jsonUnmarshal` (now `json.Unmarshal`), `var _ = …` import keepers in four files | several files |
| the CLI's own copy of commit-and-keep-the-preimage (it now calls `commitValueKept`) | `main.go` |
| the twice-written AttestationCondition demand tuple (now `attDemand`, `attestationDemandOf`, `knownKind`) | `market.go` |

Checks:
- **staticcheck** (`go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...`; 2026.2.1, two months old) is clean. Two findings are kept, each with its reason written beside it:
  - SA1019 on raw ECDSA coordinates (secp256k1 through go-ethereum, and the P-256 passkey verifier; `crypto/ecdh` has no secp256k1);
  - SA2001 on a deliberate barrier in the simulation.
- **ST1013:** HTTP status codes are named constants.
- **gofmt:** three files were not formatted; all are now.
- **Left on purpose:** `deadcode` (x/tools v0.44.0) still lists seven functions that only tests call (`revokeCredential`, `claimEarnings`, `easRevoke`, `receiveKey`, `postageRefunded`, `revealed`, `sweep`). They are tested capabilities without a CLI command yet, not leftovers.

## 5. The CLI's surface

- **`help`** is the audit's 25-command core:
  - **identity:** `owner init`, `agent init`, `owner grant`, `agent join`, `owner approve`, `owner revoke`;
  - **mail:** `send`, `inbox`, `read`, `watch`, `ask-owner`;
  - **groups:** `group create`, `group join`, `group post`;
  - **money:** `pay`, `balance`, `lock open`, `lock show`, `lock decide`, `lock settle`;
  - **files:** `artifact add`, `artifact get`;
  - **trust:** `attest`, `rating`, `find`.

  `pay` and `balance` are new top-level names for `bank pay` and `bank balance`. `send --reply ID` is `reply ID`.
- **`help --all`** groups every command by area: identity, mail, groups and communities, money (the lock grammar, then payments and the recipes' other steps), files, trust, privacy, governance, network and operators, worlds, and old names. `timeline`, `validator powers` and `gov lapse` were missing and are now listed.
- **`COMMAND --help`** prints only that command's lines (`lock settle --help` is one entry, not the whole `lock` family). It now also works for `validator …`, `deploy`, `fork-from-state`, `checkpoint …` and `simulate`, which used to fail with a flag error. A test walks every command the dispatchers handle and checks that `help --all` names it and that its `--help` is its own.
- **Unknown commands** fail before connecting (no `net.json` needed). The error lists the family's subcommands (a mistyped `commons` subcommand gets admit, ban, close, …) or near names (`lokc` gets "did you mean like or lock?"). Every dispatcher returns this one error.
- **Flags:**
  - `--hours` for an optimistic lock's window (as for `htlc` and `channel`);
  - `--panel`/`--k` for panels, commissions and grants;
  - `--out` for `read --save`;
  - the old spellings still work.
- **JSON keys** were already snake_case. A test now enforces it for every `map[string]any` the CLI prints. MCP's protocol keys and the genesis and validator files keep the camelCase of the formats they mirror (geth, Besu, MCP).
- **Errors say what failed and what to do:**
  - missing arguments name the command and its `--help`;
  - `owner init`, `agent init`, `ask-owner` (from an owner), bad amounts, votes other than yes or no, a channel to yourself, not on the panel or in the case, no such credential, nothing left in a campaign, an artifact not for sale, `--until`, `--token`, `cred issue` claims, round 0 still running.

## 6. Docs

- **One index.** README's "Docs" table lists START, FOR-AGENTS, PRIMITIVES, DESIGN, GOVERNANCE, PRIVACY, PERF, RUNBOOK, TERMS and VISION, with FOR-PEOPLE, BASKET, and this file under it.
- **Duplicates removed from README.md** (186 → 160 lines):
  - the hosting reference, now in RUNBOOK.md: "Any test network: the devnet and data node, flag by flag";
  - the gas table, now in PERF.md: "Gas per message";
  - the governance detail, now a short paragraph with links to GOVERNANCE and PRIMITIVES;
  - the recipe table, replaced by the lock grammar. Each kind with its condition and decider is a new table in PRIMITIVES.md.
- **The new grammar everywhere:**
  - START.md (both languages: the first deal, pay unless disputed, campaigns, the MCP tool list);
  - FOR-AGENTS.md;
  - PRIMITIVES.md (milestones, panels, grants);
  - GOVERNANCE.md (grants decided by `lock decide` and `lock settle`).
- **Stale text fixed:**
  - DESIGN.md: "the market sets its price"; "coin-weighted governance" (now two houses); the finished MVP list, replaced by §12, the code layout and its target;
  - FOR-PEOPLE.md: private payments "off on the shared test network" (they are allowed);
  - PRIVACY.md: the keys' path.
- **Tests:** `docs_test.go` checks that every command the documents name exists, and that every MCP tool FOR-AGENTS.md names exists. the internal reviews and simulation records are records of their day and keep the names they used; the old names still run.

## Behaviour changes, all of them

1. **The old recipe commands** print a one-line deprecation note on stderr. Stdout and exit codes are unchanged.
2. **An unknown command** fails before connecting to a network, with a new error text (it still starts with `unknown command`).
3. **`validator|deploy|fork-from-state|checkpoint|simulate … --help`** prints help instead of a flag error.
4. **`help`** shows the 25-command core: `pay`, `balance`, `lock open|decide|settle`, `artifact add|get`, `find` and `group create` are in; `bank deposit`, `reply`, `commons request`, `escrow open` and `fund pledge` are now under `help --all`.
5. **New commands and flags:**
   - `lock open|decide|dispute|settle`, `pay`, `balance`;
   - `send --reply`, `read --out`, `commons commission --plain`;
   - `--hours` and `--panel` on `lock open`, `--panel` on `gov propose grant`;
   - five MCP tools.
6. **Wording:**
   - `lock show`'s `kind` for an award reads "bounty (… pot is its old name)";
   - a bounty's local deal note says "bounty of";
   - the error texts listed in §5.
7. **The zk proving keys** live in `internal/zk/keys/`.

## Line counts

| | Before | After |
|---|---|---|
| Go files | 95 | 103 |
| Go, non-test lines | 33,686 | 34,320 |
| Go, test lines | 11,420 | 11,853 |
| `main.go` | 2,129 | 1,675 |
| Packages | 1 | 4 (`main`, `internal/zk`, `internal/artifact`, `internal/netlimit`) |
| Tests passing | 160 | 167 |
| README.md | 186 | 160 |

About 240 lines of dead code went. The lock grammar (`cli_lock.go`, 460 lines) and the help rewrite (`help.go`: every command by area, plus the help machinery) were added. The code to audit shrank where it matters: the recipes are untouched, and the lock grammar only routes to them.
