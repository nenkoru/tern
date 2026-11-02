// Final pre-launch fixes, part A (contracts and chain bootstrap): the audit 6
// PoCs (review/AUDIT-SECURITY-6.md, scratch copies tc6-*) turned into
// regression tests. Each test passes when the issue is fixed.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/nenkoru/tern/internal/ui"
)

func faUIDs(uids ...common.Hash) []byte {
	ids := make([][32]byte, len(uids))
	for i, x := range uids {
		ids[i] = x
	}
	f, _ := abi.Arguments{{Type: abiType("bytes32[]")}}.Pack(ids)
	return f
}

func faBal(c *Client) *big.Int { b, _ := c.bankBalance(c.f.Name); return b }

// RU-1: k of n, n > k: the first k deciders to record decide; nobody who
// claims or refunds picks a subset. carol and dave (10000 each) record
// first, erin's later 0 doesn't count: the payer can't take the lock back
// with {erin, carol}, the payee gets everything. The same for PanelCondition.
func TestFinalARulingNoSubsetPick(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	erin, _ := w.owner("erin")
	w.must(alice.deposit(coins(10)))
	day := uint64(86400)

	l, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*day, RulingOpts{Deciders: []string{"carol", "dave", "erin"}, K: 2}, "a logo")
	w.must(err)
	w.must(bob.acceptLock(l))
	w.later(time.Minute)
	uc, err := carol.rulingDecide(l, 10000, "delivered")
	w.must(err)
	ud, err := dave.rulingDecide(l, 10000, "delivered")
	w.must(err)
	ue, err := erin.rulingDecide(l, 0, "the payer's friend")
	w.mustFail(err, "decided")
	w.mustFail(alice.refundEarly(l, faUIDs(ue, uc)), "nothing to refund")
	b0 := faBal(bob)
	w.must(bob.claimLock(l, faUIDs(uc, ud)))
	if d := new(big.Int).Sub(faBal(bob), b0); d.Cmp(coins(1)) != 0 || alice.lockLeft(l).Sign() != 0 {
		t.Fatalf("payee got %s", fmtCoins(d))
	}

	subj := common.Hash{0xfa, 1}
	pd, err := panelDemandType().Pack(panelDemand{[][32]byte{idOf("carol"), idOf("dave"), idOf("erin")}, 2, alice.schema(), subj,
		crypto.Keccak256Hash([]byte("ruling")), alice.chainNowU() + 60})
	w.must(err)
	p, err := alice.lock("bob", coins(1), alice.chainNowU()+2*day, "PanelCondition", pd)
	w.must(err)
	w.must(bob.acceptLock(p))
	w.later(2 * time.Minute)
	pc, err := carol.decideOn(p, subj, "ruling", nil, 10000, "bob", false)
	w.must(err)
	_, err = dave.decideOn(p, subj, "ruling", nil, 10000, "bob", false)
	w.must(err)
	pe, err := erin.decideOn(p, subj, "ruling", nil, 0, "bob", false)
	w.mustFail(err, "decided")
	w.mustFail(alice.refundEarly(p, faUIDs(pe, pc)), "nothing to refund")
	w.must(bob.claimLock(p, nil))
	if alice.lockLeft(p).Sign() != 0 {
		t.Fatal("panel: the payee wasn't paid in full")
	}
}

// RU-1b: a decider's newer ruling replaces its own (before the decision is
// complete): the superseded one decides nothing.
func TestFinalARulingRedecideReplaces(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	w.must(alice.deposit(coins(10)))
	l, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*86400, RulingOpts{Deciders: []string{"carol", "dave"}, K: 2}, "a logo")
	w.must(err)
	w.must(bob.acceptLock(l))
	w.later(time.Minute)
	first, err := carol.rulingDecide(l, 0, "not delivered")
	w.must(err)
	w.later(time.Second)
	_, err = carol.rulingDecide(l, 10000, "sorry: delivered after all")
	w.must(err)
	_, err = dave.rulingDecide(l, 10000, "delivered")
	w.must(err)
	w.mustFail(alice.refundEarly(l, faUIDs(first)), "nothing to refund")
	w.must(bob.claimLock(l, nil))
	if alice.lockLeft(l).Sign() != 0 {
		t.Fatal("the newer ruling didn't decide")
	}
	// The decision is frozen once k recorded.
	_, err = carol.rulingDecide(l, 0, "changed my mind again")
	w.mustFail(err, "decided")
}

