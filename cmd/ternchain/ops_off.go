//go:build !ops

// The user binary: operator commands (devnet, faucet, advance, deploy,
// validator, fork-from-state, checkpoint, ceremony, simulate, relay serve)
// live in ternchain-ops, the same code built with -tags ops (docs/BUILD.md). The
// user binary forwards them to a ternchain-ops beside it or on the PATH,
// the way git runs git-foo; the scale simulator (scale_*.go) isn't in it.
package main

const opsBuild = false

// runSimulate: the scale simulator is in ternchain-ops only.
func runSimulate(args []string) error { return forwardOps(append([]string{"simulate"}, args...)) }
