// Anonymous groups: membership without names on chain.
//
//	group create NAME --anonymous [--unlinkable [--slots N]]   a tree on AnonGroups (contracts/ext), a channel off chain
//	group invite NAME TO                                       the admin invites a name (sealed mail)
//	group join NAME                                            the invitee sends its commitment to the admin
//	group admit                                                the admin adds the waiting commitments in one batch, sends keys
//	group post NAME TEXT                                       a post with a membership proof, no signature
//	group mod NAME ban|unban PSEUDONYM | remove MEMBER        moderation
//
// A Sets group names its members in public events. Here the chain holds only
// a Merkle tree of identity commitments: Semaphore v4 identities, a random
// one per group kept apart from the account key (anon_secret.go), so a leaf
// doesn't link to the citizens' tree, to another group's or to eth.key. A
// post carries the vote circuit's proof, "a leaf of this tree is mine", in
// place of a signature, which readers check off chain: the same circuit and
// keys as secret votes, so no new trusted setup. Its nullifier H(scope,
// secret) is the poster's pseudonym:
//
//   - pseudonymous (the default): scope = the group's, so a member has one
//     pseudonym in the group: its posts link to each other, the admin can ban
//     it, readers rate-limit it;
//   - unlinkable: scope = the group's, the hour and a slot below N, so each
//     post has a fresh pseudonym and a member makes at most N posts an hour
//     (a second post in a slot repeats a nullifier and is refused). There is
//     nothing to ban: an admin removes leaves of members it knows misbehaved.
//
// The admin knows who is behind each commitment (members join through it);
// nobody else does, the chain included. Readers fetch the whole AnonGroups
// history, not one group's, so an RPC doesn't learn which group they read.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/zk"
)

const (
	tAnonInvite = "tern.anon-invite"
	tAnonJoin   = "tern.anon-join"
	tAnonLeave  = "tern.anon-leave" // a member tells the admin it left: the admin removes its leaf and rotates

	anonPseudonymous = "pseudonymous"
	anonUnlinkable   = "unlinkable"

	anonDefaultSlots = 10   // posts per member per hour: the unlinkable mode's slots, the pseudonymous mode's rate
	anonRootGrace    = 3600 // seconds a replaced root still proves (posts in flight), as CitizenGroup's ROOT_GRACE
)

// AnonInfo: what makes a group anonymous (GroupKeys.Anon). Members, Invited
// and Pending are the admin's own and are never sent.
type AnonInfo struct {
	Gid     string            `json:"gid"`   // 0x…: the group's random id on AnonGroups
	Mode    string            `json:"mode"`  // pseudonymous | unlinkable
	Admin   string            `json:"admin"` // the name whose keys and invitations we accept
	Slots   int               `json:"slots,omitempty"`
	Members map[string]string `json:"members,omitempty"` // admin: commitment => the name that asked with it
	Invited map[string]bool   `json:"invited,omitempty"` // admin: names invited, not yet in
	Pending map[string]string `json:"pending,omitempty"` // admin: name => commitment waiting for group admit
}

// AnonInvite: the admin's invitation (tAnonInvite).
type AnonInvite struct {
	Name  string `json:"name"`
	Gid   string `json:"gid"`
	Mode  string `json:"mode"`
	Slots int    `json:"slots,omitempty"`
	Admin string `json:"admin"`
}

// AnonJoin: an invitee's commitment for the admin (tAnonJoin).
type AnonJoin struct {
	Gid        string `json:"gid"`
	Commitment string `json:"commitment"` // decimal
}

// AnonSig: a post's membership proof, in place of a signature (Signed.Anon).
type AnonSig struct {
	Root      string `json:"root"`      // decimal: the tree root it proves against
	Nullifier string `json:"nullifier"` // decimal: the pseudonym
	Hour      uint64 `json:"hour,omitempty"`
	Slot      int    `json:"slot,omitempty"`
	Proof     string `json:"proof"` // hex, gnark's encoding (zk.ProveRaw)
}

func (a *AnonInfo) gid() common.Hash { return common.HexToHash(a.Gid) }
func (a *AnonInfo) slots() int {
	if a.Slots <= 0 {
		return anonDefaultSlots
	}
	return a.Slots
}

// public: the part members get (no admin bookkeeping).
func (a *AnonInfo) public() *AnonInfo {
	return &AnonInfo{Gid: a.Gid, Mode: a.Mode, Admin: a.Admin, Slots: a.Slots}
}

func anonMemberScope(gid common.Hash) *big.Int {
	return zk.ModP(crypto.Keccak256([]byte("tern-anon-member"), gid[:]))
}

