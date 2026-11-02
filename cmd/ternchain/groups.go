// Groups are a recipe, not a primitive:
//
//	group = a set (members, roles, join rule)          — Sets contract
//	      + a channel (a view key and one key per epoch) — off chain
//	      + moderation (hide attestations, removal)      — Attestations, Sets
//
// A public group publishes its channel keys in the set's data slot, so
// anyone can read it, like a forum. A private group's admins hand the keys
// to admitted members in a sealed tern.group-key datagram.
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"filippo.io/age"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const channelKeysSlot = "channel-keys"

func randRead(b []byte) (int, error) { return rand.Read(b) }

func setID(name string) common.Hash { return crypto.Keccak256Hash([]byte("set:" + name)) }

// at returns the channel's view key and epoch recipient for epoch e.
func (g *GroupKeys) at(e uint64) (*ecdh.PrivateKey, string, error) {
	v, err := g.view()
	if err != nil {
		return nil, "", err
	}
	if e < g.First || e >= g.First+uint64(len(g.Keys)) || g.Keys[e-g.First] == "" {
		return nil, "", fmt.Errorf("group %s has no key for epoch %d (ask an admin to rotate)", g.Name, e)
	}
	_, rec, err := parseIdentity(g.Keys[e-g.First])
	if err != nil {
		return nil, "", err
	}
	return v, rec, nil
}

func (g *GroupKeys) view() (*ecdh.PrivateKey, error) {
	if perfFixes && g.vk != nil && g.vkHex == g.ViewKey {
		return g.vk, nil
	}
	raw, err := hex.DecodeString(g.ViewKey)
	if err != nil {
		return nil, err
	}
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err == nil {
		g.vk, g.vkHex = k, g.ViewKey
	}
	return k, err
}

func (g *GroupKeys) identities() []age.Identity {
	sig := ""
	if perfFixes {
		sig = strings.Join(g.Keys, ",")
		for _, o := range g.Old {
			sig += ";" + strings.Join(o.Keys, ",")
		}
		if g.ids != nil && g.idsSig == sig {
			return g.ids
		}
	}
	var out []age.Identity
	for _, gen := range g.gens() {
		for _, k := range gen.Keys {
			if id, _, err := parseIdentity(k); err == nil {
				out = append(out, id)
			}
		}
	}
	if perfFixes {
		g.ids, g.idsSig = out, sig
	}
	return out
}

// destroyOld forgets private channel keys past the retention. A public
// channel's keys are on chain for good, like a public forum.
func (g *GroupKeys) destroyOld(current, retain uint64) {
	if g.IsPublic {
		return
	}
	for _, gen := range g.gens() {
		for i := range gen.Keys {
			if gen.First+uint64(i)+retain < current {
				gen.Keys[i] = ""
			}
		}
	}
	// A generation with no key left is gone, its view key with it.
	var keep []*GroupKeys
	for _, o := range g.Old {
		for _, k := range o.Keys {
			if k != "" {
				keep = append(keep, o)
				break
			}
		}
	}
	g.Old = keep
}

func newGroupKeys(name string, first uint64, n int, public, pq bool) (*GroupKeys, error) {
	v, err := newViewKey()
	if err != nil {
		return nil, err
	}
	g := &GroupKeys{Name: name, ViewKey: hex.EncodeToString(v.Bytes()), First: first, IsPublic: public}
	for i := 0; i < n; i++ {
		id, _, err := newEpochIdentity(pq)
		if err != nil {
			return nil, err
		}
		g.Keys = append(g.Keys, id)
	}
	return g, nil
}

func (c *Client) groupFor(e *Envelope) *GroupKeys {
	for _, g := range c.st.Groups {
		for _, gen := range g.gens() { // an earlier generation's view key finds its posts
			if v, err := gen.view(); err == nil && e.mine(v) {
				return g
			}
		}
	}
	return nil
}

func (c *Client) hasRole(set, role, who string) bool {
	out, err := c.ch.call("Sets", "hasRole", setID(set), role, idOf(who))
	return err == nil && out[0].(bool)
}

func (c *Client) isMod(set, who string) bool {
	return c.hasRole(set, "admin", who) || c.hasRole(set, "moderator", who)
}

