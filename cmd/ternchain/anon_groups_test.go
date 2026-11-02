package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/nenkoru/tern/internal/zk"
)

// anonCell: oscar's anonymous group with nina and ada, admitted the way
// people do it: an invitation, a join with a commitment, one admit.
func anonCell(t *testing.T, w *world, name string, unlinkable bool, slots int, members ...*Client) *Client {
	t.Helper()
	oscar, _ := w.owner("oscar")
	// These tests are about trees, proofs and keys, not the carrier: their
	// members post from their own accounts (--direct). TestAnonPostNeedsRelay
	// covers the relay rule.
	oscar.anonDirect = true
	for _, m := range members {
		m.anonDirect = true
	}
	if _, err := oscar.createAnonGroup(name, unlinkable, slots); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if _, err := oscar.inviteAnon(name, m.f.Name); err != nil {
			t.Fatal(err)
		}
		scan(t, m)
		if m.st.AnonInvites[name] == nil {
			t.Fatalf("%s got no invitation", m.f.Name)
		}
		if _, err := m.joinGroup(name); err != nil {
			t.Fatal(err)
		}
	}
	scan(t, oscar)
	done, err := oscar.admitPending()
	if err != nil || len(done) != len(members) {
		t.Fatalf("admit: %v %v", done, err)
	}
	for _, m := range members {
		scan(t, m)
		if g := m.st.Groups[name]; g == nil || g.Anon == nil {
			t.Fatalf("%s has no keys for %s", m.f.Name, name)
		}
	}
	return oscar
}

func anonPosts(t *testing.T, c *Client, group string) []*Item {
	t.Helper()
	scan(t, c)
	c.refreshCommunity(group)
	return c.groupItems(group)
}

func TestAnonymousGroupMembership(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	eve, _ := w.owner("eve")
	oscar := anonCell(t, w, "cell", false, 0, nina, ada)

	// The chain names nobody: no Sets group, and the tree's leaves are commitments.
	if out, err := w.ch.call("Sets", "sets", setID("cell")); err == nil && out[2].(bool) {
		t.Fatal("an anonymous group made a Sets set")
	}
	s, err := ada.anonTree(ada.st.Groups["cell"].Anon.gid())
	if err != nil || s.members() != 3 || len(s.bad) != 0 {
		t.Fatalf("tree: %+v %v", s, err)
	}
	for _, ev := range []string{"Leaves", "Created"} {
		es, _ := w.ch.events("AnonGroups", ev)
		b, _ := json.Marshal(es)
		for _, n := range []string{"nina", "ada", "oscar", "cell"} {
			// A word, not a substring: "ada" is hex, and turns up in tx
			// hashes and addresses.
			if regexp.MustCompile(`\b` + n + `\b`).Match(b) {
				t.Fatalf("AnonGroups.%s names %s", ev, n)
			}
		}
	}

	// A post: a membership proof, no name; other members can't tell whose.
	if _, err := nina.postGroup("cell", "meeting moved to thursday", nil); err != nil {
		t.Fatal(err)
	}
	var post *Item
	for _, it := range anonPosts(t, ada, "cell") {
		if it.Mail != nil && it.Mail.Text == "meeting moved to thursday" {
			post = it
		}
	}
	if post == nil || post.Status != "delivered" || !strings.HasPrefix(post.From, "member:") || post.Author != "anonymous member" ||
		strings.Contains(post.Verified+post.Trust, "nina") {
		t.Fatalf("post: %+v", post)
	}
	// The same member, the same pseudonym (pseudonymous mode).
	nina.postGroup("cell", "bring the documents", nil)
	var second *Item
	for _, it := range anonPosts(t, ada, "cell") {
		if it.Mail != nil && it.Mail.Text == "bring the documents" {
			second = it
		}
	}
	if second == nil || second.From != post.From {
		t.Fatalf("pseudonym changed: %+v", second)
	}
	ownPost, _ := ada.postGroup("cell", "ok", nil)
	for _, it := range anonPosts(t, nina, "cell") {
		if it.ID == ownPost && (it.From == post.From || it.Status != "delivered") {
			t.Fatalf("two members, one pseudonym: %+v", it)
		}
	}

	// An infiltrator with the channel keys but no leaf can't post.
	stolen := *nina.st.Groups["cell"]
	eve.st.Groups["cell"] = &stolen
	if _, err := eve.postGroup("cell", "trust me", nil); err == nil || !strings.Contains(err.Error(), "not a leaf") {
		t.Fatalf("keys without a leaf posted: %v", err)
	}
	// A proof moved onto another post fails (it binds the payload).
	forged := forgeAnonPost(t, nina, "cell", "pay eve 100")
	for _, it := range anonPosts(t, ada, "cell") {
		if it.ID == forged && it.Status != "refused" {
			t.Fatalf("forged post accepted: %+v", it)
		}
	}

	// The admin bans a pseudonym: readers hide its posts; it can't post.
	if err := oscar.moderate("cell", "ban", post.From[len("member:"):len("member:")+12], "leaks"); err != nil {
		t.Fatal(err)
	}
	if _, err := nina.postGroup("cell", "again", nil); err == nil || !strings.Contains(err.Error(), "banned") {
		t.Fatalf("banned pseudonym posted: %v", err)
	}
	for _, it := range anonPosts(t, ada, "cell") {
		if it.From == post.From && it.Status != "hidden" {
			t.Fatalf("banned pseudonym shown: %+v", it)
		}
	}
	// The admin removes a member it knows: its leaf goes, its proofs stop
	// (once the old root's grace is over).
	if err := oscar.moderate("cell", "remove", "ada", "left"); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.postGroup("cell", "still here?", nil); err == nil || !strings.Contains(err.Error(), "not a leaf") {
		t.Fatalf("removed member posted: %v", err)
	}
	v, _ := oscar.anonGroupView(oscar.st.Groups["cell"])
	if v["members"] != 2 {
		t.Fatalf("members after removal: %v", v)
	}
	v, _ = nina.anonGroupView(nina.st.Groups["cell"])
	if _, ok := v["known_to_you"]; ok {
		t.Fatal("a member sees the admin's list")
	}
}

