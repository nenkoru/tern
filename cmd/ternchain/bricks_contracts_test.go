// review/BRICKS.md, the contract items: TestUSD on devnets only (1), the fee
// mechanism replaceable by vote (2), RulingCondition (5), escrow for items
// and swaps (6), ICommunity and moderation claims (7), names for types (8),
// set join rules, secret ballots for any group and personhood sources (9).
package main

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// bricksSend: a call to a contract this client has no binding for (deployed in the test).
func bricksSend(w *world, key *ecdsa.PrivateKey, name string, to common.Address, method string, args ...any) error {
	w.t.Helper()
	a, err := loadArtifact(name)
	if err != nil {
		return err
	}
	data, err := a.abi.Pack(method, args...)
	if err != nil {
		return err
	}
	_, err = w.ch.sendRaw(key, nil, &to, data)
	return err
}

func bricksNow(w *world) uint64 {
	h, err := w.ch.b.HeaderByNumber(context.Background(), nil)
	w.must(err)
	return h.Time
}

func bricksCall(w *world, name string, to common.Address, method string, args ...any) []any {
	w.t.Helper()
	a, _ := loadArtifact(name)
	data, err := a.abi.Pack(method, args...)
	w.must(err)
	out, err := w.ch.b.CallContract(context.Background(), ethereum.CallMsg{To: &to, Data: data}, nil)
	w.must(err)
	v, err := a.abi.Unpack(method, out)
	w.must(err)
	return v
}

// (1) TestUSD: devnets only; the "usd" alias only where it exists.
func TestBricksTestUSDDevnetOnly(t *testing.T) {
	for _, x := range []struct {
		id     uint64
		devnet bool
		want   bool
	}{{1337, false, true}, {31337, false, true}, {8376, false, false}, {1, false, false}, {8376, true, true}} {
		if got := testTokens(Params{Devnet: x.devnet}, x.id); got != x.want {
			t.Fatalf("chain %d devnet=%v: TestUSD %v", x.id, x.devnet, got)
		}
	}
	w := newWorld(t) // the simulated chain is 1337: a devnet
	alice, _ := w.owner("alice")
	if a, err := alice.tokenAddr("usd"); err != nil || a.Hex() != w.ch.dep.Contracts["TestUSD"] {
		t.Fatalf("usd on a devnet: %v %v", a, err)
	}
	delete(alice.ch.dep.Contracts, "TestUSD") // as on any other network
	delete(alice.ch.c, "TestUSD")
	_, err := alice.tokenAddr("usd")
	w.mustFail(err, "devnet-only")
	w.mustFail(alice.mintUSD(alice.addr(), coins(1)), "devnet-only")
}

