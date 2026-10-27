package main

import (
	"bytes"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/artifact"
)

func (w *world) treasury(c *Client, name string) *big.Int {
	b, err := c.bankBalance(name)
	w.must(err)
	return b
}

// A hash lock: the right preimage pays the payee and is published; a wrong
// one fails; the payer refunds only after the timeout, and then the payee can't claim.
func TestHashlock(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(alice.deposit(coins(10)))
	secret, err := newSecret(filepath.Join(t.TempDir(), "s"))
	w.must(err)
	h := hashlock(secret, algoSHA256)
	lock, err := alice.htlcOpen("bob", coins(1), h, algoSHA256, alice.chainNowU()+3600)
	w.must(err)

	wrong := bytes.Repeat([]byte{7}, 32)
	w.mustFail(bob.htlcClaim(lock, wrong, htlcTerms{Amount: coins(1)}), "wrong preimage")
	_, err = w.ch.send(bob.key, nil, "HashlockCondition", "claim", u(lock), wrong)
	w.mustFail(err, "wrong preimage")
	w.mustFail(bob.claimLock(lock, secret), "reveal the preimage first") // Bank.claim alone: no event, no money
	w.mustFail(alice.refundEarly(lock, secret), "nothing to refund")     // no early refund
	w.mustFail(alice.refundLock(lock), "not yet")

	b0 := w.treasury(bob, "bob")
	w.must(bob.htlcClaim(lock, secret, htlcTerms{Amount: coins(1)}))
	if d := new(big.Int).Sub(w.treasury(bob, "bob"), b0); d.Cmp(coins(1)) != 0 {
		t.Fatalf("bob got %v", d)
	}
	p, ev := alice.revealedPreimage(h)
	if !bytes.Equal(p, secret) || ev == nil {
		t.Fatalf("preimage not in events: %x", p)
	}
	v, _ := alice.htlcView(lock)
	if v["state"] != "claimed" || v["preimage"] == nil {
		t.Fatalf("view: %v", v)
	}

	// Expiry: refund only after it, and the payee can't claim any more.
	s2 := bytes.Repeat([]byte{9}, 32)
	h2 := hashlock(s2, algoKeccak)
	lock2, err := alice.htlcOpen("bob", coins(2), h2, algoKeccak, alice.chainNowU()+600)
	w.must(err)
	w.mustFail(alice.refundLock(lock2), "not yet")
	w.later(601 * time.Second)
	w.mustFail(bob.htlcClaim(lock2, s2, htlcTerms{Amount: coins(2)}), "refund time")
	_, err = w.ch.send(bob.key, nil, "HashlockCondition", "claim", u(lock2), s2)
	w.mustFail(err, "past the refund time")
	a0 := w.treasury(alice, "alice")
	w.must(alice.refundLock(lock2))
	if d := new(big.Int).Sub(w.treasury(alice, "alice"), a0); d.Cmp(coins(2)) != 0 {
		t.Fatalf("alice refunded %v", d)
	}
}

// A sale by hash lock on the artifact's key commitment: delivering the key is claiming the lock.
func TestHashlockSale(t *testing.T) {
	w := newWorld(t)
	seller, _ := w.owner("seller")
	buyer, _ := w.owner("buyer")
	w.must(buyer.deposit(coins(5)))
	ct, hash, key, _ := artifact.SealFile([]byte("the album"))
	art, err := seller.registerArtifact(hash, uint64(len(ct)), "ipfs://x", "public", coins(1), key)
	w.must(err)
	bad, err := buyer.htlcOpen("seller", coins(1), common.Hash{1}, algoKeccak, buyer.chainNowU()+86400)
	w.must(err)
	w.mustFail(seller.deliverKey(bad, art, "buyer", key), "key commitment")
	lock, err := buyer.htlcOpen("seller", coins(1), commitOf(key), algoKeccak, buyer.chainNowU()+86400)
	w.must(err)
	s0 := w.treasury(seller, "seller")
	w.must(seller.deliverKey(lock, art, "buyer", key))
	if d := new(big.Int).Sub(w.treasury(seller, "seller"), s0); d.Cmp(coins(1)) != 0 {
		t.Fatalf("seller got %v", d)
	}
	if p, _ := buyer.revealedPreimage(commitOf(key)); !bytes.Equal(p, key) {
		t.Fatal("the key is not in the events")
	}
}

