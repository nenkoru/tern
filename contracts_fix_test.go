package main

// Regression tests for the contract-side findings of review/AUDIT-SECURITY-3.md.
// Each starts from the audit's proof of concept and checks it no longer works.

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/artifact"
)

// ---- IN-1, IN-2: a hash pays for what its batch carries

func TestFixPostRootPaysForWhatItCarries(t *testing.T) {
	w := newWorld(t)
	ch := w.ch
	poster, _ := crypto.GenerateKey()
	w.fund2(crypto.PubkeyToAddress(poster.PublicKey), coins(20))
	_, err := ch.send(poster, coins(1), "Inbox", "stakeDA", "http://node.invalid")
	w.must(err)
	fee, _ := ch.fee()
	treasury := mustCall(t, ch, "Inbox", "treasury").(common.Address)

	// The PoC: five envelopes committed as one.
	var envs []Envelope
	for i := 0; i < 5; i++ {
		envs = append(envs, Envelope{Epoch: 1, CT: []byte("hello bob, this one was free")})
	}
	batch := encodeBatch(envs)
	root := crypto.Keccak256Hash(batch)
	_, err = ch.send(poster, coins(1), "Inbox", "postRoot", root, big.NewInt(1), big.NewInt(int64(len(batch))))
	w.must(err)
	paid, err := ch.call("Inbox", "paid", root)
	w.must(err)
	if paid[0].(*big.Int).Int64() != 1 || paid[1].(*big.Int).Int64() != int64(len(batch)) {
		t.Fatalf("paid(root) = %v, want 1 envelope of %d bytes", paid, len(batch))
	}
	if mustCall(t, ch, "Inbox", "matchesPaid", root, batch).(bool) {
		t.Fatal("a 5-envelope batch matches a 1-envelope payment")
	}

	// A challenged undercount can't be resolved, and blocks the poster's unstake past its lock.
	bond := mustCall(t, ch, "Inbox", "challengeBond").(*big.Int)
	challenger, _ := w.owner("watcher")
	_, err = ch.send(challenger.key, bond, "Inbox", "challenge", root)
	w.must(err)
	_, err = ch.send(poster, nil, "Inbox", "resolve", root, batch)
	w.mustFail(err, "differs from what was paid")
	w.later(3600 * time.Second) // past the challenge window, within resolve + lock
	// Anyone holding the batch proves the fraud: the poster's stake is slashed, the root is lost.
	prover, _ := w.owner("prover")
	p0, _ := ch.balance(prover.addr())
	t0, _ := ch.balance(treasury)
	c0, _ := ch.balance(challenger.addr())
	rc, err := ch.send(prover.key, nil, "Inbox", "proveFraud", root, batch)
	w.must(err)
	gas := new(big.Int).Mul(new(big.Int).SetUint64(rc.GasUsed), rc.EffectiveGasPrice)
	p1, _ := ch.balance(prover.addr())
	if got := new(big.Int).Sub(new(big.Int).Add(p1, gas), p0); got.Cmp(exact("0.5")) != 0 {
		t.Fatalf("prover got %s, want half the 1-coin stake", fmtCoins(got))
	}
	if t1, _ := ch.balance(treasury); new(big.Int).Sub(t1, t0).Cmp(exact("0.5")) != 0 {
		t.Fatal("the treasury didn't get the other half")
	}
	if c1, _ := ch.balance(challenger.addr()); new(big.Int).Sub(c1, c0).Cmp(bond) != 0 {
		t.Fatal("the challenger's bond didn't come back")
	}
	st, _ := ch.call("Inbox", "commitment", root)
	if st[2].(uint8) != 4 {
		t.Fatalf("state %d, want 4 (lost)", st[2])
	}
	if s := mustCall(t, ch, "Inbox", "stake", crypto.PubkeyToAddress(poster.PublicKey)).(*big.Int); s.Sign() != 0 {
		t.Fatalf("stake left: %s", fmtCoins(s))
	}

	// An honest batch: proveFraud refuses it, resolve takes it.
	_, err = ch.send(poster, coins(1), "Inbox", "stakeDA", "http://node.invalid")
	w.must(err)
	honest := encodeBatch(envs[:2])
	hr := crypto.Keccak256Hash(honest)
	_, err = ch.send(poster, coins(1), "Inbox", "postRoot", hr, big.NewInt(2), big.NewInt(int64(len(honest))))
	w.must(err)
	_, err = ch.send(prover.key, nil, "Inbox", "proveFraud", hr, honest)
	w.mustFail(err, "is what was paid for")
	_, err = ch.send(challenger.key, bond, "Inbox", "challenge", hr)
	w.must(err)
	// IN-2: past lockedUntil, an open challenge still keeps the stake in.
	w.later(5500 * time.Second)
	_, err = ch.send(poster, nil, "Inbox", "unstakeDA", coins(1))
	w.mustFail(err, "challenge is open")
	_, err = ch.send(prover.key, nil, "Inbox", "expire", hr)
	w.must(err)

	// The fee follows the size too: one envelope declared, 40 KB of hash = 10 envelopes' fee.
	_, err = ch.send(poster, coins(1), "Inbox", "stakeDA", "http://node.invalid")
	w.must(err)
	t2, _ := ch.balance(treasury)
	_, err = ch.send(poster, coins(1), "Inbox", "postRoot", common.Hash{7}, big.NewInt(1), big.NewInt(40000))
	w.must(err)
	t3, _ := ch.balance(treasury)
	if got, want := new(big.Int).Sub(t3, t2), new(big.Int).Mul(fee, big.NewInt(10)); got.Cmp(want) != 0 {
		t.Fatalf("a 40 KB hash paid %s, want %s", got, want)
	}
	if q := mustCall(t, ch, "Inbox", "rootFee", big.NewInt(1), big.NewInt(40000)).(*big.Int); q.Cmp(new(big.Int).Mul(fee, big.NewInt(10))) != 0 {
		t.Fatalf("rootFee quote %s", q)
	}
}

