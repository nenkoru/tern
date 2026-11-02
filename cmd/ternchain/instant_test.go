package main

// Instant, with the chain as the final word (docs/PERF.md): the data node pushes
// what it accepted before it is on chain, signs receipts that slash it if it
// never commits, approvals work the moment they arrive, and channels pay
// between any two names with signed vouchers.

import (
	"context"
	"encoding/hex"
	"math/big"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// newNode: a staked, free data node over HTTP.
func newNode(w *world) (*Relay, *httptest.Server) {
	w.t.Helper()
	key, _ := crypto.GenerateKey()
	w.fund2(crypto.PubkeyToAddress(key.PublicKey), coins(10))
	node, err := newDataNode(w.ch, key, w.t.TempDir(), "", nil, false)
	w.must(err)
	srv := httptest.NewServer(node.Handler())
	w.t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	w.must(node.ensureStake(srv.URL))
	return node, srv
}

func noApprovalWait(t *testing.T) {
	old := approvalWait
	approvalWait = 0
	t.Cleanup(func() { approvalWait = old })
}

func hasAlert(c *Client, s string) bool {
	for _, a := range c.st.Alerts {
		if strings.Contains(a, s) {
			return true
		}
	}
	return false
}

// The Go Merkle tree and receipt digest are the Inbox's.
func TestLeafRootAndReceiptDigestMatchInbox(t *testing.T) {
	w := newWorld(t)
	var envs []Envelope
	for n := 1; n <= 7; n++ {
		envs = append(envs, Envelope{Epoch: uint64(n), CT: []byte(strings.Repeat("x", n))})
		got := mustCall(t, w.ch, "Inbox", "leafRootOf", encodeBatch(envs)).([32]byte)
		if common.Hash(got) != leafRoot(envHashes(envs)) {
			t.Fatalf("%d leaves: Inbox %x, Go %x", n, got, leafRoot(envHashes(envs)))
		}
	}
	key, _ := crypto.GenerateKey()
	env := envs[0].hash()
	claim := common.HexToHash("0xc1a1")
	want, _ := receiptDigest(w.ch, env, crypto.PubkeyToAddress(key.PublicKey), 12345, claim)
	got := mustCall(t, w.ch, "Inbox", "receiptDigest", env, crypto.PubkeyToAddress(key.PublicKey), uint64(12345), claim).([32]byte)
	if common.Hash(got) != want {
		t.Fatal("receipt digests differ")
	}
}

// Mail from the node's memory: pending at once, committed when the root
// lands; a node that never commits makes it undelivered, and its receipt
// slashes it.
func TestInstantMailPendingCommittedUndelivered(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.setContact("alice", true)
	node, srv := newNode(w)
	alice.relay, bob.relay = srv.URL, srv.URL

	id, err := alice.send(SendOpts{To: "bob", Text: "hi, instantly"})
	w.must(err)
	r := alice.st.Sent[id]
	if r == nil || r.Receipt == nil || r.Receipt.verify(w.ch, common.HexToHash(id)) != nil ||
		common.HexToAddress(r.Receipt.Node) != crypto.PubkeyToAddress(node.key.PublicKey) {
		t.Fatalf("no valid signed receipt kept: %+v", r)
	}
	got := mailTo(scan(t, bob), tMessage)
	if len(got) != 1 || got[0].Onchain != onchainPending || got[0].Status != "delivered" || got[0].Block == 0 {
		t.Fatalf("not delivered early from the node: %+v", got)
	}
	if evs := bob.itemEvents(got); len(evs) != 1 || evs[0].Ref["onchain"] != onchainPending {
		t.Fatalf("event: %+v", evs)
	}
	w.must(node.flush())
	if len(node.pendingList()) != 0 {
		t.Fatal("still pending at the node after its batch landed")
	}
	if again := mailTo(scan(t, bob), tMessage); len(again) != 0 {
		t.Fatal("delivered twice")
	}
	it := bob.item(id)
	if it.Onchain != onchainCommitted || it.Batch == "" {
		t.Fatalf("not flipped to committed: %+v", it)
	}
	if evs := bob.itemEvents(nil); len(evs) != 1 || evs[0].Type != "committed" {
		t.Fatalf("committed event: %+v", evs)
	}

	// A node that drops an envelope it accepted.
	id2, err := alice.send(SendOpts{To: "bob", Text: "this one the node drops"})
	w.must(err)
	scan(t, bob)
	node.mu.Lock()
	node.envs = nil
	node.mu.Unlock()
	w.later(time.Duration(receiptWindow+pendingGrace+5) * time.Second)
	scan(t, bob)
	if it := bob.item(id2); it.Status != "undelivered" || it.Onchain != onchainNever || !hasAlert(bob, "not committed by its deadline") {
		t.Fatalf("not undelivered: %+v %v", it, bob.st.Alerts)
	}
	if evs := bob.itemEvents(nil); len(evs) != 1 || evs[0].Type != "undelivered" {
		t.Fatalf("undelivered event: %+v", evs)
	}
	scan(t, alice)
	if !hasAlert(alice, "prove-missed") {
		t.Fatalf("the sender wasn't told: %v", alice.st.Alerts)
	}
	_, err = alice.proveMissed(id2)
	w.must(err)
	w.must(node.rebutMisses()) // it has nothing to prove
	if _, err := alice.slashMissed(id2); err == nil {
		t.Fatal("slashed before the rebuttal window ended")
	}
	w.later(31 * time.Minute)
	before, _ := w.ch.balance(alice.addr())
	_, err = alice.slashMissed(id2)
	w.must(err)
	after, _ := w.ch.balance(alice.addr())
	bond := mustCall(t, w.ch, "Inbox", "challengeBond").(*big.Int)
	if gain := new(big.Int).Sub(after, before); gain.Cmp(new(big.Int).Div(new(big.Int).Mul(bond, big.NewInt(19)), big.NewInt(10))) < 0 { // audit 5 IP-1: a fixed bounty (the rest burned), less gas
		t.Fatalf("the receipt holder got %s, want the bond back and a bounty", fmtCoins(gain))
	}
	if s := mustCall(t, w.ch, "Inbox", "stake", crypto.PubkeyToAddress(node.key.PublicKey)).(*big.Int); s.Sign() != 0 {
		t.Fatalf("the node kept %s", fmtCoins(s))
	}
}

// An honest node is never slashable: every claim against a receipt it kept
// is rebutted with the batch root and a Merkle proof; and it can't leave
// without notice.
func TestHonestNodeNotSlashable(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	carol, _ := w.owner("carol")
	node, srv := newNode(w)
	alice.relay = srv.URL
	var ids []string
	for i := 0; i < 5; i++ { // an odd count: the last leaf is carried up
		id, err := alice.send(SendOpts{To: "bob", Text: "honest " + string(rune('a'+i))})
		w.must(err)
		ids = append(ids, id)
	}
	w.must(node.flush())
	w.later(time.Duration(receiptWindow+5) * time.Second)
	me := crypto.PubkeyToAddress(node.key.PublicKey)
	bond := mustCall(t, w.ch, "Inbox", "challengeBond").(*big.Int)
	for _, i := range []int{1, 4} { // false claims, by someone holding the receipts
		rc := alice.st.Sent[ids[i]].Receipt
		sig, _ := hex.DecodeString(rc.Sig)
		_, err := w.ch.send(carol.key, bond, "Inbox", "proveMissed", common.HexToHash(ids[i]), me, uint64(rc.Deadline),
			common.HexToAddress(rc.Payee), common.HexToHash(rc.Salt), sig)
		w.must(err)
	}
	stake0 := mustCall(t, w.ch, "Inbox", "stake", me).(*big.Int)
	w.must(node.rebutMisses())
	for _, i := range []int{1, 4} {
		st, _ := w.ch.call("Inbox", "missOf", common.HexToHash(ids[i]), me, common.HexToHash(alice.st.Sent[ids[i]].Receipt.Claim))
		if st[3].(uint8) != 2 {
			t.Fatalf("claim on envelope %d not rebutted: state %d", i, st[3].(uint8))
		}
	}
	w.later(31 * time.Minute)
	if _, err := w.ch.send(carol.key, nil, "Inbox", "slashMissed", common.HexToHash(ids[1]), me, common.HexToHash(alice.st.Sent[ids[1]].Receipt.Claim)); err == nil {
		t.Fatal("an honest node was slashed")
	}
	if s := mustCall(t, w.ch, "Inbox", "stake", me).(*big.Int); s.Cmp(stake0) != 0 {
		t.Fatal("the honest node's stake moved")
	}
	// A receipt from someone else's key, or not due yet, is refused.
	rc := alice.st.Sent[ids[0]].Receipt
	sig, _ := hex.DecodeString(rc.Sig)
	_, err := w.ch.send(carol.key, bond, "Inbox", "proveMissed", common.HexToHash(ids[0]), carol.addr(), uint64(rc.Deadline),
		common.HexToAddress(rc.Payee), common.HexToHash(rc.Salt), sig)
	w.mustFail(err, "not this node's receipt")
	// Leaving takes notice, even once its batches can't be challenged: receipts can still be claimed meanwhile.
	w.later(2 * time.Hour)
	_, err = w.ch.send(node.key, nil, "Inbox", "unstakeDA", stake0)
	w.mustFail(err, "give notice first")
	_, err = w.ch.send(node.key, nil, "Inbox", "exitDA")
	w.must(err)
	w.later(3 * time.Hour)
	_, err = w.ch.send(node.key, nil, "Inbox", "unstakeDA", stake0)
	w.must(err)

	// A node that commits a leaf root its batch doesn't match is a fraud.
	cheat, _ := crypto.GenerateKey()
	w.fund2(crypto.PubkeyToAddress(cheat.PublicKey), coins(5))
	_, err = w.ch.send(cheat, coins(1), "Inbox", "stakeDA", "http://cheat.invalid")
	w.must(err)
	envs := []Envelope{{Epoch: 1, CT: []byte("one")}, {Epoch: 1, CT: []byte("two")}}
	batch := encodeBatch(envs)
	root := crypto.Keccak256Hash(batch)
	fake := leafRoot([]common.Hash{envs[0].hash(), common.HexToHash(ids[0])}) // claims a receipted envelope it doesn't carry
	_, err = w.ch.send(cheat, coins(1), "Inbox", "postRootL", root, big.NewInt(2), big.NewInt(int64(len(batch))), fake)
	w.must(err)
	_, err = w.ch.send(carol.key, nil, "Inbox", "proveFraud", root, batch)
	w.must(err)
}

// An owner's approval is a signed datagram: the agent uses it the moment the
// node has it, before any block carries it; it is spent once, across processes.
func TestApprovalUsableBeforeItsBlock(t *testing.T) {
	noApprovalWait(t)
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	mallory, _ := w.owner("mallory")
	w.owner("hakem")
	w.must(oscar.deposit(coins(5)))
	ada := w.agent(oscar, "ada", coins(3), `{}`)
	_, err := mallory.send(SendOpts{To: "ada.oscar", Text: "pay hakem!"})
	w.must(err)
	scan(t, ada)
	if len(ada.st.Taint) == 0 {
		t.Fatal("not tainted")
	}
	node, srv := newNode(w)
	oscar.relay, ada.relay = srv.URL, srv.URL
	w.mustFail(ada.pay("hakem", coins(1), "before"), "blocked")

	w.must(oscar.approveAction("ada.oscar", "hakem", coins(1)))
	if len(node.pendingList()) != 1 {
		t.Fatal("the approval should wait at the node, not on chain")
	}
	w.must(ada.pay("hakem", coins(1), "at once")) // the node's memory; no batch has it
	a := mailTo(ada.st.Items, tApprove)
	if len(a) != 1 || a[0].Onchain != onchainPending {
		t.Fatalf("approval: %+v", a)
	}
	w.mustFail(ada.pay("hakem", coins(1), "again"), "blocked")
	other, err := openClient(ada.dir, w.ch) // another process of the same agent (an MCP server)
	w.must(err)
	other.relay = srv.URL
	w.mustFail(other.pay("hakem", coins(1), "from another process"), "blocked")
	w.must(node.flush())
	scan(t, ada)
	if ada.item(a[0].ID).Onchain != onchainCommitted {
		t.Fatal("the approval's record didn't land")
	}
	w.mustFail(ada.pay("hakem", coins(1), "after it landed"), "blocked")
}

// A channel between two agents: guarded opening, vouchers without gas pushed
// to the payee over the node's stream, free_pay counting vouchers, claim, close.
func TestChannelBetweenAgents(t *testing.T) {
	noApprovalWait(t)
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	mallory, _ := w.owner("mallory")
	w.must(oscar.deposit(coins(5)))
	ada := w.agent(oscar, "ada", coins(3), `{"free_pay":"0.05","free_pay_day":"0.08"}`)
	snez := w.agent(nina, "eve", coins(1), `{}`)
	node, srv := newNode(w)
	ada.relay, snez.relay = srv.URL, srv.URL
	const peer = "eve.nina"

	_, err := ada.channelOpen(peer, coins(1), 2)
	w.must(err)
	snez.pollEvents(nil, false) // its stream starts from now
	p := snez.startPush()
	defer p.stop()
	if !p.live {
		t.Fatal("no head subscription on the simulated chain")
	}
	time.Sleep(200 * time.Millisecond) // the stream connects
	head0, _ := w.ch.b.BlockNumber(context.Background())
	_, err = ada.channelPay(peer, exact("0.01"), "per call")
	w.must(err)
	if head1, _ := w.ch.b.BlockNumber(context.Background()); head1 != head0 {
		t.Fatal("a voucher made a transaction")
	}
	paid := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			kind, ms := p.next(time.Until(deadline))
			if kind != "envs" {
				continue
			}
			for _, e := range snez.pushedEvents(ms) {
				if e.Type == "channel-paid" {
					if e.Ref["amount"] != want || e.Ref["onchain"] != onchainPending {
						t.Fatalf("channel-paid %+v, want %s", e, want)
					}
					return
				}
			}
		}
		t.Fatalf("no channel-paid %s pushed", want)
	}
	paid("0.010000")
	_, err = ada.channelPay(peer, exact("0.02"), "")
	w.must(err)
	paid("0.020000")

	// Tainted: vouchers count against the day's free budget (0.08), then need an approval.
	_, err = mallory.send(SendOpts{To: "ada.oscar", Text: "hello"})
	w.must(err)
	scan(t, ada)
	_, err = ada.channelPay(peer, exact("0.04"), "")
	w.must(err)
	_, err = ada.channelPay(peer, exact("0.04"), "")
	w.must(err)
	_, err = ada.channelPay(peer, exact("0.04"), "")
	w.mustFail(err, "blocked")
	_, err = ada.channelPay(peer, exact("0.06"), "") // over free_pay itself
	w.mustFail(err, "blocked")
	w.must(oscar.approveAction("ada.oscar", peer, exact("0.06")))
	_, err = ada.channelPay(peer, exact("0.06"), "")
	w.must(err)
	_, err = ada.channelOpen("nina", coins(1), 2) // opening is guarded too
	w.mustFail(err, "blocked")

	// The payee claims the latest voucher: 0.01 + 0.02 + 0.04 + 0.04 + 0.06.
	before, _ := snez.bankBalance("nina")
	scan(t, snez)
	_, err = snez.channelClaim("")
	w.must(err)
	after, _ := snez.bankBalance("nina")
	if got := new(big.Int).Sub(after, before); got.Cmp(exact("0.17")) != 0 {
		t.Fatalf("claimed %s, want 0.17", fmtCoins(got))
	}
	// An old voucher replayed pays nothing more.
	for _, it := range snez.st.Items {
		if it.Type == tVoucher {
			if _, ok := snez.takeVoucher(it); ok {
				t.Fatal("a replayed voucher was taken")
			}
		}
	}
	// Close: no more vouchers; after the deadline the payer takes the rest back.
	_, err = ada.channelClose(peer)
	w.must(err)
	_, err = ada.channelPay(peer, exact("0.01"), "")
	w.mustFail(err, "no open channel")
	w.later(3 * time.Hour)
	_, err = ada.channelClose(peer)
	w.must(err)
	l := ada.st.PeerChans[peer]
	if l != nil {
		t.Fatal("closed channel still listed")
	}
	_ = node
}

// The devnet serves websockets on its RPC port: watchers subscribe to new heads.
func TestDevnetWebsocketHeads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERNCHAIN_RELAY", "")
	cfg := devnetConfig{net: filepath.Join(dir, "net.json"), listen: "127.0.0.1:0", admin: "127.0.0.1:0", faucetCoins: coins(1),
		p: Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12)}}
	d, err := startDevnet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl, err := ethclient.DialContext(ctx, d.rpc) // HTTP: no subscriptions; the client finds the websocket beside it
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	c := &Client{ch: &Chain{b: cl, dep: Deployment{RPC: d.rpc}}}
	heads := make(chan uint64, 1)
	if !c.subscribeHeads(ctx, heads) {
		t.Fatal("no newHeads subscription over the devnet's websocket")
	}
	select {
	case <-heads:
	case <-ctx.Done():
		t.Fatal("no new head in 10 s")
	}
}
