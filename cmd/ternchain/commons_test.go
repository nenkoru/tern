package main

import (
	"math/big"
	"strings"
	"testing"
	"time"
)

func (w *world) commons(c *Client, method string, args ...any) error {
	_, err := w.ch.send(c.key, nil, "Commons", method, args...)
	return err
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) mustFail(err error, want string) {
	w.t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		w.t.Fatalf("want %q, got %v", want, err)
	}
}

func (w *world) isMember(c, who string) bool {
	out, _ := w.ch.call("Commons", "isMember", idOf(c), idOf(who))
	return out[0].(bool)
}

func (w *world) hasRole(c, who string, role uint8) bool {
	out, _ := w.ch.call("Commons", "hasRole", idOf(c), idOf(who), role)
	return out[0].(bool)
}

// join: asks to join (Sets) and gets admitted by `by`.
func (w *world) join(community string, by *Client, cs ...*Client) {
	w.t.Helper()
	for _, c := range cs {
		if _, err := c.joinGroup(community); err != nil {
			w.t.Fatal(err)
		}
		if _, err := by.act("admit", idOf(community), by.id(), c.id()); err != nil {
			w.t.Fatal(err)
		}
	}
}

func groupPosts(t *testing.T, c *Client, group string) map[string]*Item {
	scan(t, c)
	out := map[string]*Item{}
	for _, it := range c.groupItems(group) {
		if it.Mail != nil {
			out[it.Mail.Text] = it
		}
	}
	return out
}

