// Forward secrecy for groups: key generations, rotated.
//
//	group rotate NAME [--every N] [--keep-days D]   a new generation now (an admin)
//	group admit                                     the admin's keeper also rotates when one is due
//	group leave NAME                                drop the keys; the admin removes us and rotates
//
// A private group's channel is a view key and one key per epoch. Before, the
// admin made 56 epochs of keys at once and handed them all to every member,
// and nothing changed when someone was removed: a removed member, or a seized
// device, read the group for eight weeks ahead. Now a generation holds keys
// for 2·every epochs (every: 7 by default) and a new generation, with a new
// view key, replaces it
//
//   - every `every` epochs (the admin's keeper, group admit, makes it),
//   - whenever a member was removed, banned or left since the last one,
//   - when an admin asks (group rotate).
//
// How members get it:
//
//   - a Sets group's admin knows its members: the generation is sealed to each
//     remaining one by name (one envelope per member);
//   - an anonymous group's admin doesn't know who is behind a pseudonym, so it
//     announces the generation in the old channel and every member asks for it
//     there with a membership proof against the current root (a removed leaf
//     can't make one, a banned pseudonym is refused) and a one-time key; the
//     admin answers in the old channel, sealed to that one-time key. Nobody but
//     the asker opens an answer; the admin learns a pseudonym, not a name.
//
// Earlier generations stay (to read their posts) until their epochs pass the
// retention, like every epoch key; then they are destroyed. A device seized
// today opens the current generation and what is within the retention, not
// the group's history; a removed member reads nothing after the rotation.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/ethereum/go-ethereum/common"
)

const (
	tGroupRotate = "tern.group-rotate" // an anonymous group's admin: generation N is out, ask for it
	tRekeyAsk    = "tern.rekey-ask"    // a member: proof + one-time key, for generation N
	tRekeyAnswer = "tern.rekey-answer" // the admin: generation N sealed to that one-time key

	groupRotateEvery = 7 // epochs between scheduled rotations
)

// GroupRotate: the announcement (tGroupRotate).
type GroupRotate struct {
	Gen  uint64 `json:"gen"`
	Root string `json:"root"` // the tree's root proofs must use
}

// RekeyAsk: a member's request (tRekeyAsk, membership-proved).
type RekeyAsk struct {
	Gen       uint64 `json:"gen"`
	Have      uint64 `json:"have"`      // the generation the asker holds: the answer comes in its channel
	Recipient string `json:"recipient"` // a one-time age recipient
}

// RekeyAnswer: the admin's answer (tRekeyAnswer, signed).
type RekeyAnswer struct {
	Gen uint64 `json:"gen"`
	To  string `json:"to"`  // fingerprint of the ask's recipient
	Box string `json:"box"` // base64 age file: the generation's GroupKeys
}

// pendingAsk: our open request (State.RekeyAsks).
type pendingAsk struct {
	Gen      uint64 `json:"gen"`
	Identity string `json:"identity"` // the one-time age identity (dropped once answered)
	Posted   bool   `json:"posted"`
}

// queuedAnswer: an ask the admin will answer (State.RekeyQueue).
type queuedAnswer struct {
	Group     string `json:"group"`
	Gen       uint64 `json:"gen"`
	Recipient string `json:"recipient"`
	Have      uint64 `json:"have"`
	Key       string `json:"key"` // gid/gen/nullifier: one answer per pseudonym and generation
}

func (g *GroupKeys) every() uint64 {
	if g.Every == 0 {
		return groupRotateEvery
	}
	return g.Every
}

// gens: this generation, then the earlier ones still kept.
func (g *GroupKeys) gens() []*GroupKeys { return append([]*GroupKeys{g}, g.Old...) }

// forMembers: what members get of a generation (no admin bookkeeping, no old generations).
func (g *GroupKeys) forMembers() *GroupKeys {
	p := &GroupKeys{Name: g.Name, ViewKey: g.ViewKey, First: g.First, Keys: g.Keys, IsPublic: g.IsPublic, Gen: g.Gen, Every: g.Every, KeepDays: g.KeepDays, LocalOnly: g.LocalOnly}
	if g.Anon != nil {
		p.Anon = g.Anon.public()
	}
	return p
}

// installGen: a newer generation replaces ours; ours goes to Old.
func installGen(old, ng *GroupKeys) *GroupKeys {
	if old == nil {
		return ng
	}
	if ng.Gen <= old.Gen {
		return old
	}
	prev := *old
	prev.Old, prev.Anon = nil, nil
	ng.Old = append([]*GroupKeys{&prev}, old.Old...)
	if old.Anon != nil && old.Anon.Members != nil { // the admin's own bookkeeping carries over
		ng.Anon = old.Anon
	}
	return ng
}