// RU-2: a hearing that ends at the refund time no longer lets the payer
// dispute and run out the clock.
func TestFinalARulingHearingKeepsDisputeFloor(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(alice.deposit(coins(10)))
	now := alice.chainNowU()
	refundAfter := now + 2*86400
	dm, _ := rulingDemandType().Pack(rulingDemand{[][32]byte{idOf("carol")}, 1, 0, 0, alice.schema(), common.Hash{0xfa, 2},
		crypto.Keccak256Hash([]byte("ruling")), refundAfter, now + 3600, new(big.Int), false})
	l, err := alice.lock("bob", coins(1), refundAfter, "RulingCondition", dm)
	w.must(err)
	w.mustFail(bob.acceptLock(l), "hearing") // RU-3: the client refuses it too
	w.later(time.Minute)
	w.mustFail(alice.disputeRuling(l, "never mind"), "too late for the deciders")
}

// RU-3: accept refuses a ruling lock that can never decide (no such rule,
// the payer as the only decider), and lock show warns.
func TestFinalARulingAcceptChecksDemand(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.owner("carol")
	w.must(alice.deposit(coins(10)))
	now := alice.chainNowU()
	dm, _ := rulingDemandType().Pack(rulingDemand{[][32]byte{idOf("carol")}, 1, 9, 0, alice.schema(), common.Hash{0xfa, 3},
		crypto.Keccak256Hash([]byte("ruling")), 0, 0, new(big.Int), false})
	l, err := alice.lock("bob", coins(1), now+2*86400, "RulingCondition", dm)
	w.must(err)
	w.mustFail(bob.acceptLock(l), "rule 9")
	row, _ := bob.ch.call("Bank", "locks", u(l))
	if _, has := lockSubject("RulingCondition", row[8].([]byte)); !has {
		t.Fatal("lockSubject doesn't know RulingCondition")
	}
	dm2, _ := rulingDemandType().Pack(rulingDemand{[][32]byte{idOf("alice")}, 1, 0, 0, alice.schema(), common.Hash{0xfa, 4},
		crypto.Keccak256Hash([]byte("ruling")), 0, 0, new(big.Int), false})
	l2, err := alice.lock("bob", coins(1), now+2*86400, "RulingCondition", dm2)
	w.must(err)
	row2, _ := bob.ch.call("Bank", "locks", u(l2))
	if wn := bob.deciderWarning(row2); !strings.Contains(wn, "payer's side") {
		t.Fatalf("no warning: %q", wn)
	}
	w.mustFail(bob.acceptLock(l2), "distinct deciders")
}

