package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sawText(items []*Item, text string) bool {
	for _, it := range items {
		if it.Mail != nil && it.Mail.Text == text && it.Status != "refused" {
			return true
		}
	}
	return false
}

// A private Sets group: a ban rotates the keys; the banned member reads nothing after.
func TestGroupRotationOnBan(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	eve, _ := w.owner("eve")
	must(t, nina.createGroup("club", false))
	if g := nina.st.Groups["club"]; len(g.Keys) != 2*groupRotateEvery {
		t.Fatalf("a private group made %d epochs of keys, want %d", len(g.Keys), 2*groupRotateEvery)
	}
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
	nina.postGroup("club", "before", nil)
	if !sawText(scan(t, eve), "before") {
		t.Fatal("eve can't read the group")
	}
	must(t, nina.moderate("club", "ban", "eve", "leaks"))
	if g := nina.st.Groups["club"]; g.Gen != 1 || len(g.Old) != 1 {
		t.Fatalf("no rotation on ban: gen %d, old %d", g.Gen, len(g.Old))
	}
	scan(t, ada)
	if g := ada.st.Groups["club"]; g.Gen != 1 {
		t.Fatalf("ada has generation %d", g.Gen)
	}
	scan(t, eve)
	if g := eve.st.Groups["club"]; g.Gen != 0 {
		t.Fatal("the banned member got the new keys")
	}
	nina.postGroup("club", "after", nil)
	if !sawText(scan(t, ada), "after") {
		t.Fatal("ada can't read the new generation")
	}
	if sawText(scan(t, eve), "after") {
		t.Fatal("the banned member reads the new generation")
	}
	// Posts of the old generation still open for ada (until the retention).
	if !sawText(ada.groupItems("club"), "before") {
		t.Fatal("ada lost the old generation's posts")
	}
	// Past the retention, the old generation's keys and view key are gone.
	w.later(time.Duration(2*groupRotateEvery+ada.f.Retain+1) * 24 * time.Hour)
	scan(t, ada)
	if g := ada.st.Groups["club"]; len(g.Old) != 0 {
		t.Fatalf("old generation kept past the retention: %d", len(g.Old))
	}
}

// The keeper rotates on schedule, and after a member left.
func TestGroupRotationScheduledAndOnLeave(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	eve, _ := w.owner("eve")
	must(t, nina.createGroup("club", false))
	ada.joinGroup("club")
	eve.joinGroup("club")
	nina.admitPending()
	scan(t, ada)
	scan(t, eve)
	if got, err := nina.keepGroups(); err != nil || len(got) != 0 {
		t.Fatalf("a rotation before one is due: %v %v", got, err)
	}
	if _, err := eve.leaveGroup("club"); err != nil {
		t.Fatal(err)
	}
	if eve.st.Groups["club"] != nil || len(eve.groupItems("club")) != 0 {
		t.Fatal("leaving kept the keys or the posts")
	}
	got, err := nina.keepGroups()
	if err != nil || len(got) != 1 || !strings.Contains(got[0], "left") {
		t.Fatalf("no rotation after a leave: %v %v", got, err)
	}
	w.later(time.Duration(groupRotateEvery) * 24 * time.Hour)
	must(t, ada.publishKeys()) // scheduled rotation is independent of a member's offline key horizon
	got, err = nina.keepGroups()
	if err != nil || len(got) != 1 || !strings.Contains(got[0], "scheduled") {
		t.Fatalf("no scheduled rotation: %v %v", got, err)
	}
	scan(t, ada)
	if ada.st.Groups["club"].Gen != 2 {
		t.Fatalf("ada at generation %d, want 2", ada.st.Groups["club"].Gen)
	}
}