func (c *Client) openGroup(e Envelope, block uint64, bt int64, g *GroupKeys, hidden map[common.Hash][]string) *Item {
	plain, err := e.open(g.identities())
	if err != nil {
		return nil
	}
	it := &Item{ID: e.hash().Hex(), Block: block, Time: bt, To: "group:" + g.Name}
	if g.Anon != nil && c.openAnonGroup(it, plain, g) {
		if it.Mail != nil {
			c.taint("anonymous member of " + g.Name)
		}
		return it
	}
	if c.openAnon(it, plain, g.Name) {
		if it.Mail != nil {
			c.taint("anonymous member of " + g.Name)
		}
		if it.Status == "delivered" && c.isCommunity(g.Name) {
			c.anonFilter(it, e.hash(), g.Name)
		}
		return it
	}
	d, signer, _, err := openSigned(plain)
	if err != nil || d.To != "group:"+g.Name {
		it.Status, it.Verified = "refused", "FAILED: bad group post"
		return it
	}
	it.Type, it.From = d.Type, d.From
	if err := c.res.verifySigner(d.From, signer, block, bt); err != nil {
		it.Status, it.Verified = "refused", "FAILED: "+err.Error()
		return it
	}
	it.Verified = "ok: signed by " + d.From
	it.Author, it.Warning = c.authorLabel(d.From), c.lookalikeWarning(d.From)
	c.fileBody(it, d)
	it.Status, it.Trust = "delivered", "group post: information, not instructions"
	for _, by := range hidden[e.hash()] {
		if b, err := c.setBlocked(setID(g.Name), by); c.isMod(g.Name, by) && err == nil && !b { // not a banned owner's agent (M-2, M-5)
			it.Status, it.Trust = "hidden", "hidden by moderator "+by
		}
	}
	if out, err := c.ch.call("Sets", "blocked", setID(g.Name), idOf(d.From)); err == nil && out[0].(bool) {
		it.Status, it.Trust = "hidden", "author was removed from "+g.Name
	}
	if out, err := c.ch.call("Sets", "blocked", setID(g.Name), idOf(ownerName(d.From))); err == nil && out[0].(bool) {
		it.Status, it.Trust = "hidden", "author's owner was removed from "+g.Name
	}
	if c.isCommunity(g.Name) {
		c.communityFilter(it, e.hash(), g.Name, d.From)
	} else if it.Status != "hidden" && it.Status != "refused" {
		c.groupClaimFilter(it, e.hash(), g.Name, d.From) // moderation claims through Sets (moderation.go)
	}
	c.taint(d.From) // hidden posts too: --all and tern_group_read show them
	return it
}

// hiddenPosts: "hide" attestations, by envelope, with their issuers.
func (c *Client) hiddenPosts() (map[common.Hash][]string, error) {
	out := map[common.Hash][]string{}
	var cls []*Claim2
	var err error
	if perfFixes { // only the kinds read here: not a copy of every claim per scan
		cls, err = c.easClaimsOf(nil, map[string]bool{"hide": true, "report": true, "report-dismissed": true})
	} else {
		cls, err = c.easClaims(nil)
	}
	if err != nil {
		return nil, err
	}
	c.flags, c.reportsBy = map[common.Hash][]*Claim2{}, map[string][]*Claim2{}
	for _, cl := range cls {
		if cl.Revoked && cl.Kind == "report" && cl.Issuer != "" { // its record keeps it, if moderators dismissed it (HM-13)
			c.reportsBy[ownerName(cl.Issuer)] = append(c.reportsBy[ownerName(cl.Issuer)], cl)
		}
		if cl.Revoked || cl.Issuer == "" {
			continue
		}
		switch cl.Kind {
		case "hide":
			out[cl.Subject] = append(out[cl.Subject], cl.Issuer)
		case "report", "report-dismissed":
			c.flags[cl.Subject] = append(c.flags[cl.Subject], cl)
			if cl.Kind == "report" {
				c.reportsBy[ownerName(cl.Issuer)] = append(c.reportsBy[ownerName(cl.Issuer)], cl)
			}
		}
	}
	return out, nil
}

// ---- actions

// createGroup: a set (open for public groups, by approval for private ones) and a channel.
func (c *Client) createGroup(name string, public bool) error {
	e, err := c.ch.epoch()
	if err != nil {
		return err
	}
	ahead := publicChannelAhead // a public channel's keys are public anyway
	if !public {
		ahead = 2 * groupRotateEvery // the keeper rotates before they run out (rotate.go)
	}
	g, err := newGroupKeys(name, e, ahead, public, c.f.PQ)
	if err != nil {
		return err
	}
	g.Rotated = e
	rule := uint8(1) // approval
	if public {
		rule = 0 // open
	}
	if _, err := c.ch.send(c.key, nil, "Sets", "create", name, c.id(), rule); err != nil {
		return err
	}
	if public {
		b, _ := json.Marshal(g)
		if _, err := c.ch.send(c.key, nil, "Sets", "publish", setID(name), c.id(), channelKeysSlot, b); err != nil {
			return err
		}
	}
	c.st.Groups[name] = g
	c.enableRing() // the creator counts in the group's anonymity set too
	return c.save()
}