// (2) FeeOracle: setParams and setRoundLength are the timelock's; a new round
// length starts with the next round and round numbers go on.
func TestBricksFeeOracleRounds(t *testing.T) {
	w := newWorld(t)
	me := w.faucetAddr()
	fo := deployNew(w, "FeeOracle", big.NewInt(1e15), uint64(3600), coins(0.01), uint16(1000), coins(1), me, me, big.NewInt(64), coins(100),
		[][32]byte{envelopeKind}, []*big.Int{big.NewInt(10)}, [32]byte{}, "test", []common.Address{})
	alice, _ := w.owner("alice")
	w.mustFail(bricksSend(w, alice.key, "FeeOracle", fo, "setRoundLength", uint64(600)), "governance only")
	w.mustFail(bricksSend(w, alice.key, "FeeOracle", fo, "setParams", coins(1), uint16(500), coins(1), uint16(1), uint16(1), uint64(1), uint16(8)), "governance only")
	w.mustFail(bricksSend(w, w.faucet, "FeeOracle", fo, "setParams", coins(1), uint16(0), coins(1), uint16(1), uint16(1), uint64(1), uint16(8)), "params")
	w.mustFail(bricksSend(w, w.faucet, "FeeOracle", fo, "setParams", coins(1), uint16(500), coins(1), uint16(1), uint16(1), uint64(1), uint16(300)), "1 to 256")
	w.must(bricksSend(w, w.faucet, "FeeOracle", fo, "setParams", coins(1), uint16(500), coins(2), uint16(4000), uint16(1500), uint64(5), uint16(32)))
	for m, want := range map[string]uint64{"maxStepBps": 500, "farSlashBps": 4000, "roundSlashBps": 1500, "slashCooldown": 5, "maxVotes": 32} {
		var got uint64
		switch v := bricksCall(w, "FeeOracle", fo, m)[0].(type) {
		case uint16:
			got = uint64(v)
		case uint64:
			got = v
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", m, got, want)
		}
	}
	w.later(1000 * time.Second)
	r0 := bricksCall(w, "FeeOracle", fo, "round")[0].(uint64)
	w.must(bricksSend(w, w.faucet, "FeeOracle", fo, "setRoundLength", uint64(600)))
	if l := bricksCall(w, "FeeOracle", fo, "roundLength")[0].(uint64); l != 3600 {
		t.Fatalf("the round in progress changed length: %d", l)
	}
	w.later(3000 * time.Second) // past the current round's end
	if l, r := bricksCall(w, "FeeOracle", fo, "roundLength")[0].(uint64), bricksCall(w, "FeeOracle", fo, "round")[0].(uint64); l != 600 || r <= r0 {
		t.Fatalf("after the switch: length %d, round %d (was %d)", l, r, r0)
	}
	st := bricksCall(w, "FeeOracle", fo, "start")[0].(uint64)
	if (bricksNow(w)-st)/600+r0+1 != bricksCall(w, "FeeOracle", fo, "round")[0].(uint64) {
		t.Fatal("the new schedule doesn't continue the round numbers")
	}
}