// Members fund a treasury; an admin commissions work through an escrow that a
// panel of members decides; a vetoed commission opens nothing.
func TestCommonsCommission(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	worker, _ := w.owner("worker")
	const C = "garden"
	hour := uint64(3600)
	w.must(nina.found(C, CommonsParams{Term: 30 * 86400, VoteWindow: hour, VetoWindow: hour, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 3000, Budget: coins(10), AgentsVote: false}, "grow things"))
	w.join(C, nina, alice, bob, carol)
	for _, m := range []*Client{alice, bob, carol, worker} {
		w.must(m.deposit(coins(5)))
	}
	w.must(alice.pay(C, coins(2), "fund"))
	w.must(bob.pay(C, coins(3), "fund"))
	w.must(carol.pay(C, coins(1), "fund"))
	w.must(worker.pay(C, coins(0.5), "anyone may fund")) // not a member
	if tr := w.treasury(alice, C); tr.Cmp(coins(6.5)) != 0 {
		t.Fatalf("treasury %v", tr)
	}
	v, err := alice.showCommunity(C)
	w.must(err)
	if cb := v["contributions"].(map[string]string); cb["bob"] != fmtCoins(coins(3)) || cb["worker"] != fmtCoins(coins(0.5)) {
		t.Fatalf("contributions: %v", cb)
	}

	id, err := nina.commission(C, "worker", coins(4), []string{"alice", "bob", "carol"}, 2, "", 10, 3, "build the greenhouse")
	w.must(err)
	_, err = w.ch.send(nina.key, nil, "Commons", "payOut", u(id))
	w.mustFail(err, "veto window open")
	if _, err := nina.commission(C, "worker", coins(7), []string{"alice", "bob", "carol"}, 2, "", 10, 3, "too much"); err == nil {
		t.Fatal("over the budget")
	}
	// A second commission, vetoed: nothing opens, the budget comes back.
	id2, err := nina.commission(C, "worker", coins(2), []string{"alice", "bob", "carol"}, 2, "", 10, 3, "paint the fence")
	w.must(err)
	w.must(w.commons(alice, "veto", u(id2), alice.id()))
	w.must(w.commons(bob, "veto", u(id2), bob.id()))
	if a := alice.actionView(id2); a["reversed"] != true {
		t.Fatalf("not reversed: %v", a)
	}
	w.later(time.Duration(hour+1) * time.Second)
	locks0, _ := w.ch.call("Bank", "lockCount")
	_, err = w.ch.send(nina.key, nil, "Commons", "payOut", u(id2))
	w.mustFail(err, "nothing to pay")
	if n, _ := w.ch.call("Bank", "lockCount"); n[0].(*big.Int).Cmp(locks0[0].(*big.Int)) != 0 {
		t.Fatal("a vetoed commission opened a lock")
	}

	r, err := w.ch.send(carol.key, nil, "Commons", "payOut", u(id)) // anyone pays out
	w.must(err)
	lock, err := carol.firstIndexed(r, "Bank", "Locked")
	w.must(err)
	l, _ := w.ch.call("Bank", "locks", u(lock))
	if common.Hash(l[0].([32]byte)) != idOf(C) || common.Hash(l[2].([32]byte)) != idOf("worker") {
		t.Fatal("the lock is not from the treasury to the worker")
	}
	if tr := w.treasury(alice, C); tr.Cmp(coins(2.5)) != 0 {
		t.Fatalf("treasury after the lock %v", tr)
	}
	if a := alice.actionView(id); a["lock"] != lock || a["condition"] != "PanelCondition" {
		t.Fatalf("action view: %v", a)
	}
	w.must(worker.acceptLock(lock))
	w.later(10*time.Minute + time.Second)
	_, err = alice.panelRule(lock, 10000, "built")
	w.must(err)
	_, err = bob.panelRule(lock, 7500, "mostly built")
	w.must(err)
	b0 := w.treasury(worker, "worker")
	w.must(carol.panelSettleAuto(lock))
	if d := new(big.Int).Sub(w.treasury(worker, "worker"), b0); d.Cmp(coins(3)) != 0 {
		t.Fatalf("worker paid %v", d)
	}
	if tr := w.treasury(alice, C); tr.Cmp(coins(3.5)) != 0 { // the rest back to the treasury
		t.Fatalf("treasury after the ruling %v", tr)
	}
}