func (c *Client) joinGroup(name string) (string, error) {
	if g := c.st.Groups[name]; g != nil && g.Anon != nil {
		return "already a member of the anonymous group " + name + ": post with 'group post " + name + " TEXT'", nil
	}
	if inv := c.st.AnonInvites[name]; inv != nil {
		return c.joinAnon(inv)
	}
	sid := setID(name)
	out, err := c.ch.call("Sets", "sets", sid)
	if err != nil {
		return "", err
	}
	if !out[2].(bool) {
		return "", fmt.Errorf("no group %s", name)
	}
	setBool := func(m string) bool { r, err := c.ch.call("Sets", m, sid, c.id()); return err == nil && r[0].(bool) }
	if setBool("blocked") {
		return "", fmt.Errorf("you were removed from %s: you can't join it again", name)
	}
	community := c.isCommunity(name)
	// Already in (its creator, or let in before): nothing to ask for, only keys to fetch.
	already := setBool("members")
	if already && c.st.Groups[name] != nil {
		return "already a member (you created it or were let in): post with 'group post " + name + " TEXT'", nil
	}
	if !already {
		if err := c.guardJoin(name); err != nil {
			return "", err
		}
		if _, err := c.ch.send(c.key, nil, "Sets", "request", sid, c.id()); err != nil {
			return "", err
		}
	}
	// Members are the anonymity set of anonymous posts and votes ("one of N"):
	// publish a ring key now, so this member counts in it from the start.
	if err := c.enableRing(); err != nil {
		fmt.Fprintln(os.Stderr, "note: no ring key published (anonymous posts would not count you):", err)
	}
	if out[1].(uint8) != 0 && !community {
		if already {
			return "already a member, but this device has no channel keys: an admin sends them ('group admit')", nil
		}
		return "requested: an admin will send the keys when it admits you", nil
	}
	evs, err := c.ch.events("Sets", "Published", []common.Hash{sid})
	if err != nil {
		return "", err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Args["key"].(string) != channelKeysSlot {
			continue
		}
		by, _ := c.res.nameOf(common.Hash(evs[i].Args["by"].([32]byte)))
		if !c.hasRole(name, "admin", by) {
			continue
		}
		var g GroupKeys
		if err := json.Unmarshal(evs[i].Args["value"].([]byte), &g); err != nil {
			return "", err
		}
		g.IsPublic = true
		c.st.Groups[name] = &g
		c.markBackfill(name)
		if community && (c.commonsBool("isMember", idOf(name), c.id()) || c.commonsBool("isMember", idOf(name), idOf(c.f.Owner))) {
			return "joined: a member of " + name, c.save()
		}
		if community { // a community's channel is public; posting counts once a moderator admits you
			return "requested: a moderator admits you ('commons admit " + name + " " + c.f.Name + "'); meanwhile you can read it", c.save()
		}
		return "joined", c.save()
	}
	return "", errors.New("public group has no published channel keys")
}

// admitPending admits everyone waiting to join groups we administer, and
// sends them the channel keys.
func (c *Client) admitPending() ([]string, error) {
	done, _, err := c.admitPendingFor(nil)
	return done, err
}

