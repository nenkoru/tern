//go:build redteam || redteam_c

// Red team, scenario C: a malicious data node, RPC, relay or bundler.
//
// Each test asserts what the code ACTUALLY does today. Where the attack
// works, the assertion pins the vulnerable behaviour and is marked FINDING.
// Run: go test -tags redteam_c -run TestRedTeamC -count=1 -v ./cmd/ternchain
package main

import (
	"context"
	"encoding/json"
	"math/big"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// ---- helpers (rtc*)

// rtcWithholdingNode: a staked data node that accepts envelopes, signs
// receipts and commits roots on chain, but never serves the bytes (batches,
// heads, buckets, pending memory, streams): "commit and withhold".
func rtcWithholdingNode(w *world) (*Relay, *httptest.Server) {
	w.t.Helper()
	key, _ := crypto.GenerateKey()
	w.fund2(crypto.PubkeyToAddress(key.PublicKey), coins(10))
	node, err := newDataNode(w.ch, key, w.t.TempDir(), "", nil, false)
	w.must(err)
	inner := node.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		if req.Method == "GET" && (strings.HasPrefix(p, "/batch/") || strings.HasPrefix(p, "/heads/") ||
			strings.HasPrefix(p, "/bucket/") || p == "/pending" || p == "/stream") {
			http.Error(rw, "not here", 404) // the batch "isn't here"
			return
		}
		inner.ServeHTTP(rw, req)
	}))
	w.t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	w.must(node.ensureStake(srv.URL))
	return node, srv
}

// rtcLiarNode: a staked node that serves garbage for every batch it is asked for.
func rtcLiarNode(w *world) *httptest.Server {
	w.t.Helper()
	key, _ := crypto.GenerateKey()
	w.fund2(crypto.PubkeyToAddress(key.PublicKey), coins(10))
	node, err := newDataNode(w.ch, key, w.t.TempDir(), "", nil, false)
	w.must(err)
	inner := node.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/batch/") {
			rw.Write(randBytes(700)) // not the bytes of that root
			return
		}
		inner.ServeHTTP(rw, req)
	}))
	w.t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	w.must(node.ensureStake(srv.URL))
	return srv
}

func rtcHasText(items []*Item, text string) bool {
	for _, it := range items {
		if it.Mail != nil && it.Mail.Text == text && it.Status != "refused" && it.Status != "undelivered" {
			return true
		}
	}
	return false
}

func rtcAlert(c *Client, s string) string {
	for _, a := range c.st.Alerts {
		if strings.Contains(a, s) {
			return a
		}
	}
	return ""
}

// rtcFindEnvelope: the bytes of envelope id, from the batches posted in full on chain.
func rtcFindEnvelope(t *testing.T, ch *Chain, id string) Envelope {
	t.Helper()
	evs, err := ch.eventsFrom(ch.dep.Block, "Inbox", "Posted")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		es, _ := decodeBatch(ev.Args["batch"].([]byte))
		for _, e := range es {
			if e.hash().Hex() == id {
				return e
			}
		}
	}
	t.Fatalf("envelope %s is not on chain", id[:10])
	return Envelope{}
}

func rtcLastCommittedRoot(t *testing.T, ch *Chain) common.Hash {
	t.Helper()
	evs, err := ch.eventsFrom(ch.dep.Block, "Inbox", "Committed")
	if err != nil || len(evs) == 0 {
		t.Fatalf("no committed root: %v", err)
	}
	return common.Hash(evs[len(evs)-1].Args["root"].([32]byte))
}

// ---- C2: selective dropping / censorship by "commit and withhold"

