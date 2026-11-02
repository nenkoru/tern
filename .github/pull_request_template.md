## What and why

## How it was tested

- [ ] `go vet ./...`
- [ ] `go test ./... -timeout 30m`
- [ ] If a contract changed: `node contracts/build.js` and the regenerated `build/` is committed
- [ ] Docs updated (commands and MCP tools named in docs exist: `docs_test.go`)
- [ ] No keys, identities, operator files or binaries in the diff
- [ ] Commits signed off (`git commit -s`)
