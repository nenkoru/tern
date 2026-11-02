package main

import (
	"context"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Batches live at a data node; the chain keeps their hashes. A challenged
// batch is put on chain by the node; a lost one costs the node its stake.
func TestDataAvailability(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	bob.setContact("alice", true)

	nodeKey, _ := crypto.GenerateKey()
	if err := w.ch.transfer(w.faucet, crypto.PubkeyToAddress(nodeKey.PublicKey), coins(10)); err != nil {
		t.Fatal(err)
	}
	node := &Relay{ch: w.ch, key: nodeKey, da: true, store: map[common.Hash][]byte{}}
	srv := httptest.NewServer(node.Handler())
	defer srv.Close()
	if err := node.ensureStake(srv.URL); err != nil {
		t.Fatal(err)
	}
	alice.relay = srv.URL
	lastRoot := func() common.Hash {
		evs, _ := w.ch.events("Inbox", "Committed")
		return common.Hash(evs[len(evs)-1].Args["root"].([32]byte))
	}
	gasOfLast := func() uint64 {
		h, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
		return h.GasUsed
	}

	// Twenty messages, one hash on chain.
	for i := 0; i < 20; i++ {
		if _, err := alice.send(SendOpts{To: "bob", Text: "a short message"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := node.flush(); err != nil {
		t.Fatal(err)
	}
	g := gasOfLast()
	t.Logf("20 envelopes by hash: %d gas (%d each)", g, g/20)
	if got := mailTo(scan(t, bob), tMessage); len(got) != 20 {
		t.Fatalf("bob read %d of 20 from the data node", len(got))
	}

	// A challenge: the node puts the batch on chain and earns the challenger's bond.
	root := lastRoot()
	bond, _ := w.ch.call("Inbox", "challengeBond")
	if _, err := carol.ch.send(carol.key, bond[0].(*big.Int), "Inbox", "challenge", root); err != nil {
		t.Fatal(err)
	}
	if err := node.resolveChallenges(); err != nil {
		t.Fatal(err)
	}
	if st, _ := w.ch.call("Inbox", "commitment", root); st[2].(uint8) != 3 {
		t.Fatalf("challenge not resolved: state %d", st[2].(uint8))
	}

	// A lost batch: the node drops it, can't answer the challenge, loses its stake.
	if _, err := alice.send(SendOpts{To: "bob", Text: "this one gets lost"}); err != nil {
		t.Fatal(err)
	}
	if err := node.flush(); err != nil {
		t.Fatal(err)
	}
	lost := lastRoot()
	delete(node.store, lost)
	if got := mailTo(scan(t, bob), tMessage); len(got) != 0 || len(bob.st.PendingDA) != 1 {
		t.Fatalf("an unreachable batch should wait as pending: %d read, %d pending", len(got), len(bob.st.PendingDA))
	}
	if _, err := carol.ch.send(carol.key, bond[0].(*big.Int), "Inbox", "challenge", lost); err != nil {
		t.Fatal(err)
	}
	node.resolveChallenges() // has nothing to put on chain
	if _, err := carol.ch.send(carol.key, nil, "Inbox", "expire", lost); err == nil {
		t.Fatal("expired before the resolve window passed")
	}
	before, _ := w.ch.balance(carol.addr())
	w.later(31 * time.Minute)
	if _, err := bob.ch.send(bob.key, nil, "Inbox", "expire", lost); err != nil {
		t.Fatal(err)
	}
	after, _ := w.ch.balance(carol.addr())
	stake, _ := w.ch.call("Inbox", "minStake")
	want := new(big.Int).Add(stake[0].(*big.Int), bond[0].(*big.Int))
	if new(big.Int).Sub(after, before).Cmp(want) != 0 {
		t.Fatalf("the challenger got %v, want stake+bond %v", new(big.Int).Sub(after, before), want)
	}
	scan(t, bob)
	if len(bob.st.PendingDA) != 0 {
		t.Fatal("a lost batch stayed pending")
	}
}

// A paid data node: vouchers per envelope, one claim for many; batches on
// disk survive a restart; a mirror serves and defends a dead node's batches.
func TestDataNodeIncentivesAndDisk(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	nodeco, _ := w.owner("nodeco") // the company running the node: paid to its name
	bob.setContact("alice", true)
	if err := alice.deposit(coins(1)); err != nil {
		t.Fatal(err)
	}
	price := exact("0.0001")
	dir := t.TempDir()
	node, err := newDataNode(w.ch, nodeco.key, dir, "nodeco", price, false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(node.Handler())
	if err := node.ensureStake(srv.URL); err != nil {
		t.Fatal(err)
	}
	// A mirror run by someone else.
	mk, _ := crypto.GenerateKey()
	w.ch.transfer(w.faucet, crypto.PubkeyToAddress(mk.PublicKey), coins(10))
	mirror, _ := newDataNode(w.ch, mk, t.TempDir(), "", nil, true)
	msrv := httptest.NewServer(mirror.Handler())
	defer msrv.Close()
	if err := mirror.ensureStake(msrv.URL); err != nil {
		t.Fatal(err)
	}

	// Unpaid envelopes are refused.
	if err := relayPost(srv.URL, "/envelope", []byte("x")); err == nil {
		t.Fatal("a paid node took an envelope without a voucher")
	}
	alice.relay = srv.URL
	for i := 0; i < 20; i++ {
		if _, err := alice.send(SendOpts{To: "bob", Text: "paid by voucher"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := node.flush(); err != nil {
		t.Fatal(err)
	}
	// One claim for twenty envelopes.
	before, _ := nodeco.bankBalance("nodeco")
	earned, err := node.claimEarnings()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := nodeco.bankBalance("nodeco")
	want := new(big.Int).Mul(price, big.NewInt(20))
	if earned.Cmp(want) != 0 || new(big.Int).Sub(after, before).Cmp(want) != 0 {
		t.Fatalf("node earned %v (bank +%v), want %v", earned, new(big.Int).Sub(after, before), want)
	}
	// The mirror copies; the node dies; readers fall back to the mirror.
	if n, err := mirror.mirror(); err != nil || n != 1 {
		t.Fatalf("mirrored %d batches: %v", n, err)
	}
	srv.Close()
	if got := mailTo(scan(t, bob), tMessage); len(got) != 20 {
		t.Fatalf("bob read %d of 20 with the node down", len(got))
	}
	// A challenge while the node is down: the mirror answers it and earns the bond.
	evs, _ := w.ch.events("Inbox", "Committed")
	root := common.Hash(evs[len(evs)-1].Args["root"].([32]byte))
	bond, _ := w.ch.call("Inbox", "challengeBond")
	if _, err := carol.ch.send(carol.key, bond[0].(*big.Int), "Inbox", "challenge", root); err != nil {
		t.Fatal(err)
	}
	if err := mirror.resolveChallenges(); err != nil {
		t.Fatal(err)
	}
	if st, _ := w.ch.call("Inbox", "commitment", root); st[2].(uint8) != 3 {
		t.Fatal("the mirror did not resolve the challenge")
	}
	// The node restarts from its directory: batches and vouchers are back.
	again, err := newDataNode(w.ch, nodeco.key, dir, "nodeco", price, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.get(root); !ok {
		t.Fatal("the batch didn't survive a restart")
	}
	if v := again.vouchers; len(v) != 1 {
		t.Fatalf("earned vouchers didn't survive a restart: %v", v)
	}
}
