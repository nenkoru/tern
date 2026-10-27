package main

// Regressions for what live testers ran into (sim/journal).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// A scoped, tainted agent still pays the data node its client was set up
// with; the sender can find the batch root; a relay restart loses nothing;
// an envelope a relay swallowed becomes an alert.
func TestRelayChannelSentTrackingAndQueue(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	bob, _ := w.owner("bob")
	mallory, _ := w.owner("mallory")
	nodeco, _ := w.owner("nodeco")
	oscar.deposit(coins(2))
	ada := w.agent(oscar, "ada", coins(1), `{"to":["bob"]}`)
	mallory.send(SendOpts{To: "ada.oscar", Text: "hi"})
	scan(t, ada)
	if len(ada.st.Taint) == 0 {
		t.Fatal("not tainted")
	}
	dir := t.TempDir()
	node, err := newDataNode(w.ch, nodeco.key, dir, "nodeco", exact("0.0001"), false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(node.Handler())
	defer srv.Close()
	if err := node.ensureStake(srv.URL); err != nil {
		t.Fatal(err)
	}
	ada.relay = srv.URL
	id, err := ada.send(SendOpts{To: "bob", Text: "through the relay"})
	if err != nil {
		t.Fatalf("the agent's own relay channel was blocked: %v", err)
	}
	if err := node.flush(); err != nil {
		t.Fatal(err)
	}
	var got *Item
	for _, it := range scan(t, bob) {
		if it.ID == id {
			got = it
		}
	}
	if got == nil || got.Batch == "" || got.Node != srv.URL {
		t.Fatalf("no batch root on the received item: %+v", got)
	}
	scan(t, ada)
	if r := ada.st.Sent[id]; r == nil || r.Batch != got.Batch {
		t.Fatalf("the sender can't find its batch: %+v", r)
	}
	// Accepted but not posted yet: a restart keeps it.
	id2, err := ada.send(SendOpts{To: "bob", Text: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := newDataNode(w.ch, nodeco.key, dir, "nodeco", exact("0.0001"), false)
	if err != nil || len(again.envs) != 1 {
		t.Fatalf("the queue didn't survive a restart: %d, %v", len(again.envs), err)
	}
	// A batch stored but never committed (a failed post) is posted again.
	again.put(crypto.Keccak256Hash(encodeBatch(again.envs)), encodeBatch(again.envs))
	again.envs = nil
	again.saveQueue()
	third, _ := newDataNode(w.ch, nodeco.key, dir, "nodeco", exact("0.0001"), false)
	if err := third.flush(); err != nil {
		t.Fatal(err)
	}
	if bob.scan(); bob.item(id2) == nil {
		t.Fatal("the envelope of a failed post was lost")
	}
	// A relay that never posts: the sender is told, not left believing.
	black := httptest.NewServer((&Relay{ch: w.ch}).Handler())
	defer black.Close()
	ada.relay = black.URL
	id3, err := ada.send(SendOpts{To: "bob", Text: "into the void"})
	if err != nil {
		t.Fatal(err)
	}
	w.later(6 * time.Minute)
	scan(t, ada)
	if !strings.Contains(strings.Join(ada.st.Alerts, "\n"), id3[:18]) {
		t.Fatalf("no alert for a lost envelope: %v", ada.st.Alerts)
	}
}

func TestPayReceiptRepeatAndWhoami(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	alice.deposit(coins(2))
	r, err := alice.payReceipt("bob", exact("0.2"), "my share", false)
	if err != nil || r["tx"] == nil || r["block"] == nil || r["to"] != "bob" || r["balance_after"] == nil {
		t.Fatalf("receipt %v: %v", r, err)
	}
	if _, err := alice.payReceipt("bob", exact("0.2"), "my share", false); err == nil || !strings.Contains(err.Error(), "--again") {
		t.Fatalf("a repeated payment went through: %v", err)
	}
	if _, err := alice.payReceipt("bob", exact("0.2"), "my share", true); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.payReceipt("bob", exact("0.2"), "another thing", false); err != nil {
		t.Fatal(err)
	}
	w.later(11 * time.Minute)
	if _, err := alice.payReceipt("bob", exact("0.2"), "my share", false); err != nil {
		t.Fatalf("ten minutes later: %v", err)
	}
	ada := w.agent(alice, "ada", coins(1), `{}`)
	exp, ok := ada.grantExpiry()
	if left := int64(exp) - ada.chainNow(); !ok || left < 29*86400 || left > 30*86400 {
		t.Fatalf("grant expiry %d (%v)", exp, ok)
	}
	if _, ok := alice.grantExpiry(); ok {
		t.Fatal("an owner has no grant")
	}
	if _, err := alice.send(SendOpts{To: "bob", Intent: "reply", Text: "ok"}); err != nil {
		t.Fatalf("intent reply: %v", err)
	}
}

// A non-winner is told whose a pot is; the winner, that it is already paid.
func TestPotClaimSaysWhose(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	ref, _ := w.owner("hakem")
	alice.deposit(coins(2))
	h, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	pot, subj, err := alice.openBounty("hakem", coins(1), h.Time+86400, "fix issue #3")
	if err != nil {
		t.Fatal(err)
	}
	win, _ := ref.award(subj, "bob", 10000, "bob's PR passes")
	if _, err := carol.claimAward(pot, win); err == nil || !strings.Contains(err.Error(), "not yours") || !strings.Contains(err.Error(), "bob") {
		t.Fatalf("carol: %v", err)
	}
	r, err := bob.claimAward(pot, win)
	if err != nil || r["done"] != "paid to you" || r["paid_now"] != "1.000000" {
		t.Fatalf("bob: %v %v", r, err)
	}
	if r, err := bob.claimAward(pot, win); err != nil || r["already_paid_to_you"] != "1.000000" {
		t.Fatalf("bob again: %v %v", r, err)
	}
	if _, err := carol.claimAward(pot, win); err == nil || !strings.Contains(err.Error(), "already paid") {
		t.Fatalf("carol after: %v", err)
	}
	// lock show: what kind of lock, about what.
	l, _ := w.ch.call("Bank", "locks", u(pot))
	if k := alice.lockKind(alice.conditionName(l[7].(common.Address)), l[8].([]byte)); !strings.Contains(k, "pot") {
		t.Fatalf("kind %q", k)
	}
	if d, _ := alice.decodeAttestationDemand(l[8].([]byte)); !strings.Contains(fmt.Sprint(d["about"]), "fix issue #3") {
		t.Fatalf("subject not in words: %v", d)
	}
	// A subscription shows its terms and what is due.
	sub, err := alice.subscribe("bob", exact("0.1"), 3, 60)
	if err != nil {
		t.Fatal(err)
	}
	l, _ = w.ch.call("Bank", "locks", u(sub))
	if v := alice.subscriptionView(l); v["collectable_now"] != "0.100000" || v["per_period"] != "0.100000" || v["periods"] != "3" {
		t.Fatalf("subscription view %v", v)
	}
}

// A tainted agent's bounty refused by the chain keeps the owner's approval,
// and the refusal says the limit.
func TestRefusedBountyKeepsApproval(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	mallory, _ := w.owner("mallory")
	w.owner("hakem")
	oscar.deposit(coins(5))
	ada := w.agent(oscar, "ada", coins(1), `{}`)
	mallory.send(SendOpts{To: "ada.oscar", Text: "hello"})
	scan(t, ada)
	oscar.approveAction("ada.oscar", "hakem", coins(2))
	h, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	_, _, err := ada.openBounty("hakem", coins(2), h.Time+86400, "a bounty")
	// The guard's spendCheck refuses it before the approval is touched (PROOFREAD-5 fix #5).
	if err == nil || !strings.Contains(err.Error(), "only 1.000000 are left") {
		t.Fatalf("over the limit: %v", err)
	}
	if _, _, err := ada.openBounty("hakem", coins(1), h.Time+86400, "a bounty"); err != nil {
		t.Fatalf("the approval was burnt by the refused bounty: %v", err)
	}
}

// Panelists who are a party never count: the view says so, and a panel
// that could never reach k is refused.
func TestPanelParties(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.owner("carol")
	w.owner("dave")
	alice.deposit(coins(2))
	h, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	if _, _, err := alice.openPanel("bob", coins(1), h.Time+86400, 2, []string{"alice", "bob", "carol"}, "x", h.Time+60); err == nil {
		t.Fatal("a panel that can't reach k was opened")
	}
	lock, subj, err := alice.openPanel("bob", coins(1), h.Time+86400, 2, []string{"bob", "carol", "dave"}, "x", h.Time+60)
	if err != nil {
		t.Fatal(err)
	}
	w.later(2 * time.Minute)
	if _, err := bob.rule(subj, 10000, "I win"); err == nil || !strings.Contains(err.Error(), "party's side") {
		t.Fatalf("a party's ruling was recorded: %v", err)
	}
	v, err := alice.panelView(lock)
	if err != nil {
		t.Fatal(err)
	}
	rs := v["rulings"].([]map[string]any)
	if len(rs) != 1 || rs[0]["counts"] != false || !strings.Contains(fmt.Sprint(rs[0]["why"]), "party") {
		t.Fatalf("a party's ruling: %v", rs)
	}
}

// Keys of an epoch nobody published (a long idle) are published on the first send.
func TestSendAfterLongIdle(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.later(20 * 24 * time.Hour)
	if _, err := bob.rotateEpochs(); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.send(SendOpts{To: "bob", Text: "back"}); err != nil {
		t.Fatalf("send after idle: %v", err)
	}
}

// The seller and a split's members hear of an item sale as "paid".
func TestPaidEventsForSalesAndSplits(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	bob.deposit(coins(5))
	split, err := alice.createSplit([]string{"alice", "carol"}, []uint16{7000, 3000})
	if err != nil {
		t.Fatal(err)
	}
	carol.pollEvents(nil, false) // learns the past
	alice.pollEvents(nil, false)
	sword, _ := alice.createItem("ipfs://sword.json", 10, false)
	alice.mintItem(sword, "alice", 1)
	offer, err := alice.offerItem(sword, 1, coins(1), "", split.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.buyItem(offer); err != nil {
		t.Fatal(err)
	}
	if err := bob.pay(split.Hex(), coins(1), "tip"); err != nil {
		t.Fatal(err)
	}
	evs, err := carol.pollEvents(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var paid []string
	for _, e := range evs {
		if e.Type == "paid" {
			paid = append(paid, e.Text)
		}
	}
	if len(paid) != 2 || !strings.Contains(strings.Join(paid, "|"), "0.300000") || !strings.Contains(strings.Join(paid, "|"), "item sale") {
		t.Fatalf("carol's paid events: %v", paid)
	}
	if evs, _ := alice.pollEvents(nil, false); len(evs) == 0 {
		t.Fatal("the seller heard nothing")
	}
}

// A first vote publishes the voter's ring key instead of failing.
func TestVoteWithoutPrivateEnable(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	if err := alice.createGroup("club", true); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.joinGroup("club"); err != nil {
		t.Fatal(err)
	}
	if err := alice.enableRing(); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.postAnon("club", "poll", "vote", []byte(`{"vote":"ferry"}`)); err != nil {
		t.Fatalf("vote: %v", err)
	}
	if ms, _ := alice.setMembers("club"); len(ms) != 2 {
		t.Fatalf("members %v", ms)
	}
	_ = big.NewInt
}

func TestHelpRunesAndDeciderWarning(t *testing.T) {
	if got := firstLine("правильный ответ на вопрос", 9); got != "правильны…" {
		t.Fatalf("cut mid-rune: %q", got)
	}
	if h := helpFor("reply"); !strings.Contains(h, "--attach") || strings.Contains(h, "devnet") {
		t.Fatalf("reply help: %s", h)
	}
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	alice.deposit(coins(2))
	h, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	lock, _, err := alice.openEscrow("bob", "alice", coins(1), h.Time+600, "my own arbiter")
	if err != nil {
		t.Fatal(err)
	}
	l, _ := w.ch.call("Bank", "locks", u(lock))
	if wn := alice.deciderWarning(l); !strings.Contains(wn, "payer's side") {
		t.Fatalf("no warning: %q", wn)
	}
	ada := w.agent(alice, "ada", coins(1), `{}`)
	alice.pay("ada.alice", exact("0.3"), "for you")
	e, _ := w.ch.epoch()
	if m := ada.agentEpochMoney(e); m["received_by_your_name"] != "0.300000" || m["limit"] != "1.000000" {
		t.Fatalf("agent money %v", m)
	}
}

// Tests don't wait for approvals in flight (TestApprovalFetchedOnDemand does).
func init() { approvalWait = 0 }

// cli runs the CLI against the test chain and returns what it printed.
func cli(t *testing.T, w *world, args ...string) (string, error) {
	t.Helper()
	testChain = w.ch
	defer func() { testChain = nil }()
	os.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	r, wr, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = wr
	err := run(args)
	wr.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b), err
}

// group post, its --anon form and reply honour --text-file (and - for stdin);
// an empty post is refused.
func TestTextFileForEveryTextCommand(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(alice.createGroup("club", true))
	if _, err := bob.joinGroup("club"); err != nil {
		t.Fatal(err)
	}
	w.must(bob.enableRing()) // a ring for anonymous posts needs another member's key
	dir := filepath.Join(w.dir, "alice")
	f := filepath.Join(t.TempDir(), "post.txt")
	os.WriteFile(f, []byte("it's \"quoted\" & multi\nline\n"), 0o600)
	if _, err := cli(t, w, "group", "post", "club", "--text-file", f, "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, w, "group", "post", "club", "--anon", "--thread", "t1", "--text-file", f, "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, w, "group", "post", "club", "--dir", dir); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty post: %v", err)
	}
	if _, err := cli(t, w, "group", "post", "club", "extra", "--text-file", f, "--dir", dir); err == nil {
		t.Fatal("text both as arguments and from a file")
	}
	n := 0
	for _, it := range groupPosts(t, bob, "club") {
		if it.Mail.Text == "it's \"quoted\" & multi\nline" {
			n++
		}
	}
	if n != 1 { // the two posts have the same text: one key in the map, two items
		t.Fatalf("posts from --text-file: %d", n)
	}
	for _, it := range bob.groupItems("club") {
		if it.Mail == nil || it.Mail.Text != "it's \"quoted\" & multi\nline" {
			t.Fatalf("a post arrived as %+v", it.Mail)
		}
	}
	// reply from stdin
	bob.setContact("alice", true)
	alice.setContact("bob", true)
	id, err := bob.send(SendOpts{To: "alice", Text: "hi"})
	w.must(err)
	scan(t, alice)
	r, wr, _ := os.Pipe()
	wr.WriteString("from stdin\n")
	wr.Close()
	old := os.Stdin
	os.Stdin = r
	_, err = cli(t, w, "reply", id[:18], "--text-file", "-", "--dir", dir)
	os.Stdin = old
	w.must(err)
	var got []string
	for _, it := range mailTo(scan(t, bob), tMessage) {
		if it.From == "alice" {
			got = append(got, it.Mail.Text)
		}
	}
	if len(got) != 1 || got[0] != "from stdin" {
		t.Fatalf("reply from stdin: %q", got)
	}
}

// No command prints secret keys: a group-key mail shows that keys came, not
// the keys; commit keeps its preimage to itself.
func TestSecretsNeverPrinted(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(alice.createGroup("secret-club", false))
	if _, err := bob.joinGroup("secret-club"); err != nil {
		t.Fatal(err)
	}
	if done, err := alice.admitPending(); err != nil || len(done) != 1 {
		t.Fatalf("admit: %v %v", done, err)
	}
	var keyMail *Item
	for _, it := range scan(t, bob) {
		if it.Type == tGroupKey {
			keyMail = it
		}
	}
	if keyMail == nil || bob.st.Groups["secret-club"] == nil {
		t.Fatal("the group key mail didn't arrive or wasn't kept")
	}
	g := bob.st.Groups["secret-club"]
	secrets := append([]string{"AGE-SECRET-KEY", g.ViewKey}, g.Keys...)
	bdir := filepath.Join(w.dir, "bob")
	o1, err := cli(t, w, "read", keyMail.ID, "--dir", bdir)
	w.must(err)
	o2, err := cli(t, w, "inbox", "--all", "--dir", bdir)
	w.must(err)
	mcp, _ := runTool(bob, "tern_read", map[string]any{"id": keyMail.ID})
	o3, _ := json.Marshal(mcp)
	o4, err := cli(t, w, "commit", "dice", "six", "--dir", bdir)
	w.must(err)
	for i, o := range []string{o1, o2, string(o3), o4} {
		for _, s := range secrets {
			if strings.Contains(strings.ToUpper(o), strings.ToUpper(s)) {
				t.Fatalf("output %d prints a secret key: %s", i, o)
			}
		}
	}
	if !strings.Contains(o1, "stored in your identity dir") {
		t.Fatalf("read of a group-key mail: %s", o1)
	}
	for _, p := range bob.st.Secrets {
		if strings.Contains(o4, p) {
			t.Fatalf("commit printed its preimage: %s", o4)
		}
	}
}

// An approval the owner sends while the agent is already trying is fetched
// on demand: the agent's attempt succeeds without running inbox first.
func TestApprovalFetchedOnDemand(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	mallory, _ := w.owner("mallory")
	oscar.deposit(coins(5))
	ada := w.agent(oscar, "ada", coins(2), `{}`)
	mallory.send(SendOpts{To: "ada.oscar", Text: "hello"})
	scan(t, ada)
	approvalWait = 20 * time.Second
	defer func() { approvalWait = 0 }()
	go func() {
		time.Sleep(1500 * time.Millisecond)
		oscar.approveAction("ada.oscar", "mallory", coins(1))
	}()
	start := time.Now()
	if err := ada.pay("mallory", coins(1), "approved meanwhile"); err != nil {
		t.Fatalf("the approval in flight wasn't fetched: %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("waited too long for the approval")
	}
}

// grant --gas for an account agent of a plain-key owner funds the owner's
// paymaster deposit, so agent join works.
func TestGrantFundsGasDeposit(t *testing.T) {
	w := newWorld(t)
	carol, _ := w.owner("carol")
	w.owner("bob")
	bot := w.accountAgent(carol, "bot", coins(1), exact("0.05")) // fails at join without a deposit
	d, err := w.ch.call("TernPaymaster", "deposit", idOf("carol"))
	w.must(err)
	if d[0].(*big.Int).Sign() == 0 {
		t.Fatal("no deposit after the grant")
	}
	carol.deposit(coins(2))
	if err := bot.pay("bob", exact("0.1"), "gas from the deposit"); err != nil {
		t.Fatal(err)
	}
}

// scopes.to is a hard allowlist; the refusal says an approval can't widen it.
func TestScopesToRefusalExplains(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	w.owner("bob")
	w.owner("dave")
	oscar.deposit(coins(5))
	ada := w.agent(oscar, "ada", coins(2), `{"to":["bob"]}`)
	err := ada.pay("dave", exact("0.1"), "x")
	w.mustFail(err, "scopes.to only allows bob; an owner approval can't override it")
	w.mustFail(err, "owner grant")
	w.mustFail(oscar.approveAction("ada.oscar", "dave", coins(1)), "re-grant")
	if err := ada.pay("bob", exact("0.1"), "allowed"); err != nil {
		t.Fatal(err)
	}
}

// lock release after escrow release says the lock is paid out.
func TestReleasePaidOutLock(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	w.owner("carol")
	alice.deposit(coins(5))
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	id, _, err := alice.openEscrow("bob", "carol", coins(1), head.Time+600, "fix the bike")
	w.must(err)
	w.must(alice.releaseLock(id, nil))
	w.mustFail(alice.releaseLock(id, nil), fmt.Sprintf("lock %d is already fully paid out", id))
}

// commons show lists each member's name age (names are free: Sybil votes).
func TestCommonsShowsNameAge(t *testing.T) {
	w := newWorld(t)
	ann, _ := w.owner("ann")
	w.later(10 * 24 * time.Hour)
	ben, _ := w.owner("ben")
	w.must(ann.found("porch", CommonsParams{Term: 86400, VoteWindow: 600, VetoWindow: 600, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 5000, Budget: coins(1)}, ""))
	w.join("porch", ann, ben)
	v, err := ann.showCommunity("porch")
	w.must(err)
	ages := v["member_name_age"].(map[string]string)
	if !strings.HasPrefix(ages["ann"], "10.") || !strings.HasPrefix(ages["ben"], "0.") || v["members_under_a_week_old"] != 1 {
		t.Fatalf("name ages: %v, young: %v", ages, v["members_under_a_week_old"])
	}
}
