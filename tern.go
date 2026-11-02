// Package tern holds the files the ternchain binary carries: the compiled
// contracts, the fee basket, the agents' guide, the offer schema and the
// release signer set. go:embed cannot reach above a package's directory, so
// they are embedded here, at the repository root, and the program
// (cmd/ternchain) takes them from this package.
package tern

import "embed"

// Build holds the core contracts' compiled ABIs and bytecode (build/*.abi, build/*.bin).
//
//go:embed build/*.abi build/*.bin
var Build embed.FS

// BuildExt holds the host-chain contracts' ABIs and bytecode (build/ext/*.abi, build/ext/*.bin).
//
//go:embed build/ext/*.abi build/ext/*.bin
var BuildExt embed.FS

// Basket is docs/BASKET.md, the reference basket fees are priced in (its hash goes on chain).
//
//go:embed docs/BASKET.md
var Basket []byte

// ForAgents is FOR-AGENTS.md, the guide the MCP server hands to agents.
//
//go:embed FOR-AGENTS.md
var ForAgents string

// OfferSchema is schemas/offer.schema.json.
//
//go:embed schemas/offer.schema.json
var OfferSchema []byte

// Signers is releases/signers.json, the release signer set built into the binary.
//
//go:embed releases/signers.json
var Signers []byte
