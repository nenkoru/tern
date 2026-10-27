package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/ui"
)

// Moderation claims from any ICommunity: a Sets group's moderator hides a
// post and bans a name by claim; readers hide the post, and a reader who
// follows the group's list (Sets:NAME) drops the banned name's mail.
func TestModerationClaimsAnyCommunity(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	w.must(alice.createGroup("club", true))
	w.must(errOf(bob.joinGroup("club")))
	w.must(errOf(carol.joinGroup("club")))
	post, err := bob.postGroup("club", "buy my course", nil)
	w.must(err)
	sets := w.ch.addr("Sets")
	w.must(errOf(alice.moderationClaim(sets, setID("club"), modHide, common.HexToHash(post), "spam")))
	w.must(errOf(alice.moderationClaim(sets, setID("club"), modBan, dave.id(), "spam farm")))
	scan(t, carol)
	it := carol.item(post)
	if it == nil || it.Status != "hidden" || !strings.Contains(it.Trust, "moderator claim") {
		t.Fatalf("post: %+v", it)
	}
	outW = &strings.Builder{}
	defer func() { outW = nil }()
	w.must(runBlocklist(carol, "blocklist follow", []string{"Sets:club"}))
	if carol.blockedBy("dave") != "Sets:club" || carol.blockedBy("bob") != "" {
		t.Fatalf("blocklist: %q %q", carol.blockedBy("dave"), carol.blockedBy("bob"))
	}
	if err := runBlocklist(carol, "blocklist follow", []string{"Nowhere:club"}); err == nil {
		t.Fatal("an unknown contract followed")
	}
}

// Item gifts and offers by an agent are guarded like payments: scopes.to,
// and after outside mail the owner's approval for exactly that move.
func TestItemGuard(t *testing.T) {
	old := approvalWait
	approvalWait = 0
	t.Cleanup(func() { approvalWait = old })
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	w.owner("nina")
	w.owner("bob")
	ada := w.agent(oscar, "ada", coins(1), `{"to":["nina"]}`)
	sword, err := oscar.createItem("ipfs://sword.json", 10, false)
	w.must(err)
	w.must(oscar.mintItem(sword, "ada.oscar", 3))
	w.mustFail(ada.giveItem(sword, "bob", 1), "scopes.to")
	w.must(ada.giveItem(sword, "nina", 1))
	ada.taint("bob")
	w.mustFail(ada.giveItem(sword, "nina", 1), "blocked")
	w.must(oscar.approveAction("ada.oscar", "item:give:"+itoa(sword)+":1:nina", nil))
	w.must(ada.giveItem(sword, "nina", 1))
	_, err = ada.offerItem(sword, 1, coins(1), "", "")
	w.mustFail(err, "blocked")
}

func itoa(n uint64) string { b, _ := json.Marshal(n); return string(b) }

// Card kinds, intents and reactions come from SchemaNames when the network has it.
func TestSchemaNamesKinds(t *testing.T) {
	t.Cleanup(func() { kindsOnce = sync.Once{}; ui.LoadKinds(nil) })
	kindsOnce = sync.Once{}
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	decl := `{"body":{"type":"object","required":["event"],"properties":{"event":{"type":"string"}}},"responses":{"going":"answer","skip":"local"}}`
	for _, p := range [][3]string{{"card/rsvp", "string event", "data:application/json," + url.PathEscape(decl)}, {"intent/notify", "string note", ""}, {"reaction/heart", "bool on", ""}} {
		_, _, err := alice.publishType(p[0], p[1], p[2])
		w.must(err)
	}
	bob.loadKinds()
	if _, err := ui.ParseCard([]byte(`{"card":"rsvp","v":1,"id":"c_12345","subject":"party","body":{"event":"friday"},"responses":["going"]}`)); err != nil {
		t.Fatal(err)
	}
	if !bob.knownIntent("notify") || !ui.ReactionKind("heart") {
		t.Fatal("intent or reaction not declared")
	}
	if a, ok := ui.ResponseAction("rsvp", "going"); !ok || a != ui.KindAnswer {
		t.Fatal("responses")
	}
}