// IN-3: postPaid with a ceiling.
func TestFixPostPaidCeiling(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.must(alice.deposit(coins(1)))
	batch := encodeBatch([]Envelope{{Epoch: 1, CT: []byte("x")}})
	fee, _ := w.ch.fee()
	_, err := w.ch.send(alice.key, nil, "Inbox", "postPaidUpTo", alice.id(), batch, big.NewInt(1), new(big.Int).Sub(fee, big.NewInt(1)))
	w.mustFail(err, "ceiling")
	_, err = w.ch.send(alice.key, nil, "Inbox", "postPaidUpTo", alice.id(), batch, big.NewInt(1), fee)
	w.must(err)
}

// ---- FO-1, FO-2, FO-4: the price follows honest votes, slowly

func TestFixPriceVotesCostThePusher(t *testing.T) {
	w := newWorld(t)
	// The launch values: a day per round, 10% a step, a quorum of 100 coins.
	if got := mustCall(t, w.ch, "FeeOracle", "roundLength").(uint64); got != 86400 {
		t.Fatalf("launch round %d s", got)
	}
	if got := mustCall(t, w.ch, "FeeOracle", "maxStepBps").(uint16); got != 1000 {
		t.Fatalf("launch step %d bps", got)
	}
	if got := mustCall(t, w.ch, "FeeOracle", "quorum").(*big.Int); got.Cmp(coins(100)) != 0 {
		t.Fatalf("launch quorum %s", fmtCoins(got))
	}

	r := newOracleRig(w, coins(1), coins(1), 64) // hourly rounds, ±20%: the audit's setting
	// A far vote pays the near ones: three honest coins at 1.0, a pusher's two at 10.
	rd := r.round()
	var honest []sealed
	for i := 0; i < 3; i++ {
		honest = append(honest, r.commit(r.voter(), coins(1), coins(1)))
	}
	pusher := r.commit(r.voter(), coins(10), coins(2))
	r.toReveal()
	for _, s := range append(honest, pusher) {
		r.reveal(s)
	}
	r.toNext()
	r.finalize(rd)
	if p := r.big("basketPrice"); p.Cmp(coins(1)) != 0 {
		t.Fatalf("price %s, want 1 (the honest median)", fmtCoins(p))
	}
	if got := r.big("owed", crypto.PubkeyToAddress(pusher.key.PublicKey)); got.Cmp(coins(1)) != 0 {
		t.Fatalf("the pusher got %s back of 2, want 1", fmtCoins(got))
	}
	want := new(big.Int).Add(coins(1), new(big.Int).Div(coins(1), big.NewInt(3)))
	if got := r.big("owed", crypto.PubkeyToAddress(honest[0].key.PublicKey)); got.Cmp(want) != 0 {
		t.Fatalf("an honest voter got %s, want %s", fmtCoins(got), fmtCoins(want))
	}

	// The PoC: a whale with all the bond pushes every hour. It moves one step a day, not 1.2× an hour.
	for i := 0; i < 4; i++ {
		r.voteRound([]*big.Int{coins(100)}, []*big.Int{coins(5)})
	}
	if p := r.big("basketPrice"); p.Cmp(exact("1.2")) != 0 {
		t.Fatalf("after four hourly pushes: %s, want 1.2 (one step from the day's price)", fmtCoins(p))
	}
	w.later(24 * time.Hour)
	r.finalize(r.round() - 1) // an empty round: nothing to apply
	r.voteRound([]*big.Int{coins(100)}, []*big.Int{coins(5)})
	if p := r.big("basketPrice"); p.Cmp(exact("1.44")) != 0 {
		t.Fatalf("next day: %s, want 1.44", fmtCoins(p))
	}

	// FO-2: rounds with votes are finalized in order; a newer one can't drop an older median.
	r0 := r.round()
	old := r.commit(r.voter(), exact("1.3"), coins(5))
	r.toReveal()
	r.reveal(old)
	r.toNext()
	newer := r.commit(r.voter(), coins(2), coins(5))
	r.toReveal()
	r.reveal(newer)
	r.toNext()
	_, err := w.ch.send(w.faucet, nil, r.name, "finalize", r0+1)
	w.mustFail(err, "older round")
	r.finalize(r0)
	r.finalize(r0 + 1)

	// FO-4: switching congestion pricing off resets the multiplier.
	for i := 0; i < 4; i++ {
		_, err := w.ch.send(r.gov, nil, r.name, "use", envelopeKind, big.NewInt(1000))
		w.must(err)
	}
	if m := r.big("currentMultiplier"); m.Cmp(coins(1)) <= 0 {
		t.Fatal("no load")
	}
	_, err = w.ch.send(r.gov, nil, r.name, "setCongestion", big.NewInt(0), coins(100))
	w.must(err)
	if m := r.big("currentMultiplier"); m.Cmp(coins(1)) != 0 {
		t.Fatalf("multiplier frozen at %s", fmtCoins(m))
	}
}

