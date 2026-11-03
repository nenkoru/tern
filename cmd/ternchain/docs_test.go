package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docSet: the documents README's index lists (review/ and sim/ are records
// of their day and keep the names they used).
var docSet = []string{"README.md", "START.md", "FOR-AGENTS.md", "docs/FOR-PEOPLE.md", "docs/PRIMITIVES.md", "docs/DESIGN.md", "docs/GOVERNANCE.md",
	"docs/PRIVACY.md", "docs/PERF.md", "docs/RUNBOOK.md", "TERMS.md", "docs/VISION.md", "docs/BASKET.md", "docs/REFACTOR.md", "SECURITY.md", "docs/VALIDATORS-ONBOARDING.md", "docs/BUILD.md", "docs/AGENTS-JOIN.md",
	"docs/THREATS-STATE.md", "docs/SUPPLY-CHAIN.md",
	"docs/OVERVIEW.md", "docs/CHALLENGES.md", "docs/EARLY-BUILDERS.md", "AGENTS.md",
	"examples/mailbox/README.md", "examples/jobboard/README.md", "examples/sell/README.md", "examples/cowrite/README.md", "examples/pools/README.md", "examples/boards/README.md", "examples/match/README.md", "examples/scanner/README.md"}

// repoRoot: the repository's root, absolute (the tests run in cmd/ternchain).
var repoRoot = func() string {
	d, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		panic(err)
	}
	return d
}()

// repo: a path from the repository's root (contracts/, docs/, scripts/…).
func repo(p string) string { return filepath.Join(repoRoot, p) }

// docCommands: the commands a document names: "ternchain WORD [SUB]" anywhere,
// and code spans that start with a command word.
func docCommands(text string) []string {
	var out []string
	add := func(fs []string) {
		if len(fs) == 0 {
			return
		}
		w := fs[0]
		if !hasSubs(w) && !topLevel[w] {
			return
		}
		c := w
		if hasSubs(w) && len(fs) > 1 && regexp.MustCompile(`^[a-z][a-z-]*$`).MatchString(fs[1]) {
			c += " " + fs[1]
		}
		out = append(out, c)
	}
	for _, m := range regexp.MustCompile(`ternchain ([a-z][a-z-]*(?: [a-z][a-z-]*)?)`).FindAllStringSubmatch(text, -1) {
		add(strings.Fields(m[1]))
	}
	for _, m := range regexp.MustCompile("`([a-z][a-z-]*(?: [a-z][a-z-]*)?)[^`]*`").FindAllStringSubmatch(text, -1) {
		add(strings.Fields(m[1]))
	}
	return out
}

// Every command the documents name exists.
func TestDocsNameRealCommands(t *testing.T) {
	known := knownCommands()
	for _, f := range docSet {
		b, err := os.ReadFile(repo(f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		seen := map[string]bool{}
		for _, c := range docCommands(string(b)) {
			w, _, two := strings.Cut(c, " ")
			if seen[c] || known[c] || !two && known[w] {
				continue
			}
			if two && !known[c] && hasSubs(w) {
				seen[c] = true
				t.Errorf("%s names %q, which is not a command (ternchain help --all)", f, c)
			}
		}
	}
}

// Every MCP tool FOR-AGENTS.md names exists (the MCP server sends it as its instructions).
func TestForAgentsNamesRealTools(t *testing.T) {
	b, err := os.ReadFile(repo("FOR-AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, x := range mcpTools {
		have[x.Name] = true
	}
	for _, m := range regexp.MustCompile("`(tern_[a-z_]+)`").FindAllStringSubmatch(string(b), -1) {
		if !have[m[1]] && !strings.HasSuffix(m[1], "_") {
			t.Errorf("FOR-AGENTS.md names %s, which the MCP server doesn't have", m[1])
		}
	}
}