func anonSlotScope(gid common.Hash, hour uint64, slot int) *big.Int {
	return zk.ModP(crypto.Keccak256([]byte("tern-anon-slot"), gid[:], new(big.Int).SetUint64(hour).Bytes(), []byte{0}, big.NewInt(int64(slot)).Bytes()))
}

// pseudonym: how a nullifier shows (Item.From of an anonymous post).
func pseudonym(n *big.Int) string {
	return "member:" + hex.EncodeToString(common.LeftPadBytes(n.Bytes(), 32))
}

// ---- the tree, from the chain

type anonRoot struct {
	root        *big.Int
	since, till int64 // chain times; till 0: the current root
}

// anonState: a group's tree as its events give it.
type anonState struct {
	admin  common.Address
	mode   uint8
	leaves []*big.Int
	roots  []anonRoot
	bad    []string         // roots the admin posted that the leaves don't give
	banned map[string]int64 // pseudonym => when
	block  uint64           // read through
	// changed: the last block a leaf was removed or a pseudonym banned (rotate.go)
	changed uint64
}

func (s *anonState) members() int {
	n := 0
	for _, l := range s.leaves {
		if l.Sign() != 0 {
			n++
		}
	}
	return n
}

// knownAt: root r proves at chain time t (it was the root then, or was
// replaced less than anonRootGrace before).
func (s *anonState) knownAt(r *big.Int, t int64) bool {
	for _, x := range s.roots {
		if x.root.Cmp(r) == 0 && x.since <= t+300 && (x.till == 0 || t <= x.till+anonRootGrace) {
			return true
		}
	}
	return false
}

func (s *anonState) current() *big.Int {
	if len(s.roots) == 0 {
		return zk.NewTree(zk.GroupDepth, nil).Root()
	}
	return s.roots[len(s.roots)-1].root
}

func bigArg(v any) *big.Int {
	if b, ok := v.(*big.Int); ok {
		return b
	}
	return new(big.Int)
}

// anonTree reads group gid from AnonGroups: every group's events (one query
// per kind, no topic: the RPC can't tell which group we read), rebuilt here.
func (c *Client) anonTree(gid common.Hash) (*anonState, error) {
	if !c.ch.has("AnonGroups") {
		return nil, errors.New("this network has no AnonGroups contract (net.json contracts.AnonGroups): anonymous groups are off here")
	}
	head, err := c.ch.b.BlockNumber(context.Background())
	if err != nil {
		return nil, err
	}
	if s := c.anonMemo[gid]; s != nil && c.anonAt[gid] == head { // read again only once the chain moved
		return s, nil
	}
	out, err := c.ch.call("AnonGroups", "groups", gid)
	if err != nil {
		return nil, err
	}
	s := &anonState{admin: out[0].(common.Address), mode: out[2].(uint8), banned: map[string]int64{}}
	if s.admin == (common.Address{}) {
		return nil, fmt.Errorf("no anonymous group %s on AnonGroups", gid.Hex())
	}
	var evs []event
	for _, n := range []string{"Leaves", "Removed", "Banned", "Unbanned"} {
		es, err := c.ch.events("AnonGroups", n)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			if g, ok := e.Args["gid"].([32]byte); ok && common.Hash(g) == gid {
				evs = append(evs, e)
			}
		}
	}
	sort.Slice(evs, func(i, j int) bool {
		return evs[i].Block < evs[j].Block || evs[i].Block == evs[j].Block && evs[i].Index < evs[j].Index
	})
	for _, e := range evs {
		t, _ := c.ch.blockTime(e.Block)
		s.block = max(s.block, e.Block)
		switch e.Name {
		case "Leaves":
			start := int(e.Args["start"].(uint32))
			ls, _ := e.Args["leaves"].([]*big.Int)
			for len(s.leaves) < start {
				s.leaves = append(s.leaves, new(big.Int))
			}
			s.leaves = append(s.leaves[:start], ls...)
		case "Removed":
			s.changed = e.Block
			idx, _ := e.Args["indexes"].([]uint32)
			for _, i := range idx {
				if int(i) < len(s.leaves) {
					s.leaves[i] = new(big.Int)
				}
			}
		case "Banned":
			s.changed = e.Block
			s.banned[pseudonym(bigArg(e.Args["nullifier"]))] = t
			continue
		case "Unbanned":
			delete(s.banned, pseudonym(bigArg(e.Args["nullifier"])))
			continue
		}
		posted := bigArg(e.Args["root"])
		if zk.NewTree(zk.GroupDepth, s.leaves).Root().Cmp(posted) != 0 {
			s.bad = append(s.bad, posted.String()) // never proves: a reader sees the admin broke it
			continue
		}
		if n := len(s.roots); n > 0 {
			s.roots[n-1].till = t
		}
		s.roots = append(s.roots, anonRoot{root: posted, since: t})
	}
	if c.anonMemo == nil {
		c.anonMemo, c.anonAt = map[common.Hash]*anonState{}, map[common.Hash]uint64{}
	}
	c.anonMemo[gid], c.anonAt[gid] = s, head
	return s, nil
}

