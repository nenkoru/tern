// Optional modules: application recipes built into the binary but off
// unless an identity turns them on (BRICKS #53, #54, D6). They are ordinary
// uses of the core (claims, locks, commitments) that we happen to ship; the
// core help and the agent's default MCP tool set leave them out.
//
//	modules [list] --dir D                 which are on for this identity
//	modules enable|disable NAME --dir D    writes D/modules.json
//
// TERNCHAIN_MODULES=fiat,fund (or "all") turns them on for a process; the
// operator binary (ternchain-ops, built with -tags ops) has them all on.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type moduleInfo struct {
	What     string   `json:"what"`
	Commands []string `json:"commands,omitempty"` // CLI commands ("word" or "word sub")
	Tools    string   `json:"tools,omitempty"`    // MCP tools with this name prefix
}

// optionalModules: the modules a binary carries, off by default.
var optionalModules = map[string]moduleInfo{
	"fiat":    {What: "money paid outside the network, confirmed on it (two claims: fiat.go)", Commands: []string{"fiat"}},
	"fund":    {What: "all-or-nothing campaigns (fund.go; a pledge is the core's lock open pledge)", Commands: []string{"fund create", "fund show", "fund collect", "fund refund", "fund pledge", "fund deliver"}, Tools: "tern_fund_"},
	"match":   {What: "secret mutual interest: like and mutual (social.go)", Commands: []string{"like", "mutual"}},
	"coop":    {What: "pooling anything in a group: offer, presence, receipt, credits, pool, verify, bricks (coop.go and its bricks)", Commands: []string{"offer", "presence", "receipt", "credits", "pool", "verify", "bricks"}, Tools: "tern_coop_"},
	"commons": {What: "the agent's community tools over MCP (tern_commons_*); the CLI's commons commands are core", Tools: "tern_commons_"},
}

// moduleFile: <dir>/modules.json.
type moduleFile struct {
	Enabled []string `json:"enabled"`
}

// modulesOn: the optional modules on for this identity folder (and process).
func modulesOn(dir string) map[string]bool {
	on := map[string]bool{}
	if opsBuild {
		for m := range optionalModules {
			on[m] = true
		}
		return on
	}
	for _, m := range strings.Split(os.Getenv("TERNCHAIN_MODULES"), ",") {
		m = strings.TrimSpace(m)
		if m == "all" {
			for x := range optionalModules {
				on[x] = true
			}
		} else if _, ok := optionalModules[m]; ok {
			on[m] = true
		}
	}
	if dir != "" {
		var f moduleFile
		readJSON(filepath.Join(dir, "modules.json"), &f)
		for _, m := range f.Enabled {
			if _, ok := optionalModules[m]; ok {
				on[m] = true
			}
		}
	}
	return on
}

// moduleOfCommand: the optional module a CLI command belongs to ("": core).
func moduleOfCommand(cmd string) string {
	word, _, _ := strings.Cut(cmd, " ")
	for name, m := range optionalModules {
		for _, c := range m.Commands {
			if c == cmd || c == word {
				return name
			}
		}
	}
	return ""
}

// moduleOfTool: the optional module an MCP tool belongs to ("": core).
func moduleOfTool(tool string) string {
	for name, m := range optionalModules {
		if m.Tools != "" && strings.HasPrefix(tool, m.Tools) {
			return name
		}
	}
	return ""
}

// moduleOff: the refusal for a command or tool of a module that is off.
func moduleOff(name, dir string) error {
	where := "--dir D"
	if dir != "" {
		where = "--dir " + dir
	}
	return fmt.Errorf("%s is an optional module (%s), off for this identity: turn it on with 'ternchain modules enable %s %s' (or TERNCHAIN_MODULES=%s)",
		name, optionalModules[name].What, name, where, name)
}

// checkModule: nil if cmd is core or its module is on.
func checkModule(cmd, dir string) error {
	if m := moduleOfCommand(cmd); m != "" && !modulesOn(dir)[m] {
		return moduleOff(m, dir)
	}
	return nil
}

func runModules(cmd string, pos []string, dir string) error {
	if dir == "" {
		return errors.New("--dir is required (the identity folder the modules are turned on for)")
	}
	path := filepath.Join(dir, "modules.json")
	var f moduleFile
	readJSON(path, &f)
	switch cmd {
	case "modules", "modules list":
	case "modules enable", "modules disable":
		if len(pos) < 1 {
			return fmt.Errorf("usage: ternchain %s NAME --dir D   (modules: %s)", cmd, strings.Join(moduleNames(), ", "))
		}
		if _, ok := optionalModules[pos[0]]; !ok {
			return fmt.Errorf("no optional module %q (modules: %s)", pos[0], strings.Join(moduleNames(), ", "))
		}
		var keep []string
		for _, m := range f.Enabled {
			if m != pos[0] {
				keep = append(keep, m)
			}
		}
		if cmd == "modules enable" {
			keep = append(keep, pos[0])
		}
		sort.Strings(keep)
		f.Enabled = keep
		if err := writeJSON(path, f); err != nil {
			return err
		}
	default:
		return unknownCommand(cmd)
	}
	on := modulesOn(dir)
	list := map[string]any{}
	for name, m := range optionalModules {
		list[name] = map[string]any{"on": on[name], "what": m.What, "commands": m.Commands, "mcp_tools": m.Tools}
	}
	out(map[string]any{"modules": list, "file": path, "note": "off by default: built in, left out of the core help and the agent's default tools; help modules lists their commands"})
	return nil
}

func moduleNames() []string {
	var out []string
	for n := range optionalModules {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