// A community governs itself: elected roles, bans that members can veto,
// a recall of an abusive moderator, a treasury under a budget, shared
// blocklists, and one vote per owner.
func TestCommunitySelfGovernance(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	spammer, _ := w.owner("spammer")
	reader, _ := w.owner("reader") // not a member: reads the public channel
	const C = "mersin"
	hour := uint64(3600)
	p := CommonsParams{Term: 30 * 86400, VoteWindow: hour, VetoWindow: hour, VetoBps: 5000, PetitionBps: 5000, QuorumBps: 3000,
		Budget: coins(1), AgentsVote: false}
	w.must(nina.found(C, p, "be kind; no ads"))
	if !w.hasRole(C, "nina", roleAdmin) || !w.isMember(C, "nina") {
		t.Fatal("the founder is not its admin")
	}
	// Nobody is put in without asking.
	_, err := nina.act("admit", idOf(C), nina.id(), alice.id())
	w.mustFail(err, "no join request")
	w.join(C, nina, alice, bob, carol, spammer)
	if k, _ := nina.community(C); k.Members != 5 {
		t.Fatalf("members: %d", k.Members)
	}

	// Members elect a moderator.
	r, err := w.ch.send(alice.key, nil, "Commons", "callElection", idOf(C), alice.id(), uint8(roleMod), uint8(1))
	w.must(err)
	e, _ := alice.firstIndexed(r, "Commons", "ElectionOpened")
	w.must(w.commons(bob, "stand", u(e), bob.id()))
	for _, v := range []*Client{alice, carol, nina} {
		w.must(w.commons(v, "vote", u(e), v.id(), bob.id()))
	}
	w.mustFail(w.commons(alice, "vote", u(e), alice.id(), bob.id()), "voted")
	w.mustFail(w.commons(alice, "closeElection", u(e)), "not yet")
	w.later(time.Duration(hour) * time.Second)
	w.must(w.commons(alice, "closeElection", u(e)))
	if !w.hasRole(C, "bob", roleMod) {
		t.Fatal("bob not elected")
	}
	// An election nobody votes in elects nobody.
	r, _ = w.ch.send(carol.key, nil, "Commons", "callElection", idOf(C), carol.id(), uint8(roleAdmin), uint8(1))
	e0, _ := carol.firstIndexed(r, "Commons", "ElectionOpened")
	w.must(w.commons(carol, "stand", u(e0), carol.id()))
	w.later(time.Duration(hour) * time.Second)
	w.must(w.commons(carol, "closeElection", u(e0)))
	if w.hasRole(C, "carol", roleAdmin) {
		t.Fatal("elected with zero turnout")
	}

	// The spammer posts; the moderator bans it: its posts are hidden and it can't come back.
	nina.postGroup(C, "welcome to mersin", nil)
	spammer.postGroup(C, "CHEAP FOLLOWERS", nil)
	reader.joinGroup(C) // asks to join, and reads the public channel meanwhile
	if got := groupPosts(t, reader, C); got["CHEAP FOLLOWERS"] == nil || got["CHEAP FOLLOWERS"].Status != "delivered" {
		t.Fatalf("before the ban: %+v", got["CHEAP FOLLOWERS"])
	}
	_, err = bob.act("ban", idOf(C), bob.id(), spammer.id(), "spam")
	w.must(err)
	fresh := freshReader(t, w, "reader2", C)
	if got := groupPosts(t, fresh, C); got["CHEAP FOLLOWERS"].Status != "hidden" || got["welcome to mersin"].Status != "delivered" {
		t.Fatalf("after the ban: spam %s, welcome %s", got["CHEAP FOLLOWERS"].Status, got["welcome to mersin"].Status)
	}
	if _, err := spammer.joinGroup(C); err == nil {
		t.Fatal("the banned spammer asked to join again")
	}

	// The moderator turns abusive: it bans carol for disagreeing. Members veto; carol is back.
	carol.pollEvents(nil, false)
	unfair, err := bob.act("ban", idOf(C), bob.id(), carol.id(), "disagreed with me")
	w.must(err)
	if w.isMember(C, "carol") {
		t.Fatal("ban didn't remove carol")
	}
	w.must(w.commons(alice, "veto", u(unfair), alice.id()))
	w.mustFail(w.commons(alice, "veto", u(unfair), alice.id()), "already")
	w.must(w.commons(nina, "veto", u(unfair), nina.id()))
	if !w.isMember(C, "carol") {
		t.Fatal("a reversed ban didn't re-admit carol")
	}
	if evs := eventTypes(t, carol); evs["banned"] != 1 || evs["action-reversed"] != 1 {
		t.Fatalf("carol's events: %v", evs)
	}
	carol.postGroup(C, "thanks all", nil)
	if got := groupPosts(t, fresh, C); got["thanks all"] == nil || got["thanks all"].Status != "delivered" {
		t.Fatal("carol's post hidden after the reversed ban")
	}

	// Recall: a petition (2 of 4) opens a vote; meanwhile bob's powers are suspended.
	bob.pollEvents(nil, false)
	r, err = w.ch.send(alice.key, nil, "Commons", "petition", idOf(C), alice.id(), bob.id(), uint8(roleMod))
	w.must(err)
	rc, _ := alice.firstIndexed(r, "Commons", "RecallOpened")
	_, err = w.ch.send(carol.key, nil, "Commons", "petition", idOf(C), carol.id(), bob.id(), uint8(roleMod))
	w.mustFail(err, "petition is open")
	w.must(w.commons(carol, "sign", u(rc), carol.id()))
	_, err = bob.act("ban", idOf(C), bob.id(), alice.id(), "revenge")
	w.mustFail(err, "suspended")
	for _, v := range []*Client{alice, carol, nina} {
		w.must(w.commons(v, "voteRecall", u(rc), v.id(), true))
	}
	w.must(w.commons(bob, "voteRecall", u(rc), bob.id(), false))
	w.later(time.Duration(hour) * time.Second)
	w.must(w.commons(alice, "closeRecall", u(rc)))
	if w.hasRole(C, "bob", roleMod) {
		t.Fatal("bob kept the role after the recall")
	}
	if evs := eventTypes(t, bob); evs["recall-voting"] != 1 || evs["recall-closed"] != 1 || evs["role-expired"] != 1 {
		t.Fatalf("bob's events: %v", evs)
	}

	// Treasury: a spend within the budget is paid only after the veto window.
	w.must(nina.deposit(coins(5)))
	_, err = w.ch.send(nina.key, coins(2), "Bank", "deposit", idOf(C))
	w.must(err)
	pay, err := nina.act("spend", idOf(C), nina.id(), alice.id(), exact("0.5"), "logo design")
	w.must(err)
	w.mustFail(w.commons(alice, "payOut", u(pay)), "veto window open")
	// A vetoed spend is never paid, and its budget comes back.
	bad, err := nina.act("spend", idOf(C), nina.id(), nina.id(), exact("0.4"), "my expenses")
	w.must(err)
	_, err = nina.act("spend", idOf(C), nina.id(), nina.id(), exact("0.2"), "over")
	w.mustFail(err, "budget")
	w.must(w.commons(alice, "veto", u(bad), alice.id()))
	w.must(w.commons(carol, "veto", u(bad), carol.id()))
	before, _ := alice.bankBalance("alice")
	w.later(time.Duration(hour) * time.Second)
	w.must(w.commons(carol, "payOut", u(pay)))
	w.mustFail(w.commons(carol, "payOut", u(bad)), "nothing to pay")
	after, _ := alice.bankBalance("alice")
	if new(big.Int).Sub(after, before).Cmp(exact("0.5")) != 0 {
		t.Fatalf("alice was paid %s", fmtCoins(new(big.Int).Sub(after, before)))
	}
	_, err = nina.act("spend", idOf(C), nina.id(), nina.id(), exact("0.5"), "within the budget again")
	w.must(err)
	_, err = nina.act("spend", idOf(C), nina.id(), nina.id(), exact("0.1"), "past it")
	w.mustFail(err, "budget")

	// A posting fee goes to the community, the post's id as memo.
	_, err = nina.act("setPostFee", idOf(C), nina.id(), exact("0.01"))
	w.must(err)
	w.must(alice.deposit(coins(1)))
	t0, _ := nina.bankBalance(C)
	if _, err := alice.postGroup(C, "paid post", nil); err != nil {
		t.Fatal(err)
	}
	if t1, _ := nina.bankBalance(C); new(big.Int).Sub(t1, t0).Cmp(exact("0.01")) != 0 {
		t.Fatalf("post fee: %s", fmtCoins(new(big.Int).Sub(t1, t0)))
	}

	// Another community follows mersin's bans: the spammer can't get in there either.
	w.must(alice.found("bazaar", CommonsParams{Term: 86400, VoteWindow: hour, VetoWindow: hour, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 3000, Budget: big.NewInt(0), AgentsVote: true}, ""))
	_, err = alice.act("follow", idOf("bazaar"), alice.id(), idOf(C), uint8(1))
	w.must(err)
	_, err = spammer.joinGroup("bazaar") // asking still works…
	w.must(err)
	_, err = alice.act("admit", idOf("bazaar"), alice.id(), spammer.id())
	w.mustFail(err, "banned") // …but nobody can admit a name a followed list bans

	// One vote per owner: dave's two agents are members of the bazaar (agents vote there), and vote once.
	dave, _ := w.owner("dave")
	a1 := w.agent(dave, "one", coins(1), `{}`)
	a2 := w.agent(dave, "two", coins(1), `{}`)
	w.join("bazaar", alice, a1, a2, carol)
	if k, _ := alice.community("bazaar"); k.Members != 3 {
		t.Fatalf("bazaar members per owner: %d, want 3 (alice, dave, carol)", k.Members)
	}
	r, _ = w.ch.send(alice.key, nil, "Commons", "callElection", idOf("bazaar"), alice.id(), uint8(roleMod), uint8(1))
	e2, _ := alice.firstIndexed(r, "Commons", "ElectionOpened")
	w.must(w.commons(carol, "stand", u(e2), carol.id()))
	w.must(w.commons(a1, "vote", u(e2), a1.id(), carol.id()))
	w.mustFail(w.commons(a2, "vote", u(e2), a2.id(), carol.id()), "voted")
	// In mersin agents don't vote: nina's agent is a member there, but its key can't vote.
	helper := w.agent(nina, "helper", coins(1), `{}`)
	w.join(C, nina, helper)
	r, _ = w.ch.send(alice.key, nil, "Commons", "callElection", idOf(C), alice.id(), uint8(roleMod), uint8(1))
	e3, _ := alice.firstIndexed(r, "Commons", "ElectionOpened")
	w.must(w.commons(carol, "stand", u(e3), carol.id()))
	w.mustFail(w.commons(helper, "vote", u(e3), helper.id(), carol.id()), "agents don't vote")

	// Anyone may follow a community's bans in their own client: mail from the spammer is dropped.
	w.must(runBlocklist(reader, "blocklist follow", []string{C}))
	reader.setContact("spammer", true)
	spammer.setContact("reader", true)
	if _, err := spammer.send(SendOpts{To: "reader", Text: "buy now"}); err != nil {
		t.Fatal(err)
	}
	scan(t, reader)
	for _, l := range reader.inboxView(false)["items"].([]map[string]any) {
		if l["from"] == "spammer" {
			t.Fatal("blocked mail shown")
		}
	}
	var labelled bool
	for _, l := range reader.inboxView(true)["items"].([]map[string]any) {
		labelled = labelled || l["from"] == "spammer" && l["status"] == "blocked by "+C
	}
	if !labelled {
		t.Fatal("--all doesn't show the blocked mail, labelled")
	}

	show, err := nina.showCommunity(C)
	w.must(err)
	if show["members"].(uint32) != 4 || show["rules"] != "be kind; no ads" || show["post_fee"] != "0.010000" {
		t.Fatalf("show: %v", show)
	}
	var reversed bool
	for _, a := range show["recent_actions"].([]map[string]any) {
		reversed = reversed || a["kind"] == "spend" && a["reversed"] == true
	}
	if !reversed {
		t.Fatalf("show lacks the reversed spend: %v", show["recent_actions"])
	}
}