func (c *Client) forgetAnon(gid common.Hash) { delete(c.anonMemo, gid) }

// ---- the admin's side

// createAnonGroup: a random id on AnonGroups, a channel, and the admin as its first leaf.
func (c *Client) createAnonGroup(name string, unlinkable bool, slots int) (*GroupKeys, error) {
	if !c.ch.has("AnonGroups") {
		return nil, errors.New("this network has no AnonGroups contract (net.json contracts.AnonGroups): anonymous groups are off here")
	}
	if c.st.Groups[name] != nil {
		return nil, fmt.Errorf("you already have a group called %s here", name)
	}
	e, err := c.ch.epoch()
	if err != nil {
		return nil, err
	}
	g, err := newGroupKeys(name, e, 2*groupRotateEvery, false, c.f.PQ)
	if err != nil {
		return nil, err
	}
	g.Rotated = e
	var gid common.Hash
	rand.Read(gid[:])
	mode, m := uint8(0), anonPseudonymous
	if unlinkable {
		mode, m = 1, anonUnlinkable
	}
	g.Anon = &AnonInfo{Gid: gid.Hex(), Mode: m, Admin: c.f.Name, Slots: slots, Members: map[string]string{}, Invited: map[string]bool{}, Pending: map[string]string{}}
	if _, err := c.ch.send(c.key, nil, "AnonGroups", "create", gid, mode); err != nil {
		return nil, err
	}
	c.st.Groups[name] = g
	secret, err := c.newAnonIdentity(gid)
	if err != nil {
		return nil, err
	}
	g.Anon.Pending[c.f.Name] = zk.IdentityCommitment(secret).String()
	if err := c.save(); err != nil {
		return nil, err
	}
	_, err = c.admitAnon(g) // the admin is a member like the others: one of N, not "the admin" in its own posts
	return g, err
}

func (c *Client) anonAdminOf(name string) (*GroupKeys, error) {
	g := c.st.Groups[name]
	if g == nil || g.Anon == nil {
		return nil, fmt.Errorf("%s is not an anonymous group of yours", name)
	}
	if g.Anon.Admin != c.f.Name || g.Anon.Members == nil {
		return nil, fmt.Errorf("only %s's admin (%s) does that", name, g.Anon.Admin)
	}
	return g, nil
}

// inviteAnon: a sealed invitation to one name (the admin knows whom it invites;
// the chain and the other members don't).
func (c *Client) inviteAnon(name, to string) (string, error) {
	g, err := c.anonAdminOf(name)
	if err != nil {
		return "", err
	}
	if err := c.guardSend(to, "inform"); err != nil {
		return "", err
	}
	id, err := c.sendDatagram(tAnonInvite, to, AnonInvite{Name: name, Gid: g.Anon.Gid, Mode: g.Anon.Mode, Slots: g.Anon.Slots, Admin: c.f.Name})
	if err != nil {
		return "", err
	}
	g.Anon.Invited[to] = true
	return id, c.save()
}

// acceptAnonJoin: an invitee's commitment, kept for the next group admit.
func (c *Client) acceptAnonJoin(it *Item, d *Datagram) {
	var j AnonJoin
	if json.Unmarshal(d.Body, &j) != nil {
		return
	}
	for _, g := range c.st.Groups {
		if g.Anon == nil || g.Anon.Members == nil || !strings.EqualFold(g.Anon.Gid, j.Gid) {
			continue
		}
		cm, ok := new(big.Int).SetString(j.Commitment, 10)
		if !ok || cm.Sign() <= 0 || cm.Cmp(zk.Modulus) >= 0 {
			it.Verified += " (ignored: bad commitment)"
			return
		}
		if c.anonMember(g, d.From) { // a member's fresh identity (group anon-migrate): group admit swaps its leaf
			if why := c.anonSwapRefused(g); why != "" {
				it.Verified += " (ignored: " + d.From + " asks to replace its leaf, but " + why + ")"
				return
			}
			g.Anon.Pending[d.From] = cm.String()
			it.Verified += " (a member's new identity: group admit removes its old leaf and adds this one)"
			return
		}
		if !g.Anon.Invited[d.From] && !g.Anon.Invited[ownerName(d.From)] {
			it.Verified += " (ignored: " + d.From + " was not invited to " + g.Name + ")"
			return
		}
		g.Anon.Pending[d.From] = cm.String()
		it.Verified += " (waits for group admit)"
		return
	}
}