// rotationDue: why the admin's keeper should rotate now ("" : it shouldn't).
func (c *Client) rotationDue(g *GroupKeys, e uint64) string {
	if g.IsPublic {
		return ""
	}
	if g.Rotated == 0 {
		g.Rotated = g.First
	}
	if e >= g.Rotated+g.every() {
		return "scheduled"
	}
	if g.Anon != nil {
		s, err := c.anonTree(g.Anon.gid())
		if err == nil && s.changed > g.RotatedAt {
			return "a member removed or banned"
		}
		return ""
	}
	evs, err := c.ch.events("Sets", "Removed", []common.Hash{setID(g.Name)})
	if err != nil {
		return ""
	}
	for _, ev := range evs {
		if ev.Block > g.RotatedAt {
			return "a member removed or left"
		}
	}
	return ""
}

// rotateGroup: a new generation (new view key, keys for 2·every epochs), handed out.
func (c *Client) rotateGroup(name, why string) (map[string]any, error) {
	g := c.st.Groups[name]
	if g == nil {
		return nil, fmt.Errorf("you are not in group %s", name)
	}
	if g.IsPublic {
		return nil, fmt.Errorf("%s is public: its keys are on chain for anyone, there is nothing to rotate", name)
	}
	if g.Anon != nil {
		if _, err := c.anonAdminOf(name); err != nil {
			return nil, err
		}
	} else if !c.hasRole(name, "admin", c.f.Name) {
		return nil, fmt.Errorf("only an admin of %s rotates its keys", name)
	}
	e, err := c.ch.epoch()
	if err != nil {
		return nil, err
	}
	head, err := c.ch.b.BlockNumber(context.Background())
	if err != nil {
		return nil, err
	}
	ng, err := newGroupKeys(name, e, int(2*g.every()), false, c.f.PQ)
	if err != nil {
		return nil, err
	}
	ng.Gen, ng.Every, ng.KeepDays, ng.Rotated, ng.RotatedAt, ng.LocalOnly = g.Gen+1, g.Every, g.KeepDays, e, head, g.LocalOnly
	if g.Anon != nil {
		ng.Anon = g.Anon
	}
	r := map[string]any{"group": name, "generation": ng.Gen, "why": why, "keys_through_epoch": e + 2*g.every() - 1}
	if g.Anon != nil {
		// Announce in the old channel, then switch: members ask with proofs (answerRekeys).
		s, err := c.anonTree(g.Anon.gid())
		if err != nil {
			return nil, err
		}
		for _, gen := range g.gens() { // every channel a member may still be on (one missed a rotation)
			if _, err := c.sendInGen(gen, name, tGroupRotate, GroupRotate{Gen: ng.Gen, Root: s.current().String()}); err != nil && gen == g {
				return nil, err
			}
		}
		c.st.Groups[name] = installGen(g, ng)
		r["delivery"] = "announced in the old channel: members ask for it with a membership proof, and your next group admit (or scan) answers"
		return r, c.save()
	}
	ms, err := c.setMembers(name)
	if err != nil {
		return nil, err
	}
	c.st.Groups[name] = installGen(g, ng)
	if err := c.save(); err != nil {
		return nil, err
	}
	var sent []string
	var skipped []string
	for _, m := range ms {
		if m == c.f.Name {
			continue
		}
		if b, err := c.setBlocked(setID(name), m); err != nil {
			return r, err
		} else if b { // its owner is banned here (groups.go setBlocked)
			skipped = append(skipped, m)
			continue
		}
		if _, err := c.sendDatagram(tGroupKey, m, ng.forMembers()); err != nil {
			return r, fmt.Errorf("generation %d sent to %v, not to %s: %w (group rotate %s again)", ng.Gen, sent, m, err, name)
		}
		sent = append(sent, m)
	}
	r["sent_to"] = sent
	if len(skipped) > 0 {
		r["not_sent"] = map[string]any{"names": skipped, "why": "their owner is banned from " + name + " (group remove them too)"}
	}
	return r, nil
}