// ---- CM-1, CM-2, CM-3: communities can't be captured by one role holder

func townParams() CommonsParams {
	return CommonsParams{Term: 30 * 86400, VoteWindow: 3600, VetoWindow: 3600, VetoBps: 5000, PetitionBps: 5000, QuorumBps: 3000,
		Budget: coins(1)}
}

// CM-1: a rogue admin bans everyone in one go. The banned keep their veto
// while the bans can be vetoed, and a reversed ban restores the member with its role.
func TestFixMassBanIsReversible(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	const C = "town"
	w.must(nina.found(C, townParams(), ""))
	w.join(C, nina, alice, bob, carol)
	r, err := w.ch.send(alice.key, nil, "Commons", "callElection", idOf(C), alice.id(), uint8(roleMod), uint8(1))
	w.must(err)
	e, _ := alice.firstIndexed(r, "Commons", "ElectionOpened")
	w.must(w.commons(bob, "stand", u(e), bob.id()))
	for _, v := range []*Client{alice, carol, bob} {
		w.must(w.commons(v, "vote", u(e), v.id(), bob.id()))
	}
	w.later(3600 * time.Second)
	w.must(w.commons(alice, "closeElection", u(e)))
	if !w.hasRole(C, "bob", roleMod) {
		t.Fatal("bob not elected")
	}

	// The rogue admin bans all three in one block's worth of transactions.
	ids := map[string]uint64{}
	for _, v := range []*Client{bob, alice, carol} {
		id, err := nina.act("ban", idOf(C), nina.id(), v.id(), "mine now")
		w.must(err)
		ids[v.f.Name] = id
	}
	if w.isMember(C, "bob") || w.hasRole(C, "bob", roleMod) {
		t.Fatal("the ban didn't take effect")
	}
	// Nobody vetoes their own ban; the banned veto each other's.
	w.mustFail(w.commons(carol, "veto", u(ids["carol"]), carol.id()), "not your own ban")
	w.must(w.commons(alice, "veto", u(ids["bob"]), alice.id()))
	w.must(w.commons(carol, "veto", u(ids["bob"]), carol.id()))
	if !w.isMember(C, "bob") || !w.hasRole(C, "bob", roleMod) {
		t.Fatal("a reversed ban didn't restore bob with his moderator role (TestA3BanStripsRoleForever)")
	}
	w.must(w.commons(bob, "veto", u(ids["alice"]), bob.id()))
	w.must(w.commons(carol, "veto", u(ids["alice"]), carol.id()))
	w.must(w.commons(alice, "veto", u(ids["carol"]), alice.id())) // 1 of 2: the electorate when carol was banned
	if k, _ := nina.community(C); k.Members != 4 || !w.isMember(C, "alice") || !w.isMember(C, "carol") {
		t.Fatalf("after the reversals: %d members", k.Members)
	}

	// A ban that stood can be lifted later by an admin; the lift is vetoable.
	spam, _ := w.owner("spam")
	b, err := nina.act("ban", idOf(C), nina.id(), spam.id(), "spam")
	w.must(err)
	_, err = nina.act("unban", idOf(C), nina.id(), spam.id(), "early")
	w.mustFail(err, "can still be vetoed")
	w.later(3600 * time.Second)
	w.mustFail(w.commons(spam, "veto", u(b), spam.id()), "members only")
	un, err := nina.act("unban", idOf(C), nina.id(), spam.id(), "served its time")
	w.must(err)
	if w.commonsBool(C, "banned", "spam") {
		t.Fatal("unban didn't lift the ban")
	}
	w.must(w.commons(alice, "veto", u(un), alice.id()))
	w.must(w.commons(bob, "veto", u(un), bob.id()))
	if !w.commonsBool(C, "banned", "spam") {
		t.Fatal("a vetoed unban didn't ban again")
	}
}