// freshReader: a new non-member identity reading a community's public channel.
func freshReader(t *testing.T, w *world, name, group string) *Client {
	c, _ := w.owner(name)
	if _, err := c.joinGroup(group); err != nil {
		t.Fatal(err)
	}
	return c
}

func eventTypes(t *testing.T, c *Client) map[string]int {
	t.Helper()
	evs, err := c.pollEvents(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]int{}
	for _, e := range evs {
		m[e.Type]++
	}
	return m
}

// Fixes from the review: a reversed ban of a non-member doesn't admit it; a
// banned owner's agents can't vote; a recall names the exact role; spend
// budgets come back only within their period.
func TestCommonsReviewFixes(t *testing.T) {
	w := newWorld(t)
	ann, _ := w.owner("ann")
	ben, _ := w.owner("ben")
	cat, _ := w.owner("cat")
	troll, _ := w.owner("troll")
	const C = "garden"
	w.must(ann.found(C, CommonsParams{Term: 86400, VoteWindow: 600, VetoWindow: 600, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 5000, Budget: coins(1), AgentsVote: true}, ""))
	bot := w.agent(troll, "bot", coins(1), `{}`)
	w.join(C, ann, ben, cat, bot)
	// A ban of a name that isn't a member, reversed, doesn't let it in.
	pre, err := ann.act("ban", idOf(C), ann.id(), troll.id(), "known troll")
	w.must(err)
	if _, err := troll.joinGroup(C); err == nil {
		t.Fatal("a pre-emptively banned name asked to join")
	}
	// The owner's ban covers its agent: the agent can't vote.
	r, _ := w.ch.send(ann.key, nil, "Commons", "callElection", idOf(C), ann.id(), uint8(roleMod), uint8(1))
	e, _ := ann.firstIndexed(r, "Commons", "ElectionOpened")
	w.must(w.commons(ben, "stand", u(e), ben.id()))
	w.mustFail(w.commons(bot, "vote", u(e), bot.id(), ben.id()), "members only")
	w.must(w.commons(ben, "veto", u(pre), ben.id()))
	w.must(w.commons(cat, "veto", u(pre), cat.id()))
	if w.isMember(C, "troll") {
		t.Fatal("reversing a ban admitted a non-member")
	}
	// A recall is of the exact role: an admin holds no separate moderator role to recall.
	_, err = w.ch.send(ben.key, nil, "Commons", "petition", idOf(C), ben.id(), ann.id(), uint8(roleMod))
	w.mustFail(err, "doesn't hold that role")
	// A petition that gathers no signatures in time expires; a new one may open.
	r, err = w.ch.send(ben.key, nil, "Commons", "petition", idOf(C), ben.id(), ann.id(), uint8(roleAdmin))
	w.must(err)
	rc, _ := ben.firstIndexed(r, "Commons", "RecallOpened")
	w.later(700 * time.Second)
	w.mustFail(w.commons(cat, "sign", u(rc), cat.id()), "petition over")
	_, err = w.ch.send(cat.key, nil, "Commons", "petition", idOf(C), cat.id(), ann.id(), uint8(roleAdmin))
	w.must(err)
}
