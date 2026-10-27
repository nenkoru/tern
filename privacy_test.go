package main

import (
	"context"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/zk"
)

// TestPoseidonMatchesContract: Go, the circuits and Poseidon2Hasher hash alike.
func TestPoseidonMatchesContract(t *testing.T) {
	w := newWorld(t)
	h := common.HexToAddress(w.ch.dep.Contracts["Poseidon2Hasher"])
	for _, in := range [][2]*big.Int{{big.NewInt(0), big.NewInt(0)}, {big.NewInt(1), big.NewInt(2)},
		{new(big.Int).Sub(zk.Modulus, big.NewInt(1)), big.NewInt(12345)}} {
		data := append(common.LeftPadBytes(in[0].Bytes(), 32), common.LeftPadBytes(in[1].Bytes(), 32)...)
		out, err := w.ch.b.CallContract(context.Background(), ethereum.CallMsg{To: &h, Data: data}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := new(big.Int).SetBytes(out), zk.Compress(in[0], in[1]); got.Cmp(want) != 0 {
			t.Fatalf("compress%v: contract %s, Go %s", in, got, want)
		}
	}
	data := append(common.LeftPadBytes(zk.Modulus.Bytes(), 32), make([]byte, 32)...)
	if _, err := w.ch.b.CallContract(context.Background(), ethereum.CallMsg{To: &h, Data: data}, nil); err == nil {
		t.Fatal("a non-field element hashed")
	}
	// The empty pool's root is the empty tree's.
	if r := mustCall(t, w.ch, "PrivacyPool", "root").(*big.Int); r.Cmp(zk.ZeroAt(zk.PoolDepth)) != 0 {
		t.Fatalf("empty root %s", r)
	}
}

// TestShieldedPool: deposit, relayed private sends to a stealth address, a
// bank balance and an address; association sets; double spends, redirected
// proofs and overdrafts refused; notes rebuilt from the chain; gas measured.
func TestShieldedPool(t *testing.T) {
	w := newWorld(t)
	relay := w.startRelay()
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	w.must(bob.enableStealth()) // bob receives at stealth addresses; carol doesn't

	r0, err := alice.poolDeposit(coins(1))
	w.must(err)
	_, err = dave.poolDeposit(coins(1))
	w.must(err)
	_, err = carol.poolDeposit(coins(0.5))
	w.must(err)
	// Deposit gas.
	dep, err := alice.poolDeposit(coins(0.1))
	w.must(err)
	_ = dep
	gasOfLast(t, w, "deposit")

	// No association set holds it yet.
	_, err = alice.poolSend("bob", coins(0.4), poolSendOpts{})
	w.mustFail(err, "no association set holds")
	// Anyone publishes one: all deposits. A second, tiny one: only carol's.
	m, err := dave.aspPublish("everyone", mustRule(t, dave, "all"), "")
	w.must(err)
	if m["labels"] != 4 {
		t.Fatalf("asp: %v", m)
	}
	_, err = carol.aspPublish("carol-only", []*big.Int{mustNote(t, carol).label()}, "")
	w.must(err)
	_, err = alice.aspPublish("everyone", mustRule(t, alice, "all"), "")
	w.mustFail(err, "belongs to")
	_, err = alice.poolSend("bob", coins(0.4), poolSendOpts{asp: "carol-only"})
	w.mustFail(err, "doesn't hold this note")

	// A relayed send to bob's stealth address: alice sends no transaction.
	nonce, _ := w.ch.b.NonceAt(context.Background(), alice.addr(), nil)
	relayBefore, _ := w.ch.balance(crypto.PubkeyToAddress(relay.key.PublicKey))
	res, err := alice.poolSend("bob", coins(0.4), poolSendOpts{})
	w.must(err)
	t.Logf("withdraw: proof in %d ms", res["proof_ms"])
	gasOfLast(t, w, "withdraw (relayed, to a stealth address)")
	if n, _ := w.ch.b.NonceAt(context.Background(), alice.addr(), nil); n != nonce {
		t.Fatal("alice sent a transaction herself")
	}
	if res["association_set"] != "everyone" || res["anonymity_set"] != 4 {
		t.Fatalf("send: %v", res)
	}
	ns, err := bob.notes()
	w.must(err)
	if len(ns) != 1 || ns[0].Amount.Cmp(coins(0.4)) != 0 {
		t.Fatalf("bob's stealth notes: %+v", ns)
	}
	if after, _ := w.ch.balance(crypto.PubkeyToAddress(relay.key.PublicKey)); after.Cmp(relayBefore) <= 0 {
		t.Fatal("the relayer wasn't paid its fee")
	}
	// Alice's change: 1 - 0.4 - fee, a new note with the same label.
	wl, err := alice.syncNotes()
	w.must(err)
	var change *PoolNote
	for _, n := range alice.unspent(wl) {
		if n.Step == 1 {
			change = &n
		}
	}
	if change == nil || change.value().Cmp(new(big.Int).Sub(new(big.Int).Sub(coins(1), coins(0.4)), poolRelayFee)) != 0 || change.Label != r0.Label {
		t.Fatalf("change note: %+v", change)
	}

	// A double spend: the same withdrawal again is refused.
	_, err = w.ch.send(relay.key, nil, "PrivacyPool", "withdraw", alice.lastPool.args()...)
	w.mustFail(err, "spent")
	// A proof redirected (another recipient, a bigger fee, another nullifier hash) fails.
	_, err = alice.poolSend("bob", coins(0.1), poolSendOpts{dry: true})
	w.must(err)
	req := alice.lastPool
	for i, change := range []func(r *poolRequest){
		func(r *poolRequest) { r.W.Recipient = crypto.PubkeyToAddress(relay.key.PublicKey) },
		func(r *poolRequest) { r.W.Fee = new(big.Int).Add(r.W.Fee, coins(0.3)) },
		func(r *poolRequest) { r.NullifierHash = new(big.Int).Add(r.NullifierHash, big.NewInt(1)) },
		func(r *poolRequest) { r.Value = new(big.Int).Add(r.Value, coins(0.1)) },
	} {
		stolen := *req
		change(&stolen)
		if _, err := w.ch.send(relay.key, nil, "PrivacyPool", "withdraw", stolen.args()...); err == nil || strings.Contains(err.Error(), "spent") {
			t.Fatalf("redirected withdrawal %d: %v", i, err)
		}
	}
	w.must(errOf(w.ch.send(relay.key, nil, "PrivacyPool", "withdraw", req.args()...))) // as made, it goes through

	// To a name without stealth keys: its bank balance. To an address, submitted directly.
	before, _ := alice.bankBalance("carol")
	_, err = alice.poolSend("carol", coins(0.1), poolSendOpts{})
	w.must(err)
	if after, _ := alice.bankBalance("carol"); new(big.Int).Sub(after, before).Cmp(coins(0.1)) != 0 {
		t.Fatalf("carol's bank: %s -> %s", before, after)
	}
	fresh, _ := crypto.GenerateKey()
	to := crypto.PubkeyToAddress(fresh.PublicKey)
	_, err = alice.poolSend(to.Hex(), coins(0.05), poolSendOpts{direct: true})
	w.must(err)
	if b, _ := w.ch.balance(to); b.Cmp(coins(0.05)) != 0 {
		t.Fatalf("address got %s", b)
	}
	// Overdraft: no note holds 5 coins.
	_, err = alice.poolSend("bob", coins(5), poolSendOpts{})
	w.mustFail(err, "no note holds")

	// Lost pool.notes: rebuilt from the chain and the key.
	bal1, err := alice.poolBalance()
	w.must(err)
	w.must(os.Remove(alice.notesPath()))
	bal2, err := alice.poolBalance()
	w.must(err)
	if bal1["private_balance"] != bal2["private_balance"] || bal1["private_balance"] == fmtCoins(new(big.Int)) {
		t.Fatalf("rebuilt balance %v, was %v", bal2["private_balance"], bal1["private_balance"])
	}
	// A note file another key can't open.
	b, _ := os.ReadFile(alice.notesPath())
	if strings.Contains(string(b), r0.Label) {
		t.Fatal("pool.notes is not encrypted")
	}

	// clean24h: deposits a day old, minus those a citizen flagged (none here: no citizens).
	if ls := mustRule(t, alice, "clean24h"); len(ls) != 0 {
		t.Fatalf("fresh deposits in clean24h: %d", len(ls))
	}
	w.later(25 * time.Hour)
	if ls := mustRule(t, alice, "clean24h"); len(ls) != 4 {
		t.Fatalf("clean24h after a day: %d", len(ls))
	}
}

// TestPrivateSendIsGuarded: an agent's private send counts against free_pay
// and approvals like pay.
func TestPrivateSendIsGuarded(t *testing.T) {
	w := newWorld(t)
	w.startRelay()
	oscar, _ := w.owner("oscar")
	mallory, _ := w.owner("mallory")
	_ = mallory
	ada := w.agent(oscar, "ada", coins(3), `{"free_pay":"0.1"}`)
	ada.relay = w.relay
	_, err := ada.poolDeposit(coins(1))
	w.must(err)
	_, err = ada.aspPublish("all", mustRule(t, ada, "all"), "")
	w.must(err)
	ada.taint("mallory") // it read outside mail
	_, err = ada.poolSend("mallory", coins(0.5), poolSendOpts{})
	w.mustFail(err, "approval")
	_, err = ada.poolSend("mallory", coins(0.05), poolSendOpts{}) // under free_pay
	w.must(err)
	// The CLI's private pay takes the same path, and the same guard.
	if err := runPrivate(ada, "private pay", []string{"mallory", "0.5"}, "", 0, poolSendOpts{}); err == nil {
		t.Fatal("private pay over free_pay without approval")
	}
}

// TestSecretCitizenVote: citizens vote by proof through a relayer; the tally
// is right, nobody votes twice (secretly, by name, or both), and a revoked
// citizen can't vote once the root moved, even with a proof made before.
func TestSecretCitizenVote(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov", "eli")
	a, b, c, d, e := f["ana"], f["ben"], f["cai"], f["dov"], f["eli"]
	if n := mustCall(t, w.ch, "CitizenGroup", "size").(*big.Int); n.Int64() != 5 {
		t.Fatalf("citizens' tree: %d leaves", n)
	}
	p, err := a.govProposeParam("FeeOracle", "setTarget", []string{"envelope", "12"}, "a test")
	w.must(err)
	w.later(2 * time.Second)
	nonce, _ := w.ch.b.NonceAt(context.Background(), b.addr(), nil)
	for _, v := range []struct {
		c   *Client
		yes bool
	}{{a, true}, {b, true}, {c, false}} {
		m, err := v.c.govVote(p, v.yes, "citizens")
		w.must(err)
		if !strings.Contains(m["citizens"].(string), "secret") {
			t.Fatalf("not secret: %v", m)
		}
	}
	t.Logf("vote: proof in %d ms", zk.LastProveTime.Milliseconds())
	gasOfLast(t, w, "secret vote (relayed)")
	if n, _ := w.ch.b.NonceAt(context.Background(), b.addr(), nil); n != nonce {
		t.Fatal("ben sent a transaction himself")
	}
	if evs, _ := w.ch.events("Parliament", "CitizenVoted"); len(evs) != 0 {
		t.Fatal("a secret vote named its voter")
	}
	// Twice: by proof again, by name the old way, by name with a proof.
	_, err = b.govVote(p, false, "citizens")
	w.mustFail(err, "voted already")
	_, err = b.ch.send(b.key, nil, "Parliament", "voteCitizen", u(p), b.id(), false)
	w.mustFail(err, "you have a voting identity")
	_, err = b.govVote(p, false, "citizens", true)
	w.mustFail(err, "voted already")
	nul, rootNow, proof, err := b.voteProof(p, true) // even a fresh proof: same nullifier
	if err == nil {
		_, err = w.ch.send(w.faucet, nil, "Parliament", "voteSecret", u(p), true, nul, rootNow, proof)
	}
	w.mustFail(err, "voted")
	// dov votes by name (--open): counted once, named.
	m, err := d.govVote(p, true, "citizens", true)
	w.must(err)
	if !strings.Contains(m["citizens"].(string), "by name") {
		t.Fatalf("open vote: %v", m)
	}
	x, err := a.proposal(p)
	w.must(err)
	if x.Yes != 3 || x.No != 1 {
		t.Fatalf("tally: %d yes, %d no", x.Yes, x.No)
	}
	if evs, _ := w.ch.events("Parliament", "SecretVoted"); len(evs) != 3 {
		t.Fatalf("secret votes: %d", len(evs))
	}

	// Revocation: eli is voted out while a proposal is open; the root moves;
	// eli can't vote on it, not even with a proof against the old tree.
	r, err := a.govRevoke("eli", false, "a test of secret votes")
	w.must(err)
	for _, v := range []*Client{a, b, c, d} {
		w.must(v.govCitizenCall("voteRevocation", u(r), v.id(), true))
	}
	w.later(9 * time.Minute)
	p2, err := a.govProposeParam("FeeOracle", "setTarget", []string{"envelope", "13"}, "another")
	w.must(err)
	stale, staleRoot, staleProof, err := e.voteProof(p2, true) // made while eli is still in the tree
	w.must(err)
	w.later(2 * time.Minute)
	w.must(errOf(a.ch.send(a.key, nil, "Citizens", "closeRevocation", u(r))))
	if w.citizen(a, "eli") {
		t.Fatal("eli still a citizen")
	}
	_, err = e.govVote(p2, true, "citizens")
	w.mustFail(err, "isn't in the citizens' tree now")
	_, err = w.ch.send(w.faucet, nil, "Parliament", "voteSecret", u(p2), true, stale, staleRoot, staleProof)
	w.mustFail(err, "prove again") // the proof's "now" root predates a removal: no longer known
	_, err = w.ch.send(e.key, nil, "Parliament", "voteCitizen", u(p2), e.id(), true)
	w.mustFail(err, "")
	w.must(errOf(a.govVote(p2, true, "citizens")))
	if x, _ := a.proposal(p2); x.Yes != 1 || x.No != 0 {
		t.Fatalf("tally after revocation: %d yes, %d no", x.Yes, x.No)
	}
	// A citizen admitted after a proposal opened can't vote on it secretly
	// (not in its open root), nor by name (not a citizen before it opened).
	fay, _ := w.owner("fay")
	w.later(11 * time.Minute)
	p3, err := a.govProposeParam("FeeOracle", "setTarget", []string{"envelope", "14"}, "a third")
	w.must(err)
	for _, v := range []*Client{a, b, c} {
		w.must(errOf(v.govVouch("fay")))
	}
	gasOfLast(t, w, "admission, with the identity's leaf added to the citizens' tree")
	if !w.citizen(a, "fay") {
		t.Fatal("fay not admitted")
	}
	_, err = fay.govVote(p3, true, "citizens")
	w.mustFail(err, "wasn't in the citizens' tree when proposal")
	_, err = fay.govVote(p3, true, "citizens", true)
	w.mustFail(err, "only citizens from before it")
	_, err = fay.ch.send(fay.key, nil, "Parliament", "voteCitizen", u(p3), fay.id(), true)
	w.mustFail(err, "citizens from before the proposal only")
}

// TestVoteCircuitSoundness: a secret ≥ l (same key, another nullifier) and a
// changed message don't prove.
func TestVoteCircuitSoundness(t *testing.T) {
	k, _ := crypto.GenerateKey()
	s := zk.IdentitySecret(k)
	com := zk.IdentityCommitment(s)
	tr := zk.NewTree(zk.GroupDepth, []*big.Int{big.NewInt(7), com})
	path, bits, _ := tr.Proof(1)
	scope, msg := big.NewInt(99), big.NewInt(1)
	mk := func(secret, nullifier *big.Int) *zk.VoteCircuit {
		a := &zk.VoteCircuit{RootOpen: tr.Root(), RootNow: tr.Root(), Nullifier: nullifier, Scope: scope, Message: msg, Secret: secret}
		copy(a.OpenPath[:], zk.Bigs(path))
		copy(a.OpenBits[:], zk.Ints(bits))
		copy(a.NowPath[:], zk.Bigs(path))
		copy(a.NowBits[:], zk.Ints(bits))
		return a
	}
	if _, err := zk.Prove("vote", mk(s, zk.HashN(scope, s))); err != nil {
		t.Fatal(err)
	}
	ed := new(big.Int).Add(s, edOrder())
	if _, err := zk.Prove("vote", mk(ed, zk.HashN(scope, ed))); err == nil {
		t.Fatal("secret + l proved: a second nullifier for one identity")
	}
	if _, err := zk.Prove("vote", mk(s, zk.HashN(big.NewInt(98), s))); err == nil {
		t.Fatal("a nullifier for another scope proved")
	}
}

// TestWithdrawCircuitSoundness: no coins from nothing, no label swap.
func TestWithdrawCircuitSoundness(t *testing.T) {
	val, label, nul, sec := big.NewInt(1000), big.NewInt(42), big.NewInt(5), big.NewInt(6)
	note := zk.HashN(val, label, zk.HashN(nul, sec))
	st := zk.NewTree(zk.PoolDepth, []*big.Int{big.NewInt(1), note})
	asp := zk.NewTree(zk.ASPDepth, []*big.Int{label})
	sp, sb, _ := st.Proof(1)
	ap, ab, _ := asp.Proof(0)
	mk := func(withdraw, rest *big.Int, lbl *big.Int) *zk.WithdrawCircuit {
		a := &zk.WithdrawCircuit{NewCommitment: zk.HashN(rest, lbl, zk.HashN(big.NewInt(8), big.NewInt(9))), NullifierHash: zk.HashN(nul),
			Value: withdraw, StateRoot: st.Root(), ASPRoot: asp.Root(), Context: big.NewInt(3), Label: lbl, ExistingValue: val,
			ExistingNullifier: nul, ExistingSecret: sec, NewNullifier: big.NewInt(8), NewSecret: big.NewInt(9)}
		copy(a.StatePath[:], zk.Bigs(sp))
		copy(a.StateBits[:], zk.Ints(sb))
		copy(a.ASPPath[:], zk.Bigs(ap))
		copy(a.ASPBits[:], zk.Ints(ab))
		return a
	}
	if _, err := zk.Prove("withdraw", mk(big.NewInt(400), big.NewInt(600), label)); err != nil {
		t.Fatal(err)
	}
	over := new(big.Int).Sub(zk.Modulus, big.NewInt(1)) // 1000 - 1001 wraps around the field
	if _, err := zk.Prove("withdraw", mk(big.NewInt(1001), over, label)); err == nil {
		t.Fatal("withdrew more than the note")
	}
	if _, err := zk.Prove("withdraw", mk(big.NewInt(400), big.NewInt(700), label)); err == nil {
		t.Fatal("change larger than what is left")
	}
	if _, err := zk.Prove("withdraw", mk(big.NewInt(400), big.NewInt(600), big.NewInt(43))); err == nil {
		t.Fatal("another label proved")
	}
}

func edOrder() *big.Int {
	o, _ := new(big.Int).SetString("2736030358979909402780800718157159386076813972158567259200215660948447373041", 10)
	return o
}

func mustRule(t *testing.T, c *Client, rule string) []*big.Int {
	t.Helper()
	ls, err := c.aspRule(rule)
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

func mustNote(t *testing.T, c *Client) PoolNote {
	t.Helper()
	wl, err := c.syncNotes()
	if err != nil || len(c.unspent(wl)) == 0 {
		t.Fatalf("no note: %v", err)
	}
	return c.unspent(wl)[0]
}

// gasOfLast logs the gas of the latest block's transaction.
func gasOfLast(t *testing.T, w *world, what string) uint64 {
	t.Helper()
	blk, err := w.ch.b.HeaderByNumber(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if what != "" {
		t.Logf("gas: %s: %d", what, blk.GasUsed)
	}
	return blk.GasUsed
}

// TestPrivacyGas measures what PRIVACY.md quotes: proof times, verifier gas,
// deposits and withdrawals in steady state, a secret vote.
func TestPrivacyGas(t *testing.T) {
	w := newWorld(t)
	relay := w.startRelay()
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(bob.enableStealth())
	est := func(contract string, data []byte) uint64 {
		to := common.HexToAddress(w.ch.dep.Contracts[contract])
		g, err := w.ch.b.EstimateGas(context.Background(), ethereum.CallMsg{To: &to, Data: data})
		if err != nil {
			t.Fatalf("%s: %v", contract, err)
		}
		return g - 21000
	}
	h := append(common.LeftPadBytes([]byte{1}, 32), common.LeftPadBytes([]byte{2}, 32)...)
	t.Logf("gas: Poseidon2 compress (call, minus the 21000 base): %d", est("Poseidon2Hasher", h))
	var first, last uint64
	for i := 0; i < 16; i++ {
		_, err := alice.poolDeposit(coins(1))
		w.must(err)
		g := gasOfLast(t, w, "")
		if i == 0 {
			first = g
		}
		last = g
	}
	t.Logf("gas: deposit, first in the pool %d, 16th %d", first, last)
	_, err := alice.aspPublish("all", mustRule(t, alice, "all"), "")
	w.must(err)
	t.Logf("gas: asp publish of 16 labels: %d", gasOfLast(t, w, ""))
	var times []int64
	for i := 0; i < 5; i++ {
		_, err := alice.poolSend("bob", coins(0.1), poolSendOpts{dry: true})
		w.must(err)
		times = append(times, zk.LastProveTime.Milliseconds())
	}
	req := alice.lastPool
	ctx, _ := w.ch.call("PrivacyPool", "context", req.W)
	data, err := w.ch.abis["PoolVerifier"].Pack("verifyProof", common.FromHex(req.Proof),
		[6]*big.Int{req.NewCommitment, req.NullifierHash, req.Value, req.StateRoot, req.ASPRoot, ctx[0].(*big.Int)})
	w.must(err)
	t.Logf("withdraw proofs (ms): %v; gas: verifyProof %d", times, est("PoolVerifier", data))
	w.must(errOf(w.ch.send(relay.key, nil, "PrivacyPool", "withdraw", req.args()...)))
	t.Logf("gas: withdraw to a stealth address, relayed: %d", gasOfLast(t, w, ""))
	_, err = alice.poolSend("alice", coins(0.1), poolSendOpts{})
	w.must(err)
	t.Logf("gas: withdraw to a bank balance, relayed: %d", gasOfLast(t, w, ""))
	fresh, _ := crypto.GenerateKey()
	_, err = alice.poolSend(crypto.PubkeyToAddress(fresh.PublicKey).Hex(), coins(0.1), poolSendOpts{})
	w.must(err)
	t.Logf("gas: withdraw to an address, relayed: %d", gasOfLast(t, w, ""))
}

// TestPrivateCLI: the commands as typed.
func TestPrivateCLI(t *testing.T) {
	w := newWorld(t)
	w.startRelay()
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(bob.enableStealth())
	testChain = w.ch
	defer func() { testChain = nil }()
	t.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	cli := func(args ...string) error {
		return run(append(args, "--dir", alice.dir, "--relay", w.relay))
	}
	w.must(cli("private", "deposit", "1"))
	w.must(cli("asp", "publish", "--name", "everyone", "--rule", "all"))
	w.must(cli("asp", "list"))
	w.must(cli("private", "send", "bob", "0.25", "--asp", "everyone"))
	w.must(cli("private", "balance"))
	w.mustFail(cli("private", "send", "bob", "0.25", "--asp", "nobody"), "doesn't hold")
	w.mustFail(cli("asp", "flag", "0x01", "a test"), "only citizens")
	if ns, _ := bob.notes(); len(ns) != 1 {
		t.Fatalf("bob's notes: %v", ns)
	}
	w.mustFail(run([]string{"whoami", "--proxy", "ftp://x", "--dir", alice.dir}), "--proxy")
}

// TestProxy: everything outside goes through the proxy; loopback doesn't.
func TestProxy(t *testing.T) {
	tr := http.DefaultTransport.(*http.Transport)
	saved, savedDA := tr.Proxy, daHTTP.Transport
	defer func() {
		tr.Proxy, daHTTP.Transport = saved, savedDA
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
			os.Unsetenv(k)
		}
	}()
	w := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	w(useProxy("socks5h://127.0.0.1:9050"))
	for u, want := range map[string]string{"https://rpc.example.org/": "socks5://127.0.0.1:9050", "http://abc.onion/x": "socks5://127.0.0.1:9050",
		"http://127.0.0.1:8545/": "", "http://localhost:8650/": ""} {
		req, _ := http.NewRequest("GET", u, nil)
		for _, p := range []func(*http.Request) (*url.URL, error){tr.Proxy, daHTTP.Transport.(*http.Transport).Proxy} {
			got, _ := p(req)
			if (got == nil && want != "") || (got != nil && got.String() != want) {
				t.Fatalf("%s: proxy %v, want %q", u, got, want)
			}
		}
	}
	if os.Getenv("HTTPS_PROXY") == "" {
		t.Fatal("websockets: no proxy in the environment")
	}
	if useProxy("ftp://h:1") == nil || useProxy("nonsense") == nil {
		t.Fatal("a bad proxy URL accepted")
	}
}
