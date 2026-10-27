package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// docSet: the documents README's index lists (review/ and sim/ are records
// of their day and keep the names they used).
var docSet = []string{"README.md", "START.md", "FOR-AGENTS.md", "FOR-PEOPLE.md", "PRIMITIVES.md", "DESIGN.md", "GOVERNANCE.md",
	"PRIVACY.md", "PERF.md", "RUNBOOK.md", "TERMS.md", "VISION.md", "BASKET.md", "REFACTOR.md", "SECURITY.md", "VALIDATORS-ONBOARDING.md", "BUILD.md", "AGENTS-JOIN.md"}

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
		b, err := os.ReadFile(f)
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
	b, err := os.ReadFile("FOR-AGENTS.md")
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