// forgeAnonPost: nina's valid proof, put on another payload.
func forgeAnonPost(t *testing.T, c *Client, group, text string) string {
	t.Helper()
	g := c.st.Groups[group]
	id, err := c.postGroup(group, "innocent", nil)
	if err != nil {
		t.Fatal(err)
	}
	var sig *AnonSig
	for _, it := range anonPosts(t, c, group) {
		if it.ID == id {
			sig = c.lastAnonSig
		}
	}
	if sig == nil {
		t.Fatal("no proof kept")
	}
	e, _ := c.ch.epoch()
	view, reader, _ := g.at(e)
	m, _ := json.Marshal(Mail{Thread: "t_x", Intent: "inform", Text: text})
	d, _ := json.Marshal(Datagram{Type: tMessage, To: "group:" + group, Created: c.chainNow(), Readers: []string{fingerprint(reader)}, Body: m})
	signed, _ := json.Marshal(Signed{Payload: d, Anon: sig})
	env, err := seal(signed, e, view.PublicKey(), nil, []string{reader}, c.ch.bucketBits())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.publish(env); err != nil {
		t.Fatal(err)
	}
	return env.hash().Hex()
}

func TestAnonymousGroupUnlinkable(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	anonCell(t, w, "drop", true, 2, nina, ada)
	a, err := nina.postGroup("drop", "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := nina.postGroup("drop", "two", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nina.postGroup("drop", "three", nil); err == nil || !strings.Contains(err.Error(), "2 posts per member an hour") {
		t.Fatalf("third post in the hour: %v", err)
	}
	from := map[string]string{}
	for _, it := range anonPosts(t, ada, "drop") {
		if it.ID == a || it.ID == b {
			if it.Status != "delivered" {
				t.Fatalf("post: %+v", it)
			}
			from[it.ID] = it.From
		}
	}
	if len(from) != 2 || from[a] == from[b] {
		t.Fatalf("unlinkable posts share a pseudonym: %v", from)
	}
	// A client that lies about its slots repeats a nullifier: readers refuse the repeat.
	nina.st.AnonSlots = nil
	c, err := nina.postGroup("drop", "one again", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range anonPosts(t, ada, "drop") {
		if it.ID == c && it.Status != "refused" {
			t.Fatalf("a slot used twice was accepted: %+v", it)
		}
	}
	// Bans need pseudonyms: an unlinkable group has none.
	oscar, _ := openClient(w.dir+"/oscar", w.ch)
	if err := oscar.moderate("drop", "ban", "member:00", ""); err == nil || !strings.Contains(err.Error(), "unlinkable") {
		t.Fatalf("ban in an unlinkable group: %v", err)
	}
	// The next hour has fresh slots.
	w.later(time.Hour)
	if _, err := nina.postGroup("drop", "next hour", nil); err != nil {
		t.Fatal(err)
	}
}

// Keys for an anonymous group are taken only from the admin that invited us.
func TestAnonymousGroupKeysOnlyFromAdmin(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	eve, _ := w.owner("eve")
	anonCell(t, w, "cell", false, 0, nina)
	fake, _ := newGroupKeys("cell", 0, 2, false, false)
	fake.Anon = &AnonInfo{Gid: nina.st.Groups["cell"].Anon.Gid, Mode: anonPseudonymous, Admin: "eve"}
	if _, err := eve.sendDatagram(tGroupKey, "nina", fake); err != nil {
		t.Fatal(err)
	}
	before := nina.st.Groups["cell"].ViewKey
	scan(t, nina)
	if nina.st.Groups["cell"].ViewKey != before || nina.st.Groups["cell"].Anon.Admin != "oscar" {
		t.Fatal("keys from a non-admin replaced the group's")
	}
}

// The costs docs/PRIVACY.md quotes: gas of the tree's changes, a post's proof.
func TestAnonymousGroupCosts(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	oscar := anonCell(t, w, "cell", false, 0, nina)
	start := time.Now()
	if _, err := nina.postGroup("cell", "x", nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("post: proof %v, whole post %v", zk.LastProveTime, time.Since(start))
	start = time.Now()
	anonPosts(t, oscar, "cell")
	t.Logf("read with one proof check: %v", time.Since(start))
	gid := common.HexToHash("0x01")
	gas := func(what string, method string, args ...any) {
		r, err := oscar.ch.send(oscar.key, nil, "AnonGroups", method, args...)
		if err != nil {
			t.Fatal(what, err)
		}
		t.Logf("%-28s %8d gas", what, r.GasUsed)
	}
	gas("create", "create", gid, uint8(0))
	var leaves []*big.Int
	for i := 1; i <= 17; i++ {
		leaves = append(leaves, big.NewInt(int64(i)))
	}
	gas("add 1 leaf", "add", gid, leaves[:1], zk.NewTree(zk.GroupDepth, leaves[:1]).Root())
	gas("add 16 leaves", "add", gid, leaves[1:], zk.NewTree(zk.GroupDepth, leaves).Root())
	gas("remove 1", "remove", gid, []uint32{3}, big.NewInt(5))
	gas("ban", "ban", gid, big.NewInt(42), common.Hash{})
}

// A binary with the opt-in modules still matches a network deployed before them.
func TestOptionalExtLeftOutOfTheHash(t *testing.T) {
	all, n := extContractsHash()
	without, m := extContractsHashFor(map[string]string{"SponsorPaymaster": "0x1"})
	with, k := extContractsHashFor(map[string]string{"AnonGroups": "0x2", "PrivateReviews": "0x3"})
	if without == all || m != n-2 || with != all || k != n {
		t.Fatalf("all %s/%d, without %s/%d, with %s/%d", all, n, without, m, with, k)
	}
}

// An anonymous post goes through a relay: from the member's own account the
// transaction's sender names the author to every member (red team D-10).
func TestAnonPostNeedsRelay(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	anonCell(t, w, "cell", false, 0, nina)
	nina.anonDirect = false
	if _, err := nina.postGroup("cell", "hello", nil); !errors.Is(err, errAnonDirect) || !strings.Contains(err.Error(), "--relay") {
		t.Fatalf("an anonymous post from her own account: %v", err)
	}
	r := w.startRelay()
	nina.relay = w.relay
	id, err := nina.postGroup("cell", "via the relay", nil)
	if err != nil {
		t.Fatal(err)
	}
	must(t, r.flush())
	w.sim.Commit()
	evs, err := w.ch.events("Inbox", "Posted")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		b, _ := e.Args["batch"].([]byte)
		envs, _ := decodeBatch(b)
		for _, env := range envs {
			if env.hash().Hex() != id {
				continue
			}
			found = true
			x, _, err := w.sim.Client().TransactionByHash(context.Background(), e.Tx)
			if err != nil {
				t.Fatal(err)
			}
			from, _ := types.Sender(types.LatestSignerForChainID(x.ChainId()), x)
			if from == nina.addr() || from == nina.me() {
				t.Fatal("the relayed post is carried by nina's own account")
			}
		}
	}
	if !found {
		t.Fatal("the relayed post is not on chain")
	}
	// --direct: knowingly from her own account (a warning on stderr).
	nina.relay, nina.anonDirect = "", true
	if _, err := nina.postGroup("cell", "direct", nil); err != nil {
		t.Fatal(err)
	}
}
