package main

import (
	"strings"
	"testing"
)

// A replacement connection and client use the same public deployment and
// owner-held identity files, without the original client's caches or a relay.
func TestSovereigntyReplacementClient(t *testing.T) {
	w, _ := newHostedWorldWith(t, func(p *Params) { p.Profile = "public" })
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	if err := alice.deposit(coins(5)); err != nil {
		t.Fatal(err)
	}
	bot := w.agent(alice, "bot", coins(1), `{}`)
	if _, err := bot.send(SendOpts{To: "bob", Text: "before replacement"}); err != nil {
		t.Fatal(err)
	}

	// A fresh backend handle still addresses the same chain. This deliberately
	// tests connection replacement, not a second consensus network or live RPC.
	replacement, err := newChain(w.sim.Client(), w.ch.dep, func() { w.sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	reopen := func(c *Client) *Client {
		t.Helper()
		fresh, err := openClient(c.dir, replacement)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.id() != c.id() || fresh.me() != c.me() || fresh.relay != "" {
			t.Fatal("replacement changed identity or requires a relay")
		}
		return fresh
	}
	alice, bot, bob = reopen(alice), reopen(bot), reopen(bob)
	if _, err := bot.send(SendOpts{To: "bob", Text: "after replacement"}); err != nil {
		t.Fatal(err)
	}
	got := mailTo(scan(t, bob), tMessage)
	if len(got) != 2 || got[0].Mail == nil || got[1].Mail == nil {
		t.Fatalf("replacement cannot decrypt existing and new mail: %+v", got)
	}
	texts := map[string]bool{got[0].Mail.Text: true, got[1].Mail.Text: true}
	if !texts["before replacement"] || !texts["after replacement"] {
		t.Fatalf("replacement lost mail: %v", texts)
	}
	// payNow bypasses the client's approval guard, so these checks exercise the
	// Bank/Registry permissions rather than trusting a compliant client.
	if err := bot.payNow("bob", coins(0.5), "allowed"); err != nil {
		t.Fatal(err)
	}
	if err := bot.payNow("bob", coins(0.6), "over limit"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("replacement bypassed the owner's spending limit: %v", err)
	}
	if err := alice.revokeAgent("bot.alice", false); err != nil {
		t.Fatal(err)
	}
	if err := bot.payNow("bob", coins(0.1), "revoked"); err == nil {
		t.Fatal("replacement bypassed the owner's revocation")
	}
}