// All or nothing: three backers reach the goal and the creator (a community)
// collects; a campaign below its goal at the deadline refunds everyone.
func TestCrowdfund(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	const C = "library"
	w.must(nina.found(C, CommonsParams{Term: 30 * 86400, VoteWindow: 3600, VetoWindow: 3600, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 3000, Budget: coins(10)}, ""))
	for _, b := range []*Client{alice, bob, carol} {
		w.must(b.deposit(coins(10)))
	}
	day := uint64(86400)
	ok, err := nina.fundCreate("books", coins(5), nina.chainNowU()+day, C, "ipfs://plan")
	w.must(err)
	l1, err := alice.fundPledge(ok, coins(2))
	w.must(err)
	_, err = w.ch.send(alice.key, nil, "PledgeCondition", "register", u(l1))
	w.mustFail(err, "already counted")
	_, err = bob.fundPledge(ok, coins(2))
	w.must(err)
	_, err = nina.fundCollect(ok)
	w.mustFail(err, "hasn't reached")
	_, err = alice.fundRefund(ok)
	w.mustFail(err, "is open")
	w.mustFail(alice.refundEarly(l1, nil), "no refund")
	_, err = carol.fundPledge(ok, coins(1.5))
	w.must(err)
	v, _ := carol.fundView(ok)
	if v["state"] != "succeeded" || v["backers"] != uint32(3) || v["pledged"] != fmtCoins(coins(5.5)) {
		t.Fatalf("view: %v", v)
	}
	t0 := w.treasury(nina, C)
	moved, err := bob.fundCollect(ok) // anyone collects; the money goes to the community
	w.must(err)
	if d := new(big.Int).Sub(w.treasury(nina, C), t0); d.Cmp(coins(5.5)) != 0 || moved.Cmp(coins(5.5)) != 0 {
		t.Fatalf("treasury got %v", d)
	}
	if cb, _ := nina.showCommunity(C); cb["contributions"].(map[string]string)["alice"] != fmtCoins(coins(2)) {
		t.Fatalf("contributions: %v", cb["contributions"])
	}

	// A lock with another condition can't be registered as a pledge.
	other, err := alice.htlcOpen(C, coins(1), common.Hash{1}, algoSHA256, alice.chainNowU()+30*day)
	w.must(err)
	_, err = w.ch.send(alice.key, nil, "PledgeCondition", "register", u(other))
	w.mustFail(err, "another condition")
	// Nor a pledge with a refund time before the creator could collect.
	fail, err := nina.fundCreate("roof", coins(10), nina.chainNowU()+day, "nina", "")
	w.must(err)
	d, _ := w.ch.call("PledgeCondition", "demandOf", u(fail))
	short, err := alice.lock("nina", coins(1), alice.chainNowU()+2*day, "PledgeCondition", d[0].([]byte))
	w.must(err)
	_, err = w.ch.send(alice.key, nil, "PledgeCondition", "register", u(short))
	w.mustFail(err, "refund time too early")

	_, err = alice.fundPledge(fail, coins(3))
	w.must(err)
	_, err = bob.fundPledge(fail, coins(4))
	w.must(err)
	w.later(time.Duration(day+1) * time.Second)
	_, err = carol.fundPledge(fail, coins(5))
	w.mustFail(err, "is over")
	_, err = nina.fundCollect(fail)
	w.mustFail(err, "hasn't reached")
	if v, _ := alice.fundView(fail); v["state"] != "failed" {
		t.Fatalf("state %v", v["state"])
	}
	n0 := w.treasury(nina, "nina")
	for _, b := range []*Client{alice, bob} {
		b0 := w.treasury(b, b.f.Name)
		moved, err := b.fundRefund(fail)
		w.must(err)
		if d := new(big.Int).Sub(w.treasury(b, b.f.Name), b0); d.Cmp(moved) != 0 || d.Sign() == 0 {
			t.Fatalf("%s refunded %v", b.f.Name, d)
		}
	}
	if w.treasury(nina, "nina").Cmp(n0) != 0 {
		t.Fatal("the creator got something from a failed campaign")
	}
}
