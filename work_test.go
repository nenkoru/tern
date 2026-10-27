package main

import (
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"testing"
	"time"
)

// A stranger's agent asks a service for work: the request pays the postage
// the service's card asks, so the service reads it (no quarantine), and the
// service's offer comes back delivered (a reply to what the agent sent).
func TestWorkRequest(t *testing.T) {
	w := newWorld(t)
	ada, _ := w.owner("ada")
	lara, _ := w.owner("lara")
	w.must(lara.deposit(coins(1)))
	svc := w.agent(ada, "shop", coins(1), "")
	buyer := w.agent(lara, "bot", coins(1), `{"free_pay":"0.5","send_freely":true}`)
	w.must(svc.setCard(`{"service":"translation","capabilities":["translation"],"price":"2","pay":"escrow","accepts":["tern.work-request"],"postage":{"work":"0.01","default":"0.05"}}`))
	quiet, _ := w.owner("quiet")
	w.must(quiet.setCard(`{"about":"nothing for strangers"}`))

	fs, err := buyer.find("translation")
	w.must(err)
	if len(fs) != 1 || fs[0].Contact == nil || fs[0].Contact.RequestPostage != "0.010000" || fs[0].Contact.Price != "2" ||
		!strings.Contains(fs[0].Contact.How, "ternchain request shop.ada") {
		t.Fatalf("find: %d results, contact %+v", len(fs), *fs[0].Contact)
	}
	if _, _, err := buyer.request("quiet", WorkRequest{What: "anything"}); err == nil || !strings.Contains(err.Error(), "takes no work requests") {
		t.Fatalf("a card without terms: %v", err)
	}
	id, info, err := buyer.request("shop.ada", WorkRequest{What: "300 words to Turkish", Budget: "3"})
	w.must(err)
	if info["postage_paid"] != "0.010000" {
		t.Fatalf("request: %v", info)
	}
	var req *Item
	for _, it := range scan(t, svc) {
		if it.ID == id {
			req = it
		}
	}
	if req == nil || req.Status != "delivered" || req.Type != tWorkRequest || req.Mail == nil || !strings.Contains(req.Mail.Text, "Turkish") {
		t.Fatalf("the service's view of the request: %+v", req)
	}
	_, err = svc.send(SendOpts{To: req.From, ReplyTo: req.ID, Intent: "offer", Text: "2 coins, escrow with arb"})
	w.must(err)
	var offer *Item
	for _, it := range scan(t, buyer) {
		if it.Mail != nil && it.Mail.Intent == "offer" {
			offer = it
		}
	}
	if offer == nil || offer.Status != "delivered" || !strings.Contains(offer.Trust, "shop.ada, which you asked for work") {
		t.Fatalf("the buyer's view of the offer: %+v", offer)
	}

	// unpaid: a work request without postage lands in quarantine, said so
	if _, err := buyer.sendDatagram(tWorkRequest, "shop.ada", WorkRequest{What: "free please"}); err != nil {
		t.Fatal(err)
	}
	for _, it := range scan(t, svc) {
		if it.Type == tWorkRequest && (it.Status != "quarantined" || !strings.Contains(it.Trust, "without the postage")) {
			t.Fatalf("an unpaid request: %s %s", it.Status, it.Trust)
		}
	}

	// rating: time-boxed and cached for the one-shot CLI
	r1, err := buyer.ratingTimed("shop.ada", ratingOpts{}, 20*time.Second, false)
	w.must(err)
	if r1["cached"] != nil {
		t.Fatal("first rating came from a cache")
	}
	r2, err := buyer.ratingTimed("shop.ada", ratingOpts{}, 20*time.Second, false)
	w.must(err)
	if r2["cached"] == nil {
		t.Fatal("second rating within minutes was computed again")
	}
}

// Weighted reports: three fresh members hide a post in a small community;
// members whose earlier reports moderators dismissed weigh less.
func TestWeightedReports(t *testing.T) {
	w := newWorld(t)
	mod, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	const C = "lamp"
	w.must(mod.found(C, CommonsParams{Term: 30 * 86400, VoteWindow: 3600, VetoWindow: 3600, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 3000, Budget: coins(1)}, "be kind"))
	w.join(C, mod, alice, bob, carol, dave)
	w.later(2 * time.Hour) // past probation
	post := func(text string) *Item {
		_, err := dave.postGroup(C, text, nil)
		w.must(err)
		scan(t, mod)
		for _, it := range mod.groupItems(C) {
			if it.Mail != nil && it.Mail.Text == text {
				return it
			}
		}
		t.Fatalf("no post %q", text)
		return nil
	}
	p1 := post("an honest post")
	for _, c := range []*Client{alice, bob} {
		_, err := c.report(C, p1.ID, "spam", "report")
		w.must(err)
	}
	mod.refreshCommunity(C)
	if p1.Status != "delivered" {
		t.Fatalf("two fresh members hid a post: %s", p1.Trust)
	}
	_, err := carol.report(C, p1.ID, "spam", "report")
	w.must(err)
	mod.refreshCommunity(C)
	if p1.Status != "hidden" || !strings.Contains(p1.Trust, "reported by 3 members") {
		t.Fatalf("three members' reports: %s %s", p1.Status, p1.Trust)
	}
	_, err = mod.report(C, p1.ID, "not spam", "report-dismissed")
	w.must(err)
	mod.refreshCommunity(C)
	if p1.Status != "delivered" {
		t.Fatalf("dismissed: %s", p1.Trust)
	}
	// the same three report again: their record (a dismissed report each) weighs them down
	p2 := post("another honest post")
	for _, c := range []*Client{alice, bob, carol} {
		_, err := c.report(C, p2.ID, "spam", "report")
		w.must(err)
	}
	mod.refreshCommunity(C)
	if p2.Status != "delivered" {
		t.Fatalf("reporters whose reports were dismissed still hide a post: %s", p2.Trust)
	}
	// an author whose posts moderators hid again and again: its next post is hidden before any report
	for i := 0; i < 3; i++ {
		p := post("spam " + string(rune('a'+i)))
		_, err := mod.act("hide", idOf(C), mod.id(), common.HexToHash(p.ID), "spam [author dave]")
		w.must(err)
	}
	p3 := post("one more")
	if p3.Status != "hidden" || !strings.Contains(p3.Trust, "author's record") {
		t.Fatalf("a post by an author with a record of hidden posts: %s %s", p3.Status, p3.Trust)
	}
	if got := reportWeightThreshold(300); got < 1.2 || got > 2.5 {
		t.Fatalf("threshold for 300 members: %v", got)
	}
}
