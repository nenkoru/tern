package main

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
)

// newValWorld: a governed chain whose genesis holds Validators, the first
// `bound` founders each holding one validator (docs/abuse/VALIDATORS.md).
func newValWorld(t *testing.T, rules valRules, founders []string, bound int) (*world, map[string]*Client, []common.Address) {
	t.Helper()
	faucet, _ := crypto.GenerateKey()
	boot := crypto.PubkeyToAddress(faucet.PublicKey)
	vs := someAddrs(bound)
	var names []common.Hash
	for i := 0; i < bound; i++ {
		names = append(names, idOf(founders[i]))
	}
	code, err := validatorsRuntime()
	if err != nil {
		t.Fatal(err)
	}
	alloc := types.GenesisAlloc{boot: {Balance: new(big.Int).Mul(coin, big.NewInt(1_000_000))},
		validatorsAddress: {Code: code, Storage: validatorsStorage(boot, vs, names, rules), Balance: new(big.Int)}}
	sim := simulated.NewBackend(alloc, withOsaka)
	t.Cleanup(func() { sim.Close() })
	dir := t.TempDir()
	var gen []GenesisCitizen
	for _, n := range founders {
		id, err := newIdentDir(filepath.Join(dir, n), IdentityFile{Name: n, Kind: "owner", Owner: n})
		if err != nil {
			t.Fatal(err)
		}
		gen = append(gen, GenesisCitizen{n, id.addr()})
	}
	ch, err := deploy(sim.Client(), faucet, Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12),
		Gov: GovParams{Genesis: gen, Short: true}, Validators: validatorsAddress}, "", func() { sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	ch.bundler = faucet
	w := &world{t: t, sim: sim, ch: ch, dir: dir, faucet: faucet}
	w.startRelay()
	cs := map[string]*Client{}
	for _, n := range founders {
		g, _ := newIdentDir(filepath.Join(dir, "guardian-"+n), IdentityFile{Name: n, Kind: "guardian"})
		c, err := openClient(filepath.Join(dir, n), ch)
		if err != nil {
			t.Fatal(err)
		}
		c.relay = w.relay
		w.fund(c, coins(100))
		w.must(c.registerOwner(g.addr()))
		w.must(c.govCitizenCall("claimGenesis", c.id()))
		cs[n] = c
	}
	w.later(time.Second)
	return w, cs, vs
}

// shortRules: a day's window, an hour's delays (tests run inside the founders' first term).
var shortRules = valRules{Window: 86400, RemovalDelay: 3600, LapseGrace: 3600, MinSet: 4, VoteWindow: 3600, QuorumBps: 2000}

// vote: coin-house votes and citizens' votes, yes and no, on proposal id.
func (w *world) vote(id uint64, coinYes, coinNo, citYes, citNo []*Client) {
	w.t.Helper()
	for _, c := range coinYes {
		w.must(errOf(c.govVote(id, true, "coin")))
	}
	for _, c := range coinNo {
		w.must(errOf(c.govVote(id, false, "coin")))
	}
	for _, c := range citYes {
		w.must(errOf(c.govVote(id, true, "citizens")))
	}
	for _, c := range citNo {
		w.must(errOf(c.govVote(id, false, "citizens")))
	}
}

func clients(cs map[string]*Client, names ...string) []*Client {
	var out []*Client
	for _, n := range names {
		out = append(out, cs[n])
	}
	return out
}