// A node that signs receipts and commits the root, but never serves the
// bytes. Receipts can't slash it (it rebuts with a Merkle proof and earns the
// claimant's bond); before the fix no client challenged a batch it can't
// fetch, and recipients gave the batch up after the horizon.
// Fixed (C2-WITHHOLD): the sender's alert names the commit and the challenge,
// prove-missed refuses it; the senders re-send the same envelopes; a receipt
// holder challenges the batch in the window and expires it: the node is slashed.
func TestRedTeamC_CommitAndWithhold(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	bob.setContact("alice", true)
	bob.setContact("carol", true)
	node, srv := rtcWithholdingNode(w)
	nodeAddr := crypto.PubkeyToAddress(node.key.PublicKey)
	alice.relay, carol.relay = srv.URL, srv.URL // bob reads the chain and the poster's node

	idA, err := alice.send(SendOpts{To: "bob", Text: "alice: withheld?"})
	w.must(err)
	idC, err := carol.send(SendOpts{To: "bob", Text: "carol: withheld?"})
	w.must(err)
	if alice.st.Sent[idA].Receipt == nil || carol.st.Sent[idC].Receipt == nil {
		t.Fatal("the node gave no receipts")
	}
	w.must(node.flush()) // commits the root on chain
	root := rtcLastCommittedRoot(t, w.ch)
	stake0 := mustCall(t, w.ch, "Inbox", "stake", nodeAddr).(*big.Int)

	if got := scan(t, bob); rtcHasText(got, "alice: withheld?") || rtcHasText(got, "carol: withheld?") {
		t.Fatal("bob read a withheld batch")
	}
	if _, ok := bob.st.PendingDA[root.Hex()]; !ok {
		t.Fatal("the withheld root isn't pending for bob")
	}

	// The sender's side: the alert names the commit and the challenge, not a
	// missed-receipt claim; the claim itself is refused (it would cost the bond).
	aliceBefore, _ := w.ch.balance(alice.addr())
	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, alice)
	scan(t, carol)
	a := rtcAlert(alice, "doesn't serve it")
	// FINDING C2-WITHHOLD (sender side), fixed: no advice that hands the bond to the node.
	if a == "" || !strings.Contains(a, "da challenge "+root.Hex()) || rtcAlert(alice, "prove-missed") != "" {
		t.Fatalf("alice's alert: %v", alice.st.Alerts)
	}
	t.Logf("RESULT C2a alice's alert: %s", a)
	if _, err := alice.proveMissed(idA); err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("prove-missed against a withholding node: %v", err)
	}
	if rtcAlert(alice, "re-sent on chain") == "" || rtcAlert(carol, "re-sent on chain") == "" {
		t.Fatalf("not re-sent: %v / %v", alice.st.Alerts, carol.st.Alerts)
	}
	// The re-sent envelopes (the same ids) reach bob, once each.
	scan(t, bob)
	if !rtcHasText(bob.st.Items, "alice: withheld?") || !rtcHasText(bob.st.Items, "carol: withheld?") {
		t.Fatalf("the re-sent letters didn't reach bob: %d items", len(bob.st.Items))
	}

	// The receipt holders keep asking; the root that stays unserved is challenged
	// in the window, then expired: the node is slashed, the challenger paid.
	for i := 0; i < 8 && mustCall2(t, w.ch, "Inbox", "commitment", root)[2].(uint8) == 1; i++ {
		w.later(8 * time.Minute)
		scan(t, alice)
		scan(t, carol)
	}
	st := mustCall2(t, w.ch, "Inbox", "commitment", root)
	// FINDING C2-WITHHOLD (reader side), fixed: the batch is challenged in its window.
	if st[2].(uint8) != 2 {
		t.Fatalf("the root's state is %d, want 2 (challenged)", st[2].(uint8))
	}
	if ch := mustCall(t, w.ch, "Inbox", "challenger", root).(common.Address); ch != alice.addr() {
		t.Fatalf("challenged by %s, not alice", ch.Hex())
	}
	w.later(31 * time.Minute) // the node doesn't resolve
	scan(t, alice)
	scan(t, carol)
	if st := mustCall2(t, w.ch, "Inbox", "commitment", root); st[2].(uint8) != 4 {
		t.Fatalf("the root's state is %d, want 4 (lost: the node slashed)", st[2].(uint8))
	}
	s := mustCall(t, w.ch, "Inbox", "stake", nodeAddr).(*big.Int)
	if s.Cmp(stake0) >= 0 {
		t.Fatalf("the node's stake didn't move: %s", fmtCoins(s))
	}
	aliceAfter, _ := w.ch.balance(alice.addr())
	if aliceAfter.Cmp(aliceBefore) <= 0 {
		t.Fatalf("alice is out of pocket: %s -> %s", fmtCoins(aliceBefore), fmtCoins(aliceAfter))
	}
	for _, id := range []string{idA, idC} {
		n := 0
		for _, it := range bob.st.Items {
			if it.ID == id {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("bob holds %d copies of %s", n, id[:10])
		}
	}
	t.Logf("RESULT C2-WITHHOLD HELD: alert names the commit and 'da challenge'; prove-missed refused; both letters re-sent and read once; "+
		"root challenged by alice and expired: node stake %s -> %s; alice net +%s", fmtCoins(stake0), fmtCoins(s), fmtCoins(new(big.Int).Sub(aliceAfter, aliceBefore)))
}

