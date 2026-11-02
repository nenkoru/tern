//go:build redteam || redteam_ab

// Red team, scenarios A (an exposed group member: a device seized at time T)
// and B (a stolen pairwise mail key or epoch key). Each test passes asserting
// what the code does today; a "FINDING" comment marks an attack that works,
// so a fix flips the assertion. Run:
//
//	go test -tags redteam_ab -run 'TestRedTeam[AB]' -count=1 -v ./cmd/ternchain
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/zk"
)

const rtabDay = 24 * time.Hour

// rtabSnapshot: what an attacker images from c's device now (the identity folder).
func rtabSnapshot(t *testing.T, c *Client) string {
	t.Helper()
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "seized-"+c.f.Name)
	err := filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(c.dir, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// rtabKeys: every decryption key an attacker finds in an imaged folder:
// epoch keys on disk, every group generation in state.json, open one-time keys.
func rtabKeys(t *testing.T, dir string) []age.Identity {
	t.Helper()
	var ids []age.Identity
	ents, _ := os.ReadDir(filepath.Join(dir, "epochs"))
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, "epochs", e.Name()))
		if err != nil {
			continue
		}
		if id, _, err := parseIdentity(string(b)); err == nil {
			ids = append(ids, id)
		}
	}
	var st State
	readJSON(filepath.Join(dir, "state.json"), &st)
	for _, g := range st.Groups {
		for _, gen := range g.gens() {
			for _, k := range gen.Keys {
				if id, _, err := parseIdentity(k); err == nil {
					ids = append(ids, id)
				}
			}
		}
	}
	for _, a := range st.RekeyAsks {
		if id, _, err := parseIdentity(a.Identity); err == nil {
			ids = append(ids, id)
		}
	}
	// Keys an item's body still carries (a tern.group-key datagram kept in state.json).
	for _, it := range st.Items {
		var g GroupKeys
		if it.Type == tGroupKey && json.Unmarshal(it.Body, &g) == nil {
			for _, k := range g.Keys {
				if id, _, err := parseIdentity(k); err == nil {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// rtabEnv: the envelope with hash id, from the chain.
func rtabEnv(t *testing.T, c *Client, id string) Envelope {
	t.Helper()
	bs, err := c.batchesSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		envs, _ := decodeBatch(b.Data)
		for _, e := range envs {
			if e.hash().Hex() == id {
				return e
			}
		}
	}
	t.Fatalf("envelope %s not on chain", id)
	return Envelope{}
}

// rtabOpens: does an imaged folder open envelope id and find text in it?
func rtabOpens(t *testing.T, dir string, c *Client, id, text string) bool {
	t.Helper()
	ids := rtabKeys(t, dir)
	if len(ids) == 0 {
		return false
	}
	e := rtabEnv(t, c, id)
	plain, err := e.open(ids)
	if err != nil {
		return false
	}
	var sg Signed
	if json.Unmarshal(plain, &sg) == nil && len(sg.Payload) > 0 {
		var d Datagram
		if json.Unmarshal(sg.Payload, &d) == nil {
			return bytes.Contains(d.Body, []byte(text))
		}
	}
	return bytes.Contains(plain, []byte(text))
}

// rtabDirHas: any file under dir contains s.
func rtabDirHas(t *testing.T, dir, s string) []string {
	t.Helper()
	var hits []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(s)) {
			rel, _ := filepath.Rel(dir, p)
			hits = append(hits, rel)
		}
		return nil
	})
	return hits
}

func rtabPost(t *testing.T, c *Client, group, text string) string {
	t.Helper()
	id, err := c.postGroup(group, text, nil)
	if err != nil {
		t.Fatalf("%s posting %q: %v", c.f.Name, text, err)
	}
	return id
}

func rtabItem(items []*Item, text string) *Item {
	for _, it := range items {
		if it.Mail != nil && it.Mail.Text == text {
			return it
		}
	}
	return nil
}