// (2) and (9): by a vote of both houses the FeeOracle's parameters change,
// the pricing mechanism itself is replaced (the Inbox follows the
// Directory), and a personhood source admits a citizen nobody vouched for.
func TestBricksGovernedMechanisms(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b, c, d := f["ana"], f["ben"], f["cai"], f["dov"]
	w.must(a.lockVotes(coins(30)))
	w.must(b.lockVotes(coins(20)))
	w.later(2 * time.Second)
	coin, cit := []*Client{a, b}, []*Client{a, b, c, d}

	p, err := a.govProposeParam("FeeOracle", "setParams", []string{"10000000000000000", "1000", "100000000000000000000", "4000", "1500", "2", "32"}, "softer slashing")
	w.must(err)
	w.pass(p, coin, cit)
	if v := mustCall(t, w.ch, "FeeOracle", "farSlashBps").(uint16); v != 4000 {
		t.Fatalf("farSlashBps %d", v)
	}

	// A new oracle at twice the basket price, published by vote: the Inbox charges by it at once.
	fee0 := mustCall(t, w.ch, "Inbox", "fee").(*big.Int)
	basket := new(big.Int).Mul(mustCall(t, w.ch, "FeeOracle", "basketPrice").(*big.Int), big.NewInt(2))
	tl, tr := common.HexToAddress(w.ch.dep.Contracts["TernTimelock"]), common.HexToAddress(w.ch.dep.Contracts["Treasury"])
	inbox := common.HexToAddress(w.ch.dep.Contracts["Inbox"])
	v2 := deployNew(w, "FeeOracle", basket, uint64(86400), coins(0.01), uint16(1000), coins(100), tl, tr, big.NewInt(64), coins(100),
		[][32]byte{envelopeKind}, []*big.Int{big.NewInt(10)}, crypto.Keccak256Hash(basketSpec), basketURI, []common.Address{inbox})
	spec := writeDoc(t, "oracle-v2.md", "FeeOracle v2: the same interface, another price")
	audit := writeDoc(t, "oracle-v2-audit.md", "no findings")
	p2, err := a.govProposeUpgrade("FeeOracle", v2, spec, "", audit, "", "the Inbox follows the Directory")
	w.must(err)
	w.pass(p2, coin, cit)
	if o := mustCall(t, w.ch, "Inbox", "oracle").(common.Address); o != v2 {
		t.Fatalf("the Inbox prices with %s, not v2", o.Hex())
	}
	fee1 := mustCall(t, w.ch, "Inbox", "fee").(*big.Int)
	if new(big.Int).Mul(fee0, big.NewInt(19)).Cmp(new(big.Int).Mul(fee1, big.NewInt(10))) > 0 {
		t.Fatalf("fee %s -> %s: not the new oracle's", fee0, fee1)
	}
	if _, err := a.send(SendOpts{To: "ben", Text: "priced by v2"}); err != nil {
		t.Fatal(err)
	}

	// Personhood: an issuer's claims count once both houses add the source.
	kyc, _ := w.owner("kyc")
	zed, _ := w.owner("zed")
	src := deployNew(w, "ClaimPersonhood", w.ch.addr("EAS"), w.ch.addr("Registry"), idOf("kyc"), kyc.schema())
	uid, err := kyc.easAttest(zed.id(), "personhood", nil, 10000, 0, common.Hash{})
	w.must(err)
	w.must(bricksSend(w, zed.key, "ClaimPersonhood", src, "register", zed.id(), uid))
	if n, _ := zed.personhood("zed"); n != 0 {
		t.Fatalf("personhood before the vote: %d", n)
	}
	w.mustFail(bricksSend(w, zed.key, "Citizens", w.ch.addr("Citizens"), "setSources", []common.Address{src}, []uint16{10000}, uint16(10000)), "governance only")
	p3, err := a.govProposeParam("Citizens", "setSources", []string{src.Hex(), "10000", "10000"}, "passport proofs count")
	w.must(err)
	w.pass(p3, coin, cit) // a citizens'-house change: three exit windows
	if n, _ := zed.personhood("zed"); n != 10000 {
		t.Fatalf("personhood after the vote: %d", n)
	}
	w.later(11 * time.Minute) // the name's minimum age
	w.must(errOf(w.ch.send(a.key, nil, "Citizens", "admit", zed.id())))
	if !zed.isCitizen("zed") {
		t.Fatal("zed was not admitted on the source's proof")
	}
	// A revoked claim stops counting.
	w.must(kyc.easRevoke(uid))
	if n, _ := zed.personhood("zed"); n != 0 {
		t.Fatalf("a revoked claim still counts %d", n)
	}
}

