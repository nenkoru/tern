package main

import (
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// withholdingNode: a staked data node that signs receipts and commits roots
// but serves no bytes (batches, heads, buckets, pending, stream) while serve
// is false: "commit and withhold" (red team C2-WITHHOLD).
func withholdingNode(w *world, serve *atomic.Bool) (*Relay, *httptest.Server) {
	w.t.Helper()
	key, _ := crypto.GenerateKey()
	w.fund2(crypto.PubkeyToAddress(key.PublicKey), coins(10))
	node, err := newDataNode(w.ch, key, w.t.TempDir(), "", nil, false)
	w.must(err)
	inner := node.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		if !serve.Load() && req.Method == "GET" && (strings.HasPrefix(p, "/batch/") || strings.HasPrefix(p, "/heads/") ||
			strings.HasPrefix(p, "/bucket/") || p == "/pending" || p == "/stream") {
			http.Error(rw, "not here", 404)
			return
		}
		inner.ServeHTTP(rw, req)
	}))
	w.t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	w.must(node.ensureStake(srv.URL))
	return node, srv
}

func lastRoot(t *testing.T, w *world) common.Hash {
	t.Helper()
	evs, err := w.ch.eventsFrom(w.ch.dep.Block, "Inbox", "Committed")
	if err != nil || len(evs) == 0 {
		t.Fatalf("no committed root: %v", err)
	}
	return common.Hash(evs[len(evs)-1].Args["root"].([32]byte))
}

func rootState(t *testing.T, w *world, root common.Hash) uint8 {
	t.Helper()
	st, err := w.ch.call("Inbox", "commitment", root)
	w.must(err)
	return st[2].(uint8)
}

func copiesOf(c *Client, id string) int {
	n := 0
	for _, it := range c.st.Items {
		if it.ID == id {
			n++
		}
	}
	return n
}

// A node commits a batch and withholds it: the sender is told to challenge
// (never to claim the receipt: that would hand the node the bond), the
// envelopes go out again on chain and reach the
// recipient once, and the batch that stays unserved is challenged within its
// window and expired: the node is slashed through the existing Inbox game,
// and the sender ends up ahead, not a bond down.
func TestWithholdingNodeSlashedAutomatically(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.setContact("alice", true)
	var serve atomic.Bool
	bad, bsrv := withholdingNode(w, &serve)
	good, _ := newNode(w)
	badAddr := crypto.PubkeyToAddress(bad.key.PublicKey)
	alice.relay = bsrv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "through a node that withholds"})
	w.must(err)
	if alice.st.Sent[id].Env == "" {
		t.Fatal("the envelope's bytes weren't kept for a re-send")
	}
	_, err = alice.send(SendOpts{To: "bob", Text: "a second one in the batch"}) // a batch of two: its root isn't an envelope's hash
	w.must(err)
	w.must(bad.flush())
	root := lastRoot(t, w)
	stake0 := mustCall(t, w.ch, "Inbox", "stake", badAddr).(*big.Int)
	before, _ := w.ch.balance(alice.addr())

	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, alice)
	if !hasAlert(alice, "doesn't serve it") || !hasAlert(alice, "da challenge "+root.Hex()) || hasAlert(alice, "prove-missed") {
		t.Fatalf("alert: %v", alice.st.Alerts)
	}
	if _, err := alice.proveMissed(id); err == nil || !strings.Contains(err.Error(), "da challenge") {
		t.Fatalf("prove-missed against a node that committed: %v", err)
	}
	r := alice.st.Sent[id]
	if r.Resent != 1 || r.Via != "chain" || r.Receipt != nil || len(r.Earlier) != 1 || r.Earlier[0].Withheld != root.Hex() {
		t.Fatalf("not re-sent on chain: %+v", r)
	}
	good.mu.Lock()
	queued := len(good.envs)
	good.mu.Unlock()
	if queued != 0 {
		t.Fatal("re-sent the withheld batch to a node that cannot commit its root again")
	}
	w.must(good.flush())
	if got := mailTo(scan(t, bob), tMessage); len(got) != 2 || copiesOf(bob, id) != 1 {
		t.Fatalf("bob: %+v", got)
	}
	scan(t, alice)
	if r := alice.st.Sent[id]; r.Block == 0 || r.Env != "" || hasAlert(alice, "your message "+id[:18]) {
		t.Fatalf("the re-sent copy didn't land for alice: %+v %v", r, alice.st.Alerts)
	}

	for i := 0; i < 8 && rootState(t, w, root) == 1; i++ {
		w.later(8 * time.Minute)
		scan(t, alice)
	}
	if rootState(t, w, root) != 2 || !hasAlert(alice, "challenged with a bond") {
		t.Fatalf("not challenged: state %d, %v", rootState(t, w, root), alice.st.Alerts)
	}
	if u := alice.st.Unserved[root.Hex()]; u == nil || u.Misses < daChallengeMisses || u.Challenged == 0 {
		t.Fatalf("challenged too early: %+v", u)
	}
	w.later(31 * time.Minute)
	scan(t, alice)
	if rootState(t, w, root) != 4 || !hasAlert(alice, "slashed") || len(alice.st.Unserved) != 0 {
		t.Fatalf("not expired: state %d %v", rootState(t, w, root), alice.st.Alerts)
	}
	if s := mustCall(t, w.ch, "Inbox", "stake", badAddr).(*big.Int); s.Cmp(stake0) >= 0 {
		t.Fatalf("the withholding node kept its stake: %s", fmtCoins(s))
	}
	after, _ := w.ch.balance(alice.addr())
	if after.Cmp(before) <= 0 {
		t.Fatalf("alice paid for it: %s -> %s", fmtCoins(before), fmtCoins(after))
	}
	scan(t, bob)
	if n := copiesOf(bob, id); n != 1 {
		t.Fatalf("bob holds %d copies", n)
	}
}

