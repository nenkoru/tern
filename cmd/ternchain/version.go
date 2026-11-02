// ternchain version: what this binary is, and which contract set it deploys
// and speaks. scripts/release.sh sets the version, commit and date:
//
//	go build -ldflags "-X main.version=v0.2.0 -X main.commit=abc123 -X main.buildDate=2026-10-24T20:00:00Z"
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
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

// extContractsHash: the same over contracts/ext's build (build/ext): what a
// network on a host chain (Base Sepolia) runs besides the core set.
func extContractsHash() (string, int) { return extContractsHashFor(nil) }

// optionalExt: contracts/ext modules a network may not have deployed (opt-in
// privacy modules): left out of the hash when its net.json doesn't name them,
// so a newer binary still matches a network deployed before they existed.
var optionalExt = map[string]bool{"AnonGroups": true, "PrivateReviews": true}

// extContractsHashFor: the hash a network naming these contracts was deployed
// with (nil: every ext contract, what deploy records).
func extContractsHashFor(named map[string]string) (string, int) {
	var names []string
	fs.WalkDir(extArtifacts, "build/ext", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if n, _, ok := strings.Cut(path.Base(p), "_sol_"); ok && named != nil && optionalExt[n] && named[n] == "" {
				return nil
			}
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, _ := extArtifacts.ReadFile(n)
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), len(names) / 2
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
	eh, en := extContractsHash()
	return map[string]any{"ext_contracts_hash": eh, "ext_contracts": en, "version": version, "commit": c, "built": d, "go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH,
		"contracts_hash": h, "contracts": n, "claim_schema": schemaUID(claimSchema).Hex()}
}
