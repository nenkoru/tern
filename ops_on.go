//go:build ops

// ternchain-ops: the operator binary, the same code as ternchain with the
// operator commands and the scale simulator, and every optional module on
// (go build -tags ops -o ternchain-ops .; BUILD.md).
package main

const opsBuild = true
