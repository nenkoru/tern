package main

import (
	"strings"
	"testing"
)

// talk: a and b take turns in one thread, each replying to what it just
// read; returns the first refusal (and who got it) and the last item read.
func talk(t *testing.T, a, b *Client, n int) (error, *Client, *Item) {
	t.Helper()
	if _, err := a.send(SendOpts{To: b.f.Name, Intent: "request", Text: "turn 1"}); err != nil {
		return err, a, nil
	}
	from, to := a, b
	var last *Item
	for i := 2; i <= n; i++ {
		from, to = to, from
		last = nil
		for _, it := range mailTo(scan(t, from), tMessage) {
			if it.From == to.f.Name {
				last = it
			}
		}
		if last == nil {
			t.Fatalf("turn %d: %s got nothing from %s", i, from.f.Name, to.f.Name)
		}
		if _, err := from.send(SendOpts{To: to.f.Name, Intent: "answer", Text: "turn", ReplyTo: last.ID}); err != nil {
			return err, from, last
		}
	}
	return nil, nil, last
}

func noWait(t *testing.T) {
	old := approvalWait
	approvalWait = 0
	t.Cleanup(func() { approvalWait = old })
}

// asks: the ask-owner messages an owner received.
func asks(t *testing.T, owner *Client) []*Item {
	t.Helper()
	scan(t, owner)
	var out []*Item
	for _, it := range owner.st.Items {
		if it.Mail != nil && it.Mail.Ask != nil && it.To == owner.f.Name {
			out = append(out, it)
		}
	}
	return out
}

// Two agents of the same owner have no hop limit in a thread: past 6 in a
// row is fine, and the reader doesn't refuse it as "(hop limit)".
func TestThreadSameOwnerNoHopLimit(t *testing.T) {
	noWait(t)
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	ada := w.agent(oscar, "ada", coins(1), `{}`)
	lena := w.agent(oscar, "lena", coins(1), `{}`)
	err, who, _ := talk(t, ada, lena, 10)
	if err != nil {
		t.Fatalf("%s refused at a same-owner thread: %v", who.f.Name, err)
	}
	got := mailTo(scan(t, ada), tMessage)
	in := got[len(got)-1]
	if in.Mail.Hop <= hopLimit || in.Status == "refused" || strings.Contains(in.Verified, "hop limit") {
		t.Fatalf("hop %d, status %s, %s", in.Mail.Hop, in.Status, in.Verified)
	}
	if n := len(asks(t, oscar)); n != 0 {
		t.Fatalf("%d asks filed", n)
	}
}