func (w *world) commonsBool(c, method, who string) bool {
	out, err := w.ch.call("Commons", method, idOf(c), idOf(who))
	w.must(err)
	return out[0].(bool)
}

// CM-2: a moderator admits a flood of names. They don't vote, nor raise any
// threshold, until a veto window passes; members veto the admissions meanwhile.
func TestFixAdmissionFloodCantOutvote(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	const C = "town"
	w.must(nina.found(C, townParams(), ""))
	w.join(C, nina, alice, bob) // the founding window: they vote at once
	w.later(3600 * time.Second)
	var sybils []*Client
	var admits []uint64
	for _, n := range []string{"s1", "s2", "s3", "s4"} {
		s, _ := w.owner(n)
		_, err := s.joinGroup(C)
		w.must(err)
		id, err := nina.act("admit", idOf(C), nina.id(), s.id())
		w.must(err)
		sybils, admits = append(sybils, s), append(admits, id)
	}
	if k, _ := nina.community(C); k.Members != 3 {
		t.Fatalf("members %d: new members counted before they may vote", k.Members)
	}
	_, err := w.ch.send(sybils[0].key, nil, "Commons", "callElection", idOf(C), sybils[0].id(), uint8(roleAdmin), uint8(1))
	w.mustFail(err, "after one veto window")
	w.mustFail(w.commons(sybils[1], "veto", u(admits[0]), sybils[1].id()), "after one veto window")
	// Two of the three members reverse each admission (the threshold is of the members when it was made).
	for _, id := range admits[:3] {
		w.must(w.commons(alice, "veto", u(id), alice.id()))
		w.must(w.commons(bob, "veto", u(id), bob.id()))
	}
	if w.isMember(C, "s1") || w.isMember(C, "s3") {
		t.Fatal("a vetoed admission stayed")
	}
	// The one left unvetoed votes after the window, and counts.
	w.later(3600 * time.Second)
	_, err = w.ch.send(sybils[3].key, nil, "Commons", "callElection", idOf(C), sybils[3].id(), uint8(roleMod), uint8(1))
	w.must(err)
	if k, _ := nina.community(C); k.Members != 4 {
		t.Fatalf("members %d, want 4", k.Members)
	}
}

