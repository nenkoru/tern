// Local-model mode: content that only a local model may read.
//
//	model show | model set local|cloud:PROVIDER [--mode withhold|warn]   what model reads this identity's mail
//	model local-only add|remove NAME|group:G                             mark a correspondent's or a group's content
//	send --local-only …                                                  mark one message (the recipient's client enforces it)
//	group post --local-only …                                            mark one post (each reader's client enforces it)
//	group policy NAME --local-only[=false]                               a group's admin marks the group (sent with its keys)
//
// A cloud model's provider sees everything the agent reads, and a subpoena to
// it yields the plaintext. The client can't know which model runs behind it:
// the owner declares it (identity.json "model", or TERNCHAIN_MODEL for one
// process), and an agent that declares nothing counts as cloud. Content
// marked local-only reaches a cloud agent as a notice, its text withheld
// ("withhold", the default) or shown under a warning ("warn"): the owner
// reads it on its own device. The mark travels inside the sealed datagram
// (Mail.LocalOnly) and with a group's keys (GroupKeys.LocalOnly), so the
// recipient's client honours the sender's wish; an older or modified client
// doesn't: the mark is a request between honest clients, not cryptography.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	modelLocal       = "local"
	localOnlyWithold = "withhold"
	localOnlyWarn    = "warn"
)

// model: what reads this identity's mail ("" : undeclared).
func (c *Client) model() string {
	if m := strings.TrimSpace(os.Getenv("TERNCHAIN_MODEL")); m != "" {
		return m
	}
	return c.f.Model
}

// cloudModel: a model whose provider sees plaintext (an agent that declared
// none counts as one; an owner reading on its own device doesn't).
func (c *Client) cloudModel() bool {
	m := c.model()
	if m == "" {
		return !c.isOwner()
	}
	return m != modelLocal
}

func validModel(m string) error {
	if m == modelLocal || strings.HasPrefix(m, "cloud:") && len(m) > len("cloud:") {
		return nil
	}
	return fmt.Errorf("model %q: local, or cloud:PROVIDER (cloud:anthropic, cloud:openai, …)", m)
}

// localOnly: why an item is local-model only ("" : it isn't).
func (c *Client) localOnly(it *Item) string {
	if it.Mail != nil && it.Mail.LocalOnly {
		return "its sender marked it local-model only"
	}
	if g, ok := strings.CutPrefix(it.To, "group:"); ok {
		if gk := c.st.Groups[g]; gk != nil && gk.LocalOnly {
			return "group " + g + " is local-model only"
		}
		if inStrs(c.f.LocalOnly, "group:"+g) {
			return "your owner marked group " + g + " local-model only"
		}
	}
	if it.From != "" && (inStrs(c.f.LocalOnly, it.From) || inStrs(c.f.LocalOnly, ownerName(it.From))) {
		return "your owner marked mail from " + ownerName(it.From) + " local-model only"
	}
	return ""
}

// gateLocal: a view of a local-only item, for a cloud model: the text withheld
// (or shown under a warning), and what to do.
func (c *Client) gateLocal(it *Item, v map[string]any) map[string]any {
	why := c.localOnly(it)
	if why == "" || !c.cloudModel() {
		return v
	}
	m := c.model()
	if m == "" {
		m = "undeclared (counts as cloud)"
	}
	notice := fmt.Sprintf("LOCAL-MODEL ONLY: %s, and this agent's model is %s, whose provider would see the plaintext", why, m)
	v["local_only"] = notice
	if c.f.LocalOnlyMode == localOnlyWarn {
		v["warning"] = notice
		return v
	}
	for _, k := range []string{"subject", "content", "data", "body"} {
		if _, ok := v[k]; ok {
			v[k] = "[withheld: local-model only]"
		}
	}
	v["next"] = "tell your owner: it reads this on its own device (ternchain read " + it.ID[:min(18, len(it.ID))] + " --dir OWNER-DIR); don't ask for the text"
	return v
}

// setModel: model set.
func (c *Client) setModel(m, mode string) error {
	if err := validModel(m); err != nil {
		return err
	}
	if mode != "" && mode != localOnlyWithold && mode != localOnlyWarn {
		return fmt.Errorf("--mode %q: withhold or warn", mode)
	}
	c.f.Model = m
	if mode != "" {
		c.f.LocalOnlyMode = mode
	}
	return c.saveIdentityFile()
}

func (c *Client) saveIdentityFile() error { return writeJSON(filepath.Join(c.dir, "identity.json"), c.f) }

// markLocalOnly: model local-only add|remove NAME|group:G.
func (c *Client) markLocalOnly(add bool, target string) error {
	var out []string
	for _, x := range c.f.LocalOnly {
		if x != target {
			out = append(out, x)
		}
	}
	if add {
		out = append(out, target)
	}
	sort.Strings(out)
	c.f.LocalOnly = out
	return c.saveIdentityFile()
}

func (c *Client) modelView() map[string]any {
	mode := c.f.LocalOnlyMode
	if mode == "" {
		mode = localOnlyWithold
	}
	m := c.model()
	v := map[string]any{"model": m, "cloud": c.cloudModel(), "local_only": c.f.LocalOnly, "mode": mode}
	if m == "" {
		v["model"] = "undeclared"
	}
	if c.cloudModel() {
		v["note"] = "a cloud model's provider sees everything this identity reads; local-only content is " + mode + " here"
	}
	return v
}

func (c *Client) localMode() string {
	if c.f.LocalOnlyMode == localOnlyWarn {
		return "shown under a warning"
	}
	return "withheld"
}

// groupLocalOnly: group policy NAME --local-only: the admin marks the group
// for every member (sent with a new key generation); a member marks it for itself.
func (c *Client) groupLocalOnly(name string, on bool) (map[string]any, error) {
	g := c.st.Groups[name]
	if g == nil {
		return nil, fmt.Errorf("you are not in group %s", name)
	}
	admin := g.Anon != nil && g.Anon.Members != nil && g.Anon.Admin == c.f.Name || g.Anon == nil && !g.IsPublic && c.hasRole(name, "admin", c.f.Name)
	if !admin {
		if err := c.markLocalOnly(on, "group:"+name); err != nil {
			return nil, err
		}
		return map[string]any{"group": name, "local_only": on, "for": "you (only its admin marks it for every member)"}, nil
	}
	g.LocalOnly = on
	r, err := c.rotateGroup(name, "local-only policy") // members get the mark with the new generation
	if err != nil {
		return nil, err
	}
	r["local_only"], r["for"] = on, "every member"
	return r, nil
}