// The node resolves when challenged: the batch comes on chain with the
// original envelope, the re-sent copy was read already, and the recipient
// keeps one. A batch a node serves again is never challenged.
func TestWithheldBatchResolvedNoDuplicate(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	bob.setContact("alice", true)
	var serve atomic.Bool
	node, srv := withholdingNode(w, &serve)
	alice.relay, carol.relay = srv.URL, srv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "resolved later"})
	w.must(err)
	_, err = carol.send(SendOpts{To: "bob", Text: "carol's, served later"})
	w.must(err)
	w.must(node.flush())
	root := lastRoot(t, w)

	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, alice)
	if !hasAlert(alice, "re-sent on chain") {
		t.Fatalf("no other node: not re-sent on chain: %v", alice.st.Alerts)
	}
	if got := mailTo(scan(t, bob), tMessage); len(got) != 1 || got[0].ID != id {
		t.Fatalf("bob: %+v", got)
	}
	for i := 0; i < 8 && rootState(t, w, root) == 1; i++ {
		w.later(8 * time.Minute)
		scan(t, alice)
	}
	if rootState(t, w, root) != 2 {
		t.Fatal("not challenged")
	}
	w.must(node.resolveChallenges())
	if rootState(t, w, root) != 3 {
		t.Fatal("not resolved")
	}
	scan(t, alice)
	if !hasAlert(alice, "put on chain after our challenge") || len(alice.st.Unserved) != 0 {
		t.Fatalf("alice: %v", alice.st.Alerts)
	}
	scan(t, bob)
	if n := copiesOf(bob, id); n != 1 {
		t.Fatalf("bob holds %d copies of the re-sent envelope", n)
	}

	// carol's client: the same batch, now on chain; and a second withheld
	// batch that its node serves again before it is due: never challenged.
	id2, err := carol.send(SendOpts{To: "bob", Text: "served in the end"})
	w.must(err)
	w.must(node.flush())
	root2 := lastRoot(t, w)
	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, carol)
	if carol.st.Unserved[root2.Hex()] == nil {
		t.Fatalf("carol isn't chasing the second batch: %v", carol.st.Alerts)
	}
	serve.Store(true)
	for i := 0; i < 4; i++ {
		w.later(8 * time.Minute)
		scan(t, carol)
	}
	if rootState(t, w, root2) != 1 || len(carol.st.Unserved) != 0 || hasAlert(carol, "challenged with a bond") {
		t.Fatalf("a served batch was challenged, or is still chased: state %d %v", rootState(t, w, root2), carol.st.Alerts)
	}
	scan(t, bob)
	if copiesOf(bob, id2) != 1 {
		t.Fatalf("bob holds %d copies of %s", copiesOf(bob, id2), id2[:10])
	}
}

// A node signs for an envelope and commits nothing: the claim still works
// after the re-send landed (the earlier receipt), and slashes the node.
func TestResentAfterMissedReceiptStillClaimable(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.setContact("alice", true)
	node, srv := newNode(w)
	alice.relay = srv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "dropped, then re-sent"})
	w.must(err)
	node.mu.Lock()
	node.envs = nil
	node.mu.Unlock()
	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, alice)
	if !hasAlert(alice, "committed no batch") || !hasAlert(alice, "re-sent on chain") {
		t.Fatalf("alerts: %v", alice.st.Alerts)
	}
	scan(t, alice)
	if !hasAlert(alice, "reached the chain after a re-send") || !hasAlert(alice, "prove-missed "+id[:18]) {
		t.Fatalf("alerts: %v", alice.st.Alerts)
	}
	if got := mailTo(scan(t, bob), tMessage); len(got) != 1 || got[0].ID != id {
		t.Fatalf("bob: %+v", got)
	}
	_, err = alice.proveMissed(id)
	w.must(err)
	w.must(node.rebutMisses()) // nothing to rebut with
	w.later(31 * time.Minute)
	_, err = alice.slashMissed(id)
	w.must(err)
	if s := mustCall(t, w.ch, "Inbox", "stake", crypto.PubkeyToAddress(node.key.PublicKey)).(*big.Int); s.Sign() != 0 {
		t.Fatalf("the node kept %s", fmtCoins(s))
	}
}