// The manual defense: a reader that runs 'da challenge ROOT' in time; the
// withholding operator doesn't resolve, the challenger takes the stake, the
// content is gone.
func TestRedTeamC_WithholdChallengeExpires(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.setContact("alice", true)
	node, srv := rtcWithholdingNode(w)
	nodeAddr := crypto.PubkeyToAddress(node.key.PublicKey)
	alice.relay = srv.URL
	_, err := alice.send(SendOpts{To: "bob", Text: "lost forever"})
	w.must(err)
	w.must(node.flush())
	root := rtcLastCommittedRoot(t, w.ch)
	scan(t, bob)
	bond := mustCall(t, w.ch, "Inbox", "challengeBond").(*big.Int)
	_, err = w.ch.send(bob.key, bond, "Inbox", "challenge", root)
	w.must(err)
	w.later(31 * time.Minute) // the node doesn't resolve
	before, _ := w.ch.balance(bob.addr())
	_, err = w.ch.send(bob.key, nil, "Inbox", "expire", root)
	w.must(err)
	after, _ := w.ch.balance(bob.addr())
	if s := mustCall(t, w.ch, "Inbox", "stake", nodeAddr).(*big.Int); s.Sign() != 0 {
		t.Fatalf("the withholding node kept %s", fmtCoins(s))
	}
	if rtcHasText(scan(t, bob), "lost forever") {
		t.Fatal("an expired batch was read")
	}
	t.Logf("RESULT C2d manual challenge: node slashed (challenger +%s), but the letter is gone", fmtCoins(new(big.Int).Sub(after, before)))
}

func mustCall2(t *testing.T, ch *Chain, c, m string, args ...any) []any {
	t.Helper()
	out, err := ch.call(c, m, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ---- C2: a plain relay drops: no fallback, an alert after sentGrace

func TestRedTeamC_PlainRelayDropsNoFallback(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.setContact("alice", true)
	// A relay (unstaked) that answers 202 and keeps nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/info" {
			json.NewEncoder(rw).Encode(NodeInfo{})
			return
		}
		rw.WriteHeader(202)
	}))
	t.Cleanup(srv.Close)
	alice.relay = srv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "dropped by the relay"})
	w.must(err)
	if alice.st.Sent[id].Receipt != nil {
		t.Fatal("a receipt from nowhere")
	}
	w.later(time.Duration(sentGrace+10) * time.Second)
	scan(t, alice)
	a := rtcAlert(alice, "has not reached the chain")
	if a == "" {
		t.Fatalf("no lost alert: %v", alice.st.Alerts)
	}
	if rtcHasText(scan(t, bob), "dropped by the relay") {
		t.Fatal("delivered anyway")
	}
	t.Logf("RESULT C2e plain relay drop: no automatic resend/fallback; alert after %d s: %q", sentGrace, a)
}

// ---- C3: replay