func rtabResult(t *testing.T, id, verdict, what string) {
	t.Helper()
	t.Logf("RESULT %s %s: %s", id, verdict, what)
}

// ---------------------------------------------------------------- A: Sets group

// A-1..A-4: a private Sets group; eve's device is imaged at day 10.
func TestRedTeamA_SeizedSetsMember(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina") // admin
	ada, _ := w.owner("ada")
	eve, _ := w.owner("eve") // the seized member
	must(t, nina.createGroup("club", false))
	gen0 := *nina.st.Groups["club"]
	gen0Day0Key := gen0.Keys[0]
	for _, c := range []*Client{ada, eve} {
		if _, err := c.joinGroup("club"); err != nil {
			t.Fatal(err)
		}
	}
	if done, err := nina.admitPending(); err != nil || len(done) != 2 {
		t.Fatalf("admit: %v %v", done, err)
	}
	scan(t, ada)
	scan(t, eve)
	day0 := rtabPost(t, nina, "club", "rt-day0")
	scan(t, eve)

	// Ten days pass; the admin's keeper makes the scheduled generation.
	w.later(10 * rtabDay)
	for _, c := range []*Client{nina, ada, eve} {
		if _, err := c.rotateEpochs(); err != nil {
			t.Fatal(err)
		}
	}
	if done, err := nina.keepGroups(); err != nil || len(done) != 1 {
		t.Fatalf("keeper: %v %v", done, err)
	}
	scan(t, ada)
	scan(t, eve)
	if eve.st.Groups["club"].Gen != 1 {
		t.Fatalf("eve has generation %d", eve.st.Groups["club"].Gen)
	}
	day10 := rtabPost(t, nina, "club", "rt-day10")
	scan(t, eve)

	// T: the device is imaged.
	snap := rtabSnapshot(t, eve)

	// Before T: the day-0 post's ciphertext doesn't open (its epoch key is gone).
	// A-1 fixed (redteam/fix-group-keys): the tern.group-key item no longer keeps the keys.
	if rtabOpens(t, snap, nina, day0, "rt-day0") {
		t.Fatal("A-1: a device imaged at day 10 decrypts a day-0 group post")
	}
	if hits := rtabDirHas(t, snap, gen0Day0Key); len(hits) != 0 {
		t.Fatalf("A-1: gen 0's destroyed day-0 key is still on the device in %v", hits)
	}
	rtabResult(t, "A-1", "HELD", "day-0 post ciphertext doesn't open at day 10; the destroyed key is nowhere on the device")
	// The plaintext of the day-0 post is in state.json too, by design (keep-days 30).
	if len(rtabDirHas(t, snap, "rt-day0")) == 0 {
		t.Fatal("expected the day-0 plaintext within keep-days")
	}
	rtabResult(t, "A-2", "AS-DOCUMENTED", "the day-0 post's plaintext is in state.json until keep-days (30; strict 7)")

	// At T: the current generation opens.
	if !rtabOpens(t, snap, nina, day10, "rt-day10") {
		t.Fatal("the seized device should open the current generation")
	}
	// After T, before anyone removes eve: the attacker reads on (a member is a member).
	afterT := rtabPost(t, nina, "club", "rt-after-T")
	if !rtabOpens(t, snap, nina, afterT, "rt-after-T") {
		t.Fatal("expected: a seized member device reads until removed")
	}
	rtabResult(t, "A-3", "AS-DOCUMENTED", "the seized device reads new posts until eve is removed")

	// eve's owner tells the admin; the admin bans eve: a new generation.
	w.must(nina.moderate("club", "ban", "eve", "device seized"))
	if nina.st.Groups["club"].Gen != 2 {
		t.Fatalf("no rotation on ban: gen %d", nina.st.Groups["club"].Gen)
	}
	afterBan := rtabPost(t, nina, "club", "rt-after-ban")
	if rtabOpens(t, snap, nina, afterBan, "rt-after-ban") {
		t.Fatal("the admin's post after the ban opens on the seized device")
	}
	rtabResult(t, "A-4", "HELD", "the admin's posts after the ban's rotation don't open on the seized device")

	// ada hasn't scanned since the ban: her client still posts in generation 1.
	stale := rtabPost(t, ada, "club", "rt-ada-stale")
	staleOpens := rtabOpens(t, snap, nina, stale, "rt-ada-stale")
	// FINDING A-5: a member who hasn't scanned since the rotation posts in the
	// old generation, which the banned member (or a seized device) still reads.
	if !staleOpens {
		t.Fatal("A-5 flipped: a stale member's post no longer opens for the banned member (fixed?)")
	}
	rtabResult(t, "A-5", "BROKEN", "ada (not scanned since the ban) posts in generation 1: the banned/seized device reads it")
	scan(t, ada)
	fresh := rtabPost(t, ada, "club", "rt-ada-fresh")
	if rtabOpens(t, snap, nina, fresh, "rt-ada-fresh") {
		t.Fatal("ada's post after she scanned opens on the seized device")
	}

	// The seized identity posts after the ban, with its old generation.
	thief, err := openClient(snap, w.ch)
	if err != nil {
		t.Fatal(err)
	}
	rtabPost(t, thief, "club", "rt-thief-post")
	it := rtabItem(scan(t, ada), "rt-thief-post")
	if it == nil {
		t.Fatal("ada doesn't see the thief's post at all")
	}
	if it.Status != "hidden" {
		t.Fatalf("a banned member's post is %s", it.Status)
	}
	rtabResult(t, "A-6", "HELD", "a banned member's post in the old generation is hidden ("+it.Trust+")")
}