// (5) RulingCondition: aggregation rules, a dispute window with a bond that
// anyone may post, and the 12-hour floor.
func TestBricksRulingCondition(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	erin, _ := w.owner("erin")
	frank, _ := w.owner("frank")
	w.must(alice.deposit(coins(10)))
	day := uint64(86400)
	bal := func(c *Client) *big.Int { b, _ := c.bankBalance(c.f.Name); return b }
	panel := []string{"carol", "dave", "erin"}

	// mean of 10000, 5000, 0: half to the payee, half back (the payee accepted).
	l1, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*day, RulingOpts{Deciders: panel, K: 3, Rule: "mean"}, "a logo")
	w.must(err)
	w.must(bob.acceptLock(l1))
	w.later(time.Minute)
	var uids []common.Hash
	for i, j := range []*Client{carol, dave, erin} {
		if i == 2 { // two of three recorded: no decision yet
			w.mustFail(alice.settleRuling(l1, uids), "not enough rulings")
		}
		uid, err := j.rulingDecide(l1, int64(10000-5000*i), "my view")
		w.must(err)
		uids = append(uids, uid)
	}
	b0 := bal(bob)
	w.must(alice.settleRuling(l1, uids))
	if d := new(big.Int).Sub(bal(bob), b0); d.Cmp(coins(0.5)) != 0 || alice.lockLeft(l1).Sign() != 0 {
		t.Fatalf("mean: bob got %s, left %s", fmtCoins(d), fmtCoins(alice.lockLeft(l1)))
	}
	// A ruling made for one lock decides no other.
	l1b, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*day, RulingOpts{Deciders: panel, K: 3, Rule: "mean"}, "a logo")
	w.must(err)
	w.mustFail(alice.settleRuling(l1b, uids), "nothing moved")

	// threshold, quota 2: 6000, 4000, 9000 are two yes: everything to the payee.
	l2, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*day, RulingOpts{Deciders: panel, K: 3, Rule: "threshold", Quota: 2}, "a site")
	w.must(err)
	w.later(time.Minute)
	uids = nil
	for i, j := range []*Client{carol, dave, erin} {
		uid, err := j.rulingDecide(l2, []int64{6000, 4000, 9000}[i], "")
		w.must(err)
		uids = append(uids, uid)
	}
	b0 = bal(bob)
	w.must(alice.settleRuling(l2, uids))
	if d := new(big.Int).Sub(bal(bob), b0); d.Cmp(coins(1)) != 0 {
		t.Fatalf("threshold: bob got %s", fmtCoins(d))
	}
	_, _, err = alice.openRuling("bob", coins(1), alice.chainNowU()+2*day, RulingOpts{Deciders: panel, K: 3, Rule: "threshold"}, "x")
	w.mustFail(err, "quota")

	// Pay unless disputed, anyone disputes with a bond: frank (no party) disputes,
	// carol rules for the payee in full, the bond goes to the payee.
	opt := RulingOpts{Deciders: []string{"carol"}, Window: 3600, DisputeBond: coins(0.1), AnyoneDisputes: true}
	l3, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*day, opt, "an assertion")
	w.must(err)
	w.mustFail(alice.settleRuling(l3, nil), "challenge window is open")
	_, err = w.ch.send(frank.key, nil, "RulingCondition", "dispute", u(l3), "no bond")
	w.mustFail(err, "dispute bond")
	w.must(frank.disputeRuling(l3, "the claim is false"))
	w.mustFail(frank.disputeRuling(l3, "again"), "already disputed")
	w.later(time.Minute)
	uid, err := carol.rulingDecide(l3, 10000, "the claim holds")
	w.must(err)
	b0 = bal(bob)
	w.must(alice.settleRuling(l3, []common.Hash{uid}))
	w.must(errOf(w.ch.send(alice.key, nil, "RulingCondition", "settleBond", u(l3))))
	if d := new(big.Int).Sub(bal(bob), b0); d.Cmp(new(big.Int).Add(coins(1), coins(0.1))) != 0 {
		t.Fatalf("bob got %s, want the lock and the frivolous dispute's bond", fmtCoins(d))
	}
	// Undisputed: paid after the window.
	l4, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*day, RulingOpts{Deciders: []string{"carol"}, Window: 3600}, "net 30")
	w.must(err)
	w.mustFail(frank.disputeRuling(l4, "not mine"), "only the payer disputes")
	w.later(3601 * time.Second)
	w.mustFail(alice.disputeRuling(l4, "late"), "challenge window is over")
	w.must(bob.settleRuling(l4, nil))

	// The floor: a dispute must leave the deciders 12 hours before the refund time.
	now := alice.chainNowU()
	dm, _ := rulingDemandType().Pack(rulingDemand{[][32]byte{idOf("carol")}, 1, 0, 0, alice.schema(), common.Hash{1}, crypto.Keccak256Hash([]byte("ruling")),
		0, now + 3*3600, new(big.Int), false})
	l5, err := alice.lock("bob", coins(0.1), now+13*3600, "RulingCondition", dm)
	w.must(err)
	w.later(2 * time.Hour)
	w.mustFail(alice.disputeRuling(l5, "now"), "too late for the deciders")
}