// CM-3: following another community's bans keeps names out, never takes a vote here.
func TestFixFollowDoesNotDisenfranchise(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	mallory, _ := w.owner("mallory")
	const C = "town"
	w.must(nina.found(C, townParams(), ""))
	w.join(C, nina, alice, bob, carol)
	w.must(mallory.found("evil", townParams(), ""))
	for _, v := range []*Client{alice, bob} {
		_, err := mallory.act("ban", idOf("evil"), mallory.id(), v.id(), "nothing personal")
		w.must(err)
	}
	w.later(3600 * time.Second)
	f, err := nina.act("follow", idOf(C), nina.id(), idOf("evil"), uint8(1))
	w.must(err)
	if w.commonsBool(C, "isBanned", "alice") {
		t.Fatal("the follow applied before its veto window")
	}
	// The PoC: alice and bob veto the follow, which they couldn't before.
	w.must(w.commons(alice, "veto", u(f), alice.id()))
	w.must(w.commons(bob, "veto", u(f), bob.id()))
	if out, _ := w.ch.call("Commons", "followingOf", idOf(C)); len(out[0].([][32]byte)) != 0 {
		t.Fatal("the vetoed follow stayed")
	}
	// A follow that stands keeps the listed names out, and members keep voting.
	_, err = nina.act("follow", idOf(C), nina.id(), idOf("evil"), uint8(1))
	w.must(err)
	w.later(3600 * time.Second)
	if !w.commonsBool(C, "isBanned", "alice") || w.commonsBool(C, "bannedHere", "alice") {
		t.Fatal("isBanned should include the followed list, bannedHere not")
	}
	_, err = w.ch.send(alice.key, nil, "Commons", "callElection", idOf(C), alice.id(), uint8(roleMod), uint8(1))
	w.must(err)
}

// ---- H1-R: a payee can't be made to deliver into an escrow it can't dispute

func TestFixShortEscrowCannotBeAccepted(t *testing.T) {
	w := newWorld(t)
	seller, _ := w.owner("seller")
	buyer, _ := w.owner("buyer")
	w.owner("judge")
	ct, hash, key, _ := artifact.SealFile([]byte("valuable model weights"))
	art, err := seller.registerArtifact(hash, uint64(len(ct)), "ipfs://bafyArt", "commercial", coins(2), key)
	w.must(err)
	w.must(buyer.deposit(coins(4)))
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	// The PoC: a 60-second escrow. The seller's accept (and so its delivery) is refused on chain.
	lock, _, err := buyer.buy(art, "judge", head.Time+60)
	w.must(err)
	if err := seller.deliverKey(lock, art, "buyer", key); err == nil || !strings.Contains(err.Error(), "too short to dispute") {
		t.Fatalf("delivered into a 60 s escrow: %v", err)
	}
	if mustCall(t, w.ch, "Bank", "accepted", u(lock)).(bool) {
		t.Fatal("accepted")
	}
	// What the client checks before accepting: who decides, and whose name that is.
	l, err := w.ch.call("Bank", "locks", u(lock))
	w.must(err)
	d, err := w.ch.call("AttestationCondition", "deciderOf", l[8].([]byte))
	w.must(err)
	if common.Hash(d[0].([32]byte)) != idOf("judge") || common.Hash(d[1].([32]byte)) != idOf("judge") || d[2].(uint8) != 1 {
		t.Fatalf("deciderOf: %v", d)
	}
	// A week-long one is accepted as before.
	lock2, _, err := buyer.buy(art, "judge", head.Time+7*86400)
	w.must(err)
	seller.st.Arbiters = []string{"judge"}
	w.must(seller.deliverKey(lock2, art, "buyer", key))
}