// RU-4: one seat per owner (a decider listed twice is one seat), and a
// member of a split payee is on the payee's side.
func TestFinalARulingSeatsPerOwner(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	w.must(alice.deposit(coins(10)))
	_, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*86400, RulingOpts{Deciders: []string{"carol", "carol"}, K: 2}, "x")
	w.mustFail(err, "distinct owners")
	now := alice.chainNowU()
	dm, _ := rulingDemandType().Pack(rulingDemand{[][32]byte{idOf("carol"), idOf("carol")}, 2, 0, 0, alice.schema(), common.Hash{0xfa, 5},
		crypto.Keccak256Hash([]byte("ruling")), 0, 0, new(big.Int), false})
	l, err := alice.lock("bob", coins(1), now+2*86400, "RulingCondition", dm)
	w.must(err)
	w.later(time.Minute)
	uc, err := carol.rulingDecide(l, 10000, "")
	w.must(err)
	w.later(time.Second)
	_, err = carol.rulingDecide(l, 10000, "again") // replaces its own seat, takes no second one
	w.must(err)
	w.mustFail(bob.claimLock(l, faUIDs(uc, uc)), "not enough rulings")

	bank := w.ch.addr("Bank")
	payees, bps := [][32]byte{idOf("bob"), idOf("carol")}, []uint16{5000, 5000}
	split := bricksCall(w, "Bank", bank, "createSplit", payees, bps)[0].([32]byte)
	w.must(bricksSend(w, alice.key, "Bank", bank, "createSplit", payees, bps))
	dm2, _ := rulingDemandType().Pack(rulingDemand{[][32]byte{idOf("bob")}, 1, 0, 0, alice.schema(), common.Hash{0xfa, 6},
		crypto.Keccak256Hash([]byte("ruling")), 0, 0, new(big.Int), false})
	n, _ := alice.ch.call("Bank", "lockCount")
	l2 := n[0].(*big.Int).Uint64()
	w.must(errOf(w.ch.send(alice.key, nil, "Bank", "lock", idOf("alice"), split, coins(1), now+2*86400, w.ch.addr("RulingCondition"), dm2)))
	w.later(time.Minute)
	_, err = bob.decideOn(l2, common.Hash{0xfa, 6}, "ruling", nil, 10000, "", false)
	w.mustFail(err, "party's side")
	w.mustFail(carol.claimLock(l2, nil), "not enough rulings")
}

// RU-5: PanelCondition counts only rulings made after its lock opened.
func TestFinalAPanelRulingBeforeOpen(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	w.must(alice.deposit(coins(10)))
	subj := common.Hash{0xfa, 7}
	n, err := alice.ch.call("Bank", "lockCount")
	w.must(err)
	next := n[0].(*big.Int).Uint64()
	pre, err := carol.decideOn(next, subj, "ruling", nil, 10000, "bob", false) // no lock yet: nothing recorded
	w.must(err)
	w.later(time.Minute)
	pd, err := panelDemandType().Pack(panelDemand{[][32]byte{idOf("carol")}, 1, alice.schema(), subj, crypto.Keccak256Hash([]byte("ruling")), 0})
	w.must(err)
	p, err := alice.lock("bob", coins(1), alice.chainNowU()+2*86400, "PanelCondition", pd)
	w.must(err)
	if p != next {
		t.Fatalf("lock %d, predicted %d", p, next)
	}
	_, err = w.ch.send(carol.key, nil, "PanelCondition", "record", u(p), pre)
	w.mustFail(err, "not a ruling that counts")
	w.mustFail(bob.claimLock(p, faUIDs(pre)), "not enough of the panel")
}