// keepGroups: the admin's keeper: rotations that are due.
func (c *Client) keepGroups() ([]string, error) {
	e, err := c.ch.epoch()
	if err != nil {
		return nil, err
	}
	var done []string
	for name, g := range c.st.Groups {
		admin := g.Anon != nil && g.Anon.Members != nil && g.Anon.Admin == c.f.Name || g.Anon == nil && !g.IsPublic && c.hasRole(name, "admin", c.f.Name)
		if !admin {
			continue
		}
		if why := c.rotationDue(g, e); why != "" {
			if _, err := c.rotateGroup(name, why); err != nil {
				return done, err
			}
			done = append(done, name+": generation "+fmt.Sprint(c.st.Groups[name].Gen)+" ("+why+")")
		}
	}
	return done, c.answerRekeys()
}

// ---- anonymous groups: ask and answer

// onGroupRotate: the admin announced a generation: ask for it (after the scan).
func (c *Client) onGroupRotate(it *Item, d *Datagram, g *GroupKeys) {
	var r GroupRotate
	if g == nil || g.Anon == nil || json.Unmarshal(d.Body, &r) != nil || d.From != g.Anon.Admin || r.Gen <= g.Gen {
		return
	}
	if a := c.st.RekeyAsks[g.Name]; a != nil && a.Gen >= r.Gen {
		return
	}
	id, _, err := newEpochIdentity(false)
	if err != nil {
		return
	}
	if c.st.RekeyAsks == nil {
		c.st.RekeyAsks = map[string]*pendingAsk{}
	}
	c.st.RekeyAsks[g.Name] = &pendingAsk{Gen: r.Gen, Identity: id}
	it.Verified += " (asking for generation " + fmt.Sprint(r.Gen) + ")"
}

// postRekeyAsks: our open asks, posted with membership proofs.
func (c *Client) postRekeyAsks() {
	for name, a := range c.st.RekeyAsks {
		g := c.st.Groups[name]
		if g == nil || g.Anon == nil || a.Posted || g.Gen >= a.Gen {
			if g == nil || g.Gen >= a.Gen {
				delete(c.st.RekeyAsks, name)
			}
			continue
		}
		_, rec, err := parseIdentity(a.Identity)
		if err != nil {
			delete(c.st.RekeyAsks, name)
			continue
		}
		body, _ := json.Marshal(RekeyAsk{Gen: a.Gen, Have: g.Gen, Recipient: rec})
		if _, err := c.postAnonRaw(g, tRekeyAsk, body, a.Gen); err == nil {
			a.Posted = true
		} else if errors.Is(err, errAnonDirect) {
			msg := fmt.Sprintf("%s has a new key generation; asking for it from your own account would name you: give --relay URL (or --direct) and run any command", name)
			if !slices.Contains(c.st.Alerts, msg) {
				c.st.Alerts = append(c.st.Alerts, msg)
			}
		}
	}
}

// onRekeyAsk: the admin queues an answer for a proved ask (openAnonGroup checked the proof).
func (c *Client) onRekeyAsk(it *Item, d *Datagram, g *GroupKeys, root string) {
	if g.Anon == nil || g.Anon.Members == nil || g.Anon.Admin != c.f.Name {
		return
	}
	var a RekeyAsk
	if json.Unmarshal(d.Body, &a) != nil || a.Gen > g.Gen || a.Have >= g.Gen {
		return // an ask for an older generation gets the current one
	}
	s, err := c.anonTree(g.Anon.gid())
	if err != nil || s.current().String() != root { // no grace: a leaf removed a moment ago gets nothing
		it.Verified += " (not answered: proved against an old root)"
		return
	}
	key := g.Anon.Gid + "/" + fmt.Sprint(g.Gen) + "/" + it.From
	for _, q := range c.st.RekeyQueue {
		if q.Key == key {
			return
		}
	}
	if c.st.RekeyDone[key] {
		return
	}
	c.st.RekeyQueue = append(c.st.RekeyQueue, queuedAnswer{Group: g.Name, Gen: g.Gen, Have: a.Have, Recipient: a.Recipient, Key: key})
}

// answerRekeys: the admin seals the generation to each queued one-time key,
// in the previous generation's channel (the asker can't read the new one yet).
func (c *Client) answerRekeys() error {
	var rest []queuedAnswer
	defer func() { c.st.RekeyQueue = rest }()
	for i, q := range c.st.RekeyQueue {
		g := c.st.Groups[q.Group]
		if g == nil || g.Gen != q.Gen {
			continue // superseded: the asker will ask for the newer one
		}
		var in *GroupKeys // the channel the asker reads
		for _, o := range g.Old {
			if o.Gen == q.Have {
				in = o
			}
		}
		if in == nil {
			continue // its generation is past the retention: it can't be answered there
		}
		r, err := parseRecipient(q.Recipient)
		if err != nil {
			continue
		}
		b, _ := json.Marshal(g.forMembers())
		var ct bytes.Buffer
		w, err := age.Encrypt(&ct, r)
		if err != nil {
			continue
		}
		w.Write(b)
		w.Close()
		ans := RekeyAnswer{Gen: q.Gen, To: fingerprint(q.Recipient), Box: base64.StdEncoding.EncodeToString(ct.Bytes())}
		if _, err := c.sendInGen(in, g.Name, tRekeyAnswer, ans); err != nil {
			rest = append(rest, c.st.RekeyQueue[i:]...)
			return err
		}
		if c.st.RekeyDone == nil {
			c.st.RekeyDone = map[string]bool{}
		}
		c.st.RekeyDone[q.Key] = true
	}
	return nil
}