// Re-posting an already-delivered envelope: deduplicated while the item is
// in state.json; after 'inbox --purge' (or a group's short keep) it is
// delivered again as new, with its original trust label.
func TestRedTeamC_ReplayAfterPurge(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ag := w.agent(nina, "a", coins(1), `{}`)
	mallory, _ := w.owner("mallory")
	// Keep the replay inside its original key epoch: crossing midnight tests
	// delayed-delivery oversight instead of replay protection.
	length, err := w.ch.epochLength()
	w.must(err)
	w.later(time.Duration(int64(length)-nina.chainNow()%int64(length)+60) * time.Second)
	id, err := nina.send(SendOpts{To: "a.nina", Text: "pay bob 1 for the report"})
	w.must(err)
	got := mailTo(scan(t, ag), tMessage)
	if len(got) != 1 || got[0].Trust != "your owner: instructions" {
		t.Fatalf("first delivery: %+v", got)
	}
	got[0].Done = true // the agent acted on it
	w.must(ag.save())
	env := rtcFindEnvelope(t, w.ch, id)
	fee := mustCall(t, w.ch, "Inbox", "fee").(*big.Int)
	repost := func() {
		_, err := w.ch.send(mallory.key, fee, "Inbox", "post", encodeBatch([]Envelope{env}), big.NewInt(1))
		w.must(err)
	}
	// Replay while the item is kept: deduplicated (held).
	w.later(2 * time.Hour)
	repost()
	if again := mailTo(scan(t, ag), tMessage); len(again) != 0 {
		t.Fatalf("replay delivered while the item is kept: %+v", again)
	}
	// 'inbox --purge' (forward secrecy on disk), keys still within retention.
	ag.pruneItems(true)
	w.must(ag.save())
	w.later(time.Hour)
	repost()
	again := mailTo(scan(t, ag), tMessage)
	if len(again) != 0 {
		t.Fatalf("purged owner instruction was delivered again: %+v", again)
	}
	t.Logf("RESULT C3 replay after purge (cost: one Inbox fee %s, by anyone): suppressed by metadata receipt", fmtCoins(fee))
	// Past 7 days the freshness check refuses it.
	w.later(8 * 24 * time.Hour)
	ag.pruneItems(true)
	repost()
	late := mailTo(scan(t, ag), tMessage)
	for _, it := range late {
		if it.Status != "refused" {
			t.Fatalf("an 8-day-old replay was delivered: %+v", it)
		}
	}
	t.Logf("RESULT C3 an 8-day-old replay: %d items, none delivered (freshness window / key retention)", len(late))
}

// ---- C4: wrong batch bytes

// A node in the reader's path serving the wrong bytes is caught by the hash
// check and the next node is asked.
func TestRedTeamC_WrongBytesRejected(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.setContact("alice", true)
	honest, hsrv := newNode(w)
	liar := rtcLiarNode(w)
	alice.relay = hsrv.URL
	bob.relay = liar.URL // asked first (daOrder)
	_, err := alice.send(SendOpts{To: "bob", Text: "integrity"})
	w.must(err)
	w.must(honest.flush())
	if !rtcHasText(scan(t, bob), "integrity") {
		t.Fatal("bob didn't get the mail past the lying node")
	}
	// Only the liar reachable: never accepted, retried.
	_, err = alice.send(SendOpts{To: "bob", Text: "integrity 2"})
	w.must(err)
	w.must(honest.flush())
	hsrv.Close()
	old := fetchBatchFn
	var wrong int
	fetchBatchFn = func(url string, root common.Hash) ([]byte, error) {
		b, err := fetchBatch(url, root)
		if err != nil && strings.Contains(err.Error(), "wrong bytes") {
			wrong++
		}
		return b, err
	}
	t.Cleanup(func() { fetchBatchFn = old })
	if rtcHasText(scan(t, bob), "integrity 2") {
		t.Fatal("read from garbage")
	}
	if wrong == 0 {
		t.Fatal("the hash check never fired")
	}
	t.Logf("RESULT C4 wrong bytes rejected by the hash check (%d times); honest copy read when reachable", wrong)
}

// ---- RPC: forged logs

// rtcForgeLogs: an RPC that answers honestly, except that it adds a forged
// KeysPublished log naming the victim with the attacker's keys.
type rtcForgeLogs struct {
	Backend
	match  func(q ethereum.FilterQuery) bool
	forged types.Log
}

func (f rtcForgeLogs) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	logs, err := f.Backend.FilterLogs(ctx, q)
	if err != nil {
		return logs, err
	}
	if f.match(q) {
		logs = append(logs, f.forged)
	}
	return logs, nil
}

