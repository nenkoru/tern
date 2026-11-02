package main

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
)

// A deal review is ERC-8004 Reputation feedback too: bob registers as an
// agent, alice pays him through an escrow and reviews the deal; the registry
// has her feedback (tagged with the lock), and refuses bob's about himself.
func TestERC8004DealFeedback(t *testing.T) {
	w := newWorld(t) // a devnet deploys the ERC-8004 look-alikes
	if !w.ch.erc8004() {
		t.Fatal("no ERC-8004 registries on the devnet")
	}
	bob, _ := w.owner("bob")
	alice, _ := w.owner("alice")
	w.owner("guild")
	if err := alice.deposit(coins(20)); err != nil {
		t.Fatal(err)
	}
	r, err := bob.erc8004Register("logos and icons", false)
	if err != nil {
		t.Fatal(err)
	}
	if r["agent_id"] != "0" || !strings.HasPrefix(r["registry"].(string), "eip155:1337:") {
		t.Fatalf("register: %v", r)
	}
	if again, _ := bob.erc8004Register("", false); again["done"] == nil {
		t.Fatalf("a second register made another agent id: %v", again)
	}
	if uri, err := w.ch.call("ERC8004IdentityRegistry", "tokenURI", big.NewInt(0)); err != nil || !strings.HasPrefix(uri[0].(string), "data:application/json;base64,") {
		t.Fatalf("agentURI: %v %v", uri, err)
	}
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	l1, _, err := alice.openEscrow("bob", "guild", coins(8), head.Time+7*86400, "logo design")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.releaseLock(l1, nil); err != nil {
		t.Fatal(err)
	}
	res, err := runAttest(alice, []string{"bob", "review", "great logo, on time"}, fmt.Sprint(l1), "90")
	if err != nil {
		t.Fatal(err)
	}
	fb, ok := res["erc8004"].(map[string]any)
	if !ok {
		t.Fatalf("the review was not published to ERC-8004: %v", res["erc8004"])
	}
	if fb["tag2"] != fmt.Sprint(l1) || fb["value"] != int64(90) {
		t.Fatalf("feedback: %v", fb)
	}
	s, err := bob.erc8004Summary("bob", []string{"alice"})
	if err != nil || s["feedback"] != uint64(1) || s["mean_score"] != 90.0 {
		t.Fatalf("summary: %v %v", s, err)
	}
	if _, err := bob.erc8004Feedback("bob", l1, 100, "I am great"); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("self-feedback: %v", err)
	}
}