// An owner-approved lasting correspondent: no hop limit either (each side's
// owner approved the other). A local contact is no such approval: a
// stranger's thread is refused at the hop limit, files one ask, and the
// owner's approval of it resets the count once.
func TestThreadApprovedContactAndStranger(t *testing.T) {
	noWait(t)
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	ada := w.agent(oscar, "ada", coins(1), `{}`)
	eve := w.agent(nina, "eve", coins(1), `{}`)
	w.must(oscar.contactFor("ada.oscar", "eve.nina", false))
	w.must(nina.contactFor("eve.nina", "ada.oscar", false))
	scan(t, ada)
	scan(t, eve)
	if !ada.trustedPeer("eve.nina") || !eve.trustedPeer("ada.oscar") {
		t.Fatal("approved contacts not trusted")
	}
	if err, who, _ := talk(t, ada, eve, 9); err != nil {
		t.Fatalf("%s refused with an approved contact: %v", who.f.Name, err)
	}

	// Strangers (contacts only in each agent's local list): the hop limit, as before.
	olga := w.agent(oscar, "olga", coins(1), `{}`)
	vera := w.agent(nina, "vera", coins(1), `{}`)
	olga.setContact("nina", true)
	vera.setContact("oscar", true)
	if olga.trustedPeer("vera.nina") {
		t.Fatal("a local contact counted as owner-approved")
	}
	err, who, last := talk(t, olga, vera, 7)
	if err == nil || who != olga || !strings.Contains(err.Error(), "hop limit: 6 agent messages in a row") ||
		!strings.Contains(err.Error(), "an ask was filed") || !strings.Contains(err.Error(), "owner approve --dir <oscar's folder> --ask ") {
		t.Fatalf("stranger at 7 in a row: %v (%v)", err, who)
	}
	t.Logf("refusal: %v", err)
	_, err2 := olga.send(SendOpts{To: "vera.nina", Intent: "answer", Text: "again", ReplyTo: last.ID})
	if err2 == nil || !strings.Contains(err2.Error(), "asked already") {
		t.Fatalf("second try: %v", err2)
	}
	as := asks(t, oscar)
	if len(as) != 1 || !strings.HasPrefix(as[0].Mail.Ask.To, "thread:") || !strings.Contains(as[0].Mail.Text, "olga.oscar and vera.nina hit the limit") {
		t.Fatalf("asks: %d %+v", len(as), as)
	}
	if !strings.Contains(err.Error(), as[0].ID[:18]) {
		t.Fatalf("the error doesn't name the ask %s: %v", as[0].ID[:18], err)
	}
	r, err := oscar.approveAsk(as[0].ID[:18])
	w.must(err)
	if r["allows"] == nil {
		t.Fatalf("approve: %v", r)
	}
	if _, err := olga.send(SendOpts{To: "vera.nina", Intent: "answer", Text: "go on", ReplyTo: last.ID}); err != nil {
		t.Fatalf("after the owner's approval: %v", err)
	}
	got := mailTo(scan(t, vera), tMessage)
	if h := got[len(got)-1].Mail.Hop; h != 1 {
		t.Fatalf("hop after the reset: %d", h)
	}
	// Trust lifts only the hop limit: a stranger's taint still guards new correspondents.
	if olga.trustedPeer("someone.else") {
		t.Fatal("trusted a stranger")
	}
}

// The daily budget of a trusted pair: refused at thread_per_day, exactly one
// ask filed; the owner's approval of it allows thread_per_day more today.
func TestThreadDailyBudget(t *testing.T) {
	noWait(t)
	if err := checkScopes(`{"thread_per_day":501}`); err == nil {
		t.Fatal("thread_per_day 501 granted")
	}
	if err := checkScopes(`{"thread_per_day":0}`); err == nil {
		t.Fatal("thread_per_day 0 granted")
	}
	var s agentScopes
	if s.knob("thread_per_day") != 50 {
		t.Fatal("default")
	}
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	ada := w.agent(oscar, "ada", coins(1), `{"thread_per_day":3}`)
	w.agent(oscar, "lena", coins(1), `{}`)
	send := func() error {
		_, err := ada.send(SendOpts{To: "lena.oscar", Intent: "request", Text: "x", Thread: "t_budget"})
		return err
	}
	for i := 0; i < 3; i++ {
		w.must(send())
	}
	err := send()
	if err == nil || !strings.Contains(err.Error(), "daily thread budget: 3 messages to lena.oscar") || !strings.Contains(err.Error(), "an ask was filed") {
		t.Fatalf("over the budget: %v", err)
	}
	t.Logf("refusal: %v", err)
	if err := send(); err == nil || !strings.Contains(err.Error(), "asked already") {
		t.Fatalf("again: %v", err)
	}
	as := asks(t, oscar)
	t.Logf("ask: %s | carries %s", as[0].Mail.Text, as[0].Mail.Ask.To)
	if len(as) != 1 || !strings.Contains(as[0].Mail.Ask.To, ":day:") {
		t.Fatalf("%d asks", len(as))
	}
	w.must(func() error { _, err := oscar.approveAsk(as[0].ID[:18]); return err }())
	for i := 0; i < 3; i++ {
		if err := send(); err != nil {
			t.Fatalf("after approval, %d: %v", i, err)
		}
	}
	if err := send(); err == nil || !strings.Contains(err.Error(), "daily thread budget: 6") {
		t.Fatalf("past the extension: %v", err)
	}
	if n := len(asks(t, oscar)); n != 2 {
		t.Fatalf("a new limit hit files one new ask: %d", n)
	}
	// Being trusted grants nothing else: after outside mail a new correspondent still needs the owner.
	ada.taint("mallory")
	if _, err := ada.send(SendOpts{To: "mallory", Intent: "request", Text: "hi"}); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("a tainted agent wrote to a new correspondent: %v", err)
	}
}