// onRekeyAnswer: the admin's answer to our ask, opened with the one-time key.
func (c *Client) onRekeyAnswer(it *Item, d *Datagram, g *GroupKeys) {
	a := c.st.RekeyAsks[g.Name]
	var ans RekeyAnswer
	if a == nil || g.Anon == nil || d.From != g.Anon.Admin || json.Unmarshal(d.Body, &ans) != nil || ans.Gen < a.Gen {
		return
	}
	id, rec, err := parseIdentity(a.Identity)
	if err != nil || fingerprint(rec) != ans.To {
		return // someone else's
	}
	box, err := base64.StdEncoding.DecodeString(ans.Box)
	if err != nil {
		return
	}
	rd, err := age.Decrypt(bytes.NewReader(box), id)
	if err != nil {
		return
	}
	b, err := io.ReadAll(io.LimitReader(rd, 1<<20))
	if err != nil {
		return
	}
	var ng GroupKeys
	if json.Unmarshal(b, &ng) != nil || ng.Anon == nil || !strings.EqualFold(ng.Anon.Gid, g.Anon.Gid) || ng.Gen != ans.Gen || ng.Name != g.Name {
		return
	}
	c.st.Groups[g.Name] = installGen(g, &ng)
	delete(c.st.RekeyAsks, g.Name) // the one-time key goes with it
	it.Verified += " (generation " + fmt.Sprint(ng.Gen) + " received)"
}

// sendInGen: a signed datagram in a given generation's channel (not the current one).
func (c *Client) sendInGen(gen *GroupKeys, group, typ string, body any) (string, error) {
	e, err := c.ch.epoch()
	if err != nil {
		return "", err
	}
	view, reader, err := gen.at(e)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(body)
	d := Datagram{Type: typ, From: c.f.Name, To: "group:" + group, Created: c.chainNow(), Readers: []string{fingerprint(reader)}, Body: b}
	signed, err := signDatagram(d, ethSigner{c.key})
	if err != nil {
		return "", err
	}
	env, err := seal(signed, e, view.PublicKey(), nil, []string{reader}, c.ch.bucketBits())
	if err != nil {
		return "", err
	}
	if err := c.publish(env); err != nil {
		return "", err
	}
	return env.hash().Hex(), nil
}

// leaveGroup: drop the keys and tell the admin (it removes us and rotates).
func (c *Client) leaveGroup(name string) (string, error) {
	g := c.st.Groups[name]
	if g == nil {
		return "", fmt.Errorf("you are not in group %s", name)
	}
	if g.Anon != nil {
		if g.Anon.Members != nil {
			return "", errors.New("you are its admin: hand the group over first (AnonGroups.setAdmin) or keep it")
		}
		if _, err := c.sendDatagram(tAnonLeave, g.Anon.Admin, AnonJoin{Gid: g.Anon.Gid}); err != nil {
			return "", err
		}
		if err := c.dropAnonIdentity(g.Anon.gid()); err != nil { // the secret that finds our leaf and pseudonyms
			return "", err
		}
		for k := range c.st.AnonSlots {
			if strings.HasPrefix(k, g.Anon.Gid+"/") {
				delete(c.st.AnonSlots, k)
			}
		}
	} else if _, err := c.ch.send(c.key, nil, "Sets", "remove", setID(name), c.id(), c.id(), false, "left"); err != nil {
		return "", err
	}
	delete(c.st.Groups, name)
	c.dropGroupItems(name)
	return "left " + name + ": its keys and posts are gone from this device; its admin rotates the keys", c.save()
}

// dropGroupItems: a group's posts, gone from state.json.
func (c *Client) dropGroupItems(name string) {
	var keep []*Item
	for _, it := range c.st.Items {
		if it.To != "group:"+name {
			keep = append(keep, it)
		}
	}
	c.st.Items = keep
}
