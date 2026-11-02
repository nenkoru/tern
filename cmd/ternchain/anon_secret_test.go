package main

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/zk"
)

// anonStored: what c's anon.json holds for group gid (nil: nothing).
func anonStored(t *testing.T, c *Client, gid common.Hash) *AnonSecret {
	t.Helper()
	var e *AnonSecret
	if err := c.anonSecrets(func(m map[string]*AnonSecret) bool {
		if x := m[anonKey(gid)]; x != nil {
			cp := *x
			e = &cp
		}
		return false
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// anonLeafEver: is commitment cm in any Leaves event (any group, ever)?
func anonLeafEver(t *testing.T, c *Client, cm *big.Int) bool {
	t.Helper()
	es, err := c.ch.events("AnonGroups", "Leaves")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		ls, _ := e.Args["leaves"].([]*big.Int)
		for _, l := range ls {
			if l.Cmp(cm) == 0 {
				return true
			}
		}
	}
	return false
}

func anonPseudonymOf(t *testing.T, c *Client, group, text string) string {
	t.Helper()
	for _, it := range anonPosts(t, c, group) {
		if it.Mail != nil && it.Mail.Text == text && it.Status == "delivered" {
			return it.From
		}
	}
	t.Fatalf("%s didn't read %q in %s", c.f.Name, text, group)
	return ""
}

// A-7, A-8: a join's identity is random and stored apart from eth.key (and
// from state.json); leaving destroys it.
func TestAnonSecretRandomStoredAndDestroyed(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	oscar := anonCell(t, w, "cell", false, 0, nina, ada)
	gid := ada.st.Groups["cell"].Anon.gid()
	for _, c := range []*Client{ada, oscar} { // a member's join, the admin's creation
		e := anonStored(t, c, gid)
		if e == nil || e.Legacy || e.Next != "" {
			t.Fatalf("%s's stored identity: %+v", c.f.Name, e)
		}
		s := hexBig(e.Secret)
		if s == nil || s.Cmp(legacyAnonSecret(c.key, gid)) == 0 {
			t.Fatalf("%s's identity is the eth.key derivation", c.f.Name)
		}
		if !anonLeafEver(t, c, zk.IdentityCommitment(s)) || anonLeafEver(t, c, zk.IdentityCommitment(legacyAnonSecret(c.key, gid))) {
			t.Fatalf("%s: the stored identity isn't the leaf, or the eth.key one is", c.f.Name)
		}
		must(t, c.save())
		st, _ := os.ReadFile(c.statePath())
		if strings.Contains(string(st), e.Secret) {
			t.Fatalf("%s's state.json holds the secret (a --state-dir copies it)", c.f.Name)
		}
		if fi, err := os.Stat(filepath.Join(c.dir, anonSecretsFile)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s's anon.json: %v %v", c.f.Name, fi, err)
		}
	}
	if anonStored(t, nina, gid).Secret == anonStored(t, ada, gid).Secret {
		t.Fatal("two members, one secret")
	}
	ada.postGroup("cell", "from ada", nil)
	p := anonPseudonymOf(t, nina, "cell", "from ada")
	if p == pseudonym(zk.HashN(anonMemberScope(gid), legacyAnonSecret(ada.key, gid))) {
		t.Fatal("the pseudonym is recomputable from eth.key")
	}
	// Leaving destroys it: nothing on the device finds the leaf any more.
	if _, err := ada.leaveGroup("cell"); err != nil {
		t.Fatal(err)
	}
	if e := anonStored(t, ada, gid); e != nil {
		t.Fatalf("the secret outlived the leave: %+v", e)
	}
	if _, err := os.Stat(filepath.Join(ada.dir, anonSecretsFile)); !os.IsNotExist(err) {
		t.Fatalf("anon.json left behind empty: %v", err)
	}
}

// The CLI and a long-running MCP server of one identity: the server's saves
// neither lose a secret the CLI made nor revive one the CLI destroyed.
func TestAnonSecretTwoProcesses(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	server, err := openClient(ada.dir, w.ch) // opened before the join, never reloaded
	if err != nil {
		t.Fatal(err)
	}
	server.anonDirect = true // this test exercises identity storage, not relay transport
	anonCell(t, w, "cell", false, 0, nina, ada)
	gid := ada.st.Groups["cell"].Anon.gid()
	must(t, server.save())
	scan(t, server) // the server reads the keys too
	if anonStored(t, ada, gid) == nil {
		t.Fatal("the server's save lost the join's secret")
	}
	if _, err := server.postGroup("cell", "from the server", nil); err != nil {
		t.Fatalf("the server can't post with the CLI's identity: %v", err)
	}
	if _, err := ada.leaveGroup("cell"); err != nil {
		t.Fatal(err)
	}
	must(t, server.save())
	if e := anonStored(t, ada, gid); e != nil {
		t.Fatalf("the server's save revived the destroyed secret: %+v", e)
	}
	// The server still holds the group's keys in memory, but not the identity.
	if _, err := server.postGroup("cell", "after the leave", nil); err == nil || !strings.Contains(err.Error(), "not a leaf") {
		t.Fatalf("posted after the leave: %v", err)
	}
}

// legacyJoin: a join as clients made it before random identities: the
// eth.key derivation, nothing stored.
func legacyJoin(t *testing.T, admin, m *Client, name string) {
	t.Helper()
	if _, err := admin.inviteAnon(name, m.f.Name); err != nil {
		t.Fatal(err)
	}
	scan(t, m)
	inv := m.st.AnonInvites[name]
	gid := common.HexToHash(inv.Gid)
	cm := zk.IdentityCommitment(legacyAnonSecret(m.key, gid))
	if _, err := m.sendDatagram(tAnonJoin, inv.Admin, AnonJoin{Gid: inv.Gid, Commitment: cm.String()}); err != nil {
		t.Fatal(err)
	}
	scan(t, admin)
	if done, err := admin.admitPending(); err != nil || len(done) != 1 {
		t.Fatalf("admit: %v %v", done, err)
	}
	scan(t, m)
}

// A group joined with the eth.key identity keeps working (stored, marked
// legacy) and group anon-migrate moves it to a random one.
func TestAnonSecretLegacyMigration(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	ada.anonDirect = true // legacyJoin bypasses anonCell's member setup
	oscar := anonCell(t, w, "cell", false, 0, nina)
	legacyJoin(t, oscar, ada, "cell")
	gid := ada.st.Groups["cell"].Anon.gid()
	legacy := legacyAnonSecret(ada.key, gid)
	if anonStored(t, ada, gid) != nil {
		t.Fatal("a legacy join stored something")
	}
	if _, err := ada.postGroup("cell", "old identity", nil); err != nil {
		t.Fatal(err)
	}
	if e := anonStored(t, ada, gid); e == nil || !e.Legacy || hexBig(e.Secret).Cmp(legacy) != 0 {
		t.Fatalf("legacy identity not stored as such: %+v", e)
	}
	oldP := anonPseudonymOf(t, nina, "cell", "old identity")
	if oldP != pseudonym(zk.HashN(anonMemberScope(gid), legacy)) {
		t.Fatal("the legacy post isn't the legacy pseudonym")
	}
	v, err := ada.anonGroupView(ada.st.Groups["cell"])
	if err != nil || v["identity_warning"] == nil {
		t.Fatalf("no warning for a legacy identity: %v %v", v, err)
	}

	r, err := ada.migrateAnon("cell")
	if err != nil || !strings.HasPrefix(r["status"].(string), "requested") || !strings.Contains(r["warning"].(string), "can't unlink") {
		t.Fatalf("migrate: %v %v", r, err)
	}
	if _, err := ada.postGroup("cell", "still the old one", nil); err != nil { // until the admin swaps the leaf
		t.Fatal(err)
	}
	scan(t, oscar)
	if done, err := oscar.admitPending(); err != nil || len(done) != 1 {
		t.Fatalf("admit: %v %v", done, err)
	}
	s, err := oscar.anonTree(gid)
	if err != nil || s.members() != 3 {
		t.Fatalf("after the swap: %d members, %v", s.members(), err)
	}
	if _, err := ada.postGroup("cell", "new identity", nil); err != nil {
		t.Fatal(err)
	}
	e := anonStored(t, ada, gid)
	if e == nil || e.Legacy || e.Next != "" || hexBig(e.Secret).Cmp(legacy) == 0 {
		t.Fatalf("after the swap: %+v", e)
	}
	newP := anonPseudonymOf(t, nina, "cell", "new identity")
	if newP == oldP {
		t.Fatal("the pseudonym didn't change")
	}
	// The old leaf is zeroed in the tree, but its Leaves event stays: history
	// made with the legacy identity remains linkable to eth.key.
	for _, l := range s.leaves {
		if l.Cmp(zk.IdentityCommitment(legacy)) == 0 {
			t.Fatal("the old leaf is still in the tree")
		}
	}
	if !anonLeafEver(t, ada, zk.IdentityCommitment(legacy)) {
		t.Fatal("expected the legacy leaf to stay in the public history")
	}
	if r, err := ada.migrateAnon("cell"); err != nil || !strings.Contains(r["status"].(string), "already") {
		t.Fatalf("migrating twice: %v %v", r, err)
	}
	// The removal rotates the keys; the migrated member gets the new generation.
	if _, err := oscar.keepGroups(); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		scan(t, ada)
		scan(t, nina)
		scan(t, oscar)
	}
	if g := ada.st.Groups["cell"]; g.Gen != oscar.st.Groups["cell"].Gen || g.Gen == 0 {
		t.Fatalf("ada at generation %d, the admin at %d", g.Gen, oscar.st.Groups["cell"].Gen)
	}

	// The admin's own leaf swaps at once (marked legacy here by hand).
	must(t, oscar.anonSecrets(func(m map[string]*AnonSecret) bool { m[anonKey(gid)].Legacy = true; return true }))
	if r, err := oscar.migrateAnon("cell"); err != nil || !strings.HasPrefix(r["status"].(string), "migrated") {
		t.Fatalf("the admin's migration: %v %v", r, err)
	}
	if _, err := oscar.postGroup("cell", "admin, new identity", nil); err != nil {
		t.Fatal(err)
	}
	if e := anonStored(t, oscar, gid); e == nil || e.Legacy || e.Next != "" {
		t.Fatalf("the admin after its swap: %+v", e)
	}

	// With a banned pseudonym in the group, a swap would shed the ban: refused.
	must(t, oscar.moderate("cell", "ban", newP, "test"))
	if why := oscar.anonSwapRefused(oscar.st.Groups["cell"]); !strings.Contains(why, "banned") {
		t.Fatalf("a swap with bans in place: %q", why)
	}
}