// (6) Items are plain ERC-1155; the Bank holds them; SwapCondition sells them.
func TestBricksItemEscrowAndSwap(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	w.must(bob.deposit(coins(10)))
	if _, ok := w.ch.abis["Items"].Methods["offer"]; ok {
		t.Fatal("Items still has its own offer book")
	}
	sword, err := alice.createItem("ipfs://sword.json", 10, false)
	w.must(err)
	w.must(alice.mintItem(sword, "alice", 4))
	a0, _ := alice.bankBalance("alice")

	offer, err := alice.offerItem(sword, 1, coins(2), "", "")
	w.must(err)
	if list, _ := bob.itemOffers(); len(list) != 1 || list[0]["price"] != fmtCoins(coins(2)) {
		t.Fatalf("offers: %v", list)
	}
	if _, err := bob.buyItem(offer); err != nil {
		t.Fatal(err)
	}
	if n, _ := bob.itemBalance("bob", sword); n != 1 {
		t.Fatalf("bob has %d swords", n)
	}
	if a1, _ := alice.bankBalance("alice"); new(big.Int).Sub(a1, a0).Cmp(coins(2)) != 0 {
		t.Fatalf("alice was paid %s", fmtCoins(new(big.Int).Sub(a1, a0)))
	}
	if list, _ := bob.itemOffers(); len(list) != 0 {
		t.Fatalf("a sold offer is still listed: %v", list)
	}
	_, err = bob.buyItem(offer)
	w.mustFail(err, "not open")

	// Reserved for carol: bob's answer doesn't swap (his lock comes back), carol's does.
	o2, err := alice.offerItem(sword, 1, coins(1), "carol", "")
	w.must(err)
	lb, err := bob.buyItem(o2)
	w.mustFail(err, "reserved")
	w.must(bob.refundEarly(lb, nil))
	w.must(carol.deposit(coins(2)))
	_, err = carol.buyItem(o2)
	w.must(err)
	if n, _ := carol.itemBalance("carol", sword); n != 1 {
		t.Fatalf("carol has %d swords", n)
	}

	// Cancelled: the items come back.
	o3, err := alice.offerItem(sword, 2, coins(1), "", "")
	w.must(err)
	w.mustFail(bob.refundEarly(o3, nil), "payer's side")
	w.must(alice.cancelItemOffer(o3))
	if n, _ := alice.itemBalance("alice", sword); n != 2 {
		t.Fatalf("alice has %d swords after cancelling", n)
	}

	// Sent straight to the Bank with the owner in the data: held for that owner.
	data, _ := abi.Arguments{{Type: abiType("bytes32")}}.Pack(alice.id())
	w.must(errOf(w.ch.send(alice.key, nil, "Items", "safeTransferFrom", alice.me(), w.ch.addr("Bank"), u(sword), u(1), data)))
	if n := mustCall(t, w.ch, "Bank", "itemBalance", alice.id(), w.ch.addr("Items"), u(sword)).(*big.Int); n.Uint64() != 1 {
		t.Fatalf("held for alice: %s", n)
	}
	w.mustFail(errOf(w.ch.send(alice.key, nil, "Items", "safeTransferFrom", alice.me(), w.ch.addr("Bank"), u(sword), u(1), []byte{})), "say whose")
	// Items go to names, not splits.
	split, err := alice.createSplit([]string{"alice", "carol"}, []uint16{5000, 5000})
	w.must(err)
	_, err = w.ch.send(alice.key, nil, "Bank", "lockItem", alice.id(), split, w.ch.addr("Items"), u(sword), u(1), alice.chainNowU()+86400,
		w.ch.addr("SwapCondition"), []byte{})
	w.mustFail(err, "not a split")
}

