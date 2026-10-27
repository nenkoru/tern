// ternchain version: what this binary is, and which contract set it deploys
// and speaks. scripts/release.sh sets the version, commit and date:
//
//	go build -ldflags "-X main.version=v0.2.0 -X main.commit=abc123 -X main.buildDate=2026-10-24T20:00:00Z"
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"runtime"
	"runtime/debug"
	"sort"
)

var (
	version   = "dev"
	commit    = ""
	buildDate = ""
)

// contractsHash: sha256 over the embedded ABIs and bytecode (sorted by file
// name) and the claim schema. Two binaries with the same hash deploy the same
// contracts and read the same events; a network made by another set needs the
// binary that made it.
func contractsHash() (string, int) {
	var names []string
	fs.WalkDir(artifacts, "build", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, _ := artifacts.ReadFile(n)
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	h.Write([]byte(claimSchema))
	return hex.EncodeToString(h.Sum(nil)), len(contractNames)
}

// versionInfo: version, commit and build date (from -ldflags, else from the
// Go build info when built from a checkout), and the contract set's hash.
func versionInfo() map[string]any {
	c, d, dirty := commit, buildDate, false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if c == "" {
		c = "unknown (not built by scripts/release.sh)"
	} else if dirty {
		c += "-dirty"
	}
	if d == "" {
		d = "unknown"
	}
	h, n := contractsHash()
	return map[string]any{"version": version, "commit": c, "built": d, "go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH,
		"contracts_hash": h, "contracts": n, "claim_schema": schemaUID(claimSchema).Hex()}
}