// RU-6 and the free dispute block: anyone-disputes needs a bond; a dispute
// the deciders never decide forfeits the bond to the payee.
func TestFinalARulingDisputeBond(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	frank, _ := w.owner("frank")
	w.owner("carol")
	w.must(alice.deposit(coins(10)))
	w.must(frank.deposit(coins(1)))
	_, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*86400, RulingOpts{Deciders: []string{"carol"}, Window: 3600, AnyoneDisputes: true}, "free")
	w.mustFail(err, "only with a bond")
	now := alice.chainNowU()
	dm, _ := rulingDemandType().Pack(rulingDemand{[][32]byte{idOf("carol")}, 1, 0, 0, alice.schema(), common.Hash{0xfa, 8},
		crypto.Keccak256Hash([]byte("ruling")), 0, now + 3600, new(big.Int), true})
	l0, err := alice.lock("bob", coins(1), now+2*86400, "RulingCondition", dm)
	w.must(err)
	w.mustFail(frank.disputeRuling(l0, "grief"), "only with a bond")

	opt := RulingOpts{Deciders: []string{"carol"}, Window: 3600, DisputeBond: coins(0.5), AnyoneDisputes: true}
	l, _, err := alice.openRuling("bob", coins(1), alice.chainNowU()+2*86400, opt, "an assertion")
	w.must(err)
	w.must(bob.acceptLock(l))
	w.must(frank.disputeRuling(l, "grief"))
	w.later(49 * time.Hour) // carol never rules
	w.must(alice.refundLock(l))
	b0 := faBal(bob)
	f0, _ := w.ch.b.BalanceAt(t.Context(), frank.addr(), nil)
	w.must(errOf(w.ch.send(alice.key, nil, "RulingCondition", "settleBond", u(l))))
	f1, _ := w.ch.b.BalanceAt(t.Context(), frank.addr(), nil)
	if d := new(big.Int).Sub(faBal(bob), b0); d.Cmp(coins(0.5)) != 0 || f1.Cmp(f0) != 0 {
		t.Fatalf("the payee got %s of the bond, the disputer %s back", fmtCoins(d), fmtCoins(new(big.Int).Sub(f1, f0)))
	}
}

// IT-4: an ERC-20 (or any non-item contract) can't be deposited as an item.
func TestFinalAItemDepositNeedsACollection(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	usd := common.HexToAddress(w.ch.dep.Contracts["TestUSD"])
	_, err := w.ch.send(alice.key, nil, "Bank", "depositItem", alice.id(), usd, u(0), u(1))
	w.mustFail(err, "not an item collection")
	if n := mustCall(t, w.ch, "Bank", "itemBalance", alice.id(), usd, u(0)).(*big.Int); n.Sign() != 0 {
		t.Fatalf("credited %s units", n)
	}
	if !mustCall(t, w.ch, "Bank", "supportsInterface", [4]byte{0x15, 0x0b, 0x7a, 0x02}).(bool) {
		t.Fatal("the Bank should take ERC-721 safe transfers")
	}
}

// CM-3: names are per publisher: a flood of another publisher's names
// can't hide a pinned publisher's kind, and the generic name isn't
// squatted for everyone.
func TestFinalASchemaNamesPerPublisher(t *testing.T) {
	t.Cleanup(func() { kindsOnce = sync.Once{}; ui.LoadKinds(nil) })
	w := newWorld(t)
	mal, _ := w.owner("mal")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.must(w.ch.transfer(w.faucet, mal.addr(), coins(5000)))
	_, uid, err := mal.publishType("card/rsvp", "string squat", "")
	w.must(err)
	for i := 1; i < 4096; i++ {
		w.must(errOf(w.ch.send(mal.key, nil, "SchemaNames", "publish", mal.id(), fmt.Sprintf("x/j%04d", i), uid, "")))
	}
	decl := `{"body":{"type":"object"},"responses":{"going":"answer"}}`
	_, _, err = alice.publishType("card/rsvp", "string event", "data:application/json,"+url.PathEscape(decl)) // the same name: alice's own
	w.must(err)
	b, _ := json.Marshal(map[string]any{"publishers": []string{"alice"}})
	w.must(os.WriteFile(filepath.Join(bob.dir, "types.json"), b, 0o644))
	ds, err := (&schemaNamesSource{c: bob}).CardKinds()
	w.must(err)
	found := false
	for _, d := range ds {
		if d.Kind == "rsvp" {
			found = true
			if !strings.Contains(d.Source, "alice") {
				t.Fatalf("rsvp from %s", d.Source)
			}
		}
	}
	if !found {
		t.Fatal("alice's card/rsvp is hidden by mal's flood")
	}
}