// (7) Commons is an ICommunity and issues moderation claims; a moderator's own
// claim counts, a stranger's doesn't, a reversed action's claim is revoked.
func TestBricksModerationClaims(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	spammer, _ := w.owner("spammer")
	const C = "mersin"
	hour := uint64(3600)
	w.must(nina.found(C, CommonsParams{Term: 30 * 86400, VoteWindow: hour, VetoWindow: hour, VetoBps: 5000, PetitionBps: 5000, QuorumBps: 3000,
		Budget: coins(1)}, "be kind"))
	w.join(C, nina, alice, bob, spammer)
	commons, cid := w.ch.addr("Commons"), idOf(C)
	if !nina.canModerate(commons, cid, nina.id()) || nina.canModerate(commons, cid, alice.id()) {
		t.Fatal("canModerate")
	}
	if !mustCall(t, w.ch, "Commons", "holdsRole", cid, crypto.Keccak256Hash([]byte("admin")), nina.id()).(bool) ||
		!mustCall(t, w.ch, "Commons", "isMember", cid, alice.id()).(bool) {
		t.Fatal("ICommunity views through Commons")
	}
	ban, err := nina.act("ban", cid, nina.id(), spammer.id(), "spam")
	w.must(err)
	w.must(errOf(w.ch.send(bob.key, nil, "Commons", "attestAction", u(ban))))
	_, err = w.ch.send(bob.key, nil, "Commons", "attestAction", u(ban))
	w.mustFail(err, "attested already")
	ref := crypto.Keccak256Hash([]byte("post 7"))
	_, err = nina.moderationClaim(commons, cid, modHide, ref, "off topic")
	w.must(err)
	_, err = alice.moderationClaim(commons, cid, modHide, crypto.Keccak256Hash([]byte("post 8")), "I don't like it")
	w.mustFail(err, "don't moderate")
	// alice attests anyway, by hand: it doesn't count.
	enc, _ := moderationArgs.Pack(commons, cid, uint8(modBan), bob.id(), alice.id(), "mine")
	w.must(errOf(w.ch.send(alice.key, nil, "EAS", "attest", easRequest{Schema: schemaUID(moderationSchema),
		Data: easRequestData{Revocable: true, Data: enc, Value: big.NewInt(0)}})))
	hidden, banned, err := bob.moderationState(commons, cid)
	w.must(err)
	if !banned[spammer.id()] || banned[bob.id()] || !hidden[ref] || len(hidden) != 1 {
		t.Fatalf("state: hidden %v banned %v", hidden, banned)
	}
	// Members reverse the ban: the claim is revoked and stops counting.
	_, err = w.ch.send(bob.key, nil, "Commons", "revokeAction", u(ban))
	w.mustFail(err, "not reversed")
	w.must(w.commons(alice, "veto", u(ban), alice.id()))
	w.must(w.commons(bob, "veto", u(ban), bob.id()))
	w.must(errOf(w.ch.send(bob.key, nil, "Commons", "revokeAction", u(ban))))
	if _, banned, _ = bob.moderationState(commons, cid); banned[spammer.id()] {
		t.Fatal("a reversed ban still counts")
	}
}

// (8) Names for types: anyone publishes in its own namespace and alone adds its versions.
func TestBricksSchemaNames(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	v, uid1, err := alice.publishType("card/poll", "string question,string[] options", "ipfs://poll-v1")
	w.must(err)
	if v != 1 {
		t.Fatalf("version %d", v)
	}
	// Names are per publisher (audit 6 CM-3): bob's card/poll is his own, alice's stays hers.
	bv, buid, err := bob.publishType("card/poll", "string question", "ipfs://mine")
	w.must(err)
	v, uid2, err := alice.publishType("card/poll", "string question,string[] options,uint64 closes", "ipfs://poll-v2")
	w.must(err)
	if got, n, err := bob.typeOf("alice", "card/poll"); err != nil || got != uid2 || n != 2 || v != 2 || uid1 == uid2 {
		t.Fatalf("typeOf: %s %d %v", got.Hex(), n, err)
	}
	if got, n, err := alice.typeOf("bob", "card/poll"); err != nil || got != buid || n != 1 || bv != 1 {
		t.Fatalf("bob's typeOf: %s %d %v", got.Hex(), n, err)
	}
	for _, bad := range []string{"Card/Poll", "poll", "card//x", "/card", "card/"} {
		_, _, err := bob.publishType(bad, "string x", "")
		w.mustFail(err, "name")
	}
	_, err = w.ch.send(bob.key, nil, "SchemaNames", "publish", bob.id(), "reaction/heart", common.Hash{7}, "")
	w.mustFail(err, "no such EAS schema")
	_, _, err = bob.publishType("reaction/heart", "bytes32 target", "")
	w.must(err)
	if n := mustCall(t, w.ch, "SchemaNames", "count").(*big.Int); n.Uint64() != 3 {
		t.Fatalf("names: %s", n)
	}
	if n := mustCall(t, w.ch, "SchemaNames", "countBy", bob.id()).(*big.Int); n.Uint64() != 2 {
		t.Fatalf("bob's names: %s", n)
	}
}

