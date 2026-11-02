// Regression tests for the frictions of wave 6 (sim/CASES-6.md).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A community commissions work: the hearing waits for the first evidence,
// evidence keys reach the panel and the community's admin (not quarantined,
// nobody skipped), and the organiser and a panelist may review the payee
// citing the lock: one counted review per lock, for the community (1, 3).
func TestWave6CommissionHearingAndReviews(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	worker, _ := w.owner("worker")
	mallory, _ := w.owner("mallory")
	const C = "garden"
	hour := uint64(3600)
	w.must(nina.found(C, CommonsParams{Term: 30 * 86400, VoteWindow: hour, VetoWindow: hour, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 3000, Budget: coins(10)}, ""))
	w.join(C, nina, alice, bob, carol)
	w.must(alice.deposit(coins(5)))
	w.must(alice.pay(C, coins(4), "fund"))
	id, err := nina.commission(C, "worker", coins(3), []string{"alice", "bob", "carol"}, 2, "", 1, 3, "build the greenhouse")
	w.must(err)
	w.later(time.Duration(hour+1) * time.Second)
	r, err := w.ch.send(carol.key, nil, "Commons", "payOut", u(id))
	w.must(err)
	lock, err := carol.firstIndexed(r, "Bank", "Locked")
	w.must(err)
	w.must(worker.acceptLock(lock))

	// The hearing set at the opening ends before the delivery: the first evidence opens it anyway.
	w.later(2 * time.Minute)
	proof := filepath.Join(t.TempDir(), "delivery.txt")
	os.WriteFile(proof, []byte("greenhouse built, photos attached"), 0o600)
	if _, err := worker.panelEvidence(lock, proof, "delivery"); err != nil {
		t.Fatalf("the first evidence after hearingUntil was refused: %v", err)
	}
	if _, err := alice.panelRule(lock, 10000, "too early"); err == nil || !strings.Contains(err.Error(), "hearing is open") {
		t.Fatalf("ruled right after the first evidence: %v", err)
	}
	// The key reached the panel and the payer community's admin, delivered (expected), not quarantined.
	for _, c := range []*Client{alice, nina} {
		got := mailTo(scan(t, c), tEvidence)
		if len(got) != 1 || got[0].Status != "delivered" {
			t.Fatalf("%s: evidence key %+v", c.f.Name, got)
		}
	}
	v, err := alice.panelView(lock)
	w.must(err)
	if ev := v["evidence"].([]map[string]any); len(ev) != 1 || !strings.Contains(ev[0]["text"].(string), "greenhouse built") {
		t.Fatalf("panelist can't open the evidence: %v", v["evidence"])
	}
	w.later((panelAfterEvidence + 1) * time.Second)
	_, err = alice.panelRule(lock, 10000, "built")
	w.must(err)
	_, err = bob.panelRule(lock, 9000, "built")
	w.must(err)
	w.must(carol.panelSettleAuto(lock))

	// Reviews citing the community's lock: its admin and a panelist may; a stranger may not.
	if _, err := mallory.reviewData("worker", lock, "x"); err == nil {
		t.Fatal("a stranger cited the community's lock")
	}
	for _, c := range []*Client{nina, alice} {
		d, err := c.reviewData("worker", lock, "good work, "+c.f.Name)
		if err != nil {
			t.Fatalf("%s can't review for %s: %v", c.f.Name, C, err)
		}
		_, err = c.attestScored("worker", "review", d, 90)
		w.must(err)
	}
	rt, err := mallory.rating("worker", ratingOpts{})
	w.must(err)
	if rt["reviews_counted"] != 1 {
		t.Fatalf("want one counted review per community lock: %v", rt["reviews_counted"])
	}
	if top := rt["recent_reviews"].([]map[string]any); top[0]["for"] != C {
		t.Fatalf("not counted for the community: %v", top)
	}
	if s := rt["score"].(float64); s < 60 {
		t.Fatalf("one review of 90 for a 3-coin deal scores %v", s)
	}
}

// Ratings: three good reviews of small deals show high stars; no reviews
// show "new", not a made-up middle (2).
func TestWave6RatingShrinkage(t *testing.T) {
	w := newWorld(t)
	carrier, _ := w.owner("carrier")
	arb, _ := w.owner("arb")
	head, _ := w.ch.b.HeaderByNumber(t.Context(), nil)
	week := head.Time + 7*86400
	for i, score := range []int64{90, 85, 95} {
		s, _ := w.owner([]string{"ship1", "ship2", "ship3"}[i])
		w.must(s.deposit(coins(1)))
		l, _, err := s.openEscrow("carrier", "arb", coins(0.2), week, "a pallet run")
		w.must(err)
		w.must(s.releaseLock(l, nil))
		d, err := s.reviewData("carrier", l, "on time")
		w.must(err)
		_, err = s.attestScored("carrier", "review", d, score)
		w.must(err)
	}
	r, err := arb.rating("carrier", ratingOpts{})
	w.must(err)
	if st := r["stars"].(float64); st < 4.2 {
		t.Fatalf("3 reviews of 85-95 show %v stars (score %v)", st, r["score"])
	}
	r, err = carrier.rating("arb", ratingOpts{})
	w.must(err)
	if r["score"] != nil || !strings.HasPrefix(r["stars"].(string), "new: 0 deals") {
		t.Fatalf("empty history: score %v stars %v", r["score"], r["stars"])
	}
}