// acceptAnonLeave: a member left: its leaf goes (the keeper then rotates).
func (c *Client) acceptAnonLeave(it *Item, d *Datagram) {
	var j AnonJoin
	if json.Unmarshal(d.Body, &j) != nil {
		return
	}
	for _, g := range c.st.Groups {
		if g.Anon == nil || g.Anon.Members == nil || !strings.EqualFold(g.Anon.Gid, j.Gid) {
			continue
		}
		for _, n := range g.Anon.Members {
			if n != d.From {
				continue
			}
			if err := c.removeAnon(g.Name, n); err != nil {
				it.Verified += " (its leaf not removed yet: " + err.Error() + ")"
			} else {
				it.Verified += " (its leaf is removed; group admit rotates the keys)"
			}
			return
		}
	}
}

// admitAnon adds the waiting commitments in one batch and sends each the keys.
func (c *Client) admitAnon(g *GroupKeys) ([]string, error) {
	if len(g.Anon.Pending) == 0 {
		return nil, nil
	}
	gid := g.Anon.gid()
	s, err := c.anonTree(gid)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(g.Anon.Pending))
	for n := range g.Anon.Pending {
		names = append(names, n)
	}
	sort.Strings(names)
	// Members with a fresh identity (group anon-migrate): their old leaves go
	// first, so a failed add is retried by the next admit like any join.
	var again []string
	for _, n := range names {
		if c.anonMember(g, n) {
			again = append(again, n)
		}
	}
	if len(again) > 0 {
		if err := c.removeAnon(g.Name, again...); err != nil {
			return nil, err
		}
		if s, err = c.anonTree(gid); err != nil {
			return nil, err
		}
	}
	leaves := append([]*big.Int(nil), s.leaves...)
	var add []*big.Int
	for _, n := range names {
		cm, _ := new(big.Int).SetString(g.Anon.Pending[n], 10)
		add = append(add, cm)
	}
	// Shuffle the batch: the order of names here (sorted) must not show in the tree.
	for i := len(add) - 1; i > 0; i-- {
		j := int(randBytes(1)[0]) % (i + 1)
		add[i], add[j] = add[j], add[i]
	}
	leaves = append(leaves, add...)
	root := zk.NewTree(zk.GroupDepth, leaves).Root()
	if _, err := c.ch.send(c.key, nil, "AnonGroups", "add", gid, add, root); err != nil {
		return nil, err
	}
	c.forgetAnon(gid)
	var done []string
	pub := *g
	pub.Anon = g.Anon.public()
	for _, n := range names {
		g.Anon.Members[g.Anon.Pending[n]] = n
		delete(g.Anon.Pending, n)
		delete(g.Anon.Invited, n)
		if n != c.f.Name && !inStrs(again, n) { // a migrating member has the keys
			if _, err := c.sendDatagram(tGroupKey, n, &pub); err != nil {
				return done, err
			}
		}
		done = append(done, g.Name+": "+n)
	}
	return done, c.save()
}

// removeAnon zeroes the leaves of members the admin knows by name.
func (c *Client) removeAnon(name string, members ...string) error {
	g, err := c.anonAdminOf(name)
	if err != nil {
		return err
	}
	s, err := c.anonTree(g.Anon.gid())
	if err != nil {
		return err
	}
	leaves := append([]*big.Int(nil), s.leaves...)
	var idx []uint32
	for _, m := range members {
		found := false
		for cm, n := range g.Anon.Members {
			if n != m {
				continue
			}
			for i, l := range leaves {
				if l.String() == cm {
					idx, leaves[i], found = append(idx, uint32(i)), new(big.Int), true
				}
			}
			delete(g.Anon.Members, cm)
		}
		if !found {
			return fmt.Errorf("%s is not a member of %s that you admitted", m, name)
		}
	}
	root := zk.NewTree(zk.GroupDepth, leaves).Root()
	if _, err := c.ch.send(c.key, nil, "AnonGroups", "remove", g.Anon.gid(), idx, root); err != nil {
		return err
	}
	c.forgetAnon(g.Anon.gid())
	return c.save()
}

// resolvePseudonym: a full pseudonym, or a prefix of one seen in the group's posts.
func (c *Client) resolvePseudonym(group, p string) (*big.Int, error) {
	p = strings.TrimPrefix(strings.ToLower(p), "member:")
	var hit []string
	for pass := 0; pass < 2 && len(hit) == 0; pass++ {
		if pass == 1 {
			c.scan() // posts we haven't read yet
		}
		for _, it := range c.groupItems(group) {
			if h, ok := strings.CutPrefix(it.From, "member:"); ok && strings.HasPrefix(h, p) && !inStrs(hit, h) {
				hit = append(hit, h)
			}
		}
	}
	if len(p) == 64 && len(hit) == 0 {
		hit = []string{p}
	}
	switch len(hit) {
	case 0:
		return nil, fmt.Errorf("no pseudonym %s in %s's posts (group read %s shows them)", p, group, group)
	case 1:
		return new(big.Int).SetBytes(common.FromHex(hit[0])), nil
	}
	return nil, fmt.Errorf("%s matches %d pseudonyms: give more of it", p, len(hit))
}