// Automatic retry/challenge are off; explicit refresh remains available.
func TestDAAutoOff(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	var serve atomic.Bool
	node, srv := withholdingNode(w, &serve)
	alice.relay = srv.URL
	w.must(alice.setDAAuto("off", 0))
	if v := alice.daAutoView(); v["da_autochallenge"] != "off" || v["da_resend"] != 0 {
		t.Fatalf("settings: %v", v)
	}
	if err := alice.setDAAuto("maybe", -1); err == nil {
		t.Fatal("a bad --challenge was taken")
	}
	id, err := alice.send(SendOpts{To: "bob", Text: "no auto"})
	w.must(err)
	if alice.st.Sent[id].Env == "" || !alice.st.Sent[id].Refreshable {
		t.Fatal("manual refresh source missing with automatic re-sends off")
	}
	w.must(node.flush())
	root := lastRoot(t, w)
	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, alice)
	if !hasAlert(alice, "da_autochallenge is off") || !hasAlert(alice, "Send the message again") || hasAlert(alice, "re-sent") {
		t.Fatalf("alerts: %v", alice.st.Alerts)
	}
	for i := 0; i < 5; i++ {
		w.later(8 * time.Minute)
		scan(t, alice)
	}
	if rootState(t, w, root) != 1 {
		t.Fatal("challenged with da_autochallenge off")
	}
	if r := alice.st.Sent[id]; r.Resent != 0 || r.Env == "" {
		t.Fatal("automatic re-send ran or discarded the manual refresh source")
	}
}

// A withheld batch of one envelope: its root is the envelope's hash, which no
// other node could commit again (and would then miss its own receipt): the
// re-send goes on chain, not to the other node; and the sender doesn't take
// the committed root for its envelope having landed.
func TestLoneWithheldEnvelopeResentOnChain(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.setContact("alice", true)
	var serve atomic.Bool
	bad, bsrv := withholdingNode(w, &serve)
	good, _ := newNode(w)
	alice.relay = bsrv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "alone in its batch"})
	w.must(err)
	w.must(bad.flush())
	if lastRoot(t, w).Hex() != id {
		t.Fatal("not a batch of its own")
	}
	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, alice)
	if r := alice.st.Sent[id]; r.Via != "chain" || r.Withheld != "" || len(r.Earlier) != 1 || r.Earlier[0].Withheld != id {
		t.Fatalf("not re-sent on chain: %+v %v", r, alice.st.Alerts)
	}
	good.mu.Lock()
	queued := len(good.envs)
	good.mu.Unlock()
	if queued != 0 {
		t.Fatal("handed to a node that couldn't commit it")
	}
	if got := mailTo(scan(t, bob), tMessage); len(got) != 1 || got[0].ID != id {
		t.Fatalf("bob: %+v", got)
	}
}

// A node that never commits still permits a retry through a different node.
func TestUncommittedEnvelopeResentThroughAnotherNode(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.setContact("alice", true)
	bad, srv := newNode(w)
	good, _ := newNode(w)
	alice.relay = srv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "retry through a different node"})
	w.must(err)
	bad.mu.Lock()
	bad.envs = nil
	bad.mu.Unlock()
	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, alice)
	r := alice.st.Sent[id]
	if r.Resent != 1 || r.Via == "chain" || r.Via == srv.URL || r.Receipt == nil || len(r.Earlier) != 1 {
		t.Fatalf("not re-sent through the other node: %+v", r)
	}
	w.must(good.flush())
	if got := mailTo(scan(t, bob), tMessage); len(got) != 1 || got[0].ID != id || copiesOf(bob, id) != 1 {
		t.Fatalf("bob: %+v", got)
	}
}

func TestWithheldResendPreservesStrictPrivacy(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	var serve atomic.Bool
	bad, srv := withholdingNode(w, &serve)
	good, _ := newNode(w)
	alice.relay = srv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "do not expose my posting account"})
	w.must(err)
	w.must(bad.flush())
	oldPrivacy := privacyLevel
	privacyLevel = privacyStrict
	t.Cleanup(func() { privacyLevel = oldPrivacy })
	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, alice)
	if r := alice.st.Sent[id]; r.Via != srv.URL || len(r.Earlier) != 0 || !r.Lost {
		t.Fatalf("strict privacy re-posted the envelope: %+v", r)
	}
	if !hasAlert(alice, "strict privacy never posts from your own account") {
		t.Fatalf("missing privacy alert: %v", alice.st.Alerts)
	}
	good.mu.Lock()
	queued := len(good.envs)
	good.mu.Unlock()
	if queued != 0 || copiesOf(bob, id) != 0 {
		t.Fatal("strict retry published the envelope")
	}
}