// A malicious RPC substitutes the recipient's epoch keys.
// Before redteam/fix-key-pin: the sender sealed to the attacker, with no
// warning (FINDING C3-RPC). Now a KeysPublished log counts only when its
// transaction is the name's controller's: the forged log's transaction is
// mallory's, so it is never taken, the send is refused with an alert, and
// nothing is pinned through that RPC (keypin.go).
func TestRedTeamC_RPCForgedRecipientKeys(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	mallory, _ := w.owner("mallory")
	bob.setContact("alice", true)
	reg := w.ch.addr("Registry")
	topic := w.ch.abis["Registry"].Events["KeysPublished"].ID
	ml, err := w.ch.b.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: big.NewInt(0), Addresses: []common.Address{reg},
		Topics: [][]common.Hash{{topic}, {idOf("mallory")}}})
	if err != nil || len(ml) == 0 {
		t.Fatalf("mallory's keys: %v", err)
	}
	forged := ml[len(ml)-1]
	forged.Topics = []common.Hash{topic, idOf("bob")}
	forged.Index += 1000
	bobID := idOf("bob")
	fake := rtcForgeLogs{Backend: w.ch.b, forged: forged, match: func(q ethereum.FilterQuery) bool {
		if len(q.Addresses) > 0 && !containsAddr(q.Addresses, reg) {
			return false
		}
		if len(q.Topics) > 0 && len(q.Topics[0]) > 0 && !containsHash(q.Topics[0], topic) {
			return false
		}
		if len(q.Topics) > 1 && len(q.Topics[1]) > 0 && !containsHash(q.Topics[1], bobID) {
			return false
		}
		if q.FromBlock != nil && q.FromBlock.Uint64() > forged.BlockNumber || q.ToBlock != nil && q.ToBlock.Uint64() < forged.BlockNumber {
			return false
		}
		return true
	}}
	ch2, err := newChain(fake, w.ch.dep, w.ch.mine)
	w.must(err)
	alice2, err := openClient(alice.dir, ch2)
	w.must(err)
	e, err := ch2.epoch()
	w.must(err)
	_, fp, err := alice2.res.keysAt("bob", e)
	w.must(err)
	_, mfp, _ := alice.res.keysAt("mallory", e)
	_, bfp, _ := bob.res.keysAt("bob", e)
	if fp == mfp || fp != bfp {
		t.Fatalf("the forged log was taken: %s (mallory %s, bob %s)", fp, mfp, bfp)
	}
	sent := len(alice2.st.Sent)
	_, err = alice2.send(SendOpts{To: "bob", Text: "for bob's eyes only"})
	if err == nil || !strings.Contains(err.Error(), "didn't sign") {
		t.Fatalf("send through the forging RPC: %v", err)
	}
	if !hasAlert(alice2, "didn't sign") || alice2.pin("bob") != nil || len(alice2.st.Sent) != sent {
		t.Fatalf("no alert, a pin, or something sent: %q", alice2.st.Alerts)
	}
	for _, it := range scan(t, mallory) {
		if it.Mail != nil && it.Mail.Text == "for bob's eyes only" {
			t.Fatal("mallory read it")
		}
	}
	t.Logf("RESULT RPC forged KeysPublished: HELD (fixed): the log's transaction is mallory's, so it is never taken; the send is refused with an alert (%q)", err)
}

func containsAddr(xs []common.Address, a common.Address) bool {
	for _, x := range xs {
		if x == a {
			return true
		}
	}
	return false
}

// ---- C1: what a node learns