// admitPendingFor admits the requesters of our Sets groups that ok accepts
// (nil: everyone waiting); the others are returned as waiting ("GROUP: NAME").
// An explicit accepted name also resends the current keys to an existing member,
// allowing delivery failures to be retried after a restart. Anyone
// can ask to join a private group on chain, so the CLI's keeper run (FOR-AGENTS
// rule 21: daily, for rotation) admits nobody it wasn't told to (redteam E1).
// An anonymous group's pending members were invited one by one: they are admitted.
func (c *Client) admitPendingFor(ok func(group, member string) bool) ([]string, []string, error) {
	done, waiting := []string{}, []string{}
	for name, g := range c.st.Groups {
		if g.Anon != nil {
			if g.Anon.Members != nil && g.Anon.Admin == c.f.Name {
				d, err := c.admitAnon(g)
				done = append(done, d...)
				if err != nil {
					return done, waiting, err
				}
			}
			continue
		}
		if !c.hasRole(name, "admin", c.f.Name) {
			continue
		}
		sid := setID(name)
		reqs, err := c.ch.events("Sets", "Requested", []common.Hash{sid})
		if err != nil {
			return done, waiting, err
		}
		if ok != nil {
			// Some join rules admit without a Requested event. Explicit retries
			// must also reach those members, while --all stays pending-only.
			added, err := c.ch.events("Sets", "Added", []common.Hash{sid})
			if err != nil {
				return done, waiting, err
			}
			reqs = append(reqs, added...)
		}
		seen := map[common.Hash]bool{}
		for _, r := range reqs {
			mid := common.Hash(r.Args["member"].([32]byte))
			if seen[mid] {
				continue
			}
			seen[mid] = true
			out, err := c.ch.call("Sets", "members", sid, mid)
			if err != nil {
				return done, waiting, err
			}
			admitted := out[0].(bool)
			if admitted && ok == nil { // --all admits pending names; resends need an explicit name
				continue
			}
			if !admitted {
				out, err := c.ch.call("Sets", "requested", sid, mid)
				if err != nil {
					return done, waiting, err
				}
				if !out[0].(bool) { // removal clears the request; old logs cannot authorize re-admission
					continue
				}
			}
			member, err := c.res.nameOf(mid)
			if err != nil {
				continue
			}
			if b, err := c.setBlocked(sid, member); err != nil {
				return done, waiting, err
			} else if b {
				continue
			}
			if ok != nil && !ok(name, member) {
				if !admitted {
					waiting = append(waiting, name+": "+member)
				}
				continue
			}
			if !admitted {
				if _, err := c.ch.send(c.key, nil, "Sets", "add", sid, c.id(), mid); err != nil {
					return done, waiting, err
				}
			}
			// Membership commits before key delivery. Recheck it on every attempt,
			// including retries after a removal, ban or recipient-key renewal.
			out, err = c.ch.call("Sets", "members", sid, mid)
			if err != nil {
				return done, waiting, err
			}
			if !out[0].(bool) {
				continue
			}
			if b, err := c.setBlocked(sid, member); err != nil {
				return done, waiting, err
			} else if b {
				continue
			}
			if _, err := c.sendDatagram(tGroupKey, member, g.forMembers()); err != nil {
				return done, waiting, fmt.Errorf("%s is admitted to %s, but key delivery failed: %w (retry group admit %s %s)", member, name, err, name, member)
			}
			done = append(done, name+": "+member)
		}
	}
	sort.Strings(waiting)
	return done, waiting, nil
}

// setBlocked: is member, or its owner, banned from the Sets group sid? Sets
// bans one exact name, so a banned owner would come back as a new agent
// (audit ext M-5): its keys never go to any name of that family.
func (c *Client) setBlocked(sid common.Hash, member string) (bool, error) {
	for _, n := range []string{member, ownerName(member)} {
		out, err := c.ch.call("Sets", "blocked", sid, idOf(n))
		if err != nil {
			return false, err
		}
		if out[0].(bool) {
			return true, nil
		}
	}
	return false, nil
}

// adminGroupMembers: the members of each group we administer.
func (c *Client) adminGroupMembers() map[string][]string {
	out := map[string][]string{}
	for name := range c.st.Groups {
		if c.hasRole(name, "admin", c.f.Name) {
			ms, _ := c.setMembers(name)
			out[name] = ms
		}
	}
	return out
}

// acceptGroupKey stores channel keys sent by a group's admin.
func (c *Client) acceptGroupKey(it *Item, d *Datagram) {
	var g GroupKeys
	if json.Unmarshal(d.Body, &g) != nil || g.Name == "" {
		return
	}
	if g.Anon != nil {
		c.acceptAnonKeys(it, d, &g)
		return
	}
	if !c.hasRole(g.Name, "admin", d.From) {
		it.Verified += " (ignored: sender is not an admin of " + g.Name + ")"
		return
	}
	g.Old, g.Rotated, g.RotatedAt = nil, 0, 0 // a member gets one generation, never the admin's history (redteam E2)
	old := c.st.Groups[g.Name]
	if old != nil && g.Gen < old.Gen {
		it.Verified += " (ignored: an older key generation)"
		return
	}
	c.st.Groups[g.Name] = installGen(old, &g) // a rotation: ours stays as Old, to read its posts
	if old == nil || g.Gen == old.Gen {
		if old != nil { // the same generation again: what we already kept stays
			g.Old, g.Rotated, g.RotatedAt = old.Old, old.Rotated, old.RotatedAt
		}
		c.st.Groups[g.Name] = &g
	}
	c.markBackfill(g.Name)
}

