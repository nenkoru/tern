package main

import (
	"strings"
	"testing"

	"github.com/nenkoru/tern/internal/artifact"
)

// A crowdfunded commission: fans pledge all or nothing; the goal is met and
// collected; the studio seals the build (an encrypted artifact: its hash, a
// torrent magnet and the key's commitment on chain) and delivers the key to
// every backer, tied to their pledge. Backers open it; nobody else gets it.
func TestCrowdfundedCommission(t *testing.T) {
	w := newWorld(t)
	studio, _ := w.owner("studio")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	for _, c := range []*Client{bob, carol, dave} {
		w.must(c.deposit(coins(3)))
	}
	h, _ := w.ch.b.HeaderByNumber(ctxb(), nil)
	id, err := studio.fundCreate("the game from our script", coins(2), h.Time+3*86400, "", "")
	w.must(err)
	lb, err := bob.fundPledge(id, coins(1))
	w.must(err)
	ct, hash, key, err := artifact.SealFile([]byte("the game build"))
	w.must(err)
	art, err := studio.registerArtifact(hash, uint64(len(ct)), "magnet:?xt=urn:btmh:1220aa tern:merkle-sha256:4194304:0xbb", "for backers", coins(0), key)
	w.must(err)
	if _, err := studio.fundDeliver(id, art, key); err == nil || !strings.Contains(err.Error(), "hasn't met its goal") {
		t.Fatalf("a key before the goal: %v", err)
	}
	lc, err := carol.fundPledge(id, coins(1))
	w.must(err)
	_, err = studio.fundCollect(id)
	w.must(err)

	if _, err := studio.fundDeliver(id, art, []byte("a wrong key, 32 bytes long......")); err == nil || !strings.Contains(err.Error(), "commitment") {
		t.Fatalf("a key that doesn't match: %v", err)
	}
	if _, err := dave.fundDeliver(id, art, key); err == nil || !strings.Contains(err.Error(), "only campaign") {
		t.Fatalf("someone else delivering: %v", err)
	}
	res, err := studio.fundDeliver(id, art, key)
	w.must(err)
	if got := strings.Join(res["delivered_to"].([]string), ","); got != "bob,carol" {
		t.Fatalf("delivered to %s", got)
	}
	for c, lock := range map[*Client]uint64{bob: lb, carol: lc} {
		_, plain, err := c.receiveKey(lock, ct)
		if err != nil || string(plain) != "the game build" {
			t.Fatalf("%s opens the build: %q %v", c.f.Name, plain, err)
		}
		for _, it := range c.st.Items {
			if it.Type == tKey && it.Status != "delivered" {
				t.Fatalf("%s's key arrived %s", c.f.Name, it.Status)
			}
		}
	}
	if _, _, err := dave.receiveKey(lb, ct); err == nil {
		t.Fatal("a non-backer got the key")
	}
}