// Elections, join requests, posts by non-members, spam repeats, reports (4, 5).
func TestWave6CommunityFrictions(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	eve, _ := w.owner("eve")
	const C = "lamp"
	hour := uint64(3600)
	w.must(nina.found(C, CommonsParams{Term: 30 * 86400, VoteWindow: hour, VetoWindow: hour, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 3000, Budget: coins(1)}, "be kind"))
	w.join(C, nina, alice, bob, carol, dave)
	eventTypes(t, nina) // the moderator's stream starts now

	testChain = w.ch
	defer func() { testChain = nil }()
	os.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	dir := func(c *Client) string { return filepath.Join(w.dir, c.f.Name) }
	r, err := w.ch.send(alice.key, nil, "Commons", "callElection", idOf(C), alice.id(), uint8(roleMod), uint8(1))
	w.must(err)
	e, _ := alice.firstIndexed(r, "Commons", "ElectionOpened")
	es := u(e).String()
	if err := run([]string{"commons", "vote", es, "bob", "--dir", dir(carol)}); err == nil || !strings.Contains(err.Error(), "opens when candidates stood") {
		t.Fatalf("vote before anyone stands: %v", err)
	}
	w.must(w.commons(bob, "stand", u(e), bob.id()))
	if err := run([]string{"commons", "vote", es, "dave", "--dir", dir(carol)}); err == nil || !strings.Contains(err.Error(), "candidates are bob") {
		t.Fatalf("vote for a non-candidate: %v", err)
	}
	w.must(run([]string{"commons", "vote", es, "bob", "--dir", dir(carol)}))
	w.must(run([]string{"commons", "vote", es, "bob", "--dir", dir(alice)}))
	if err := run([]string{"commons", "close", es, "--dir", dir(alice)}); err == nil || !strings.Contains(err.Error(), "more seconds") {
		t.Fatalf("close during the vote: %v", err)
	}
	w.later(time.Duration(hour) * time.Second)
	w.must(run([]string{"commons", "close", es, "--dir", dir(alice)}))
	if err := run([]string{"commons", "close", es, "--dir", dir(carol)}); err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Fatalf("second close: %v", err)
	}

	// eve asks to join: the moderator is told and can list who waits; eve's posts are refused, with the reason.
	_, err = eve.joinGroup(C)
	w.must(err)
	rs, err := nina.joinRequests(C)
	w.must(err)
	if len(rs) != 1 || rs[0]["name"] != "eve" {
		t.Fatalf("join requests: %v", rs)
	}
	if _, err := eve.postGroup(C, "hello", nil); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("a non-member's post: %v", err)
	}

	// Spam: repeats are hidden; three members' reports hide a post, and the moderator hears of them.
	for range 3 {
		_, err := dave.postGroup(C, "BUY FOLLOWERS", nil)
		w.must(err)
	}
	_, err = dave.postGroup(C, "a link to a scam", nil)
	w.must(err)
	scan(t, eve)
	var spam []*Item
	var scam *Item
	for _, it := range eve.groupItems(C) {
		switch {
		case it.Mail != nil && it.Mail.Text == "BUY FOLLOWERS":
			spam = append(spam, it)
		case it.Mail != nil && it.Mail.Text == "a link to a scam":
			scam = it
		}
	}
	if len(spam) != 3 || spam[0].Status != "delivered" || spam[1].Status != "hidden" || spam[2].Status != "hidden" {
		t.Fatalf("repeats: %+v", spam)
	}
	if _, err := eve.report(C, scam.ID, "scam", "report"); err == nil {
		t.Fatal("a non-member reported")
	}
	for _, c := range []*Client{alice, bob, carol} {
		_, err := c.report(C, scam.ID, "scam", "report")
		w.must(err)
	}
	if got := eventTypes(t, nina); got["report"] != 3 || got["join-request"] != 1 {
		t.Fatalf("moderator events: %v", got)
	}
	eve.refreshCommunity(C)
	if scam.Status != "hidden" || !strings.Contains(scam.Trust, "reported by 3") {
		t.Fatalf("reported post: %s %s", scam.Status, scam.Trust)
	}
	_, err = nina.report(C, scam.ID, "not spam", "report-dismissed")
	w.must(err)
	eve.refreshCommunity(C)
	if scam.Status != "delivered" {
		t.Fatalf("dismissed reports still hide it: %s", scam.Trust)
	}
}