func (c *Client) markBackfill(group string) {
	if c.st.Backfill == nil {
		c.st.Backfill = map[string]bool{}
	}
	c.st.Backfill[group] = true
}

// communityFilter: in a community, readers hide posts from banned names
// (here or in a list it follows), posts and names its moderators hid, posts
// from non-members, a post its author already made (spam repeats), and a
// post reported by reportHideAt members until a moderator dismisses the reports.
func (c *Client) communityFilter(it *Item, post common.Hash, group, from string) {
	cid, fid, oid := idOf(group), idOf(from), idOf(ownerName(from))
	if hide, n, w := c.reportHides(cid, post, from, group); hide {
		if reportWeights && w < c.postThreshold(group, from) {
			it.Status, it.Trust = "hidden", fmt.Sprintf("author's record: %s's names were banned, hidden or widely reported in communities (penalty %.0f): hidden until a moderator decides ('commons dismiss' shows it)", ownerName(from), c.conductPenalty(ownerName(from)))
			return
		}
		it.Status, it.Trust = "hidden", fmt.Sprintf("reported by %d members of %s (weight %.2f; %s): hidden until a moderator decides ('commons hide' or 'commons dismiss')", n, group, w, c.reportNeed(group))
		return
	}
	if it.Mail != nil && strings.TrimSpace(it.Mail.Text) != "" {
		sp := c.policyOf("spam") // the spam module in use (its params; the built-in: the constants below)
		dupAt, series, perHour, window := sp.Param("near_dup_at"), int(sp.Param("near_dup_series")), int(sp.Param("posts_per_hour")), int64(sp.Param("rate_window_minutes")*60)
		words := shingles(it.Mail.Text)
		recent, similar := 0, 0
		for _, o := range c.itemsOf(it.To) { // the posts fetched before this one
			if o.ID == it.ID {
				break
			}
			if o.To != it.To || o.Mail == nil || ownerName(o.From) != ownerName(from) || o.From == "" {
				continue
			}
			if o.Mail.Text == it.Mail.Text {
				it.Status, it.Trust = "hidden", "repeats "+o.From+"'s earlier post "+o.ID[:18]+" word for word"
				return
			}
			if abuseFixesOff {
				continue
			}
			// near-duplicates: at once across one owner's names (a farm's signature);
			// from one name, a short series is fine, more than nearDupSeries is not
			if sim := jaccard(words, shingles(o.Mail.Text)); sim >= dupAt {
				if similar++; o.From != from || similar >= series {
					it.Status, it.Trust = "hidden", fmt.Sprintf("near-duplicate (%.0f%% the same words) of %s's earlier post %s", 100*sim, o.From, o.ID[:18])
					return
				}
			}
			if it.Time-o.Time < window && o.Status != "refused" {
				recent++
			}
		}
		if recent >= perHour && !abuseFixesOff {
			within := "within the hour"
			if window != 3600 {
				within = fmt.Sprintf("within %d minutes", window/60)
			}
			it.Status, it.Trust = "hidden", fmt.Sprintf("rate: %s's names posted %d times in %s %s (readers show %d)", ownerName(from), recent+1, group, within, perHour)
			return
		}
	}
	switch {
	case c.commonsBool("isBanned", cid, fid) && c.banCounts(cid, fid, oid):
		it.Status, it.Trust = "hidden", "author is banned from "+group
	case c.citizensFilter(it, group, from):
	case c.announceFilter(it, group, from): // announce-only: its admins and moderators post (news.go)
	case c.commonsBool("hidden", cid, post) && c.commonsActed(cid, 3, post) || c.commonsBool("hidden", cid, fid) && c.commonsActed(cid, 3, fid): // 3: hide
		it.Status, it.Trust = "hidden", "hidden by "+group+"'s moderators"
	case !c.commonsBool("isMember", cid, fid) && !c.commonsBool("isMember", cid, oid):
		it.Status, it.Trust = "hidden", "author is not a member of "+group
	}
}

