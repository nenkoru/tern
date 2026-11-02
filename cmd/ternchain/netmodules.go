// Network modules: parts of ternchain a network may not have. The public
// profile (deploy --contract-set public) leaves out the shielded pool, stealth
// payments, the coin house, pots and fiat; a client on such a network
// doesn't offer their commands. What a network has is read from its
// contracts (net.json and its Directory), not from its name: a network that
// adds such a module later (ModuleRegistry, a new deployment) gets the
// commands back where the contracts are there.
package main

import (
	"fmt"
	"strings"
)

type netModule struct {
	Name      string
	What      string
	Contracts []string // all must be on the network
	Commands  []string // "word" or "word sub"
	NotPublic bool     // never on a public-profile network (gambling, fiat), contracts or not
}

var netModules = []netModule{
	{Name: "shielded-pool", What: "shielded transfers (Privacy Pools)", Contracts: []string{"PrivacyPool", "AssociationSets", "PoolVerifier"},
		Commands: []string{"private deposit", "private shield", "private send", "private balance", "private notes", "asp"}},
	{Name: "stealth", What: "stealth payments (ERC-5564/6538)", Contracts: []string{"StealthPay", "ERC5564Announcer", "ERC6538Registry"},
		Commands: []string{"private enable", "private pay"}},
	{Name: "coin-house", What: "the coin house of governance (wTERN)", Contracts: []string{"WrappedTern", "TernGovernor"},
		Commands: []string{"gov lock", "gov unlock"}},
	{Name: "anon-groups", What: "anonymous groups (AnonGroups: membership as a tree of commitments)", Contracts: []string{"AnonGroups"},
		Commands: []string{"group invite"}},
	{Name: "private-reputation", What: "private reviews and reputation proofs (PrivateReviews)", Contracts: []string{"PrivateReviews"},
		Commands: []string{"rep"}},
	{Name: "pot", What: "pots and lotteries", Commands: []string{"pot"}, NotPublic: true},
	{Name: "fiat", What: "fiat payments confirmed on chain", Commands: []string{"fiat"}, NotPublic: true},
	// A community's post fee is in the chain's coin (Commons, a core contract), which a public network's users don't hold: none there.
	{Name: "community-fees", What: "community post fees (in the chain's coin, which users here don't hold)", Commands: []string{"commons fee"}, NotPublic: true},
}

// netModuleOf: the network module a command belongs to, if any.
func netModuleOf(cmd string) *netModule {
	word, _, _ := strings.Cut(cmd, " ")
	for i := range netModules {
		for _, c := range netModules[i].Commands {
			if c == cmd || c == word {
				return &netModules[i]
			}
		}
	}
	return nil
}

// hasNetModule: is the module on this network?
func (ch *Chain) hasNetModule(m *netModule, profile string) bool {
	if m.NotPublic && profile == "public" {
		return false
	}
	for _, c := range m.Contracts {
		if !ch.has(c) {
			return false
		}
	}
	return true
}

// netModuleRefusal: nil if cmd is not a module's, or the network has it.
func netModuleRefusal(cmd string, ch *Chain, f *DevnetFile) error {
	m := netModuleOf(cmd)
	if m == nil || ch == nil {
		return nil
	}
	profile := ""
	if f != nil {
		profile = f.Profile
	}
	if ch.hasNetModule(m, profile) {
		return nil
	}
	return fmt.Errorf("%s isn't on this network: it has no %s (module %s; the public profile ships without it — anyone may deploy such a module "+
		"and publish it in the ModuleRegistry, under their own responsibility: docs/base-sepolia.md, \"Optional modules\")", cmd, m.What, m.Name)
}
