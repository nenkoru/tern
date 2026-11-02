package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// friends: three owners in a private group (oscar created it and admitted
// the others), each with the group's keys, all using one data node.
func friends(t *testing.T, w *world) (oscar, nina, ada *Client, node *Relay) {
	t.Helper()
	oscar, _ = w.owner("oscar")
	nina, _ = w.owner("nina")
	ada, _ = w.owner("ada")
	w.must(oscar.createGroup("friends", false))
	for _, c := range []*Client{nina, ada} {
		if msg, err := c.joinGroup("friends"); err != nil || !strings.HasPrefix(msg, "requested") {
			t.Fatalf("join: %s %v", msg, err)
		}
	}
	if done, err := oscar.admitPending(); err != nil || len(done) != 2 {
		t.Fatalf("admit: %v %v", done, err)
	}
	for _, c := range []*Client{nina, ada} {
		scan(t, c)
		if c.st.Groups["friends"] == nil {
			t.Fatalf("%s got no keys", c.f.Name)
		}
	}
	node, srv := newNode(w)
	for _, c := range []*Client{oscar, nina, ada} {
		c.relay = srv.URL
	}
	return
}

func layers(t *testing.T, c *Client, pieces string) {
	t.Helper()
	cs, err := parseOffers([]byte(`{"schema":"tern.offer/1","kind":"gpu","title":"positions ` + pieces + `","whole":"tiny-32","holds":"` + pieces +
		`","of":32,"amount":{"value":8,"unit":"gb-vram"},"free_for":["friends"],"settle":["credits"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.offerSet(cs, false); err != nil {
		t.Fatal(err)
	}
}

// Members announce what they hold, sealed to the group; each reads who is
// online, which pieces of the model are held and which nobody holds. A
// member that leaves is seen leaving at once; the node never sees a name.
func TestPresence(t *testing.T) {
	w := newWorld(t)
	oscar, nina, ada, node := friends(t, w)
	layers(t, oscar, "0-11")
	layers(t, nina, "12-23")
	layers(t, ada, "20-31")
	for _, c := range []*Client{oscar, nina, ada} {
		_, err := c.announcePresence("friends", coopFlags{state: "online", ttl: 120, reach: "tcp://10.8.0." + c.f.Name + ":50052", load: 0.2})
		w.must(err)
	}
	online, left, dropped, err := nina.presence("friends")
	w.must(err)
	if len(online) != 3 || len(left) != 0 || len(dropped) != 0 || online[0].Name != "ada" || online[0].Reach != "tcp://10.8.0.ada:50052" {
		t.Fatalf("online %+v left %+v dropped %v", online, left, dropped)
	}
	cov := presenceCoverage(online, 2)["tiny-32"].(map[string]any)
	if cov["missing"] != "" || cov["least_copies"] != 1 || cov["thin"] != "0-19,24-31" {
		t.Fatalf("coverage: %v", cov)
	}

	// the node holds only sealed bytes under a topic: no names, no endpoints
	for _, t2 := range node.pres.topics {
		for _, e := range t2 {
			if bytes.Contains(e.Blob, []byte("oscar")) || bytes.Contains(e.Blob, []byte("10.8.0")) {
				t.Fatal("the node can read an announcement")
			}
		}
	}

	// ada's laptop closes cleanly: "leaving" replaces her entry
	_, err = ada.announcePresence("friends", coopFlags{state: "leaving"})
	w.must(err)
	online, left, _, err = oscar.presence("friends")
	w.must(err)
	if len(online) != 2 || len(left) != 1 || left[0].Name != "ada" {
		t.Fatalf("after ada left: online %+v left %+v", online, left)
	}
	if cov := presenceCoverage(online, 2)["tiny-32"].(map[string]any); cov["missing"] != "24-31" {
		t.Fatalf("pieces nobody holds once ada left: %v", cov)
	}
	// nina picks up ada's pieces
	layers(t, nina, "12-31")
	_, err = nina.announcePresence("friends", coopFlags{state: "online", ttl: 120})
	w.must(err)
	online, _, _, _ = oscar.presence("friends")
	if cov := presenceCoverage(online, 2)["tiny-32"].(map[string]any); cov["missing"] != "" {
		t.Fatalf("after nina re-held them: %v", cov)
	}

	// a member can't speak for another: an announcement signed by nina as oscar is dropped
	g := nina.st.Groups["friends"]
	p := Presence{Schema: tPresence, Group: "friends", Name: "oscar", At: time.Now().Unix(), TTL: 60, State: "online", Memo: "pay nina"}
	p.Sig, _ = nina.coopSign(tPresence, jsonOf(p))
	e, _ := w.ch.epoch()
	_, rec, _ := g.at(e)
	r, _ := parseRecipient(rec)
	var ct bytes.Buffer
	aw, _ := age.Encrypt(&ct, r)
	aw.Write(jsonOf(p))
	aw.Close()
	topic, _ := presenceTopic(g)
	w.must(node.pres.put(topic, common.HexToHash("0x01"), ct.Bytes(), 60, time.Now().Unix()))
	online, _, dropped, _ = ada.presence("friends")
	if len(dropped) != 1 || !strings.Contains(dropped[0], "signature") {
		t.Fatalf("a forged announcement: dropped %v", dropped)
	}
	for _, v := range online {
		if strings.Contains(v.Memo, "pay nina") {
			t.Fatal("the forgery was shown")
		}
	}

	// an outsider has no keys; a public group's presence never carries an endpoint
	mallory, _ := w.owner("mallory")
	mallory.relay = oscar.relay
	if _, _, _, err := mallory.presence("friends"); err == nil || !strings.Contains(err.Error(), "join it first") {
		t.Fatalf("outsider: %v", err)
	}
	w.must(mallory.createGroup("lan-party", true))
	if _, err := mallory.announcePresence("lan-party", coopFlags{state: "online", ttl: 60, reach: "tcp://1.2.3.4:27015"}); err == nil || !strings.Contains(err.Error(), "public group") {
		t.Fatalf("an endpoint in a public group: %v", err)
	}
	if _, err := mallory.announcePresence("lan-party", coopFlags{state: "online", ttl: 60, memo: "hosting tonight"}); err != nil {
		t.Fatal(err)
	}
}

// The node's side: TTL bounds, expiry, a cap per group, slots owned by
// whoever knows their key.
func TestPresenceStore(t *testing.T) {
	var p presenceStore
	topic := crypto.Keccak256Hash([]byte("t"))
	now := int64(1_000_000)
	slot := func(i int) common.Hash { return crypto.Keccak256Hash([]byte{byte(i), byte(i >> 8)}) }
	for i := 0; i < presencePerSlot; i++ {
		if err := p.put(topic, slot(i), []byte("x"), 30, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.put(topic, slot(presencePerSlot), []byte("x"), 30, now); err == nil {
		t.Fatal("over the cap")
	}
	if err := p.put(topic, slot(3), []byte("y"), 30, now); err != nil {
		t.Fatal("replacing one's own entry is always allowed")
	}
	if len(p.list(topic, now)) != presencePerSlot {
		t.Fatal("list")
	}
	if err := p.put(topic, slot(presencePerSlot), []byte("x"), 30, now+31); err != nil || len(p.list(topic, now+31)) != 1 {
		t.Fatalf("expired entries make room: %v", err)
	}
}