// (9) A set with a join rule (citizens only), as an ICommunity, and a secret
// ballot for its members.
func TestBricksSetJoinRuleAndBallot(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b := f["ana"], f["ben"]
	zed, _ := w.owner("zed")
	w.must(errOf(w.ch.send(a.key, nil, "Sets", "create", "club", a.id(), uint8(0))))
	set := common.Hash(mustCall(t, w.ch, "Sets", "idOf", "club").([32]byte))
	_, err := w.ch.send(b.key, nil, "Sets", "setJoinRule", set, b.id(), w.ch.addr("CitizenJoinRule"))
	w.mustFail(err, "admins only")
	w.must(errOf(w.ch.send(a.key, nil, "Sets", "setJoinRule", set, a.id(), w.ch.addr("CitizenJoinRule"))))
	w.must(errOf(w.ch.send(b.key, nil, "Sets", "request", set, b.id())))
	_, err = w.ch.send(zed.key, nil, "Sets", "request", set, zed.id())
	w.mustFail(err, "join rule refuses")
	if !mustCall(t, w.ch, "Sets", "isMember", set, b.id()).(bool) || mustCall(t, w.ch, "Sets", "isMember", set, zed.id()).(bool) {
		t.Fatal("membership by the join rule")
	}
	if a.canModerate(w.ch.addr("Sets"), set, b.id()) {
		t.Fatal("ben moderates without a role")
	}
	w.must(errOf(w.ch.send(a.key, nil, "Sets", "setRole", set, a.id(), "moderator", b.id(), true)))
	if !a.canModerate(w.ch.addr("Sets"), set, b.id()) || !mustCall(t, w.ch, "Sets", "holdsRole", set, crypto.Keccak256Hash([]byte("moderator")), b.id()).(bool) {
		t.Fatal("ben's moderator role")
	}

	// The set's tree and a secret ballot.
	sets := w.ch.addr("Sets")
	w.must(errOf(w.ch.send(zed.key, nil, "GroupTrees", "open", sets, set)))
	g := a.groupID(sets, set)
	w.must(a.groupJoinTree(g))
	w.must(b.groupJoinTree(g))
	w.must(zed.groupJoinTree(g)) // not a member: no leaf
	if li := mustCall(t, w.ch, "GroupTrees", "leafOf", g, zed.id()).(*big.Int); li.Sign() != 0 {
		t.Fatal("a non-member is in the group's tree")
	}
	r, err2 := w.ch.send(a.key, nil, "GroupTrees", "openBallot", g, uint8(2), a.chainNowU()+3600, "new logo?")
	w.must(err2)
	id, err := a.firstIndexed(r, "GroupTrees", "BallotOpened")
	w.must(err)
	_, err = a.ballotVote(id, 1, w.faucet) // a relayer submits
	w.must(err)
	_, err = b.ballotVote(id, 0, w.faucet)
	w.must(err)
	_, err = a.ballotVote(id, 0, w.faucet)
	w.mustFail(err, "voted already")
	out, err := w.ch.call("GroupTrees", "ballot", u(id))
	w.must(err)
	if tally := out[1].([]*big.Int); len(tally) != 2 || tally[0].Uint64() != 1 || tally[1].Uint64() != 1 {
		t.Fatalf("tally %v", tally)
	}
	_, err = zed.ballotVote(id, 1, w.faucet)
	w.mustFail(err, "wasn't in the group's tree")
}