// ---------------------------------------------------------------- A: anonymous groups

// rtabAnonGroup: admin's anonymous group with members, admitted as people do it.
func rtabAnonGroup(t *testing.T, admin *Client, name string, unlinkable bool, members ...*Client) {
	t.Helper()
	admin.anonDirect = true // the carrier is D-10's (redteam/fix-anon-relay); here: keys and trees
	for _, m := range members {
		m.anonDirect = true
	}
	if _, err := admin.createAnonGroup(name, unlinkable, 0); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if _, err := admin.inviteAnon(name, m.f.Name); err != nil {
			t.Fatal(err)
		}
		scan(t, m)
		if _, err := m.joinGroup(name); err != nil {
			t.Fatal(err)
		}
	}
	scan(t, admin)
	if done, err := admin.admitPending(); err != nil || len(done) != len(members) {
		t.Fatalf("admit: %v %v", done, err)
	}
	for _, m := range members {
		scan(t, m)
		if g := m.st.Groups[name]; g == nil || g.Anon == nil {
			t.Fatalf("%s has no keys for %s", m.f.Name, name)
		}
	}
}

// rtabLeafEver: is commitment cm a leaf of any anonymous group, ever (Leaves events)?
func rtabLeafEver(t *testing.T, c *Client, cm *big.Int) []common.Hash {
	t.Helper()
	es, err := c.ch.events("AnonGroups", "Leaves")
	if err != nil {
		t.Fatal(err)
	}
	var out []common.Hash
	for _, e := range es {
		ls, _ := e.Args["leaves"].([]*big.Int)
		for _, l := range ls {
			if l.Cmp(cm) == 0 {
				out = append(out, common.Hash(e.Args["gid"].([32]byte)))
			}
		}
	}
	return out
}

