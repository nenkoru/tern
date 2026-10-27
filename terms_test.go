package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The worker reads the terms it accepts: lock open sends them sealed, lock
// show prints them only when they hash to the lock's subject.
func TestLockTermsReachWorker(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	w.must(alice.deposit(coins(10)))
	m := lockOut(t, func() error {
		return runLockOpen(alice, []string{"escrow", "bob", "carol", "1", "translate", "the", "site"}, lockFlags{prim: primFlags{days: 30}})
	})
	if fmt.Sprint(m["terms_sent_to"]) != "[bob carol]" {
		t.Fatalf("terms sent: %v", m)
	}
	id := fmt.Sprint(uint64(m["lock"].(float64)))
	for _, who := range []*Client{bob, carol} {
		s := lockOut(t, func() error { return runPrimitive(who, "lock show", []string{id}, primFlags{}) })
		if s["terms_checked"] != true || s["terms"] != "translate the site" {
			t.Fatalf("%s: lock show: %v", who.f.Name, s)
		}
	}

	// Pay unless disputed: the subject has the window's end in it.
	o := lockOut(t, func() error {
		return runLockOpen(alice, []string{"optimistic", "bob", "0.5", "logo", "v2"}, lockFlags{prim: primFlags{days: 2, hours: 48, arbiter: "carol"}})
	})
	oid := fmt.Sprint(uint64(o["lock"].(float64)))
	s := lockOut(t, func() error { return runPrimitive(bob, "lock show", []string{oid}, primFlags{}) })
	if s["terms_checked"] != true || s["terms"] != "logo v2" {
		t.Fatalf("optimistic: lock show: %v / open: %v", s, o)
	}

	// A panel: the terms reach the payee and every panelist.
	w.owner("dave")
	w.owner("erin")
	p := lockOut(t, func() error {
		return runLockOpen(alice, []string{"panel", "bob", "0.5", "a", "poster"}, lockFlags{prim: primFlags{days: 2, panel: "carol,dave,erin", k: 2}})
	})
	if fmt.Sprint(p["terms_sent_to"]) != "[bob carol dave erin]" {
		t.Fatalf("panel terms sent: %v", p)
	}
	s = lockOut(t, func() error { return runPrimitive(bob, "lock show", []string{fmt.Sprint(uint64(p["case"].(float64)))}, primFlags{}) })
	if s["terms_checked"] != true || s["terms"] != "a poster" {
		t.Fatalf("panel: lock show: %v", s)
	}
}

// Text that doesn't hash to the subject is never shown as the terms.
func TestCheckTermsRejectsForgery(t *testing.T) {
	parts := []string{"alice", "bob", "translate the site", "100"}
	subj := dealSubject(parts...)
	ok := func(d dealTerms) bool {
		b, _ := json.Marshal(dealTermsData{&d})
		_, good := checkTerms(b, 3, "alice", "bob", subj)
		return good
	}
	if !ok(dealTerms{Lock: 3, Parts: parts, Terms: "translate the site"}) {
		t.Fatal("the real terms refused")
	}
	forged := []string{"alice", "bob", "translate the site and pay me twice", "100"}
	for _, d := range []dealTerms{
		{Lock: 3, Parts: forged, Terms: forged[2]},                         // other text
		{Lock: 3, Parts: parts, Terms: "pay me twice"},                     // text not among the parts
		{Lock: 4, Parts: parts, Terms: "translate the site"},               // another lock
		{Lock: 3, Parts: []string{"mallory", "bob", "x", "1"}, Terms: "x"}, // another payer
	} {
		if ok(d) {
			t.Errorf("accepted %+v", d)
		}
	}
}