func valEvents(t *testing.T, ch *Chain, contract, name string) int {
	t.Helper()
	evs, err := ch.events(contract, name)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

// TestValidatorCapture: F1 (a faction with the coin house and two citizens
// can't remove honest validators), F3 (one validator per citizen), F2 (the
// rolling cap stops salami), F6 (governance-critical changes need 2/3 and
// three exit windows; clients say so loudly and don't follow), F7 (the
// deployer has no power over the set).
func TestValidatorCapture(t *testing.T) {
	honest := []string{"h1", "h2", "h3", "h4", "h5", "h6", "h7"}
	w, cs, vs := newValWorld(t, shortRules, append(append([]string{}, honest...), "max", "zed"), 7)
	ch := w.ch
	max, zed, h1 := cs["max"], cs["zed"], cs["h1"]
	w.must(max.lockVotes(coins(60))) // the faction holds the coin house
	w.must(h1.lockVotes(coins(20)))
	w.later(2 * time.Second)
	faction := clients(cs, "max", "zed")

	// F7: the deployer, the genesis bootstrap, has no power over the set.
	me := crypto.PubkeyToAddress(w.faucet.PublicKey)
	for _, x := range []struct {
		m    string
		args []any
	}{{"setGovernance", []any{me, me, me}}, {"addValidator", []any{me, idOf("max")}}, {"removeValidator", []any{vs[0]}},
		{"cancelRemoval", []any{vs[0]}}, {"setRules", []any{shortRules}}, {"scheduleUpgrade", []any{uint64(1 << 40), [32]byte{1}, "x"}},
		{"rotate", []any{idOf("h1"), me}}} {
		if _, err := ch.send(w.faucet, nil, "Validators", x.m, x.args...); err == nil {
			t.Fatalf("the deployer could still call Validators.%s", x.m)
		}
	}

	// F1: remove an honest validator. Honest citizens vote no: defeated. Nobody
	// honest votes: the citizens' turnout floor (min(5, half)) defeats it.
	p1, err := max.govProposeParam("Validators", "removeValidator", []string{vs[0].Hex()}, "h1's node is slow")
	w.must(err)
	p2, err := max.govProposeParam("Validators", "removeValidator", []string{vs[1].Hex()}, "h2's node is slow")
	w.must(err)
	w.later(2 * time.Second)
	w.vote(p1, faction[:1], nil, faction, clients(cs, "h1", "h2", "h3", "h4", "h5"))
	w.vote(p2, faction[:1], nil, faction, nil)
	w.later(11 * time.Minute)
	for _, p := range []uint64{p1, p2} {
		if st, _ := max.govState(p); st != "defeated" {
			t.Fatalf("F1: proposal %d %s", p, st)
		}
	}

	// F3: one validator per citizen. max adds one (honest citizens fooled: yes);
	// a second for max is refused by the client, and by the contract at execution.
	x := someAddrs(3)
	w.must(runGovValidator(max, []string{"offer", x[0].Hex()}, govFlags{})) // the citizen consents first (VA-3)
	pA, err := max.govProposeParam("Validators", "addValidator", []string{x[0].Hex(), idOf("max").Hex()}, "max runs a node")
	w.must(err)
	w.pass(pA, faction[:1], clients(cs, "max", "zed", "h1", "h2", "h3"))
	if mustCall(t, ch, "Validators", "validatorOf", idOf("max")).(common.Address) != x[0] {
		t.Fatal("F3: max's validator not added")
	}
	w.mustFail(max.validatorAddCheck(x[1], "max"), "one validator per citizen")
	pB, err := max.govProposeParam("Validators", "addValidator", []string{x[1].Hex(), idOf("max").Hex()}, "a second node")
	w.must(err)
	w.later(2 * time.Second)
	w.vote(pB, faction[:1], nil, clients(cs, "max", "zed", "h1", "h2", "h3"), nil)
	w.later(11 * time.Minute)
	w.must(errOf(max.govQueue(pB)))
	w.later(11 * time.Minute)
	w.mustFail(max.govExecute(pB), "one validator per citizen")
	pN, err := max.govProposeParam("Validators", "addValidator", []string{x[1].Hex(), idOf("nobody").Hex()}, "")
	w.must(err)
	w.later(2 * time.Second)
	w.vote(pN, faction[:1], nil, faction, nil) // not even passed: but if it were, the contract refuses non-citizens
	if crit := mustCall(t, ch, "Parliament", "critical", u(pA)).(bool); crit {
		t.Fatal("an addition is the capped path, not governance-critical")
	}

	// F2: salami. zed adds a second faction node (2 changes: past a quarter of 7,
	// a churn alert); removing an honest one is then over the cap (⌊7/3⌋ = 2 per window).
	w.must(runGovValidator(zed, []string{"offer", x[2].Hex()}, govFlags{}))
	pC, err := zed.govProposeParam("Validators", "addValidator", []string{x[2].Hex(), idOf("zed").Hex()}, "zed runs a node")
	w.must(err)
	w.pass(pC, faction[:1], clients(cs, "max", "zed", "h1", "h2", "h3"))
	if valEvents(t, ch, "Validators", "ChurnAlert") == 0 {
		t.Fatal("F2: no churn alert after 2 changes of 7")
	}
	pD, err := max.govProposeParam("Validators", "removeValidator", []string{vs[6].Hex()}, "h7 moved away")
	w.must(err)
	w.pass(pD, faction[:1], clients(cs, "max", "zed", "h1", "h2", "h3"))
	w.later(time.Duration(shortRules.RemovalDelay) * time.Second)
	_, err = ch.send(w.faucet, nil, "Validators", "applyRemoval", vs[6])
	w.mustFail(err, "rolling cap")
	if left := mustCall(t, ch, "Validators", "changesLeft").(*big.Int); left.Sign() != 0 {
		t.Fatalf("F2: %d changes left", left)
	}
	w.later(time.Duration(shortRules.Window) * time.Second) // the window rolls on
	w.must(errOf(ch.send(w.faucet, nil, "Validators", "applyRemoval", vs[6])))
	if mustCall(t, ch, "Validators", "isValidator", vs[6]).(bool) {
		t.Fatal("F2: not removed after the window")
	}

	// F6: changing the set's own rules is governance-critical: 5 of 9 citizens
	// say yes (a majority, not 2/3): defeated, though the coin house said yes.
	rulesArg, _ := json.Marshal(map[string]any{"window": 1, "removalDelay": 0, "lapseGrace": 0, "minSet": 1, "voteWindow": 1, "quorumBps": 1})
	pE, err := max.govProposeParam("Validators", "setRules", []string{string(rulesArg)}, "faster set changes")
	w.must(err)
	if !mustCall(t, ch, "Parliament", "critical", u(pE)).(bool) {
		t.Fatal("F6: setRules not governance-critical")
	}
	show, _ := max.govShow(pE)
	if !strings.Contains(show["governance_critical"].(string), "2/3") {
		t.Fatalf("F6: show %v", show)
	}
	w.later(2 * time.Second)
	w.vote(pE, faction[:1], clients(cs, "h1"), clients(cs, "max", "zed", "h1", "h2", "h3"), clients(cs, "h4", "h5", "h6", "h7"))
	w.later(11 * time.Minute)
	if st, _ := max.govState(pE); st != "defeated" {
		t.Fatalf("F6: a 5/9 majority passed a critical change: %s", st)
	}
	// A hostile Validators in the Directory: 2/3 fooled, it passes, but waits
	// three exit windows; clients raise the alarm and don't follow.
	hostile := deployNew(w, "Commitments", common.HexToAddress(ch.dep.Contracts["Registry"]))
	doc := writeDoc(t, "validators-v2.md", "Validators v2: faster governance")
	from, _ := ch.b.BlockNumber(context.Background())
	pF, err := max.govProposeUpgrade("Validators", hostile, doc, "", doc, "", "")
	w.must(err)
	if !mustCall(t, ch, "Parliament", "critical", u(pF)).(bool) {
		t.Fatal("F6: an upgrade of Validators not critical")
	}
	w.later(2 * time.Second)
	w.vote(pF, faction[:1], nil, clients(cs, "max", "zed", "h1", "h2", "h3", "h4", "h5"), clients(cs, "h6"))
	w.later(11 * time.Minute)
	q, err := max.govQueue(pF)
	w.must(err)
	exit := mustCall(t, ch, "TernTimelock", "getMinDelay").(*big.Int).Int64()
	if win := mustCall(t, ch, "Parliament", "exitWindow", u(pF)).(*big.Int).Int64(); win != 3*exit {
		t.Fatalf("F6: exit window %d, want 3×%d (%v)", win, exit, q)
	}
	w.later(time.Duration(exit+60) * time.Second)
	w.mustFail(max.govExecute(pF), "queued") // one exit window is not enough
	w.later(time.Duration(2*exit) * time.Second)
	w.must(max.govExecute(pF))
	var seen []Event
	head, _ := ch.b.BlockNumber(context.Background())
	w.must(h1.govEvents(from, uint64(h1.chainNow()), &WatchState{}, func(e Event) { seen = append(seen, e) }, func(string, Event) {}))
	var critical, changed bool
	for _, e := range seen {
		critical = critical || e.Type == "governance-critical"
		changed = changed || (e.Type == "governance-contract-changed" && strings.Contains(e.Text, "does NOT follow"))
	}
	if !critical || !changed {
		t.Fatalf("F6: watch from %d to %d didn't alarm: %v", from, head, seen)
	}
	if h1.ch.dep.Contracts["Validators"] != validatorsAddress.Hex() {
		t.Fatal("F6: the client followed a governance contract by itself")
	}
	ups, err := h1.govUpdates(false)
	w.must(err)
	warned := false
	for _, u := range ups {
		if u["key"] == "Validators" && strings.Contains(u["warning"].(string), "GOVERNANCE CONTRACT CHANGED") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("F6: gov updates: %v", ups)
	}
}

// TestValidatorRotateLapseChallenge: F8 (a citizen rotates a stolen key),
// F3 (a revoked or departed citizen's validator lapses; a same-operator
// pair is challenged and the second removed), F5 (a censorship report on
// chain), the minimum set.
func TestValidatorRotateLapseChallenge(t *testing.T) {
	names := []string{"h1", "h2", "h3", "h4", "h5", "max", "zed"}
	w, cs, vs := newValWorld(t, shortRules, names, 7)
	ch := w.ch
	h1, h3, h5 := cs["h1"], cs["h3"], cs["h5"]

	// F8: h3's node key is stolen: h3 rotates to a new one; nobody else can.
	newKey := someAddrs(1)[0]
	_, err := ch.send(h1.key, nil, "Validators", "rotate", idOf("h3"), newKey)
	w.mustFail(err, "the owner's own key only")
	w.must(runGovValidator(h3, []string{"rotate", newKey.Hex()}, govFlags{}))
	if mustCall(t, ch, "Validators", "isValidator", vs[2]).(bool) || !mustCall(t, ch, "Validators", "isValidator", newKey).(bool) ||
		mustCall(t, ch, "Validators", "validatorOf", idOf("h3")).(common.Address) != newKey {
		t.Fatal("F8: not rotated")
	}
	if mustCall(t, ch, "Validators", "changeCount").(*big.Int).Sign() != 0 {
		t.Fatal("a rotation counted as a set change")
	}

	// A citizen leaves: its validator lapses (after the grace), via the capped path.
	w.must(h5.govCitizenCall("leave", h5.id()))
	_, err = ch.send(w.faucet, nil, "Validators", "lapse", vs[4])
	w.mustFail(err, "the grace")
	w.later(time.Duration(shortRules.LapseGrace) * time.Second)
	_, err = ch.send(w.faucet, nil, "Validators", "lapse", vs[0])
	w.mustFail(err, "its citizen is a citizen")
	w.must(errOf(ch.send(w.faucet, nil, "Validators", "lapse", vs[4]))) // anyone
	w.later(time.Duration(shortRules.RemovalDelay) * time.Second)
	w.must(runGovValidator(h1, []string{"apply", vs[4].Hex()}, govFlags{}))
	if mustCall(t, ch, "Validators", "isValidator", vs[4]).(bool) {
		t.Fatal("lapsed validator kept")
	}

	// F3: max and zed run one operator's two nodes. h1 challenges with evidence;
	// the accused don't vote; 2/3 of the rest say yes: a finding (VA-4), and
	// the removal it supports passes both houses.
	ev := writeDoc(t, "same-operator.txt", "both enodes on 10.77.0.0/24, one hoster account, same SSH host key")
	w.must(runGovValidator(h1, []string{"challenge", vs[5].Hex(), vs[6].Hex()}, govFlags{why: ev}))
	_, err = ch.send(cs["max"].key, nil, "Validators", "voteChallenge", big.NewInt(0), idOf("max"), false)
	w.mustFail(err, "the accused don't vote")
	for _, n := range []string{"h1", "h2", "h3", "h4"} {
		w.must(runGovValidator(cs[n], []string{"challenge-vote", "0", "yes"}, govFlags{}))
	}
	w.later(time.Duration(shortRules.VoteWindow+1) * time.Second)
	w.must(runGovValidator(h1, []string{"challenge-close", "0"}, govFlags{}))
	if mustCall(t, ch, "Validators", "removalAt", vs[6]).(uint64) != 0 {
		t.Fatal("F3: citizens alone scheduled a removal")
	}
	w.must(h1.lockVotes(coins(30)))
	w.later(2 * time.Second)
	pR, err := h1.govProposeParam("Validators", "removeValidator", []string{vs[6].Hex()}, "challenge 0 passed: one operator")
	w.must(err)
	w.pass(pR, []*Client{h1}, clients(cs, "h1", "h2", "h3", "h4"))
	if mustCall(t, ch, "Validators", "removalAt", vs[6]).(uint64) == 0 {
		t.Fatal("F3: the removal by vote scheduled nothing")
	}
	w.later(time.Duration(shortRules.RemovalDelay) * time.Second)
	w.must(errOf(ch.send(w.faucet, nil, "Validators", "applyRemoval", vs[6])))
	if mustCall(t, ch, "Validators", "isValidator", vs[6]).(bool) {
		t.Fatal("F3: zed's node kept")
	}

	// F5: a watcher's report, put on chain by a citizen.
	rep := CensorshipReport{Tx: common.HexToHash("0xabc"), FirstSeen: 10, BlocksPending: 12, Proposers: vs[:2]}
	b, _ := json.Marshal(rep)
	rp := filepath.Join(t.TempDir(), "censorship.json")
	os.WriteFile(rp, b, 0o644)
	w.must(runGovValidator(h1, []string{"report", rp}, govFlags{}))
	if valEvents(t, ch, "Validators", "CensorshipReported") != 1 {
		t.Fatal("F5: no report on chain")
	}
	stranger, _ := w.owner("stranger")
	w.mustFail(runGovValidator(stranger, []string{"report", rp}, govFlags{}), "citizens only")

	// The minimum set: 5 left (h1, h2, h3', h4, max); two more removals would leave 3.
	var types []string
	w.must(h1.validatorEvents(0, func(e Event) { types = append(types, e.Type) }))
	for _, want := range []string{"validator-rotated", "validator-removal", "validator-removed", "validator-challenge", "censorship"} {
		if !strings.Contains(strings.Join(types, " "), want) {
			t.Fatalf("watch: no %s in %v", want, types)
		}
	}
}

// TestValidatorMinimumSet: with 4 validators a removal stays scheduled: the set never drops below 4.
func TestValidatorMinimumSet(t *testing.T) {
	w, cs, vs := newValWorld(t, shortRules, []string{"ana", "ben", "cai", "dov", "eve"}, 4)
	a, b := cs["ana"], cs["ben"]
	w.must(a.lockVotes(coins(30)))
	w.later(2 * time.Second)
	p, err := a.govProposeParam("Validators", "removeValidator", []string{vs[3].Hex()}, "")
	w.must(err)
	w.pass(p, []*Client{a}, []*Client{a, b, cs["cai"]})
	w.later(time.Duration(shortRules.RemovalDelay) * time.Second)
	_, err = w.ch.send(w.faucet, nil, "Validators", "applyRemoval", vs[3])
	w.mustFail(err, "the minimum set")
	if mustCall(t, w.ch, "Validators", "count").(*big.Int).Int64() != 4 {
		t.Fatal("below the minimum set")
	}
}

// TestForkRewriteValidators: the fork's Validators storage is exactly what a
// genesis for the honest set would hold (bindings moved, removals and the
// change log cleared, governance and rules kept).
func TestForkRewriteValidators(t *testing.T) {
	old := someAddrs(4)
	names := []common.Hash{idOf("a"), idOf("b"), idOf("c"), idOf("d")}
	boot := common.Address{}
	st := validatorsStorage(boot, old, names, shortRules)
	gov := common.HexToAddress("0x1234")
	st[slotWord(valSlotGovernance)] = addrKey(gov)
	st[mapSlot(addrKey(old[3]), valSlotRemovalAt)] = slotWord(99)
	st[slotWord(valSlotChanges)] = slotWord(2)
	fresh := someAddrs(2)
	nv := []common.Address{old[0], old[1], fresh[0], fresh[1]}
	nn := []common.Hash{idOf("a"), idOf("b"), idOf("c"), idOf("e")} // c moves to a new key, d out, e in
	removed := rewriteValidators(st, nv, nn)
	if len(removed) != 2 || removed[0] != old[2] || removed[1] != old[3] {
		t.Fatalf("removed %v", removed)
	}
	want := validatorsStorage(boot, nv, nn, shortRules)
	want[slotWord(valSlotGovernance)] = addrKey(gov)
	if len(st) != len(want) {
		t.Fatalf("%d slots, want %d", len(st), len(want))
	}
	for k, v := range want {
		if st[k] != v {
			t.Fatalf("slot %s: %s, want %s", k.Hex(), st[k].Hex(), v.Hex())
		}
	}
}

// TestAllocRoot: the root fork-from-state checks a dump against is the chain's own.
func TestAllocRoot(t *testing.T) {
	k, _ := crypto.GenerateKey()
	code, _ := validatorsRuntime()
	alloc := types.GenesisAlloc{crypto.PubkeyToAddress(k.PublicKey): {Balance: big.NewInt(12345), Nonce: 3},
		validatorsAddress: {Code: code, Storage: validatorsStorage(common.Address{}, someAddrs(4), nil, devnetRules), Balance: new(big.Int)}}
	sim := simulated.NewBackend(alloc, withOsaka)
	defer sim.Close()
	h, err := sim.Client().HeaderByNumber(context.Background(), big.NewInt(0))
	if err != nil {
		t.Fatal(err)
	}
	if got := allocRoot(alloc); got != h.Root {
		t.Fatalf("allocRoot %s, chain %s", got.Hex(), h.Root.Hex())
	}
}

// TestFeeOracleLoadSlot: the slot fork-from-state resets is FeeOracle's congestion meter.
func TestFeeOracleLoadSlot(t *testing.T) {
	w, _ := newGovWorld(t, "ana", "ben", "cai", "dov")
	fo := common.HexToAddress(w.ch.dep.Contracts["FeeOracle"])
	v, err := w.sim.Client().StorageAt(context.Background(), fo, slotWord(feeOracleLoadSlot), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := mustCall(t, w.ch, "FeeOracle", "multiplier").(*big.Int)
	low := new(big.Int).And(new(big.Int).SetBytes(v), new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1)))
	if low.Cmp(m) != 0 || m.Sign() == 0 {
		t.Fatalf("slot %d holds %x; multiplier() = %s", feeOracleLoadSlot, v, m)
	}
	st := map[common.Hash]common.Hash{slotWord(feeOracleLoadSlot): common.BytesToHash(v)}
	st[slotWord(feeOracleLoadSlot)] = common.BigToHash(new(big.Int).Or(new(big.Int).SetBytes(v), new(big.Int).Lsh(big.NewInt(777), 128)))
	resetFeeMeter(st)
	if st[slotWord(feeOracleLoadSlot)].Big().Cmp(m) != 0 {
		t.Fatal("reset didn't keep only the multiplier")
	}
}