// An anonymous group: members ask for the new generation with a proof; a
// banned pseudonym and a removed leaf get nothing.
func TestAnonymousGroupRekey(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	eve, _ := w.owner("eve")
	oscar := anonCell(t, w, "cell", false, 0, nina, ada, eve)
	nina.postGroup("cell", "hello", nil)
	var pseud string
	for _, it := range anonPosts(t, oscar, "cell") {
		if it.Mail != nil && it.Mail.Text == "hello" {
			pseud = it.From
		}
	}
	must(t, oscar.moderate("cell", "ban", pseud, "leaks"))   // rotates
	must(t, oscar.moderate("cell", "remove", "eve", "left")) // rotates again
	if g := oscar.st.Groups["cell"]; g.Gen != 2 {
		t.Fatalf("admin at generation %d, want 2", g.Gen)
	}
	// Members read the announcements and ask; the admin answers; members install.
	for round := 0; round < 3; round++ {
		for _, c := range []*Client{ada, nina, eve} {
			scan(t, c)
		}
		scan(t, oscar)
	}
	if g := ada.st.Groups["cell"]; g.Gen != 2 {
		for _, it := range ada.groupItems("cell") {
			t.Logf("ada item %s %s %s %s", it.Type, it.Status, it.Verified, it.Trust)
		}
		for _, it := range oscar.groupItems("cell") {
			t.Logf("oscar item %s %s %s %s", it.Type, it.Status, it.Verified, it.Trust)
		}
		t.Logf("asks %+v queue %+v done %v", ada.st.RekeyAsks, oscar.st.RekeyQueue, oscar.st.RekeyDone)
		t.Fatalf("ada at generation %d, want 2", g.Gen)
	}
	if g := nina.st.Groups["cell"]; g.Gen != 0 {
		t.Fatalf("the banned pseudonym got generation %d", g.Gen)
	}
	if g := eve.st.Groups["cell"]; g.Gen != 0 {
		t.Fatalf("the removed member got generation %d", g.Gen)
	}
	if len(ada.st.RekeyAsks) != 0 {
		t.Fatal("the one-time key outlived its answer")
	}
	ada.postGroup("cell", "new keys", nil)
	if !sawText(anonPosts(t, oscar, "cell"), "new keys") {
		t.Fatal("the admin can't read the new generation")
	}
	for _, c := range []*Client{nina, eve} {
		if sawText(anonPosts(t, c, "cell"), "new keys") {
			t.Fatalf("%s reads the new generation", c.f.Name)
		}
	}
}

// Every scan destroys expired epoch keys (not only 'keys'), and read mail
// past its keep is dropped.
func TestRetentionByDefault(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	nina.setContact("oscar", true)
	oscar.send(SendOpts{To: "nina", Intent: "inform", Text: "old news"})
	got := scan(t, nina)
	if len(got) != 1 {
		t.Fatalf("mail: %d", len(got))
	}
	got[0].Done = true
	e0, _ := w.ch.epoch()
	w.later(time.Duration(nina.f.Retain+2) * 24 * time.Hour)
	scan(t, nina)
	if _, err := os.Stat(filepath.Join(nina.dir, "epochs", strconv.FormatUint(e0, 10)+".key")); !os.IsNotExist(err) {
		t.Fatalf("epoch %d's key outlived the retention: %v", e0, err)
	}
	if nina.item(got[0].ID) == nil {
		t.Fatal("handled mail dropped before its keep")
	}
	w.later(time.Duration(defaultKeepDays) * 24 * time.Hour)
	scan(t, nina)
	if nina.item(got[0].ID) != nil {
		t.Fatal("handled mail kept past its keep")
	}
	// inbox --purge: at once.
	must(t, nina.publishKeys())
	oscar.send(SendOpts{To: "nina", Intent: "inform", Text: "fresh"})
	got = scan(t, nina)
	got[0].Done = true
	if n := nina.pruneItems(true); n != 1 || nina.item(got[0].ID) != nil {
		t.Fatalf("purge dropped %d", n)
	}
	must(t, nina.setRetention(2, 7))
	if nina.f.Retain != 2 || nina.keepDays() != 7 {
		t.Fatal("retention not set")
	}
}