// banCounts: a ban of the author (or its owner) here was by a name not itself
// banned here (M-2: commonsActed); a ban through a followed list counts as is.
func (c *Client) banCounts(cid, fid, oid common.Hash) bool {
	if !c.commonsBool("banned", cid, fid) && !c.commonsBool("banned", cid, oid) {
		return true // a followed list's ban
	}
	return c.commonsBool("banned", cid, fid) && c.commonsActed(cid, 2, fid) || c.commonsBool("banned", cid, oid) && c.commonsActed(cid, 2, oid) // 2: ban
}

// anonFilter: an anonymous post in a community is still the community's:
// moderators' hides and members' reports apply, and a pseudonym is fresh per
// thread, so near-duplicates are checked against every earlier anonymous post.
func (c *Client) anonFilter(it *Item, post common.Hash, group string) {
	cid := idOf(group)
	switch {
	case c.announceFilter(it, group, it.From): // no admin speaks anonymously in an announce-only community
		return
	case c.commonsBool("hidden", cid, post) && c.commonsActed(cid, 3, post):
		it.Status, it.Trust = "hidden", "hidden by "+group+"'s moderators"
		return
	case func() bool { h, _, _ := c.reportHides(cid, post, it.From, group); return h }():
		it.Status, it.Trust = "hidden", fmt.Sprintf("reported by members of %s: hidden until a moderator decides", group)
		return
	}
	if it.Mail == nil || abuseFixesOff {
		return
	}
	words, dupAt := shingles(it.Mail.Text), c.policyOf("spam").Param("near_dup_at")
	for _, o := range c.st.Items {
		if o.ID == it.ID {
			break
		}
		if o.To == it.To && o.Mail != nil && strings.HasPrefix(o.From, "anon:") && jaccard(words, shingles(o.Mail.Text)) >= dupAt {
			it.Status, it.Trust = "hidden", "near-duplicate of an earlier anonymous post "+o.ID[:18]+" (anonymous posts are checked against each other)"
			return
		}
	}
}

// postGroup posts; in a community with a posting fee it also pays the fee
// to the community, with the post's id as the memo.
func (c *Client) postGroup(group, text string, data json.RawMessage) (string, error) {
	return c.postGroupReply(group, text, data, "")
}

// postGroupReply: a post that answers another (a comment: ui post --reply).
func (c *Client) postGroupReply(group, text string, data json.RawMessage, replyTo string) (string, error) {
	if g := c.st.Groups[group]; g != nil && g.Anon != nil {
		return c.postAnonGroup(g, text, data, replyTo)
	}
	fee := new(big.Int)
	done := func(bool) {}
	if c.isCommunity(group) { // the fee is the admins' to set, at any moment: guarded before posting
		k, err := c.community(group)
		if err != nil {
			return "", err
		}
		if !c.commonsBool("isMember", idOf(group), c.id()) && !c.commonsBool("isMember", idOf(group), idOf(c.f.Owner)) {
			return "", fmt.Errorf("you are not admitted to %s yet: readers hide posts from non-members. "+
				"A moderator admits you ('commons admit %s %s'; 'commons requests %s' lists who waits)", group, group, c.f.Name, group)
		}
		if h, _ := c.citizensHouse(group); h != (common.Address{}) && (fromAgent(c.f.Name) || !c.citizenNow(c.f.Name)) {
			return "", fmt.Errorf("%s is citizens only: only citizens post there, as themselves (agents read; readers hide anything else)", group)
		}
		if c.announceOnly(group) && !c.newsPoster(group, c.f.Name) {
			return "", fmt.Errorf("%s is announce-only: only its admins and moderators post there (members read; readers hide anything else)", group)
		}
		if fee = k.PostFee; fee.Sign() > 0 {
			if done, err = c.guardFee(group, fee); err != nil {
				return "", err
			}
		}
	}
	id, err := c.send(SendOpts{To: "group:" + group, Intent: "inform", Text: text, Data: data, ReplyTo: replyTo, KeepPostage: true})
	if err != nil || fee.Sign() == 0 {
		done(false)
		return id, err
	}
	if _, err := c.ch.send(c.key, nil, "Bank", "pay", c.id(), idOf(group), fee, common.HexToHash(id)); err != nil {
		done(false)
		return id, fmt.Errorf("posted %s, but the community's post fee (%s) wasn't paid: %w", id, fmtCoins(fee), err)
	}
	done(true)
	return id, nil
}

func (c *Client) groupItems(group string) []*Item {
	return append([]*Item(nil), c.itemsOf("group:"+group)...)
}