// GroupTrees: a member who leaves frees its slot; churn reuses it.
func TestFinalAGroupTreesReuseSlots(t *testing.T) {
	w := newWorld(t)
	a, _ := w.owner("ana")
	z, _ := w.owner("zed")
	w.must(errOf(w.ch.send(a.key, nil, "Sets", "create", "open-club", a.id(), uint8(0))))
	set := common.Hash(mustCall(t, w.ch, "Sets", "idOf", "open-club").([32]byte))
	sets := w.ch.addr("Sets")
	w.must(errOf(w.ch.send(z.key, nil, "GroupTrees", "open", sets, set)))
	g := a.groupID(sets, set)
	w.must(a.groupJoinTree(g))
	for i := 0; i < 5; i++ {
		w.must(errOf(w.ch.send(z.key, nil, "Sets", "request", set, z.id())))
		w.must(z.groupJoinTree(g))
		w.must(errOf(w.ch.send(z.key, nil, "Sets", "remove", set, z.id(), z.id(), false, "bye")))
		w.must(errOf(w.ch.send(z.key, nil, "GroupTrees", "sync", g, z.id())))
	}
	out, err := w.ch.call("GroupTrees", "trees", g)
	w.must(err)
	if size := out[3].(*big.Int).Uint64(); size != 2 {
		t.Fatalf("slots used after churn: %d", size)
	}
}

// faZeroFounders: a chain with Validators in its genesis, deployed with no founders.
func faZeroFounders(t *testing.T, keep bool) (*Chain, error) {
	faucet, _ := crypto.GenerateKey()
	boot := crypto.PubkeyToAddress(faucet.PublicKey)
	code, err := validatorsRuntime()
	if err != nil {
		t.Fatal(err)
	}
	alloc := types.GenesisAlloc{boot: {Balance: new(big.Int).Mul(coin, big.NewInt(1_000_000))},
		validatorsAddress: {Code: code, Storage: validatorsStorage(boot, someAddrs(1), []common.Hash{{}}, shortRules), Balance: new(big.Int)}}
	sim := simulated.NewBackend(alloc, withOsaka)
	t.Cleanup(func() { sim.Close() })
	return deploy(sim.Client(), faucet, Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12),
		Gov: GovParams{Short: true}, Validators: validatorsAddress, KeepValidators: keep}, "", func() { sim.Commit() })
}

// VB-1: deploy refuses to hand the validator set to a Parliament with no
// founders; with --keep-validators the bootstrap keeps it (not frozen).
func TestFinalAZeroFoundersNoFrozenHandover(t *testing.T) {
	if _, err := faZeroFounders(t, false); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("deploy handed the set to a citizen-less Parliament: %v", err)
	}
	ch, err := faZeroFounders(t, true)
	if err != nil {
		t.Fatal(err)
	}
	if g := mustCall(t, ch, "Validators", "governance").(common.Address); g != (common.Address{}) {
		t.Fatalf("handed over to %s", g.Hex())
	}
}

// VB-1/VB-2 option A: a header-voting genesis for test networks; VB-6: the
// contract mode's minimum set stays at least 4.
func TestFinalAHeaderVotingGenesis(t *testing.T) {
	vs := someAddrs(1)
	g, err := validatorGenesis(genesisOpts{chainID: l1TestnetChainID, period: 1, timeout: 4, validators: vs, deployer: someAddrs(1)[0],
		supply: coins(1000), gasLimit: 60_000_000, headerVoting: true})
	if err != nil {
		t.Fatal(err)
	}
	q := g["config"].(map[string]any)["qbft"].(map[string]any)
	if _, has := q["validatorcontractaddress"]; has {
		t.Fatal("header voting with a validator contract")
	}
	if _, has := g["alloc"].(map[string]any)[strings.ToLower(validatorsAddress.Hex())]; has {
		t.Fatal("a Validators contract in a header-voting genesis")
	}
	var extra struct {
		Vanity     []byte
		Validators []common.Address
		Vote       []rlp.RawValue
		Round      uint
		Seals      [][]byte
	}
	if err := rlp.DecodeBytes(common.FromHex(g["extraData"].(string)), &extra); err != nil || len(extra.Validators) != 1 || extra.Validators[0] != vs[0] {
		t.Fatalf("extraData validators %v (%v)", extra.Validators, err)
	}
	out := filepath.Join(t.TempDir(), "g.json")
	err = runValidatorCmd([]string{"validator", "genesis", "--validators", vs[0].Hex(), "--out", out, "--deployer", vs[0].Hex(), "--min-set", "1"})
	if err == nil || !strings.Contains(err.Error(), "floor is 4") {
		t.Fatalf("--min-set 1 in contract mode: %v", err)
	}
	err = runValidatorCmd([]string{"validator", "genesis", "--validators", vs[0].Hex(), "--out", out, "--deployer", vs[0].Hex(), "--selection", "header",
		"--chain-id", fmt.Sprint(l1MainnetChainID)})
	if err == nil || !strings.Contains(err.Error(), "mainnet") {
		t.Fatalf("header voting on the mainnet: %v", err)
	}
}

