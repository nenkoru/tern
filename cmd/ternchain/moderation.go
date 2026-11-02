// Moderation claims in the reader's client (BRICKS #33, D5): hiding and
// followed blocklists work for any community that issues the standard's
// claims (contracts/Community.sol MODERATION_SCHEMA; readers in bricks.go),
// not only for Commons. Commons keeps its own path (its on-chain bans and
// hides, the veto window before a list's ban reaches its followers); a Sets
// group or anyone's ICommunity contract is read through its claims.
//
//	blocklist follow CONTRACT:NAME    CONTRACT: Sets, Commons, or an 0x address of any ICommunity
package main

import (
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// modCache: one community's moderation state, read at a time.
type modCache struct {
	hidden, banned map[common.Hash]bool
	at             time.Time
}

// modTTL: how long a community's moderation state is used before it is read again.
var modTTL = 30 * time.Second

// claimList: a list (or community) named CONTRACT:NAME: the ICommunity
// contract and the id it knows the community by (Sets: setID(NAME); others:
// the name's id). ok false: a plain name (a Commons community, as before).
func (c *Client) claimList(l string) (common.Address, common.Hash, bool) {
	k, name, ok := strings.Cut(l, ":")
	if !ok || name == "" {
		return common.Address{}, common.Hash{}, false
	}
	var a common.Address
	switch {
	case common.IsHexAddress(k):
		a = common.HexToAddress(k)
	case c.ch.dep.Contracts[k] != "":
		a = common.HexToAddress(c.ch.dep.Contracts[k])
	default:
		return common.Address{}, common.Hash{}, false
	}
	if c.ch.has("Sets") && a == common.HexToAddress(c.ch.dep.Contracts["Sets"]) {
		return a, setID(name), true
	}
	return a, idOf(name), true
}

// modState: what a community's counting moderation claims say now (cached for modTTL).
func (c *Client) modState(a common.Address, id common.Hash) (hidden, banned map[common.Hash]bool) {
	if !c.ch.has("EAS") {
		return nil, nil
	}
	k := a.Hex() + id.Hex()
	if c.modMemo == nil {
		c.modMemo = map[string]modCache{}
	}
	if m, ok := c.modMemo[k]; ok && time.Since(m.at) < modTTL {
		return m.hidden, m.banned
	}
	h, b, err := c.moderationState(a, id)
	if err != nil {
		return nil, nil
	}
	c.modMemo[k] = modCache{h, b, time.Now()}
	return h, b
}

// claimBans: the claim list bans this name.
func (c *Client) claimBans(l, name string) bool {
	a, id, ok := c.claimList(l)
	if !ok {
		return false
	}
	_, banned := c.modState(a, id)
	return banned[idOf(name)]
}

// claimBansOwnerWide: the claim list bans some name of this owner (an agent of it).
func (c *Client) claimBansOwnerWide(l, owner string) bool {
	a, id, ok := c.claimList(l)
	if !ok {
		return false
	}
	_, banned := c.modState(a, id)
	for h := range banned {
		if n, err := c.res.nameOf(h); err == nil && ownerName(n) == owner {
			return true
		}
	}
	return false
}

// groupClaimFilter: a post in a group that isn't a Commons community: hidden
// when the group's moderators' claims (through Sets, its ICommunity) hide it
// or ban its author or the author's owner. Returns true if it hid the post.
func (c *Client) groupClaimFilter(it *Item, post common.Hash, group, from string) bool {
	if !c.ch.has("Sets") {
		return false
	}
	hidden, banned := c.modState(common.HexToAddress(c.ch.dep.Contracts["Sets"]), setID(group))
	switch {
	case hidden[post]:
		it.Status, it.Trust = "hidden", "hidden by moderator claim in "+group
	case banned[idOf(from)] || banned[idOf(ownerName(from))]:
		it.Status, it.Trust = "hidden", "author banned by moderator claim in "+group
	default:
		return false
	}
	return true
}