// Groups: the creator's join, ring keys on join, attachments in posts and
// reading a post before 'group read' (10, 12, 13).
func TestWave6Groups(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(alice.createGroup("club", true))
	msg, err := alice.joinGroup("club")
	if err != nil || !strings.Contains(msg, "already a member") {
		t.Fatalf("creator's join: %q %v", msg, err)
	}
	_, err = bob.joinGroup("club")
	w.must(err)
	rs, err := bob.ringSign("club", "club|t", []byte("m"))
	if err != nil || len(rs.Ring) != 2 {
		t.Fatalf("anonymity set after join (no 'private enable'): %+v %v", rs, err)
	}
	f := filepath.Join(t.TempDir(), "scenario.md")
	os.WriteFile(f, []byte("# the lamp\nact one"), 0o600)
	data, err := attachFile(f)
	w.must(err)
	id, err := alice.postGroup("club", "the scenario", data)
	w.must(err)
	testChain = w.ch
	defer func() { testChain = nil }()
	os.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	out := filepath.Join(t.TempDir(), "got.md")
	if err := run([]string{"read", id[:18], "--save", out, "--dir", filepath.Join(w.dir, "bob")}); err != nil {
		t.Fatalf("read of a group post before group read: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "# the lamp\nact one" {
		t.Fatalf("attachment: %q", b)
	}
}

// find matches any language and case; fiat has the payee's request; a
// credential presentation arrives delivered and readable (8, 9, 11).
func TestWave6FindFiatPresentation(t *testing.T) {
	w := newWorld(t)
	molokoff, _ := w.owner("molokoff")
	buyer, _ := w.owner("buyer")
	kyc, _ := w.owner("kyc")
	w.must(molokoff.setCard(`{"about":"Молочный завод: молоко 3,2%, кефир, творог; Тула","category":["молоко"]}`))
	for _, q := range []string{"молоко", "МОЛОКО кефир", "Тула"} {
		f, err := buyer.find(q)
		w.must(err)
		if len(f) != 1 || f[0].Name != "molokoff" {
			t.Fatalf("find %q: %v", q, f)
		}
	}
	if f, _ := buyer.find("молоко сыр"); len(f) != 0 {
		t.Fatalf("every word must match: %v", f)
	}

	req, err := molokoff.fiatRequest(FiatPayment{To: "buyer", Amount: "1415400", Currency: "RUB", Method: "bank", Ref: "KV-1"})
	w.must(err)
	_, err = buyer.fiatPaid(FiatPayment{To: "molokoff", Amount: "1415400", Currency: "RUB", Method: "bank", Ref: "KV-1"})
	w.must(err)
	l, err := molokoff.fiatList("molokoff")
	w.must(err)
	found := false
	for _, x := range l {
		if x["uid"] == req.Hex() {
			found = strings.HasPrefix(x["status"].(string), "answered")
		}
	}
	if !found {
		t.Fatalf("request not answered by the payment: %v", l)
	}

	kyc.setContact("molokoff", true)
	molokoff.setContact("kyc", true)
	_, err = kyc.issue("molokoff", map[string]string{"quality": "GOST-31450", "audit": "2026"}, 365)
	w.must(err)
	scan(t, molokoff)
	p, err := molokoff.present(molokoff.heldCredentials()[0], []string{"quality"}, "buyer")
	w.must(err)
	_, err = molokoff.sendDatagram(tPresentation, "buyer", p)
	w.must(err)
	got := mailTo(scan(t, buyer), tPresentation)
	if len(got) != 1 || got[0].Status != "delivered" {
		t.Fatalf("presentation: %+v", got)
	}
	body, _ := json.Marshal(buyer.readViewOf(got[0])["body"])
	if !strings.Contains(string(body), "GOST-31450") || !strings.Contains(string(body), `"issuer":"kyc"`) {
		t.Fatalf("presentation body: %s", body)
	}
}

// A command that ran during a watch and saved an older cursor doesn't make
// the next watch replay what was told (7).
func TestWave6WatchCursorSurvivesOtherCommands(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(bob.deposit(coins(5)))
	eventTypes(t, alice) // the stream starts now
	stale, _ := os.ReadFile(filepath.Join(alice.dir, "state.json"))
	w.must(bob.pay("alice", coins(1), "first"))
	if got := eventTypes(t, alice); got["paid"] != 1 {
		t.Fatalf("first: %v", got)
	}
	os.WriteFile(filepath.Join(alice.dir, "state.json"), stale, 0o600) // another command saves what it loaded before
	again, err := openClient(alice.dir, w.ch)
	w.must(err)
	if got := eventTypes(t, again); got["paid"] != 0 {
		t.Fatalf("replayed: %v", got)
	}
}