// ZK-4 on a test network: a one-party ceremony deploys only on a network
// marked test (never the mainnet); elsewhere 3 contributions per phase.
func TestFinalACeremonyGate(t *testing.T) {
	dir := t.TempDir()
	hd, _ := json.Marshal(mpcHeader{N: 2, Phase1: []mpcEntry{{Name: "op", SHA256: []string{"00"}}}, Beacon1: "aa",
		Phase2: []mpcEntry{{Name: "op", SHA256: []string{"00", "00"}}}, Beacon2: "bb"})
	var b []byte
	b = append(b, mpcMagic...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(hd)))
	b = append(b, hd...)
	h := sha256.Sum256(b)
	if err := os.WriteFile(filepath.Join(dir, "transcript.mpc"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CEREMONY"), []byte(fmt.Sprintf("0x%x\n", h)), 0o644); err != nil {
		t.Fatal(err)
	}
	p1, p2, err := ceremonyParties(dir)
	if err != nil || p1 != 1 || p2 != 1 {
		t.Fatalf("parties %d %d %v", p1, p2, err)
	}
	if err := ceremonyGate(l1TestnetChainID, false, p1, p2); err == nil {
		t.Fatal("one-party keys on 83760 without --test-network")
	}
	if err := ceremonyGate(l1TestnetChainID, true, p1, p2); err != nil {
		t.Fatalf("a test network refused operator keys: %v", err)
	}
	if err := ceremonyGate(l1MainnetChainID, true, 5, 5); err == nil {
		t.Fatal("the mainnet as a test network")
	}
	if err := ceremonyGate(l1MainnetChainID, false, p1, p2); err == nil {
		t.Fatal("one-party keys on the mainnet")
	}
	if err := ceremonyGate(l1MainnetChainID, false, 3, 3); err != nil {
		t.Fatalf("a 3-party ceremony on the mainnet: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "CEREMONY"), []byte("0x"+strings.Repeat("11", 32)), 0o644)
	if _, _, err := ceremonyParties(dir); err == nil {
		t.Fatal("a transcript that isn't the CEREMONY's")
	}
	if n := testNetworkNotice("tern-testnet-1", "Oscar"); !strings.Contains(n, "NOT private from the operator") {
		t.Fatalf("notice: %s", n)
	}
}

// PROOFREAD-6 #28, #30: the Parliament and admission constants are deploy parameters.
func TestFinalAGovernanceRulesAtDeploy(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct {
		contract, getter string
		want         int64
	}{{"Parliament", "CRITICAL_DELAY_X", 3}, {"Parliament", "MIN_TURNOUT", 5}, {"Parliament", "SUPER_NUM", 2}, {"Parliament", "SUPER_DEN", 3},
		{"Citizens", "ADMIT_BPS", 2000}, {"Citizens", "ADMIT_FLOOR", 3}} {
		if v := mustCall(t, w.ch, c.contract, c.getter).(*big.Int).Int64(); v != c.want {
			t.Fatalf("%s.%s = %d, want %d", c.contract, c.getter, v, c.want)
		}
	}
	faucet, _ := crypto.GenerateKey()
	sim := simulated.NewBackend(types.GenesisAlloc{crypto.PubkeyToAddress(faucet.PublicKey): {Balance: coins(1_000_000)}}, withOsaka)
	t.Cleanup(func() { sim.Close() })
	ch, err := deploy(sim.Client(), faucet, Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12),
		Gov: GovParams{Short: true, AdmitBps: 500, AdmitFloor: 7, CriticalDelayX: 2, MinTurnout: 9, SuperNum: 3, SuperDen: 4}}, "", func() { sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	if v := mustCall(t, ch, "Citizens", "ADMIT_FLOOR").(*big.Int).Int64(); v != 7 {
		t.Fatalf("ADMIT_FLOOR %d", v)
	}
	if v := mustCall(t, ch, "Parliament", "MIN_TURNOUT").(*big.Int).Int64(); v != 9 {
		t.Fatalf("MIN_TURNOUT %d", v)
	}
	if v := mustCall(t, ch, "Parliament", "SUPER_NUM").(*big.Int).Int64(); v != 3 {
		t.Fatalf("SUPER_NUM %d", v)
	}
}

// FO-1, FO-2: the oracle's cooldown and round length are bounded.
func TestFinalAOracleBounds(t *testing.T) {
	w := newWorld(t)
	r := newOracleRig(w, coins(1), coins(1), 64)
	_, err := w.ch.send(r.gov, nil, r.name, "setParams", exact("0.01"), uint16(2000), coins(1), uint16(5000), uint16(2000),
		uint64(math.MaxUint64), uint16(64))
	w.mustFail(err, "slash cooldown")
	_, err = w.ch.send(r.gov, nil, r.name, "setRoundLength", uint64(1)<<62)
	w.mustFail(err, "30 days")
	w.must(errOf(w.ch.send(r.gov, nil, r.name, "setRoundLength", uint64(30*86400))))
}

// FO-3: replacing the FeeOracle is governance-critical, and postPaid has a
// ceiling: an oracle a vote installs can't drain a name's balance.
func TestFinalAOracleCriticalPostPaidCapped(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b := f["ana"], f["ben"]
	w.must(a.lockVotes(coins(30)))
	w.later(2 * time.Second)
	tl, tr := common.HexToAddress(w.ch.dep.Contracts["TernTimelock"]), common.HexToAddress(w.ch.dep.Contracts["Treasury"])
	inbox := common.HexToAddress(w.ch.dep.Contracts["Inbox"])
	basket := new(big.Int).Mul(mustCall(t, w.ch, "FeeOracle", "basketPrice").(*big.Int), big.NewInt(1_000_000))
	v2 := deployNew(w, "FeeOracle", basket, uint64(86400), coins(0.01), uint16(1000), coins(100), tl, tr, big.NewInt(64), coins(100),
		[][32]byte{envelopeKind}, []*big.Int{big.NewInt(10)}, crypto.Keccak256Hash(basketSpec), basketURI, []common.Address{inbox})
	p, err := a.govProposeUpgrade("FeeOracle", v2, writeDoc(t, "o.md", "oracle"), "", writeDoc(t, "oa.md", "audit"), "", "")
	w.must(err)
	if !mustCall(t, w.ch, "Parliament", "critical", u(p)).(bool) {
		t.Fatal("a FeeOracle upgrade isn't critical")
	}
	w.pass(p, []*Client{a}, clients(f, "ana", "ben", "cai"))
	_, err = w.ch.send(b.key, coins(5), "Bank", "deposit", idOf("ben"))
	w.must(err)
	env := append([]byte("TC2"), make([]byte, 60)...)
	_, err = w.ch.send(b.key, nil, "Inbox", "postPaid", idOf("ben"), env, big.NewInt(1))
	w.mustFail(err, "ceiling")
}
