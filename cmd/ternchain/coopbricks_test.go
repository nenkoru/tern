package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// Bricks are versioned through the same path as contracts: a vote of both
// houses lists brick/NAME in the Directory; a newer version than the client
// speaks comes as an update, and a brick the client doesn't know is listed
// with its spec.
func TestBricksInDirectory(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	testChain = w.ch
	defer func() { testChain = nil }()
	os.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	audit := []string{"--audit", "ipfs://bafy-review", "--audit-hash", crypto.Keccak256Hash([]byte("review")).Hex()}
	gov := func(who string, args ...string) {
		t.Helper()
		if err := run(append(append([]string{"gov"}, args...), "--dir", f[who].dir, "--relay", w.relay)); err != nil {
			t.Fatalf("gov %v: %v", args, err)
		}
	}
	pass := func(ids ...int) {
		t.Helper()
		w.later(2 * time.Second)
		for _, id := range ids {
			gov("ana", "vote", fmt.Sprint(id), "yes")
			gov("ben", "vote", fmt.Sprint(id), "yes", "--house", "citizens")
			gov("cai", "vote", fmt.Sprint(id), "yes", "--house", "citizens")
			gov("dov", "vote", fmt.Sprint(id), "yes", "--house", "citizens")
		}
		w.later(11 * time.Minute)
		for _, id := range ids {
			gov("ana", "queue", fmt.Sprint(id))
		}
		w.later(11 * time.Minute)
		for _, id := range ids {
			gov("ben", "execute", fmt.Sprint(id))
		}
	}
	gov("ana", "lock", "30") // coins for the coin house
	c := f["ana"]
	mark := func(schema string) string {
		t.Helper()
		a, err := c.brickMark(schema)
		w.must(err)
		if !c.markerSays(a, schema) || c.markerSays(a, schema+"x") {
			t.Fatalf("the marker of %s", schema)
		}
		return a.Hex()
	}
	l, err := c.bricksList()
	w.must(err)
	if s := fmt.Sprint(l["speaks"]); !strings.Contains(s, "not listed") || l["unknown"] != nil {
		t.Fatalf("before any vote: %v", l)
	}
	gov("ana", "propose", "upgrade", "brick/offer", mark(tOffer), "--spec", writeDoc(t, "offer.md", "tern.offer/1"), audit[0], audit[1], audit[2], audit[3])
	gov("ana", "propose", "upgrade", "brick/carpool", mark("tern.carpool/1"), "--spec", writeDoc(t, "carpool.md", "tern.carpool/1: seats and routes"), audit[0], audit[1], audit[2], audit[3])
	pass(0, 1)
	l, err = c.bricksList()
	w.must(err)
	offer := l["speaks"].([]map[string]any)[0]
	if offer["directory"] != "current" || len(l["unknown"].([]map[string]any)) != 1 || l["unknown"].([]map[string]any)[0]["key"] != "brick/carpool" {
		t.Fatalf("listed: %v", l)
	}
	if ups, _ := c.govUpdates(false); len(ups) != 0 {
		t.Fatalf("nothing to update yet: %v", ups)
	}

	gov("ana", "propose", "upgrade", "brick/offer", mark("tern.offer/2"), "--spec", writeDoc(t, "offer2.md", "tern.offer/2"), "--notes", "adds seats", audit[0], audit[1], audit[2], audit[3])
	pass(2)
	ups, err := c.govUpdates(false)
	w.must(err)
	if len(ups) != 1 || ups[0]["key"] != "brick/offer" || !strings.Contains(fmt.Sprint(ups[0]["update"]), "a ternchain that speaks") {
		t.Fatalf("a newer brick version: %v", ups)
	}
	l, _ = c.bricksList()
	if d := fmt.Sprint(l["speaks"].([]map[string]any)[0]["directory"]); !strings.Contains(d, "another version") {
		t.Fatalf("speaks: %s", d)
	}
}

// Anyone publishes a brick under their own name, with no vote; readers see
// it unless they pin other publishers, and opt in by running what speaks it.
func TestBricksPublished(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	if _, err := oscar.brickPublish("rota", "tern.offer/1x", "ipfs://spec", ""); err == nil {
		t.Fatal("a schema id without a version")
	}
	_, err := oscar.brickPublish("rota", "oscar.rota/1", "ipfs://bafy-rota-spec", "who hosts the guild's server on which night")
	w.must(err)
	l, err := nina.bricksList()
	w.must(err)
	pub := l["published"].([]map[string]any)
	if len(pub) != 1 || pub[0]["brick"] != "rota" || pub[0]["publisher"] != "oscar" || pub[0]["schema"] != "oscar.rota/1" ||
		!strings.Contains(fmt.Sprint(pub[0]["what"]), "data not instructions") {
		t.Fatalf("published: %v", pub)
	}
	if !strings.Contains(fmt.Sprint(l["speaks"]), "listing is optional") {
		t.Fatalf("speaks: %v", l["speaks"])
	}
	w.must(writeJSON(filepath.Join(nina.dir, "types.json"), map[string]any{"publishers": []string{"ada"}}))
	extOnce = sync.Once{}
	l, err = nina.bricksList()
	w.must(err)
	if len(l["published"].([]map[string]any)) != 0 {
		t.Fatalf("pinned to another publisher, still shown: %v", l["published"])
	}
}