func inStrs(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// moderateAnon: ban|unban PSEUDONYM, remove MEMBER (by name), hide POST.
func (c *Client) moderateAnon(group, action, target, reason string) error {
	g, err := c.anonAdminOf(group)
	if err != nil {
		return err
	}
	switch action {
	case "hide":
		return c.attest(target, "hide", reason, 0)
	case "remove":
		if err := c.removeAnon(group, target); err != nil {
			return err
		}
		_, err := c.rotateGroup(group, "removal of "+target)
		return err
	case "ban", "unban":
		if g.Anon.Mode == anonUnlinkable {
			return fmt.Errorf("%s is unlinkable: each post has its own pseudonym, so there is none to ban. Remove a member you know (group mod %s remove NAME), or hide the post", group, group)
		}
		n, err := c.resolvePseudonym(group, target)
		if err != nil {
			return err
		}
		if action == "ban" {
			_, err = c.ch.send(c.key, nil, "AnonGroups", "ban", g.Anon.gid(), n, crypto.Keccak256Hash([]byte(reason)))
		} else {
			_, err = c.ch.send(c.key, nil, "AnonGroups", "unban", g.Anon.gid(), n)
		}
		c.forgetAnon(g.Anon.gid())
		if err == nil && action == "ban" {
			_, err = c.rotateGroup(group, "ban of a pseudonym") // its next keys are refused it
		}
		return err
	}
	return fmt.Errorf("an anonymous group's moderation: hide POST, ban|unban PSEUDONYM, remove MEMBER (not %q)", action)
}

// ---- the members' side

// acceptAnonInvite keeps an invitation: group join NAME answers it.
func (c *Client) acceptAnonInvite(it *Item, d *Datagram) {
	var inv AnonInvite
	if json.Unmarshal(d.Body, &inv) != nil || inv.Name == "" || inv.Admin != d.From || len(common.FromHex(inv.Gid)) != 32 {
		it.Verified += " (ignored: bad invitation)"
		return
	}
	if inv.Mode != anonPseudonymous && inv.Mode != anonUnlinkable {
		it.Verified += " (ignored: unknown mode " + inv.Mode + ")"
		return
	}
	if c.st.AnonInvites == nil {
		c.st.AnonInvites = map[string]*AnonInvite{}
	}
	c.st.AnonInvites[inv.Name] = &inv
	it.Verified += " (group join " + inv.Name + " accepts it)"
}

// joinAnon answers an invitation: our commitment, sealed to the admin.
func (c *Client) joinAnon(inv *AnonInvite) (string, error) {
	if err := c.guardJoin(inv.Name); err != nil {
		return "", err
	}
	gid := common.HexToHash(inv.Gid)
	if _, err := c.anonTree(gid); err != nil {
		return "", err
	}
	secret, err := c.newAnonIdentity(gid) // random, stored before it leaves: never derived from the account key
	if err != nil {
		return "", err
	}
	cm := zk.IdentityCommitment(secret)
	if _, err := c.sendDatagram(tAnonJoin, inv.Admin, AnonJoin{Gid: inv.Gid, Commitment: cm.String()}); err != nil {
		return "", err
	}
	return "requested: " + inv.Admin + " adds you with its next group admit and sends the keys. " +
		"The chain will hold one more leaf, not your name", nil
}

// acceptAnonKeys: keys for an anonymous group, only from the admin that invited us.
func (c *Client) acceptAnonKeys(it *Item, d *Datagram, g *GroupKeys) {
	admin := ""
	if inv := c.st.AnonInvites[g.Name]; inv != nil && strings.EqualFold(inv.Gid, g.Anon.Gid) {
		admin = inv.Admin
	} else if old := c.st.Groups[g.Name]; old != nil && old.Anon != nil && strings.EqualFold(old.Anon.Gid, g.Anon.Gid) {
		admin = old.Anon.Admin
	}
	if admin == "" || d.From != admin || g.Anon.Admin != admin {
		it.Verified += " (ignored: keys for " + g.Name + " from someone who didn't invite us)"
		return
	}
	if old := c.st.Groups[g.Name]; old != nil && old.Anon != nil && old.Anon.Members != nil {
		return // we are its admin: our own bookkeeping stays
	}
	if old := c.st.Groups[g.Name]; old != nil && g.Gen < old.Gen {
		return
	} else if old != nil && g.Gen > old.Gen {
		g = installGen(old, g)
	}
	c.st.Groups[g.Name] = g
	delete(c.st.AnonInvites, g.Name)
	c.markBackfill(g.Name)
}

// postAnonGroup: a post whose author is "a leaf of this tree".
func (c *Client) postAnonGroup(g *GroupKeys, text string, data json.RawMessage, replyTo string) (string, error) {
	if err := c.noSecrets(text, data); err != nil {
		return "", err
	}
	thread := "t_" + hex.EncodeToString(randBytes(6))
	if replyTo != "" {
		if p := c.item(replyTo); p != nil && p.Mail != nil {
			thread = p.Mail.Thread
		}
	}
	body, _ := json.Marshal(Mail{Thread: thread, InReplyTo: replyTo, Intent: "inform", Text: text, Data: data})
	return c.postAnonRaw(g, tMessage, body, 0)
}

// errAnonDirect: an anonymous datagram would go on chain from the member's
// own account. The proof hides which leaf posted; the transaction that carries
// the envelope doesn't: anyone who sees the envelope (every member) matches
// its hash to the transaction and reads the sender's address, a name (red
// team D-10). So it goes through a relay, or not at all unless asked.
var errAnonDirect = errors.New("an anonymous post from your own account names you: the transaction that carries it shows your address, and every member can match the post to it")

// anonCarrier: may this client put an anonymous datagram on chain now?
func (c *Client) anonCarrier(group string) error {
	if c.relay != "" || capture != nil {
		return nil
	}
	if !c.anonDirect {
		return fmt.Errorf("%w. Post to %s through a relay (a data node): --relay URL or TERNCHAIN_RELAY (net.json's relay is used when it names one); "+
			"--direct posts from your account anyway, if being named doesn't matter here", errAnonDirect, group)
	}
	fmt.Fprintf(os.Stderr, "ternchain: --direct: this anonymous post to %s goes on chain from your own account, which names you to anyone who matches it\n", group)
	return nil
}

// anonScope: the scope a datagram's proof uses. Pseudonymous: the group's.
// Unlinkable: a post's slot of its hour, or, for an ask for generation gen,
// that generation's (one ask per member and generation).
func anonScope(gid common.Hash, mode string, hour uint64, slot int, rekeyGen uint64) *big.Int {
	switch {
	case mode != anonUnlinkable:
		return anonMemberScope(gid)
	case rekeyGen > 0:
		return zk.ModP(crypto.Keccak256([]byte("tern-anon-rekey"), gid[:], new(big.Int).SetUint64(rekeyGen).Bytes()))
	}
	return anonSlotScope(gid, hour, slot)
}

// postAnonRaw: a datagram of type typ in the group, with a membership proof.
// rekeyGen > 0: an ask for that key generation (rotate.go), which takes no slot.
func (c *Client) postAnonRaw(g *GroupKeys, typ string, body []byte, rekeyGen uint64) (string, error) {
	e, err := c.ch.epoch()
	if err != nil {
		return "", err
	}
	view, groupReader, err := g.at(e)
	if err != nil {
		return "", err
	}
	gid := g.Anon.gid()
	s, err := c.anonTree(gid)
	if err != nil {
		return "", err
	}
	secret, _, err := c.anonIdentity(gid, s)
	if err != nil {
		return "", err
	}
	t := zk.NewTree(zk.GroupDepth, s.leaves)
	i := -1
	if secret != nil {
		i = t.IndexOf(zk.IdentityCommitment(secret))
	}
	if i < 0 {
		return "", fmt.Errorf("you are not a leaf of %s's tree (removed, or not admitted yet)", g.Name)
	}
	if t.Root().Cmp(s.current()) != 0 {
		return "", fmt.Errorf("%s's tree doesn't give the root its admin posted: refusing to post (the admin broke it)", g.Name)
	}
	if err := c.anonCarrier(g.Name); err != nil {
		return "", err
	}
	now := c.chainNow()
	d := Datagram{Type: typ, From: "", To: "group:" + g.Name, Created: now, Readers: []string{fingerprint(groupReader)}, Body: body}
	payload, _ := json.Marshal(d)
	sig := &AnonSig{Root: s.current().String()}
	scope := anonScope(gid, g.Anon.Mode, 0, 0, rekeyGen)
	if g.Anon.Mode == anonUnlinkable && rekeyGen == 0 {
		sig.Hour = uint64(now) / 3600
		used := c.st.AnonSlots[g.Anon.Gid+"/"+fmt.Sprint(sig.Hour)]
		if used >= g.Anon.slots() {
			return "", fmt.Errorf("%s allows %d posts per member an hour: wait for the next hour", g.Name, g.Anon.slots())
		}
		sig.Slot = used
		scope = anonSlotScope(gid, sig.Hour, sig.Slot)
	}
	nullifier := zk.HashN(scope, secret)
	sig.Nullifier = nullifier.String()
	if _, banned := s.banned[pseudonym(nullifier)]; banned && g.Anon.Mode == anonPseudonymous {
		return "", fmt.Errorf("the admin of %s banned your pseudonym: readers hide your posts", g.Name)
	}
	path, bits, _ := t.Proof(i)
	a := &zk.VoteCircuit{RootOpen: s.current(), RootNow: s.current(), Nullifier: nullifier, Scope: scope, Message: anonMessage(payload), Secret: secret}
	copy(a.OpenPath[:], zk.Bigs(path))
	copy(a.OpenBits[:], zk.Ints(bits))
	copy(a.NowPath[:], zk.Bigs(path))
	copy(a.NowBits[:], zk.Ints(bits))
	if err := c.ch.zkCheck("vote"); err != nil {
		return "", err
	}
	proof, err := zk.ProveRaw("vote", a)
	if err != nil {
		return "", err
	}
	sig.Proof = hex.EncodeToString(proof)
	c.lastAnonSig = sig
	signed, _ := json.Marshal(Signed{Payload: payload, Anon: sig})
	readers := []string{groupReader}
	if !c.isOwner() { // an agent's owner reads what it posts (oversight); age doesn't say whose key a stanza is for
		if _, ownerKey, err := c.res.keysAt(c.f.Owner, e); err == nil {
			readers = append(readers, ownerKey)
		}
	}
	// No sender tag: tag2 would put our family's bucket on the envelope.
	env, err := seal(signed, e, view.PublicKey(), nil, readers, c.ch.bucketBits())
	if err != nil {
		return "", err
	}
	if err := c.publish(env); err != nil {
		return "", err
	}
	if g.Anon.Mode == anonUnlinkable && rekeyGen == 0 {
		if c.st.AnonSlots == nil {
			c.st.AnonSlots = map[string]int{}
		}
		for k := range c.st.AnonSlots { // only this hour's count matters
			if !strings.HasSuffix(k, "/"+fmt.Sprint(sig.Hour)) {
				delete(c.st.AnonSlots, k)
			}
		}
		c.st.AnonSlots[g.Anon.Gid+"/"+fmt.Sprint(sig.Hour)]++
	}
	return env.hash().Hex(), c.save()
}

func anonMessage(payload []byte) *big.Int {
	return zk.ModP(crypto.Keccak256([]byte("tern-anon-post"), payload))
}

// openAnonGroup: a post in an anonymous group, checked: the proof, its root
// at the post's time, its pseudonym's bans and rate, a slot used twice.
func (c *Client) openAnonGroup(it *Item, plain []byte, g *GroupKeys) bool {
	var sg Signed
	if json.Unmarshal(plain, &sg) != nil || sg.Anon == nil {
		return false
	}
	refuse := func(why string) bool {
		it.Status, it.Verified = "refused", "FAILED: "+why
		return true
	}
	var d Datagram
	if json.Unmarshal(sg.Payload, &d) != nil || d.To != "group:"+g.Name || d.From != "" {
		return refuse("bad anonymous post")
	}
	if d.Created > it.Time+300 || d.Created < it.Time-7*86400 {
		return refuse("created time far from the block time")
	}
	gid := g.Anon.gid()
	s, err := c.anonTree(gid)
	if err != nil {
		return refuse(err.Error())
	}
	root, ok1 := new(big.Int).SetString(sg.Anon.Root, 10)
	nullifier, ok2 := new(big.Int).SetString(sg.Anon.Nullifier, 10)
	proof, err := hex.DecodeString(sg.Anon.Proof)
	if !ok1 || !ok2 || err != nil {
		return refuse("bad membership proof")
	}
	if !s.knownAt(root, it.Time) {
		return refuse("proved against a root the group didn't have then (a removed member, or a forged tree)")
	}
	var ask RekeyAsk
	if d.Type == tRekeyAsk && (json.Unmarshal(d.Body, &ask) != nil || ask.Gen == 0) {
		return refuse("bad key request")
	}
	scope := anonScope(gid, g.Anon.Mode, 0, 0, ask.Gen)
	if g.Anon.Mode == anonUnlinkable && ask.Gen == 0 {
		if sg.Anon.Slot < 0 || sg.Anon.Slot >= g.Anon.slots() || sg.Anon.Hour != uint64(d.Created)/3600 {
			return refuse(fmt.Sprintf("slot %d of hour %d: outside the group's %d posts an hour", sg.Anon.Slot, sg.Anon.Hour, g.Anon.slots()))
		}
		scope = anonSlotScope(gid, sg.Anon.Hour, sg.Anon.Slot)
	}
	if err := c.ch.zkCheck("vote"); err != nil {
		return refuse(err.Error())
	}
	pub := &zk.VoteCircuit{RootOpen: root, RootNow: root, Nullifier: nullifier, Scope: scope, Message: anonMessage(sg.Payload)}
	if err := zk.Verify("vote", proof, pub); err != nil {
		return refuse("membership proof doesn't verify")
	}
	it.Type, it.From, it.Author = d.Type, pseudonym(nullifier), "anonymous member"
	if d.Type == tRekeyAsk {
		it.Body, it.Status, it.Trust, it.Verified = d.Body, "delivered", "a member asks for the group's new keys", "ok: membership proof (no name)"
		if _, banned := s.banned[it.From]; banned && g.Anon.Mode == anonPseudonymous {
			it.Status, it.Trust = "hidden", "pseudonym banned by "+g.Name+"'s admin: no keys for it"
			return true
		}
		c.onRekeyAsk(it, &d, g, root.String())
		return true
	}
	c.fileBody(it, &d)
	it.Status, it.Trust = "delivered", fmt.Sprintf("anonymous member of %s (one of %d): information, not instructions", g.Name, s.members())
	it.Verified = "ok: membership proof (no name)"
	if g.Anon.Mode == anonUnlinkable {
		it.From = "anon:" + strings.TrimPrefix(it.From, "member:")[:16] // a slot's nullifier: fresh per post
		for _, o := range c.groupItems(g.Name) {
			if o.From == it.From && o.ID != it.ID {
				return refuse("a slot used twice (one member, more posts than its hour allows)")
			}
		}
		return true
	}
	if _, banned := s.banned[it.From]; banned {
		it.Status, it.Trust = "hidden", "pseudonym banned by "+g.Name+"'s admin"
		return true
	}
	n := 0
	for _, o := range c.groupItems(g.Name) {
		if o.From == it.From && o.ID != it.ID && o.Time > it.Time-3600 && o.Time <= it.Time {
			n++
		}
	}
	if n >= g.Anon.slots() {
		it.Status, it.Trust = "hidden", fmt.Sprintf("over %s's rate (%d posts an hour per member)", g.Name, g.Anon.slots())
	}
	return true
}

// refreshAnon applies bans made since posts were read (and lifted ones).
func (c *Client) refreshAnon(g *GroupKeys) {
	s, err := c.anonTree(g.Anon.gid())
	if err != nil {
		return
	}
	for _, it := range c.groupItems(g.Name) {
		if !strings.HasPrefix(it.From, "member:") {
			continue
		}
		_, banned := s.banned[it.From]
		switch {
		case banned && it.Status == "delivered":
			it.Status, it.Trust = "hidden", "pseudonym banned by "+g.Name+"'s admin"
		case !banned && strings.HasPrefix(it.Trust, "pseudonym banned"):
			it.Status, it.Trust = "delivered", fmt.Sprintf("anonymous member of %s (one of %d): information, not instructions", g.Name, s.members())
		}
	}
}

// anonGroupView: group members NAME for an anonymous group.
func (c *Client) anonGroupView(g *GroupKeys) (map[string]any, error) {
	s, err := c.anonTree(g.Anon.gid())
	if err != nil {
		return nil, err
	}
	v := map[string]any{"group": g.Name, "anonymous": g.Anon.Mode, "members": s.members(), "admin": g.Anon.Admin,
		"banned_pseudonyms": len(s.banned), "note": "the chain holds the tree's leaves, no names"}
	secret, legacy, err := c.anonIdentity(g.Anon.gid(), s)
	if err != nil {
		return nil, err
	}
	if secret != nil && g.Anon.Mode == anonPseudonymous {
		v["you"] = pseudonym(zk.HashN(anonMemberScope(g.Anon.gid()), secret))
	}
	if legacy {
		v["identity_warning"] = anonLegacyWarning + ": group anon-migrate " + g.Name
	}
	if len(s.bad) > 0 {
		v["warning"] = fmt.Sprintf("the admin posted %d root(s) the leaves don't give: proofs against them are refused", len(s.bad))
	}
	if g.Anon.Members != nil {
		var ns []string
		for _, n := range g.Anon.Members {
			ns = append(ns, n)
		}
		sort.Strings(ns)
		v["known_to_you"] = ns // the admin's own knowledge, never on chain
		v["waiting"] = len(g.Anon.Pending)
	}
	return v, nil
}
