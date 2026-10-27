// Client policy modules (tern.policy/1; internal/ui/policy.go): the reader's
// social policies — triage, rating, reports, spam, look-alike names — are
// signed modules like the ranking modules. Today's formulas are the built-in
// modules; an owner installs another (ui install / policy install: checked,
// pinned, never auto-updated) and switches to it (policy use). The security
// floors stay in this client around each module.
//
//	policy [list]                 the module in use for each kind, the installed ones, updates
//	policy show KIND|LINE         what a module reads, its rules and params, the floors around it
//	policy use LINE | policy reset KIND
//	policy export KIND DIR [--name NEW]   the built-in module as a starting point (then ui publish DIR)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nenkoru/tern/internal/confuse"
	"github.com/nenkoru/tern/internal/ui"
)

// policyPrefs: <dir>/policy.json: kind => the installed line in use (absent: the built-in module).
type policyPrefs map[string]string

func (c *Client) policyPrefs() policyPrefs {
	p := policyPrefs{}
	readJSON(filepath.Join(c.dir, "policy.json"), &p)
	return p
}

func (c *Client) savePolicyPrefs(p policyPrefs) error {
	b, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(filepath.Join(c.dir, "policy.json"), b, 0o600)
}

// policyOf: the module in use for a kind (loaded once per process). A module
// that no longer loads (its files changed, its schema fails) falls back to
// the built-in one, and policy list says why.
func (c *Client) policyOf(kind string) *ui.Policy {
	if p := c.pol[kind]; p != nil {
		return p
	}
	if c.pol == nil {
		c.pol, c.polNote = map[string]*ui.Policy{}, map[string]string{}
	}
	p := ui.BuiltinPolicy(kind)
	if line := c.policyPrefs()[kind]; line != "" && c.dir != "" {
		lp, err := c.loadPolicyLine(line)
		switch {
		case err != nil:
			c.polNote[kind] = fmt.Sprintf("%s doesn't load (%v): the built-in module is used", line, err)
		case lp.M.AppliesTo != kind:
			c.polNote[kind] = fmt.Sprintf("%s is a %s policy, not %s: the built-in module is used", line, lp.M.AppliesTo, kind)
		default:
			p = lp
		}
	}
	c.pol[kind] = p
	return p
}

func (c *Client) loadPolicyLine(line string) (*ui.Policy, error) {
	b, err := c.uiStore().Load(line)
	if err != nil {
		return nil, err
	}
	return ui.LoadPolicy(b)
}

// lookTable: the look-alike tables in use: the built-in ones, extended by the
// lookalike module's tables (never reduced).
func (c *Client) lookTable() *confuse.Table {
	if c.look != nil {
		return c.look
	}
	t := confuse.Default()
	p := c.policyOf("lookalike")
	if !p.Builtin {
		if x, err := t.With(p.Table("confusables"), p.Table("words")); err == nil {
			t = x
		} else {
			c.polNote["lookalike"] = fmt.Sprintf("%s's tables don't parse (%v): the built-in tables are used", p.M.Line, err)
		}
	}
	c.look = t
	return t
}

// policyLines: the installed policy modules, by kind.
func (c *Client) policyLines() map[string][]ui.Pin {
	out := map[string][]ui.Pin{}
	for _, pin := range c.uiStore().Pins() {
		if pin.Kind != ui.KindPolicy {
			continue
		}
		if p, err := c.loadPolicyLine(pin.Line); err == nil {
			out[p.M.AppliesTo] = append(out[p.M.AppliesTo], pin)
		}
	}
	return out
}

type policyFlags struct {
	name, share string
}

func runPolicy(c *Client, cmd string, pos []string, f policyFlags) error {
	switch cmd {
	case "policy list", "policy":
		out(c.policyList())
	case "policy show":
		if len(pos) < 1 {
			return errors.New("usage: ternchain policy show KIND|LINE   (kinds: " + strings.Join(ui.PolicyKindNames(), ", ") + ")")
		}
		p, err := c.policyRef(pos[0])
		if err != nil {
			return err
		}
		out(p.Explain())
	case "policy use":
		if len(pos) < 1 {
			return errors.New("usage: ternchain policy use LINE   (an installed policy module, e.g. quiet-triage@1; policy reset KIND for the built-in one)")
		}
		r, err := c.policyUse(pos[0])
		if err != nil {
			return err
		}
		out(r)
	case "policy reset":
		if len(pos) < 1 || ui.PolicyKinds[pos[0]] == nil {
			return errors.New("usage: ternchain policy reset KIND   (kinds: " + strings.Join(ui.PolicyKindNames(), ", ") + ")")
		}
		r, err := c.policyUse(ui.BuiltinPolicy(pos[0]).M.Line)
		if err != nil {
			return err
		}
		out(r)
	case "policy export":
		if len(pos) < 2 || ui.PolicyKinds[pos[0]] == nil {
			return errors.New("usage: ternchain policy export KIND DIR [--name NEW]   (kinds: " + strings.Join(ui.PolicyKindNames(), ", ") + ")")
		}
		r, err := policyExport(pos[0], pos[1], f.name)
		if err != nil {
			return err
		}
		out(r)
	case "policy install":
		if len(pos) < 1 {
			return errors.New("usage: ternchain policy install NAME[@VERSION]|HASH")
		}
		pin, err := c.uiInstall(pos[0])
		if err != nil {
			return err
		}
		out(map[string]any{"installed": pin, "next": "policy show " + pin.Line + ", then policy use " + pin.Line,
			"note": "pinned to this version: newer ones are shown by 'policy list', never applied by themselves"})
	case "policy publish":
		if len(pos) < 1 {
			return errors.New("usage: ternchain policy publish DIR [--share FOLDER]")
		}
		r, err := c.uiPublish(pos[0], f.share)
		if err != nil {
			return err
		}
		out(r)
	default:
		return unknownCommand(cmd)
	}
	return nil
}

