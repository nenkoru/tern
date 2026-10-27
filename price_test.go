package main

import (
	"crypto/ecdsa"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// oracleRig: a FeeOracle of its own, with the test as governance and meter.
type oracleRig struct {
	w        *world
	name     string
	gov      *ecdsa.PrivateKey
	treasury common.Address
}

func newOracleRig(w *world, price, quorum *big.Int, perBlock int64) *oracleRig {
	w.t.Helper()
	gov, _ := crypto.GenerateKey()
	tk, _ := crypto.GenerateKey()
	r := &oracleRig{w: w, name: "TestOracle", gov: gov, treasury: crypto.PubkeyToAddress(tk.PublicKey)}
	w.fund2(crypto.PubkeyToAddress(gov.PublicKey), coins(10))
	a, _ := loadArtifact("FeeOracle")
	opts, _ := w.ch.opts(w.faucet, nil)
	addr, tx, bc, err := bind.DeployContract(opts, a.abi, a.bin, w.ch.b, price, uint64(3600), exact("0.01"), uint16(2000), quorum,
		crypto.PubkeyToAddress(gov.PublicKey), r.treasury, big.NewInt(perBlock), coins(100), [][32]byte{envelopeKind},
		[]*big.Int{big.NewInt(10)}, crypto.Keccak256Hash(basketSpec), basketURI, []common.Address{crypto.PubkeyToAddress(gov.PublicKey)})
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.ch.wait(tx); err != nil {
		w.t.Fatal(err)
	}
	w.ch.c[r.name], w.ch.abis[r.name], w.ch.dep.Contracts[r.name] = bc, a.abi, addr.Hex()
	return r
}

func (w *world) fund2(a common.Address, amount *big.Int) {
	if err := w.ch.transfer(w.faucet, a, amount); err != nil {
		w.t.Fatal(err)
	}
}

func (r *oracleRig) big(method string, args ...any) *big.Int {
	out, err := r.w.ch.call(r.name, method, args...)
	if err != nil {
		r.w.t.Fatal(err)
	}
	return out[0].(*big.Int)
}

func (r *oracleRig) round() uint64 {
	out, _ := r.w.ch.call(r.name, "round")
	return out[0].(uint64)
}

func (r *oracleRig) voter() *ecdsa.PrivateKey {
	k, _ := crypto.GenerateKey()
	r.w.fund2(crypto.PubkeyToAddress(k.PublicKey), coins(20))
	return k
}

type sealed struct {
	key   *ecdsa.PrivateKey
	price *big.Int
	salt  [32]byte
}

func (r *oracleRig) commit(k *ecdsa.PrivateKey, price, bond *big.Int) sealed {
	r.w.t.Helper()
	s := sealed{key: k, price: price}
	copy(s.salt[:], randBytes(32))
	if _, err := r.w.ch.send(k, bond, r.name, "commit", priceSeal(price, s.salt, crypto.PubkeyToAddress(k.PublicKey))); err != nil {
		r.w.t.Fatal(err)
	}
	return s
}

// toReveal and toNextRound move chain time to the reveal half, and past the round.
func (r *oracleRig) toReveal() { r.w.later(1800 * time.Second) }
func (r *oracleRig) toNext()   { r.w.later(1800 * time.Second) }

func (r *oracleRig) reveal(s sealed) {
	r.w.t.Helper()
	if _, err := r.w.ch.send(s.key, nil, r.name, "reveal", s.price, s.salt); err != nil {
		r.w.t.Fatal(err)
	}
}

func (r *oracleRig) finalize(round uint64) {
	r.w.t.Helper()
	if _, err := r.w.ch.send(r.w.faucet, nil, r.name, "finalize", round); err != nil {
		r.w.t.Fatal(err)
	}
}

// voteRound: everyone commits, reveals, and the round is finalized.
func (r *oracleRig) voteRound(prices, bonds []*big.Int) {
	r.w.t.Helper()
	rd := r.round()
	var ss []sealed
	for i := range prices {
		ss = append(ss, r.commit(r.voter(), prices[i], bonds[i]))
	}
	r.toReveal()
	for _, s := range ss {
		r.reveal(s)
	}
	r.toNext()
	r.finalize(rd)
}

func TestPriceVotes(t *testing.T) {
	w := newWorld(t)
	r := newOracleRig(w, exact("1.1"), coins(1), 64)
	// A bond-weighted median, not a head count: 3 coins say 1.0, 1+1 say 2 and 3.
	r.voteRound([]*big.Int{coins(1), coins(2), coins(3)}, []*big.Int{coins(3), coins(1), coins(1)})
	if p := r.big("basketPrice"); p.Cmp(coins(1)) != 0 {
		t.Fatalf("weighted median: %s, want 1", fmtCoins(p))
	}
	// Step cap up: everyone says 10, the price moves 20%.
	r.voteRound([]*big.Int{coins(10), coins(10)}, []*big.Int{coins(1), coins(1)})
	if p := r.big("basketPrice"); p.Cmp(exact("1.2")) != 0 {
		t.Fatalf("capped up: %s, want 1.2", fmtCoins(p))
	}
	// And down: 0.1 moves it to 0.96.
	r.voteRound([]*big.Int{exact("0.1")}, []*big.Int{coins(2)})
	if p := r.big("basketPrice"); p.Cmp(exact("0.96")) != 0 {
		t.Fatalf("capped down: %s, want 0.96", fmtCoins(p))
	}
	// Quorum not met: half a coin of revealed bond moves nothing.
	r.voteRound([]*big.Int{coins(5)}, []*big.Int{exact("0.5")})
	if p := r.big("basketPrice"); p.Cmp(exact("0.96")) != 0 {
		t.Fatalf("moved without quorum: %s", fmtCoins(p))
	}
	// A sealed vote that is never revealed goes to the treasury; revealers take their bonds back.
	rd := r.round()
	silent := r.commit(r.voter(), coins(3), exact("0.7"))
	open := r.commit(r.voter(), coins(1), exact("1.5"))
	r.toReveal()
	r.reveal(open)
	r.toNext()
	r.finalize(rd)
	if got := r.big("owed", r.treasury); got.Cmp(exact("0.7")) != 0 {
		t.Fatalf("forfeit to treasury: %s", fmtCoins(got))
	}
	if got := r.big("owed", crypto.PubkeyToAddress(silent.key.PublicKey)); got.Sign() != 0 {
		t.Fatal("a non-revealer kept its bond")
	}
	if _, err := w.ch.send(w.faucet, nil, r.name, "withdraw", r.treasury); err != nil {
		t.Fatal(err)
	}
	if b, _ := w.ch.balance(r.treasury); b.Cmp(exact("0.7")) != 0 {
		t.Fatalf("treasury got %s", fmtCoins(b))
	}
	if b0, _ := w.ch.balance(crypto.PubkeyToAddress(open.key.PublicKey)); true {
		if _, err := w.ch.send(open.key, nil, r.name, "withdraw", crypto.PubkeyToAddress(open.key.PublicKey)); err != nil {
			t.Fatal(err)
		}
		if b1, _ := w.ch.balance(crypto.PubkeyToAddress(open.key.PublicKey)); new(big.Int).Sub(b1, b0).Cmp(exact("1.4")) < 0 {
			t.Fatalf("revealer's bond not back: %s", fmtCoins(new(big.Int).Sub(b1, b0)))
		}
	}
}

func TestPriceRoundFullEvictsSmallest(t *testing.T) {
	w := newWorld(t)
	r := newOracleRig(w, coins(1), coins(1), 64)
	var small *ecdsa.PrivateKey
	for i := 0; i < 64; i++ {
		k := r.voter()
		bond := exact("0.05")
		if i == 17 {
			bond, small = exact("0.02"), k
		}
		r.commit(k, coins(1), bond)
	}
	late := r.voter()
	if _, err := w.ch.send(late, exact("0.01"), r.name, "commit", common.Hash{1}); err == nil || !strings.Contains(err.Error(), "round full") {
		t.Fatalf("a smaller bond got in: %v", err)
	}
	r.commit(late, coins(1), exact("0.03"))
	if got := r.big("owed", crypto.PubkeyToAddress(small.PublicKey)); got.Cmp(exact("0.02")) != 0 {
		t.Fatalf("evicted bond owed: %s", fmtCoins(got))
	}
	if _, err := w.ch.send(small, nil, r.name, "reveal", coins(1), [32]byte{}); err == nil {
		t.Fatal("an evicted voter could still reveal")
	}
}

func TestCongestionMultiplier(t *testing.T) {
	w := newWorld(t)
	r := newOracleRig(w, exact("0.1"), coins(1), 10)
	mult := func() *big.Int { return r.big("currentMultiplier") }
	for i := 0; i < 6; i++ { // three times the target load, block after block
		if _, err := w.ch.send(r.gov, nil, r.name, "use", envelopeKind, big.NewInt(30)); err != nil {
			t.Fatal(err)
		}
	}
	w.sim.Commit()
	high := mult()
	// One block moves it at most 12.5% (1.125^6 ≈ 2.03), however big the load.
	if high.Cmp(exact("1.9")) < 0 || high.Cmp(exact("2.1")) > 0 {
		t.Fatalf("multiplier under load: %s", fmtCoins(high))
	}
	fee := r.big("fee", envelopeKind, big.NewInt(1))
	base := new(big.Int).Div(coins(1), big.NewInt(1e6)) // 10 ppm of 0.1 coin
	if fee.Cmp(base) <= 0 {
		t.Fatalf("fee didn't rise: %s", fee)
	}
	for i := 0; i < 3; i++ {
		w.sim.Commit()
	}
	if m := mult(); m.Cmp(high) >= 0 {
		t.Fatalf("no decay: %s", fmtCoins(m))
	}
	for i := 0; i < 10; i++ {
		w.sim.Commit()
	}
	if m := mult(); m.Cmp(coins(1)) != 0 {
		t.Fatalf("not back to 1: %s", fmtCoins(m))
	}
	if fee := r.big("fee", envelopeKind, big.NewInt(1)); fee.Cmp(base) != 0 {
		t.Fatalf("fee at rest: %s, want %s", fee, base)
	}
	// Only meters report load.
	if _, err := w.ch.send(w.faucet, nil, r.name, "use", envelopeKind, big.NewInt(1)); err == nil {
		t.Fatal("a stranger reported load")
	}
}

func TestInboxChargesOracleFee(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	alice.setContact("bob", true)
	var envs []Envelope
	capture = func(e Envelope) { envs = append(envs, e) }
	for i := 0; i < 3; i++ {
		if _, err := alice.send(SendOpts{To: "bob", Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	capture = nil
	ch := w.ch
	fee, _ := ch.fee()
	if fee.Cmp(big.NewInt(1e12)) != 0 {
		t.Fatalf("launch fee %s, want what it cost before (1e12)", fee)
	}
	treasury := mustCall(t, ch, "Inbox", "treasury").(common.Address)
	t0, _ := ch.balance(treasury)
	a0, _ := ch.balance(alice.addr())
	paid := new(big.Int).Mul(fee, big.NewInt(10)) // ten times the fee for three envelopes
	rc, err := ch.send(alice.key, paid, "Inbox", "post", encodeBatch(envs), big.NewInt(3))
	if err != nil {
		t.Fatal(err)
	}
	t1, _ := ch.balance(treasury)
	a1, _ := ch.balance(alice.addr())
	want := new(big.Int).Mul(fee, big.NewInt(3))
	if got := new(big.Int).Sub(t1, t0); got.Cmp(want) != 0 {
		t.Fatalf("treasury got %s, want %s", got, want)
	}
	gas := new(big.Int).Mul(new(big.Int).SetUint64(rc.GasUsed), rc.EffectiveGasPrice)
	if spent := new(big.Int).Sub(a0, a1); spent.Cmp(new(big.Int).Add(want, gas)) != 0 {
		t.Fatalf("alice spent %s, want fee %s + gas %s: overpayment not refunded", spent, want, gas)
	}
	if _, err := ch.send(alice.key, new(big.Int).Sub(want, big.NewInt(1)), "Inbox", "post", encodeBatch(envs), big.NewInt(3)); err == nil {
		t.Fatal("underpaid post accepted")
	}

	// A price vote moves what an envelope costs: 10% up per (daily) round at most,
	// once 100 coins of revealed bond make the quorum (launch values, AUDIT-SECURITY-3 FO-1).
	v, _ := w.owner("voter")
	w.fund(v, coins(100))
	if _, err := v.priceCommit(coins(1), coins(150)); err != nil { // the basket is 0.1 coin at launch
		t.Fatal(err)
	}
	w.later(12 * time.Hour)
	if _, _, err := v.priceReveal(); err != nil {
		t.Fatal(err)
	}
	w.later(12 * time.Hour)
	res, err := v.priceFinalize(nil)
	if err != nil {
		t.Fatal(err)
	}
	if now, _ := ch.fee(); now.Cmp(big.NewInt(1.1e12)) != 0 {
		t.Fatalf("fee after the vote: %s (%v)", now, res)
	}
	if show, err := v.priceShow(); err != nil || show["basket_price_coins"] != "0.110000" || show["envelope_fee_coins"] != "0.0000011" || show["max_step_bps"] != uint64(1000) {
		t.Fatalf("price show: %v %v", show, err)
	}
	// The client pays the moved fee with headroom, and the Inbox refunds the rest.
	if _, err := alice.send(SendOpts{To: "bob", Text: "after the vote"}); err != nil {
		t.Fatal(err)
	}

	// Governance only: no deployer or stranger key sets targets, the basket or meters.
	for _, c := range [][]any{{"setTarget", envelopeKind, big.NewInt(1)}, {"setBasket", [32]byte{}, "x"},
		{"setCongestion", big.NewInt(1), coins(2)}, {"setMeter", alice.addr(), true}} {
		if _, err := ch.send(w.faucet, nil, "FeeOracle", c[0].(string), c[1:]...); err == nil || !strings.Contains(err.Error(), "governance only") {
			t.Fatalf("%s by the deployer: %v", c[0], err)
		}
	}
	if gov := mustCall(t, ch, "FeeOracle", "governance").(common.Address); gov.Hex() != ch.dep.Contracts["TernTimelock"] {
		t.Fatalf("governance is %s, not the timelock", gov.Hex())
	}
	// The Inbox has no oracle setter (BRICKS #2): it prices with the Directory's current FeeOracle,
	// which only both houses change (an upgrade of the key).
	if o := mustCall(t, ch, "Inbox", "oracle").(common.Address); o.Hex() != ch.dep.Contracts["FeeOracle"] {
		t.Fatalf("the Inbox prices with %s, not the Directory's FeeOracle", o.Hex())
	}
	if _, ok := ch.abis["Inbox"].Methods["setOracle"]; ok {
		t.Fatal("the Inbox still has a one-time oracle setter")
	}
	if spec := mustCall(t, ch, "FeeOracle", "basketSpec").([32]byte); common.Hash(spec) != crypto.Keccak256Hash(basketSpec) {
		t.Fatal("basket spec hash")
	}
}

func mustCall(t *testing.T, ch *Chain, contract, method string, args ...any) any {
	t.Helper()
	out, err := ch.call(contract, method, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out[0]
}