func (c *Client) moderate(group, action, target, reason string) error {
	if g := c.st.Groups[group]; g != nil && g.Anon != nil {
		return c.moderateAnon(group, action, target, reason)
	}
	sid := setID(group)
	switch action {
	case "hide":
		return c.attest(target, "hide", reason, 0)
	case "ban", "unban", "remove": // remove: a ban, the word people use (and anonymous groups' action)
		if action == "remove" {
			action = "ban"
		}
		if _, err := c.ch.send(c.key, nil, "Sets", "remove", sid, c.id(), idOf(target), action == "ban", reason); err != nil {
			return err
		}
		if g := c.st.Groups[group]; g != nil && !g.IsPublic && c.hasRole(group, "admin", c.f.Name) {
			_, err := c.rotateGroup(group, action+" of "+target) // the removed member reads nothing after this
			return err
		}
		return nil
	case "mod", "unmod":
		_, err := c.ch.send(c.key, nil, "Sets", "setRole", sid, c.id(), "moderator", idOf(target), action == "mod")
		return err
	case "admin":
		_, err := c.ch.send(c.key, nil, "Sets", "setRole", sid, c.id(), "admin", idOf(target), true)
		return err
	}
	return fmt.Errorf("unknown action %q (hide, ban|remove, unban, mod, unmod, admin)", action)
}

// ---- anonymous posts: "a member of this group wrote this"

// postAnon posts to a group with a ring signature. The key image is stable
// per (group, thread), so a thread shows a pseudonym per member and a poll
// counts one vote per member, while nobody learns which member it was.
// The envelope is sealed to the group and to our owner, and lists only the
// group as reader (fingerprints of our keys would give us away).
func (c *Client) postAnon(group, thread, text string, data json.RawMessage) (string, error) {
	gk := c.st.Groups[group]
	if gk == nil {
		return "", fmt.Errorf("you are not in group %s", group)
	}
	e, err := c.ch.epoch()
	if err != nil {
		return "", err
	}
	view, groupReader, err := gk.at(e)
	if err != nil {
		return "", err
	}
	if thread == "" {
		thread = "t_" + hex.EncodeToString(randBytes(6))
	}
	m := Mail{Thread: thread, Intent: "inform", Text: text, Data: data}
	body, _ := json.Marshal(m)
	now := time.Now().Unix()
	if h, err := c.ch.b.HeaderByNumber(context.Background(), nil); err == nil {
		now = int64(h.Time)
	}
	d := Datagram{Type: tMessage, From: "anon", To: "group:" + group, Created: now, Readers: []string{fingerprint(groupReader)}, Body: body}
	payload, _ := json.Marshal(d)
	rs, err := c.ringSign(group, group+"|"+thread, payload)
	if err != nil {
		return "", err
	}
	signed, _ := json.Marshal(Signed{Payload: payload, Ring: rs})
	readers := []string{groupReader}
	if _, ownerKey, err := c.res.keysAt(c.f.Owner, e); err == nil {
		readers = append(readers, ownerKey)
	}
	env, err := seal(signed, e, view.PublicKey(), c.view.PublicKey(), readers, c.ch.bucketBits())
	if err != nil {
		return "", err
	}
	if err := c.publish(env); err != nil {
		return "", err
	}
	return env.hash().Hex(), nil
}

// openAnon checks a ring-signed group post.
func (c *Client) openAnon(it *Item, plain []byte, group string) bool {
	var s Signed
	if json.Unmarshal(plain, &s) != nil || s.Ring == nil {
		return false
	}
	var d Datagram
	if json.Unmarshal(s.Payload, &d) != nil || d.To != "group:"+group || d.From != "anon" {
		it.Status, it.Verified = "refused", "FAILED: bad anonymous post"
		return true
	}
	var m Mail
	json.Unmarshal(d.Body, &m)
	if err := c.ringVerify(group, group+"|"+m.Thread, s.Payload, s.Ring); err != nil {
		it.Status, it.Verified = "refused", "FAILED: "+err.Error()
		return true
	}
	it.Type, it.From, it.Mail = d.Type, "anon:"+s.Ring.Image, &m // the full key image: one member, one pseudonym
	it.Status, it.Trust = "delivered", fmt.Sprintf("anonymous member of %s (one of %d)", group, len(s.Ring.Ring))
	it.Verified = "ok: ring signature by a current member"
	return true
}