// Bucket decoys are fresh each epoch while a name's real bucket is computed
// from its PUBLIC view key: a node that links a reader's queries across epochs
// (the same IP, a stream that reconnects) intersects them and names the reader.
func TestRedTeamC_BucketDecoysIntersection(t *testing.T) {
	w := newWorld(t)
	bob, _ := w.owner("bob")
	for _, bits := range []int{10, 12} {
		for _, decoys := range []int{3, strictDecoys} {
			w.ch.dep.BucketBits = bits
			old := bucketDecoys
			bucketDecoys = decoys
			bob.bkMemo = nil
			const n = 100_000 // names in the directory (their view keys are public)
			r := rand.New(rand.NewPCG(uint64(bits), uint64(decoys)))
			keys := make([][]byte, n)
			for i := range keys {
				k := make([]byte, 32)
				for j := range k {
					k[j] = byte(r.Uint32())
				}
				keys[i] = k
			}
			bobPub := bob.view.PublicKey().Bytes()
			keys = append(keys, bobPub)
			e0, _ := w.ch.epoch()
			cand := make([]bool, len(keys))
			for i := range cand {
				cand[i] = true
			}
			var trail []int
			for k := 0; k < 6; k++ {
				e := e0 + uint64(k)
				q := bob.queryBuckets(e) // what bob's queries show the node this epoch
				left := 0
				for i, kk := range keys {
					if cand[i] && !q[bucketOf(e, kk, bits)] {
						cand[i] = false
					}
					if cand[i] {
						left++
					}
				}
				trail = append(trail, left)
				if left == 1 {
					break
				}
			}
			bucketDecoys = old
			if !cand[len(keys)-1] {
				t.Fatal("bob dropped out of his own candidate set")
			}
			// FINDING C1-BUCKETS: one candidate left (bob) after a few epochs of linked queries.
			t.Logf("RESULT C1 bits=%d decoys=%d, %d names: candidates per epoch %v (1 = the reader named)", bits, decoys, n+1, trail)
			if trail[len(trail)-1] != 1 {
				t.Fatalf("bits=%d decoys=%d: still %d candidates after %d epochs", bits, decoys, trail[len(trail)-1], len(trail))
			}
		}
	}
}

// Envelope sizes follow the text: not padded.
func TestRedTeamC_EnvelopeSizesUnpadded(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	var sizes []int
	old := capture
	capture = func(e Envelope) { sizes = append(sizes, len(e.encode())) }
	t.Cleanup(func() { capture = old })
	_, err := alice.send(SendOpts{To: "bob", Text: "ok"})
	w.must(err)
	n1 := len(sizes)
	_, err = alice.send(SendOpts{To: "bob", Text: common.Bytes2Hex(randBytes(1500))}) // incompressible: 3000 characters
	w.must(err)
	if n1 == 0 || len(sizes) == n1 {
		t.Fatal("nothing captured")
	}
	short, long := sizes[0], sizes[len(sizes)-1]
	if long-short < 2000 { // 3000 more characters of text
		t.Fatalf("sizes look padded: %v", sizes)
	}
	t.Logf("RESULT C1 envelope sizes: short %d B, long %d B (no size buckets): a node or chain observer reads message length", short, long)
}

// Standard privacy without Tor goes direct (a stderr note only); strict refuses.
func TestRedTeamC_PrivacyDefaultGoesDirect(t *testing.T) {
	oldPorts, oldLevel, oldProxy, oldNoted, oldDecoys := torPorts, privacyLevel, proxyInUse, privacyNoted, bucketDecoys
	t.Cleanup(func() {
		torPorts, privacyLevel, proxyInUse, privacyNoted, bucketDecoys = oldPorts, oldLevel, oldProxy, oldNoted, oldDecoys
	})
	t.Setenv("TERNCHAIN_QUIET", "1")
	torPorts = []string{"127.0.0.1:1"} // no Tor here
	proxyInUse = ""
	privacyLevel = privacyStandard
	if err := ensurePrivacy([]string{"https://rpc.example.org"}); err != nil || proxyInUse != "" {
		t.Fatalf("standard: %v, proxy %q", err, proxyInUse)
	}
	privacyLevel = privacyStrict
	if err := ensurePrivacy([]string{"https://rpc.example.org"}); err == nil {
		t.Fatal("strict went direct")
	}
	if relayFirst("") == nil {
		t.Fatal("strict posted from the account")
	}
	t.Log("RESULT C1 default (standard) privacy without Tor: direct, IP visible to RPC/node/relay (stderr note only); strict refuses: held")
	_ = time.Second
}