// policyRef: a kind (its module in use) or a line (installed, or a built-in one).
func (c *Client) policyRef(ref string) (*ui.Policy, error) {
	if ui.PolicyKinds[ref] != nil {
		p := c.policyOf(ref)
		if p.M.AppliesTo == "lookalike" {
			c.lookTable()
		}
		return p, nil
	}
	if !strings.Contains(ref, "@") {
		ref += "@1"
	}
	for _, k := range ui.PolicyKindNames() {
		if b := ui.BuiltinPolicy(k); b.M.Line == ref {
			return b, nil
		}
	}
	p, err := c.loadPolicyLine(ref)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w ('policy install %s', or a kind: %s)", ref, err, strings.TrimSuffix(ref, "@1"), strings.Join(ui.PolicyKindNames(), ", "))
	}
	return p, nil
}

// policyUse: switch a kind to an installed module (or back to the built-in
// one). The owner's move; an agent that read outside mail needs its owner's
// approval for exactly this switch (a laxer triage is what an attacker would ask for).
func (c *Client) policyUse(line string) (map[string]any, error) {
	if !strings.Contains(line, "@") {
		line += "@1"
	}
	p, err := c.policyRef(line)
	if err != nil {
		return nil, err
	}
	if !c.isOwner() {
		c.syncGuard()
		if len(c.st.Taint) > 0 {
			key := "policy:" + p.M.AppliesTo + ":" + p.Hash
			if err := c.useApproval(key, nil, fmt.Sprintf("switching your %s policy to %s", p.M.AppliesTo, p.Line())); err != nil {
				return nil, err
			}
		}
	}
	prefs := c.policyPrefs()
	if p.Builtin {
		delete(prefs, p.M.AppliesTo)
	} else {
		prefs[p.M.AppliesTo] = p.M.Line
	}
	if err := c.savePolicyPrefs(prefs); err != nil {
		return nil, err
	}
	c.pol, c.look = nil, nil
	return map[string]any{"in_use": map[string]any{p.M.AppliesTo: p.Line()}, "author": p.M.Author, "hash": p.Hash,
		"floors": p.Kind.Floors, "note": "applies to what this identity reads from now on; 'policy reset " + p.M.AppliesTo + "' goes back to the built-in module"}, nil
}

func (c *Client) policyList() map[string]any {
	inUse := map[string]any{}
	lines := c.policyLines()
	for _, k := range ui.PolicyKindNames() {
		p := c.policyOf(k)
		if k == "lookalike" {
			c.lookTable()
		}
		v := map[string]any{"module": p.Line(), "author": p.M.Author, "hash": p.Hash, "what": p.Kind.What}
		if n := c.polNote[k]; n != "" {
			v["warning"] = n
		}
		var inst []string
		for _, pin := range lines[k] {
			inst = append(inst, pin.Line+" "+pin.Version+" by "+pin.Author)
		}
		sort.Strings(inst)
		if len(inst) > 0 {
			v["installed"] = inst
		}
		inUse[k] = v
	}
	r := map[string]any{"policies": inUse, "note": "policy show KIND explains one; policy use LINE switches; nothing updates itself"}
	if cls, err := c.uiPublished(); err == nil {
		var refs []ui.Ref
		for _, cl := range cls {
			if cl.Schema == ui.KindPolicy {
				refs = append(refs, ui.Ref{Kind: cl.Schema, Name: cl.Name, Version: cl.Version, Line: cl.Line, Hash: cl.Hash, Author: cl.Author, URI: cl.URI})
			}
		}
		if ups := c.uiStore().Updates(refs); len(ups) > 0 {
			r["updates"] = ups
		}
	}
	return r
}

// policyExport writes the built-in module of a kind into dir as the start
// of a builder's own module (name NEW, version 1.0.0): edit its rules and
// params, add tables, then 'policy publish DIR' (seals, signs, claims it).
func policyExport(kind, dir, name string) (map[string]any, error) {
	raw, ok := ui.BuiltinManifest(ui.KindPolicy, kind)
	if !ok {
		return nil, fmt.Errorf("no built-in %s module", kind)
	}
	if name == "" {
		name = "my-" + kind
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["name"], m["version"], m["line"] = name, "1.0.0", name+"@1"
	m["description"] = "based on the built-in " + kind + " module: " + fmt.Sprint(m["description"])
	if d := m["description"].(string); len([]rune(d)) > 500 {
		m["description"] = string([]rune(d)[:499]) + "…"
	}
	delete(m, "author")
	delete(m, "payee")
	m["files"] = map[string]any{}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	k := ui.PolicyKinds[kind]
	r := map[string]any{"exported": kind, "dir": dir, "line": name + "@1", "inputs": k.Inputs, "rules": k.Outputs, "params": k.Params, "floors": k.Floors,
		"next": "edit " + filepath.Join(dir, "manifest.json") + ", then: ternchain policy publish " + dir + " --dir YOU"}
	if len(k.Tables) > 0 {
		r["tables"] = k.Tables
		r["next"] = "add table files (e.g. " + filepath.Join(dir, "confusables.txt") + ") and name them in the manifest's \"tables\", then: ternchain policy publish " + dir + " --dir YOU"
	}
	return r, nil
}