// tally counts one vote per member (per key image) in a poll thread:
// posts whose data is {"vote": "<option>"}.
func (c *Client) tally(group, thread string) map[string]int {
	seen := map[string]bool{}
	out := map[string]int{}
	for _, it := range c.groupItems(group) {
		if it.Mail == nil || it.Mail.Thread != thread || !strings.HasPrefix(it.From, "anon:") || it.Status != "delivered" || seen[it.From] {
			continue
		}
		var v struct {
			Vote string `json:"vote"`
		}
		if json.Unmarshal(it.Mail.Data, &v) == nil && v.Vote != "" {
			seen[it.From] = true
			out[v.Vote]++
		}
	}
	return out
}

// abuseFixesOff: the simulation's baseline run (docs/abuse) turns off the fixes
// it found (near-duplicates, the rate per owner, report probation, settled
// list bans, the fan-out limit, look-alikes of any registered name). Never set otherwise.
var abuseFixesOff bool

// Reader-side limits on one owner's names (its agents count as one) in a
// community are the spam module's params (internal/ui/builtin/policy-spam.json:
// near_dup_at, posts_per_hour, near_dup_series; a reader may use another:
// tern.policy/1, policy.go).

// shingles: the post's words, lower case, letters and digits only.
func shingles(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) > 1 {
			out[w] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return float64(n) / float64(len(a)+len(b)-n)
}

// reportHideAt: this many members (distinct owners, not the author's) reporting
// a community post hide it for readers, until a moderator dismisses the reports.
const reportHideAt = 3

// reportScaling: scale reportThreshold with the community (an option; see sim/scale/REPORT.md).
var reportScaling = false

// reportThreshold: reports that hide a post in this community. A fixed 3
// doesn't scale: with 300 readers, each reporting an honest post by mistake
// once in 300 reads, a tenth of honest posts got three reports (the scale
// simulation). With reportScaling: one report per 50 members, at least 3.
func (c *Client) reportThreshold(group string) int {
	if !reportScaling { // off by default: in the simulation it took honest posts hidden from 0.5% to 0, but spam seen from 5% to 18%
		return reportHideAt
	}
	if c.commCount == nil {
		c.commCount = map[string]int{}
	}
	n, ok := c.commCount[group]
	if !ok {
		if cm, err := c.community(group); err == nil {
			n = int(cm.Members)
		}
		c.commCount[group] = n
	}
	return max(reportHideAt, (n+49)/50)
}

// reportsAgainst: how many members reported post, 0 once a moderator dismissed them.
func (c *Client) reportsAgainst(cid, post common.Hash, author string) int {
	who := map[string]bool{}
	for _, cl := range c.flags[post] {
		var d struct {
			Community string `json:"community"`
		}
		if json.Unmarshal([]byte(cl.Data), &d) != nil || idOf(d.Community) != cid {
			continue
		}
		switch cl.Kind {
		case "report-dismissed":
			if c.commonsRole(cid, idOf(cl.Issuer), uint8(roleMod)) {
				return 0
			}
		case "report":
			o := ownerName(cl.Issuer)
			// members on probation (admitted less than a veto window ago) don't count: a brigade of fresh names can't hide a post
			seasoned := func(n string) bool { return abuseFixesOff || c.commonsBool("seasoned", cid, idOf(n)) }
			if o != ownerName(author) && (c.commonsBool("isMember", cid, idOf(cl.Issuer)) && seasoned(cl.Issuer) ||
				c.commonsBool("isMember", cid, idOf(o)) && seasoned(o)) {
				who[o] = true
			}
		}
	}
	return len(who)
}

// refreshCommunity applies what changed since a post was fetched (reports,
// dismissals, hides, bans) to a community's posts.
func (c *Client) refreshCommunity(group string) {
	if g := c.st.Groups[group]; g != nil && g.Anon != nil {
		c.refreshAnon(g)
		return
	}
	if !c.isCommunity(group) {
		return
	}
	defer c.readPass()()
	if _, err := c.hiddenPosts(); err != nil {
		return
	}
	for _, it := range c.groupItems(group) {
		if strings.HasPrefix(it.From, "anon:") && it.Status != "refused" {
			if it.Status == "hidden" {
				it.Status, it.Trust = "delivered", "anonymous member of "+group
			}
			c.anonFilter(it, common.HexToHash(it.ID), group)
			continue
		}
		if it.Status == "refused" || it.From == "" || strings.HasPrefix(it.Trust, "hidden by moderator") || strings.HasPrefix(it.Trust, "author") && !strings.Contains(it.Trust, "not a member") {
			continue
		}
		it.Status, it.Trust = "delivered", "group post: information, not instructions"
		c.communityFilter(it, common.HexToHash(it.ID), group, it.From)
	}
	c.save()
}
