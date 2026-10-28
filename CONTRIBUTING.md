# Contributing

Thanks for looking. ternchain is a **prototype**: not audited, testnet only, no
real money. Expect large changes.

## Security issues

Do not open a public issue for a vulnerability. Follow [SECURITY.md](SECURITY.md).

## Build and test

Prerequisites: Go 1.26.4 or newer. Node.js (any current LTS) only if you
change the contracts.

```bash
go build -o ternchain .                   # the client
go build -tags ops -o ternchain-ops .     # the operator binary (devnet, validator, deploy, …)
go vet ./...
go test ./... -timeout 20m                # end-to-end tests on a simulated chain, about 5 minutes
```

The tests start simulated chains and local devnets in-process; they need no
network access and no external services.

## Contracts

Sources are in `contracts/*.sol`; the vendored OpenZeppelin, EAS and ERC-4337
sources are in `contracts/vendor/` (only the files the build uses). The
compiled ABI and bytecode in `build/` are committed, because the Go binary
embeds them (`//go:embed build/*.abi build/*.bin`).

After changing a contract:

```bash
npm ci --prefix contracts/.solc       # solc 0.8.29 (solcjs), pinned
node contracts/build.js               # rewrites build/ (about a minute)
go test ./... -timeout 20m
```

Commit the `.sol` change and the regenerated `build/` together. CI rebuilds the
contracts and fails if `build/` differs from what is committed.

The zk circuits' keys in `internal/zk/keys/` and the generated
`contracts/Poseidon2*.sol` and verifiers are made by
`TERN_ZK_SETUP=1 go test -run ZK` (see `zk_setup_test.go`). They come from a
single-party development setup: never use them for anything of value.

## Pull requests

- One topic per pull request; describe what changes and why.
- Add or update a test. Most behaviour is tested end to end against a
  simulated chain (`newWorld(t)` in the tests).
- Keep documents in sync: `docs_test.go` checks that every command the docs
  name exists, and that every MCP tool `FOR-AGENTS.md` names exists.
- Never commit keys, identities (`id/` folders), operator files
  (`*.operator.json`) or binaries. `.gitignore` covers the usual ones.
- Sign off your commits (Developer Certificate of Origin, `git commit -s`).
  Contributions are accepted under the Apache License 2.0; no CLA.

## Language

English is the main language of the code and docs. Some documents are
bilingual (Russian, then English) and `VISION.md` is in Russian.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