// A-7, A-8: the anonymous identity was derived from the account key
// (eth.key), so one imaged key showed every anonymous group the person ever
// joined (left ones too) from public chain data, and attributed her past
// posts. Since redteam/fix-anon-secret a join's identity is random, kept in
// anon.json (not derived from eth.key), and destroyed by group leave; a group
// joined the old way keeps its legacy identity until group anon-migrate, and
// what was made with it stays attributable.
func TestRedTeamA_AnonIdentityFromAccountKey(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	oscar, _ := w.owner("oscar")
	rtabAnonGroup(t, oscar, "cell", false, nina, ada)
	rtabAnonGroup(t, oscar, "cell2", true, nina, ada)
	gid := ada.st.Groups["cell"].Anon.gid()
	gid2 := ada.st.Groups["cell2"].Anon.gid()
	// cell3: joined before random identities (the eth.key one), then migrated.
	rtabAnonGroup(t, oscar, "cell3", false, nina)
	legacyJoin(t, oscar, ada, "cell3")
	gid3 := ada.st.Groups["cell3"].Anon.gid()
	if _, err := ada.postGroup("cell3", "rt-legacy-ada", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.migrateAnon("cell3"); err != nil {
		t.Fatal(err)
	}
	scan(t, oscar)
	if _, err := oscar.admitPending(); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.postGroup("cell", "rt-anon-ada", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.postGroup("cell2", "rt-unlinkable-ada", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.postGroup("cell3", "rt-migrated-ada", nil); err != nil {
		t.Fatal(err)
	}
	// What nina (any member, or an infiltrator) archived: pseudonyms on posts.
	seen := map[string]string{}
	for _, g := range []string{"cell", "cell2", "cell3"} {
		for _, it := range anonPosts(t, nina, g) {
			if it.Mail != nil {
				seen[it.Mail.Text] = it.From
			}
		}
	}
	for _, x := range []string{"rt-anon-ada", "rt-unlinkable-ada", "rt-legacy-ada", "rt-migrated-ada"} {
		if seen[x] == "" {
			t.Fatalf("nina didn't read %s: %v", x, seen)
		}
	}
	var secrets []string
	for _, g := range []common.Hash{gid, gid2, gid3} {
		if e := anonStored(t, ada, g); e != nil {
			secrets = append(secrets, e.Secret)
		}
	}
	if len(secrets) != 3 {
		t.Fatalf("ada's identities: %v", secrets)
	}

	// ada leaves all three groups: her device forgets them.
	for _, g := range []string{"cell", "cell2", "cell3"} {
		if _, err := ada.leaveGroup(g); err != nil {
			t.Fatal(err)
		}
	}
	scan(t, oscar)
	if _, err := oscar.keepGroups(); err != nil {
		t.Fatal(err)
	}
	if ada.st.Groups["cell"] != nil || ada.st.Groups["cell2"] != nil || ada.st.Groups["cell3"] != nil {
		t.Fatal("ada still holds the groups")
	}

	// Later her device is imaged. The attacker has eth.key, the folder and public chain data.
	snap := rtabSnapshot(t, ada)
	thief, err := openClient(snap, w.ch)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range secrets {
		if hits := rtabDirHas(t, snap, sec); len(hits) > 0 {
			t.Fatalf("A-7: a destroyed identity is still in the image: %v", hits)
		}
	}
	found := map[common.Hash]bool{}
	for _, g := range []common.Hash{gid, gid2, gid3} {
		for _, h := range rtabLeafEver(t, thief, zk.IdentityCommitment(legacyAnonSecret(thief.key, g))) {
			found[h] = true
		}
		if e := anonStored(t, thief, g); e != nil {
			t.Fatalf("A-7: the image holds an identity for %s", g.Hex())
		}
	}
	// FIXED A-7: groups joined with a random identity aren't found from eth.key.
	if found[gid] || found[gid2] {
		t.Fatalf("A-7: the account key finds ada's leaves in fresh joins: %v", found)
	}
	rtabResult(t, "A-7", "HELD", "fresh joins: eth.key and the imaged folder find none of ada's leaves after she left (random identity, destroyed on leave)")
	// By design: a group joined before the fix keeps its legacy leaf in the
	// public Leaves history; migration can't erase it.
	if !found[gid3] {
		t.Fatal("A-7: expected the legacy leaf (joined before the fix) to stay findable")
	}
	rtabResult(t, "A-7", "BY DESIGN", "a group joined with the legacy eth.key identity stays findable in the on-chain Leaves history, migrated or not (documented)")

	// FIXED A-8: no post made with a random identity is attributed.
	if pseudonym(zk.HashN(anonMemberScope(gid), legacyAnonSecret(thief.key, gid))) == seen["rt-anon-ada"] {
		t.Fatal("A-8: the pseudonym is recomputable from eth.key")
	}
	attributed := map[string]bool{}
	h := uint64(nina.chainNow()) / 3600
	for hour := h - 2; hour <= h; hour++ {
		for slot := 0; slot < anonDefaultSlots; slot++ {
			n := zk.HashN(anonSlotScope(gid2, hour, slot), legacyAnonSecret(thief.key, gid2))
			attributed["anon:"+strings.TrimPrefix(pseudonym(n), "member:")[:16]] = true
		}
	}
	if attributed[seen["rt-unlinkable-ada"]] {
		t.Fatal("A-8: the unlinkable post is attributable from eth.key")
	}
	legacyP := pseudonym(zk.HashN(anonMemberScope(gid3), legacyAnonSecret(thief.key, gid3)))
	if legacyP == seen["rt-migrated-ada"] {
		t.Fatal("A-8: the post after migration is attributable from eth.key")
	}
	rtabResult(t, "A-8", "HELD", "eth.key recomputes neither ada's pseudonym nor her slot nullifiers in fresh joins, nor her pseudonym after migrating")
	if legacyP != seen["rt-legacy-ada"] {
		t.Fatal("A-8: expected the post made before migrating to stay attributable")
	}
	rtabResult(t, "A-8", "BY DESIGN", "posts made with the legacy identity before migrating stay attributable to eth.key (documented)")
}

// A-9, A-10: an anonymous member speaks as herself without meaning to.
func TestRedTeamA_AnonNameLeaks(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	oscar, _ := w.owner("oscar")
	rtabAnonGroup(t, oscar, "cell", false, nina, ada)

	// A plain send to the group (CLI send --to group:cell, MCP tern_send) would sign with the name.
	// A-9 fixed (redteam/fix-anon-send): refused.
	if _, err := ada.send(SendOpts{To: "group:cell", Intent: "inform", Text: "rt-signed-in-anon"}); err == nil {
		t.Fatal("A-9: send --to group:cell in an anonymous group went out signed by name")
	} else {
		rtabResult(t, "A-9", "HELD", "send to an anonymous group refused: "+err.Error())
	}
	if it := rtabItem(anonPosts(t, nina, "cell"), "rt-signed-in-anon"); it != nil {
		t.Fatalf("A-9: a member received the name-signed post: %+v", it)
	}

	// Coop presence in an anonymous group carries the signed name.
	_, srv := newNode(w)
	for _, c := range []*Client{nina, ada} {
		c.relay = srv.URL
	}
	if _, err := ada.announcePresence("cell", coopFlags{state: "online", ttl: 120}); err != nil {
		rtabResult(t, "A-10", "HELD", "presence refused in an anonymous group: "+err.Error())
		return
	}
	online, _, dropped, err := nina.presence("cell")
	w.must(err)
	named := false
	for _, p := range online {
		named = named || p.Name == "ada"
	}
	for _, d := range dropped { // dropped as "not a member" (no Sets membership), but named all the same
		named = named || strings.HasPrefix(d, "ada:")
	}
	t.Logf("presence: online %d, dropped %v", len(online), dropped)
	// FINDING A-10: presence (and coop credits/offers posts) in an anonymous group name the member.
	if !named {
		t.Fatalf("A-10 flipped: %+v", online)
	}
	rtabResult(t, "A-10", "BROKEN", "coop presence announce in an anonymous group is sealed to the group key with the signed name: every member decrypts 'ada' (listed under dropped: not a Sets member)")
}

// A-11..A-13: ban and removal in an anonymous group.
func TestRedTeamA_AnonBanRemoveWindows(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	eve, _ := w.owner("eve")
	oscar, _ := w.owner("oscar")
	rtabAnonGroup(t, oscar, "cell", false, nina, ada, eve)
	if _, err := eve.postGroup("cell", "rt-eve-pseud", nil); err != nil {
		t.Fatal(err)
	}
	var evePseud string
	for _, it := range anonPosts(t, oscar, "cell") {
		if it.Mail != nil && it.Mail.Text == "rt-eve-pseud" {
			evePseud = it.From
		}
	}
	snap := rtabSnapshot(t, eve)
	w.must(oscar.moderate("cell", "ban", evePseud, "spy"))
	// nina and ada see the announcement and ask; until oscar's next scan answers, they post in the old generation.
	scan(t, nina)
	scan(t, eve) // eve asks too (refused: banned)
	window := rtabPost(t, nina, "cell", "rt-before-answer")
	inWindow := rtabOpens(t, snap, nina, window, "rt-before-answer")
	scan(t, oscar)
	if _, err := oscar.keepGroups(); err != nil { // answers the asks
		t.Fatal(err)
	}
	scan(t, nina)
	scan(t, eve)
	if g := eve.st.Groups["cell"]; g.Gen != 0 {
		t.Fatalf("the banned pseudonym got generation %d", g.Gen)
	}
	if nina.st.Groups["cell"].Gen != 1 {
		t.Fatalf("nina has generation %d", nina.st.Groups["cell"].Gen)
	}
	after := rtabPost(t, nina, "cell", "rt-after-answer")
	if rtabOpens(t, snap, nina, after, "rt-after-answer") || rtabOpens(t, eve.dir, nina, after, "rt-after-answer") {
		t.Fatal("the banned member reads the new generation")
	}
	rtabResult(t, "A-11", "HELD", "a banned pseudonym is refused the new generation; posts after the answer are sealed from it")
	// FINDING A-12: between the ban and the admin's answer, members post in the old generation.
	if !inWindow {
		t.Fatal("A-12 flipped: posts before the admin's answer no longer open for the banned member")
	}
	rtabResult(t, "A-12", "BROKEN", "after a ban, members keep posting in the old generation until the admin's next scan answers their asks: the banned member reads those")

	// Removal (by name) of ada. Her imaged device keeps its last view of the
	// tree (a modified client needn't re-read it) and proves against the old root.
	adaSnap := rtabSnapshot(t, ada)
	gid := ada.st.Groups["cell"].Anon.gid()
	ada.forgetAnon(gid)
	pre, err := ada.anonTree(gid)
	if err != nil {
		t.Fatal(err)
	}
	w.must(oscar.moderate("cell", "remove", "ada", "left the city"))
	ghost, err := openClient(adaSnap, w.ch)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := w.ch.b.BlockNumber(context.Background())
	ghost.anonMemo = map[common.Hash]*anonState{gid: pre}
	ghost.anonAt = map[common.Hash]uint64{gid: head}
	if _, err := ghost.postGroup("cell", "rt-removed-post", nil); err != nil {
		rtabResult(t, "A-13", "HELD", "a removed member's client can't post: "+err.Error())
		return
	}
	it := rtabItem(anonPosts(t, nina, "cell"), "rt-removed-post")
	// FINDING A-13: a removed member posts for anonRootGrace (1 h) after the removal, and members accept it.
	if it == nil || it.Status != "delivered" {
		t.Fatalf("A-13 flipped: %+v", it)
	}
	rtabResult(t, "A-13", "BROKEN(low)", "a removed member posts with the pre-removal root within the 1 h grace: delivered to members still on the old generation")
}

// ---------------------------------------------------------------- B: pairwise mail

func rtabMailPair(t *testing.T) (*world, *Client, *Client) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.st.Contacts["alice"] = true
	return w, alice, bob
}

func rtabMail(t *testing.T, from *Client, to, text string) string {
	t.Helper()
	id, err := from.send(SendOpts{To: to, Intent: "inform", Text: text})
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return id
}

// B-1..B-3: past letters after the retention; key files and state.json.
func TestRedTeamB_PastMailSealed(t *testing.T) {
	w, alice, bob := rtabMailPair(t)
	e0, _ := w.ch.epoch()
	id0 := rtabMail(t, alice, "bob", "rt-letter-day0")
	scan(t, bob)
	key0, err := os.ReadFile(bob.epochPath(e0))
	if err != nil {
		t.Fatal(err)
	}
	key0s := strings.TrimSpace(string(key0))
	early := rtabSnapshot(t, bob)
	if !rtabOpens(t, early, alice, id0, "rt-letter-day0") {
		t.Fatal("a same-day image should open the day's letter")
	}

	w.later(9 * rtabDay)
	scan(t, bob) // any scan destroys keys past the retention
	if _, err := os.Stat(bob.epochPath(e0)); !os.IsNotExist(err) {
		t.Fatal("day-0 epoch key still on disk after the retention")
	}
	if hits := rtabDirHas(t, bob.dir, key0s); len(hits) != 0 {
		t.Fatalf("day-0 epoch key material still in %v", hits)
	}
	late := rtabSnapshot(t, bob)
	if rtabOpens(t, late, alice, id0, "rt-letter-day0") {
		t.Fatal("B-1 broken: a day-9 image opens the day-0 letter")
	}
	rtabResult(t, "B-1", "HELD", "after the retention the day-0 epoch key is gone from epochs/ and state.json; the letter's ciphertext doesn't open")

	// The letter was read but never marked done: its plaintext stays.
	w.later(25 * rtabDay)
	if _, err := bob.rotateEpochs(); err != nil {
		t.Fatal(err)
	}
	scan(t, bob)
	bob.pruneItems(true) // inbox --purge
	w.must(bob.save())
	hits := rtabDirHas(t, bob.dir, "rt-letter-day0")
	// FINDING B-2: read-but-not-done mail survives keep-days and inbox --purge.
	if len(hits) == 0 {
		t.Fatal("B-2 flipped: unhandled read mail is purged now")
	}
	rtabResult(t, "B-2", "BROKEN", "a letter read but never marked done stays in plaintext in state.json after 34 days and inbox --purge ("+strings.Join(hits, ",")+")")
	for _, it := range bob.st.Items {
		if it.Mail != nil && it.Mail.Text == "rt-letter-day0" {
			it.Done = true
		}
	}
	bob.pruneItems(false)
	w.must(bob.save())
	if len(rtabDirHas(t, bob.dir, "rt-letter-day0")) != 0 {
		t.Fatal("handled mail past keep-days stays")
	}
	rtabResult(t, "B-3", "HELD", "handled (done) mail past keep-days is dropped from state.json")
}

// B-4: a stolen current epoch key folder reads the mail to come, unless the owner re-keys.
func TestRedTeamB_FutureMailWithStolenKeys(t *testing.T) {
	w, alice, bob := rtabMailPair(t)
	scan(t, bob)
	snap := rtabSnapshot(t, bob) // T: device imaged

	// bob learns of it at once and re-keys as compromised.
	if _, err := bob.rekey(true); err != nil {
		t.Fatal(err)
	}
	opened, prev, got := 0, 0, 0
	count := func(items []*Item) {
		for _, it := range items {
			if it.Mail != nil && it.Mail.Text == "rt-future" {
				got++
			}
		}
	}
	for _, d := range []int{0, 1, 6, 13} {
		w.later(time.Duration(d-prev) * rtabDay)
		prev = d
		count(scan(t, bob)) // bob's device runs now and then: it publishes again before the horizon runs out
		id := rtabMail(t, alice, "bob", "rt-future")
		if rtabOpens(t, snap, alice, id, "rt-future") {
			opened++
		}
		// Test replacement-key confidentiality, not expired first delivery:
		// instructions first scanned seven days later are intentionally refused.
		count(scan(t, bob))
	}
	// Fixed (B-4): rekey --compromised replaces today's and every later key, and the view key.
	if opened != 0 {
		t.Fatalf("B-4 broken: the image opened %d of 4 later letters", opened)
	}
	count(scan(t, bob))
	if got != 4 {
		t.Fatalf("bob read %d of the 4 letters sent to his new keys", got)
	}
	rtabResult(t, "B-4", "HELD", "after rekey --compromised at T, the image opens none of the letters sent at T, T+1, T+6 and T+13 days; bob reads all four")

	if bob.ahead() != defaultKeysAhead || defaultKeysAhead != 7 {
		t.Fatalf("default horizon %d", bob.ahead())
	}
	old := privacyLevel
	privacyLevel = privacyStrict
	defer func() { privacyLevel = old }()
	if bob.retain() != strictRetain || bob.ahead() != strictKeysAhead {
		t.Fatalf("strict retention %d, ahead %d", bob.retain(), bob.ahead())
	}
	rtabResult(t, "B-4", "INFO", "keys are published 7 epochs ahead (strict mode: 3), renewed by any scan before they run out; without a rekey an image opens at most the next 6 (strict 2) days")
}

// B-5: strict retention really destroys keys after 2 epochs.
func TestRedTeamB_StrictRetention(t *testing.T) {
	w, alice, bob := rtabMailPair(t)
	old := privacyLevel
	privacyLevel = privacyStrict
	defer func() { privacyLevel = old }()
	privacyLevel = old // strict sends only through a relay; the retention is what's tested
	e0, _ := w.ch.epoch()
	id := rtabMail(t, alice, "bob", "rt-strict")
	privacyLevel = privacyStrict
	scan(t, bob)
	w.later(3 * rtabDay)
	scan(t, bob)
	if _, err := os.Stat(bob.epochPath(e0)); !os.IsNotExist(err) {
		t.Fatal("strict: day-0 key kept past 2 epochs")
	}
	if rtabOpens(t, rtabSnapshot(t, bob), alice, id, "rt-strict") {
		t.Fatal("strict: a day-3 image opens the day-0 letter")
	}
	rtabResult(t, "B-5", "HELD", "strict: the day-0 epoch key is gone by day 3")
}

// B-6: the static view key identifies a person's whole mail history (metadata).
func TestRedTeamB_StaticViewKeyHistory(t *testing.T) {
	w, alice, bob := rtabMailPair(t)
	id0 := rtabMail(t, alice, "bob", "rt-view-day0")
	scan(t, bob)
	w.later(20 * rtabDay)
	for _, c := range []*Client{alice, bob} {
		if _, err := c.rotateEpochs(); err != nil {
			t.Fatal(err)
		}
	}
	scan(t, bob)
	snap := rtabSnapshot(t, bob)
	si, err := loadIdent(snap)
	if err != nil {
		t.Fatal(err)
	}
	e := rtabEnv(t, bob, id0)
	if rtabOpens(t, snap, alice, id0, "rt-view-day0") {
		t.Fatal("content of a 20-day-old letter opens")
	}
	// FINDING B-6: view.key never rotates: a day-20 image still tags the day-0 envelope as bob's.
	if !e.mine(si.view) {
		t.Fatal("B-6 flipped: the current view key no longer recognises old envelopes")
	}
	rtabResult(t, "B-6", "BROKEN(metadata)", "the static view.key (also in every agent's grant bundle) recognises bob's envelopes from day 0 on: who-got-mail-when history, forever")
}
