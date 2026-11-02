package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	netscan "github.com/nenkoru/tern/internal/scan"
)

// The scanner (internal/scan, examples/scanner) reads a network's names,
// cards, payments, locks, swaps, claims and mail volume from its events alone,
// and a second run picks up only what is new.
func TestScanNetwork(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	ai := w.agent(alice, "ai", coins(3), `{"free_pay":"0.3"}`)
	w.must(alice.deposit(coins(10)))
	w.must(bob.deposit(coins(10)))
	w.must(alice.setCard(`{"offer":"proofreading"}`))
	w.must(alice.pay("bob", coins(1.5), "thanks"))

	// An escrow ruled 70/30, and one left open.
	open := func(args ...string) uint64 {
		m := lockOut(t, func() error { return runLockOpen(alice, args, lockFlags{prim: primFlags{days: 30}}) })
		return uint64(m["lock"].(float64))
	}
	esc := open("escrow", "bob", "carol", "1", "translate", "the", "site")
	lockOut(t, func() error { return runLockDecide(carol, []string{fmt.Sprint(esc), "7000", "partly", "done"}) })
	lockOut(t, func() error { return runLockSettle(bob, []string{fmt.Sprint(esc)}) })
	pending := open("escrow", "bob", "carol", "0.5", "logo")

	w.must(alice.attest("bob", "review", "on time", 0))
	if _, err := ai.send(SendOpts{To: "alice", Intent: "inform", Text: "hello from your agent"}); err != nil {
		t.Fatal(err)
	}

	net := netscan.Net{Network: "test", ChainID: w.ch.chainID.Int64(), Contracts: w.ch.dep.Contracts, Block: w.ch.dep.Block, Schema: w.ch.dep.Schema}
	if net.Schema == "" {
		net.Schema = alice.schema().Hex()
	}
	cache := filepath.Join(t.TempDir(), "scan.json")
	ctx := context.Background()
	s := netscan.Load(cache, net)
	update := func() {
		t.Helper()
		if err := s.Update(ctx, w.sim.Client(), net, netscan.Options{Save: func(s *netscan.State) error { return s.Save(cache) }}); err != nil {
			t.Fatal(err)
		}
	}
	update()

	if n := s.Names["alice"]; n == nil || n.Kind != "owner" || n.Card != `{"offer":"proofreading"}` || len(n.Agents) != 1 || n.Agents[0] != "ai.alice" {
		t.Fatalf("alice: %+v", n)
	}
	if a := s.Names["ai.alice"]; a == nil || a.Kind != "agent" || a.Owner != "alice" || a.Limits[netscan.Coin] != coins(3).String() {
		t.Fatalf("ai.alice: %+v", a)
	}
	var paid bool
	for _, p := range s.Payments {
		if p.From == "alice" && p.To == "bob" && p.Amount == "1.5" && p.Token == netscan.Coin {
			paid = true
		}
	}
	if !paid {
		t.Fatalf("alice's payment to bob is missing: %+v", s.Payments)
	}
	l := lockByID(s, esc)
	if l == nil || l.Kind != "escrow" || l.Decider != "carol" || l.From != "alice" || l.Payee != "bob" || l.Status != "split" ||
		l.Paid != "0.7" || l.Refunded != "0.3" {
		t.Fatalf("the ruled escrow: %+v", l)
	}
	if l := lockByID(s, pending); l == nil || l.Status != "open" || l.Total != "0.5" {
		t.Fatalf("the open escrow: %+v", l)
	}
	var reviewed bool
	for _, c := range s.Claims {
		if c.Attester == "alice" && c.Subject == "bob" && c.Kind == "review" && c.Data == "on time" {
			reviewed = true
		}
	}
	if !reviewed {
		t.Fatalf("alice's claim about bob is missing: %+v", s.Claims)
	}
	if mailCount(s) == 0 {
		t.Fatal("no mail counted")
	}

	// A second run from the cache adds only what is new: an item sold for coins (a swap).
	before := len(s.Payments) + len(s.Locks)
	sword, err := alice.createItem("ipfs://sword.json", 10, false)
	w.must(err)
	w.must(alice.mintItem(sword, "alice", 2))
	offer, err := alice.offerItem(sword, 1, coins(2), "", "")
	w.must(err)
	_, err = bob.buyItem(offer)
	w.must(err)
	s = netscan.Load(cache, net)
	update()
	if len(s.Swaps) != 1 {
		t.Fatalf("swaps: %+v", s.Swaps)
	}
	item, coin := lockByID(s, s.Swaps[0].Offer), lockByID(s, s.Swaps[0].Answer)
	if item == nil || coin == nil || item.Item != fmt.Sprint(sword) || item.Total != "1" || coin.Total != "2" || item.Status != "paid" || coin.Status != "paid" {
		t.Fatalf("the swap's locks: %+v %+v", item, coin)
	}
	if got := len(s.Payments) + len(s.Locks); got != before+2 {
		t.Fatalf("the second run counted %d new payments and locks, want 2 (the swap's locks)", got-before)
	}
}

func lockByID(s *netscan.State, id uint64) *netscan.Lock {
	for _, l := range s.Locks {
		if l.ID == id {
			return l
		}
	}
	return nil
}

func mailCount(s *netscan.State) (n uint64) {
	for _, m := range s.Mail {
		n += m.Envelopes
	}
	return n
}