// TestCheckpointSignVerify: citizens sign "this fork is the continuation"
// (after re-deriving it from the old chain); clients follow it only with
// more than half the old chain's citizens, and only the checkpoint they signed.
func TestCheckpointSignVerify(t *testing.T) {
	w, _, vs := a5valWorld(t, []string{"ana", "ben", "cai", "dov", "eve"}, 4, nil, a5valPreimages)
	ctx := context.Background()
	f := a5valMakeFork(t, w, []string{"ana=" + vs[0].Hex(), "ben=" + vs[1].Hex(), "cai=" + vs[2].Hex(), "dov=" + vs[3].Hex()})
	old, fork := a5valRPC(w.sim), a5valRPC(f.sim)
	cp, cpPath := f.cp, f.cpPath
	for _, n := range []string{"ana", "ben"} {
		w.must(errOf(checkpointSign(ctx, cpPath, filepath.Join(w.dir, n), old, f.gb, true)))
	}
	stranger, _ := w.owner("stranger")
	w.must(errOf(checkpointSign(ctx, cpPath, stranger.dir, old, f.gb, true)))
	var sigs []CheckpointSig
	w.must(readJSON(sigsPath(cpPath), &sigs))
	r, err := checkpointCheck(ctx, old, fork, cp, sigs, checkOpts{unsealed: true})
	w.must(err)
	if r["follow"] != false || len(r["signed_by"].([]string)) != 2 || len(r["rejected"].([]string)) != 1 {
		t.Fatalf("2 of 5 citizens and a stranger: %v", r)
	}
	w.must(errOf(checkpointSign(ctx, cpPath, filepath.Join(w.dir, "cai"), old, f.gb, true)))
	sigs = nil
	w.must(readJSON(sigsPath(cpPath), &sigs))
	r, err = checkpointCheck(ctx, old, fork, cp, sigs, checkOpts{unsealed: true})
	w.must(err)
	if r["follow"] != true {
		t.Fatalf("3 of 5 citizens: %v", r)
	}
	tampered := cp
	tampered.Validators = []string{"mallory=0x0000000000000000000000000000000000000003"}
	r, err = checkpointCheck(ctx, old, fork, tampered, sigs, checkOpts{unsealed: true})
	w.must(err)
	if r["follow"] != false || len(r["signed_by"].([]string)) != 0 {
		t.Fatalf("a tampered checkpoint: %v", r)
	}
	other := cp
	other.FromHash = common.HexToHash("0x99").Hex()
	if r, _ := checkpointCheck(ctx, old, fork, other, sigs, checkOpts{unsealed: true}); r["follow"] != false {
		t.Fatalf("a chain that isn't the fork: %v", r)
	}
}
