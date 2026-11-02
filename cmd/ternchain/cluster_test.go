package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenkoru/tern/internal/stubengine"
)

// A cluster of friends (the coop demo, scripts/demo-cluster.sh, in one
// process): three people's agents in a private group pool their machines to
// run one model split into 32 pieces. The engine (internal/stubengine)
// does the compute; tern carries the membership, who is online with which
// pieces, the receipts, the credits and the spot checks. One laptop
// closes and another picks up its pieces; one node cheats and is outvoted.
func TestClusterOfFriends(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	w.must(oscar.createGroup("friends", false))
	boxes := map[string]*Client{}
	for _, o := range []*Client{oscar, nina, ada} {
		b := w.agent(o, "box", coins(1), `{}`)
		if msg, err := b.joinGroup("friends"); err != nil || !strings.HasPrefix(msg, "requested") {
			t.Fatalf("%s joins: %s %v", b.f.Name, msg, err)
		}
		boxes[o.f.Name] = b
	}
	if done, err := oscar.admitPending(); err != nil || len(done) != 3 {
		t.Fatalf("admit: %v %v", done, err)
	}
	node, srv := newNode(w)
	engines := map[string]*stubengine.Node{}
	for name, pieces := range map[string]string{"oscar": "0-10", "nina": "11-21", "ada": "22-31"} {
		b := boxes[name]
		scan(t, b)
		if b.st.Groups["friends"] == nil {
			t.Fatalf("%s has no keys", b.f.Name)
		}
		b.relay = srv.URL
		lazy := 0
		if name == "nina" {
			lazy = 2 // every second job: claims the work, does none
		}
		e, err := stubengine.NewNode(pieces, 32, lazy)
		w.must(err)
		es := httptest.NewServer(e.Handler())
		t.Cleanup(es.Close)
		engines[name] = e
		hold(t, b, pieces)
		_, err = b.announcePresence("friends", coopFlags{state: "online", ttl: 120, reach: es.URL})
		w.must(err)
	}
	bo, bn, ba := boxes["oscar"], boxes["nina"], boxes["ada"]

	online := func(by *Client) []stubengine.Member {
		t.Helper()
		on, _, dropped, err := by.presence("friends")
		w.must(err)
		if len(dropped) > 0 {
			t.Fatalf("dropped: %v", dropped)
		}
		var ms []stubengine.Member
		w.must(json.Unmarshal(jsonOf(on), &ms))
		return ms
	}
	// everyone gets their own chat; each provider's work becomes a receipt
	// the consumer countersigns from its own count, posted for credits
	chat := func(consumer *Client, job string, avoid ...string) stubengine.Chat {
		t.Helper()
		av := map[string]bool{}
		for _, a := range avoid {
			av[a] = true
		}
		ch, err := stubengine.Run(online(consumer), "stub-32", job, "hello from "+consumer.f.Name, 8, av)
		w.must(err)
		for _, seg := range ch.Route {
			p := boxes[ownerName(seg.Name)]
			if ownerName(seg.Name) == ownerName(consumer.f.Name) {
				continue
			}
			u, err := p.receiptIssue(Usage{Consumer: consumer.f.Name, Count: uint64(seg.Units), Unit: "layer-token", Group: "friends", Resource: fmt.Sprintf("stub-32 %d-%d", seg.From, seg.To)})
			w.must(err)
			u, err = consumer.receiptAccept(u, uint64(seg.Units))
			w.must(err)
			_, err = p.creditsPostUsage("friends", u)
			w.must(err)
		}
		return ch
	}
	c1 := chat(bo, "chat-1")
	if len(c1.Route) != 3 || c1.Route[1].Name != "box.nina" || c1.Route[1].Units != 11*8 {
		t.Fatalf("route: %+v", c1.Route)
	}
	c2 := chat(ba, "chat-2") // nina's node skips this one

	// ada spot-checks nina's part of her chat: oscar's and her own machine
	// load those pieces and run the same pass again
	seg := c2.Route[1]
	pass := seg.Passes[0]
	job := fmt.Sprintf("chat-2:%d-%d:t0", seg.From, seg.To)
	statements := []WorkStatement{}
	sn, err := bn.workStatement(WorkStatement{Job: job, Input: pass.Input, Output: pass.Output, Canon: "stub/sha256"})
	w.must(err)
	statements = append(statements, sn)
	in, _ := stubengine.ParseState(pass.Input)
	for _, name := range []string{"oscar", "ada"} {
		w.must(engines[name].Load(fmt.Sprintf("%d-%d", seg.From, seg.To)))
		var reach string
		for _, m := range online(ba) {
			if m.Name == "box."+name {
				reach = m.Reach
			}
		}
		p, err := stubengine.Forward(reach, "check-"+job, in, seg.From, seg.To)
		w.must(err)
		s, err := boxes[name].workStatement(WorkStatement{Job: job, Input: p.Input, Output: p.Output, Canon: "stub/sha256"})
		w.must(err)
		statements = append(statements, s)
	}
	r, err := ba.compareWork(statements, "")
	w.must(err)
	if r.Verdict != "mismatch" || len(r.Outvoted) != 1 || r.Outvoted[0] != "box.nina" {
		t.Fatalf("the check: %+v", r)
	}
	_, err = ba.checkReport(r, "friends", false)
	w.must(err)

	// ada's laptop closes: nobody holds 22-31 until oscar's machine picks them up
	_, err = ba.announcePresence("friends", coopFlags{state: "leaving"})
	w.must(err)
	on, left, _, err := bo.presence("friends")
	w.must(err)
	if cov := presenceCoverage(on, 2)["stub-32"].(map[string]any); len(left) != 1 || cov["missing"] != "22-31" {
		t.Fatalf("after ada left: %v, left %v", cov, left)
	}
	if _, err := stubengine.Run(online(bo), "stub-32", "chat-x", "x", 1, nil); err == nil || !strings.Contains(err.Error(), "nobody online holds") {
		t.Fatalf("a chat with pieces missing: %v", err)
	}
	w.must(engines["oscar"].Load("22-31"))
	hold(t, bo, "0-31")
	_, err = bo.announcePresence("friends", coopFlags{state: "online", ttl: 120, reach: online(bo)[1].Reach})
	w.must(err)
	// and the group routes around the node that was outvoted
	c3 := chat(bn, "chat-3", "box.nina")
	if len(c3.Route) != 1 || c3.Route[0].Name != "box.oscar" || c3.Output != c1Output(t, "hello from box.nina", 8) {
		t.Fatalf("chat 3: %+v", c3.Route)
	}

	// credits: oscar's admin policy prices a layer-token at 0.1
	_, err = oscar.creditsSetPolicy("friends", CreditsPolicy{Rates: map[string]float64{"layer-token": 0.1}, Floor: -5})
	w.must(err)
	w.must(node.flush())
	scan(t, bn)
	l, err := bn.ledger("friends")
	w.must(err)
	// chat 1: nina 88 and ada 80 for oscar; chat 2: oscar 88 and nina 88 for ada; chat 3: oscar 256 for nina
	want := map[string]float64{"oscar": (-88 - 80 + 88 + 256) / 10.0, "nina": (88 + 88 - 256) / 10.0, "ada": (80 - 88 - 88) / 10.0}
	for n, v := range want {
		if d := l.Balances[n] - v; d > 1e-9 || d < -1e-9 {
			t.Fatalf("balances %v, want %v (ignored %v)", l.Balances, want, l.Ignored)
		}
	}
	q := l.priority()
	if q[0].Name != "oscar" || q[0].Waits || q[1].Name != "nina" || !q[1].Waits || q[2].Name != "ada" {
		t.Fatalf("queue: %+v", q)
	}
	scan(t, bo)
	sc, err := bo.checkScore("box.nina", "friends")
	w.must(err)
	if sc.Failed != 1 {
		t.Fatalf("nina's spot checks: %+v", sc)
	}
}

// hold: what a member's machine holds now (its capacity offer).
func hold(t *testing.T, c *Client, pieces string) {
	t.Helper()
	cs, err := parseOffers([]byte(`{"schema":"tern.offer/1","kind":"gpu","title":"stub-32 positions ` + pieces + `","whole":"stub-32","holds":"` +
		pieces + `","of":32,"amount":{"value":8,"unit":"gb-vram"},"free_for":["friends"],"settle":["credits"],"protocol":"stubengine/1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.offerSet(cs, false); err != nil {
		t.Fatal(err)
	}
}

// c1Output: what an honest pipeline gives for a prompt.
func c1Output(t *testing.T, prompt string, tokens int) string {
	n, _ := stubengine.NewNode("0-31", 32, 0)
	s := httptest.NewServer(n.Handler())
	defer s.Close()
	ch, err := stubengine.Run([]stubengine.Member{{Name: "x", Reach: s.URL, Offers: []stubengine.Offer{{Whole: "stub-32", Holds: "0-31", Of: 32}}}}, "stub-32", "j", prompt, tokens, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ch.Output
}
