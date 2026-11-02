package main

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
)

// newGovWorld: a network whose founding citizens are the given names (their
// keys go into the deployment), each registered and claimed. Short (devnet)
// timings: votes and the exit window take 10 minutes, a name must be 10 minutes old.
func newGovWorld(t *testing.T, founders ...string) (*world, map[string]*Client) {
	t.Helper()
	w, cs := newGovWorldClaimed(t, founders...)
	// Founders confirm one another, then serve probation before they vouch (audit 5 GV-2).
	for i, n := range founders {
		for j := 1; j < len(founders); j++ {
			if v, _ := cs[n].citizen(n); v.Confirmed {
				break
			}
			w.must(errOf(cs[founders[(i+j)%len(founders)]].govVouch(n)))
		}
	}
	p, err := cs[founders[0]].citizenParams()
	w.must(err)
	w.later(time.Duration(p.MinAge+1) * time.Second)
	return w, cs
}

// newGovWorldClaimed: founders that claimed their names, nothing more.
func newGovWorldClaimed(t *testing.T, founders ...string) (*world, map[string]*Client) {
	t.Helper()
	faucet, _ := crypto.GenerateKey()
	alloc := types.GenesisAlloc{crypto.PubkeyToAddress(faucet.PublicKey): {Balance: new(big.Int).Mul(coin, big.NewInt(1_000_000))}}
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
		Gov: GovParams{Genesis: gen, Short: true}}, "", func() { sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	ch.bundler = faucet
	w := &world{t: t, sim: sim, ch: ch, dir: dir, faucet: faucet}
	w.startRelay() // secret citizen votes go through a relayer
	cs := map[string]*Client{}
	for _, n := range founders {
		g, err := newIdentDir(filepath.Join(dir, "guardian-"+n), IdentityFile{Name: n, Kind: "guardian"})
		if err != nil {
			t.Fatal(err)
		}
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
	return w, cs
}

func (w *world) citizen(c *Client, name string) bool { return c.isCitizen(name) }

// TestCitizenshipByVouchingAndAge: K=3 vouches and a name 10 minutes old;
// agents never; a sybil ring can't vouch itself in.
func TestCitizenshipByVouchingAndAge(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b, c := f["ana"], f["ben"], f["cai"]
	if n := mustCall(t, w.ch, "Citizens", "count").(uint32); n != 4 {
		t.Fatalf("founders: %d", n)
	}
	eve, _ := w.owner("eve")
	w.must(errOf(a.govVouch("eve")))
	w.must(errOf(b.govVouch("eve")))
	_, err := a.govVouch("eve")
	w.mustFail(err, "vouched already")
	st, err := c.govVouch("eve") // three vouches, but the name is minutes old
	w.must(err)
	if st["citizen"] != false || !strings.Contains(st["age"].(string), "old enough") {
		t.Fatalf("too young a name became a citizen: %v", st)
	}
	_, err = eve.ch.send(eve.key, nil, "Citizens", "admit", eve.id())
	w.mustFail(err, "not eligible")
	w.later(11 * time.Minute)
	if _, err := eve.ch.send(eve.key, nil, "Citizens", "admit", eve.id()); err != nil { // anyone may admit once eligible
		t.Fatal(err)
	}
	if !w.citizen(a, "eve") {
		t.Fatal("eve is not a citizen")
	}
	m, err := a.govCitizens()
	w.must(err)
	if m["count"].(uint32) != 5 {
		t.Fatalf("citizens: %v", m)
	}
	// Agents are never citizens, and never vote for their owner.
	bot := w.agent(eve, "bot", coins(1), `{}`)
	_, err = a.ch.send(a.key, nil, "Citizens", "vouch", a.id(), idOf("bot.eve"))
	w.mustFail(err, "an owner name")
	_, err = bot.govVouch("frank")
	w.mustFail(err, "agents don't")
	_, err = bot.ch.send(bot.key, nil, "Citizens", "vouch", eve.id(), idOf("ana"))
	w.mustFail(err, "the owner's own key only")

	// A sybil ring: old names vouching each other get nowhere without citizens.
	ring := []*Client{}
	for _, n := range []string{"s1", "s2", "s3", "s4"} {
		c, _ := w.owner(n)
		ring = append(ring, c)
	}
	w.later(11 * time.Minute)
	for i, s := range ring {
		_, err := s.govVouch(ring[(i+1)%len(ring)].f.Name)
		w.mustFail(err, "only citizens vouch")
	}
	// Even one citizen's vouch is not enough: K are needed.
	w.must(errOf(a.govVouch("s1")))
	if w.citizen(a, "s1") {
		t.Fatal("one vouch made a citizen")
	}
	// Leaving: a citizen can go.
	w.must(eve.govCitizenCall("leave", eve.id()))
	if w.citizen(a, "eve") {
		t.Fatal("eve left but is still a citizen")
	}
}

func errOf(_ any, err error) error { return err }

// TestRevocationPunishesVouchers: citizens vote a name out as a sybil; those
// who vouched for it lose citizenship for the penalty, then come back.
func TestRevocationPunishesVouchers(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b, c, d := f["ana"], f["ben"], f["cai"], f["dov"]
	sy, _ := w.owner("sybil")
	w.later(11 * time.Minute)
	for _, v := range []*Client{a, b, c} {
		w.must(errOf(v.govVouch("sybil")))
	}
	if !w.citizen(a, "sybil") {
		t.Fatal("not admitted")
	}
	w.later(2 * time.Second)
	r, err := d.govRevoke("sybil", true, "the same person as ana: same IP, same writing, found by dov")
	w.must(err)
	_, err = sy.ch.send(sy.key, nil, "Citizens", "voteRevocation", u(r), sy.id(), false)
	w.mustFail(err, "citizens from before the vote only") // not on its own case
	w.must(d.govCitizenCall("voteRevocation", u(r), d.id(), true))
	// Its vouchers don't judge their own sybil (audit 4 GV-1): dov, the one independent citizen, decides.
	w.mustFail(b.govCitizenCall("voteRevocation", u(r), b.id(), true), "you vouched for this name")
	_, err = d.ch.send(d.key, nil, "Citizens", "closeRevocation", u(r))
	w.mustFail(err, "vote open")
	w.later(11 * time.Minute)
	w.must(errOf(d.ch.send(d.key, nil, "Citizens", "closeRevocation", u(r))))
	for _, n := range []string{"sybil", "ana", "ben", "cai"} {
		if w.citizen(d, n) {
			t.Fatalf("%s is still a citizen after the sybil vote", n)
		}
	}
	if !w.citizen(d, "dov") || mustCall(t, w.ch, "Citizens", "count").(uint32) != 1 {
		t.Fatal("dov should be the only citizen left")
	}
	// The penalty runs out: vouchers come back; the sybil stays barred.
	_, err = a.ch.send(a.key, nil, "Citizens", "reinstate", a.id())
	w.mustFail(err, "too early")
	w.later(61 * time.Minute)
	for _, v := range []*Client{a, b, c} {
		w.must(errOf(v.ch.send(v.key, nil, "Citizens", "reinstate", v.id())))
	}
	if !w.citizen(d, "ana") || w.citizen(d, "sybil") {
		t.Fatal("reinstatement")
	}
}

// TestFoundersMustBeReconfirmed: no permanent founders.
func TestFoundersMustBeReconfirmed(t *testing.T) {
	w, f := newGovWorldClaimed(t, "ana", "ben", "cai", "dov")
	// ana is re-confirmed by three others; nobody vouches for dov.
	for _, n := range []string{"ben", "cai", "dov"} {
		w.must(errOf(f[n].govVouch("ana")))
	}
	if v, _ := f["ana"].citizen("ana"); !v.Confirmed {
		t.Fatal("ana not confirmed")
	}
	// A squatter can't claim a founding name it registered first.
	sq, _ := w.owner("zed")
	_, err := sq.ch.send(sq.key, nil, "Citizens", "claimGenesis", sq.id())
	w.mustFail(err, "not a founding name")
	w.later(8 * 24 * time.Hour) // past the first term (7 days on a devnet)
	if !w.citizen(f["ben"], "ana") || w.citizen(f["ana"], "dov") {
		t.Fatal("after the first term only re-confirmed founders remain")
	}
	w.must(errOf(f["ana"].ch.send(f["ana"].key, nil, "Citizens", "lapse", idOf("dov"))))
}

func deployNew(w *world, name string, args ...any) common.Address {
	w.t.Helper()
	art, _ := loadArtifact(name)
	opts, _ := w.ch.opts(w.faucet, nil)
	addr, tx, _, err := bind.DeployContract(opts, art.abi, art.bin, w.ch.b, args...)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.ch.wait(tx); err != nil {
		w.t.Fatal(err)
	}
	return addr
}

func writeDoc(t *testing.T, name, text string) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// pass runs a proposal through both houses (yes from the given voters) and the timelock.
func (w *world) pass(id uint64, coinYes []*Client, citizenYes []*Client) {
	w.t.Helper()
	w.later(2 * time.Second)
	for _, c := range coinYes {
		w.must(errOf(c.govVote(id, true, "coin")))
	}
	for _, c := range citizenYes {
		w.must(errOf(c.govVote(id, true, "citizens")))
	}
	w.later(11 * time.Minute)
	w.must(errOf(coinYes[0].govQueue(id)))
	for i := 0; i < 4; i++ { // a governance-critical proposal waits 3 exit windows
		w.later(11 * time.Minute)
		if st, _ := coinYes[0].govState(id); st == "ready" {
			break
		}
	}
	w.must(coinYes[0].govExecute(id))
}

// TestUpgradeNeedsBothHouses: an upgrade passes only with both houses, and
// only after the exit window; either house alone stops one; the Directory
// keeps every version; old contracts keep working; clients are told, not switched.
func TestUpgradeNeedsBothHouses(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b, c := f["ana"], f["ben"], f["cai"]
	w.must(a.lockVotes(coins(30))) // over the coin house's 10-coin quorum floor
	w.must(b.lockVotes(coins(20)))
	w.later(2 * time.Second)
	oldBank := common.HexToAddress(w.ch.dep.Contracts["Bank"])
	bankV2 := deployNew(w, "Bank", common.HexToAddress(w.ch.dep.Contracts["Registry"]), common.HexToAddress(w.ch.dep.Contracts["Inbox"]))
	spec := writeDoc(t, "bank-v2.md", "Bank v2: the same interface, plus …")
	audit := writeDoc(t, "bank-v2-audit.pdf", "audit report: no high findings")
	_, err := a.govProposeUpgrade("Bank", bankV2, spec, "", "", "", "")
	w.mustFail(err, "--audit")
	p1, err := a.govProposeUpgrade("Bank", bankV2, spec, "", audit, "", "move balances: withdraw from v1, deposit to v2")
	w.must(err)
	w.later(2 * time.Second)
	w.must(errOf(a.govVote(p1, true, "")))
	w.must(errOf(b.govVote(p1, true, "citizens")))
	_, err = a.govQueue(p1)
	w.mustFail(err, "voting")
	w.later(11 * time.Minute)
	if st, _ := a.govState(p1); st != "passed" {
		t.Fatalf("p1: %s", st)
	}
	w.must(errOf(a.govQueue(p1)))
	w.mustFail(a.govExecute(p1), "queued") // the exit window
	w.later(11 * time.Minute)
	w.must(a.govExecute(p1))
	if cur, _ := a.dirCurrent("Bank"); cur != bankV2 {
		t.Fatal("Bank v2 is not current")
	}
	vs, err := a.dirVersions("Bank")
	w.must(err)
	if len(vs) != 2 || vs[0].Value != oldBank || vs[1].Value != bankV2 || vs[1].Audit != crypto.Keccak256Hash([]byte("audit report: no high findings")) ||
		vs[1].Notes == "" {
		t.Fatalf("history: %+v", vs)
	}

	// The coin house says yes, the citizens no: stopped.
	commitV2 := deployNew(w, "Commitments", common.HexToAddress(w.ch.dep.Contracts["Registry"]))
	p2, err := a.govProposeUpgrade("Commitments", commitV2, spec, "", audit, "", "")
	w.must(err)
	w.later(2 * time.Second)
	w.must(errOf(a.govVote(p2, true, "coin")))
	w.must(errOf(a.govVote(p2, true, "citizens")))
	w.must(errOf(b.govVote(p2, false, "citizens")))
	w.must(errOf(c.govVote(p2, false, "citizens")))
	// The citizens say yes, the coin house no: stopped.
	p3, err := c.govProposeUpgrade("Commitments", commitV2, spec, "", audit, "", "")
	w.must(err)
	w.later(2 * time.Second)
	w.must(errOf(a.govVote(p3, false, "coin")))
	w.must(errOf(b.govVote(p3, true, "coin")))
	for _, x := range []*Client{a, b, c} {
		w.must(errOf(x.govVote(p3, true, "citizens")))
	}
	// Coins alone, with nobody in the citizens' house: stopped (quorum).
	p4, err := a.govProposeUpgrade("Commitments", commitV2, spec, "", audit, "", "")
	w.must(err)
	w.later(2 * time.Second)
	w.must(errOf(a.govVote(p4, true, "coin")))
	w.later(11 * time.Minute)
	for _, p := range []uint64{p2, p3, p4} {
		if st, _ := a.govState(p); st != "defeated" {
			t.Fatalf("proposal %d: %s, want defeated", p, st)
		}
		_, err := a.govQueue(p)
		w.mustFail(err, "defeated")
	}

	// A new primitive arrives the same way.
	notary := deployNew(w, "Commitments", common.HexToAddress(w.ch.dep.Contracts["Registry"]))
	p5, err := b.govProposeUpgrade("Notary", notary, spec, "", audit, "", "")
	w.must(err)
	w.pass(p5, []*Client{a}, []*Client{a, b})
	ch2, err := newChain(w.sim.Client(), Deployment{Contracts: map[string]string{"Directory": w.ch.dep.Contracts["Directory"]}}, func() { w.sim.Commit() })
	w.must(err)
	ps, err := ch2.refresh()
	w.must(err)
	byKey := map[string]Primitive{}
	for _, p := range ps {
		byKey[p.Key] = p
	}
	if n := byKey["Notary"]; n.Address != notary.Hex() || n.Known {
		t.Fatalf("new primitive: %+v", n)
	}
	if ch2.dep.Contracts["Bank"] != bankV2.Hex() { // a fresh client starts on the current versions
		t.Fatal("a new client should use Bank v2")
	}
}

// TestNewVersionIsAnnouncedNotSwitched: clients keep the version they use,
// are told about the new one, and switch only when their owner says so.
func TestNewVersionIsAnnouncedNotSwitched(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b := f["ana"], f["ben"]
	w.must(a.lockVotes(coins(30)))
	w.must(a.deposit(coins(5))) // money in Bank v1
	if _, err := a.pollEvents(nil, false); err != nil {
		t.Fatal(err)
	}
	oldBank := w.ch.dep.Contracts["Bank"]
	bankV2 := deployNew(w, "Bank", common.HexToAddress(w.ch.dep.Contracts["Registry"]), common.HexToAddress(w.ch.dep.Contracts["Inbox"]))
	doc := writeDoc(t, "spec.md", "Bank v2")
	p, err := b.govProposeUpgrade("Bank", bankV2, doc, "", doc, "", "withdraw from v1, deposit into v2")
	w.must(err)
	w.pass(p, []*Client{a}, []*Client{a, b})
	evs, err := a.pollEvents(nil, false)
	w.must(err)
	seen := map[string]string{}
	for _, e := range evs {
		seen[e.Type] = e.Text
	}
	for _, want := range []string{"proposal-opened", "proposal-executed", "new-version"} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("no %s event in %v", want, seen)
		}
	}
	if !strings.Contains(seen["new-version"], "Bank v2 is current") || !strings.Contains(seen["new-version"], "you use v1") {
		t.Fatalf("new-version: %s", seen["new-version"])
	}
	// The client still uses v1, where its money is; v1 keeps working.
	ps, err := w.ch.refresh()
	w.must(err)
	for _, p := range ps {
		if p.Key == "Bank" && (p.Address != bankV2.Hex() || p.Using != oldBank) {
			t.Fatalf("Bank: %+v", p)
		}
	}
	if w.ch.dep.Contracts["Bank"] != oldBank {
		t.Fatal("the client switched by itself")
	}
	if bal, _ := a.bankBalance("ana"); bal.Cmp(coins(5)) != 0 {
		t.Fatalf("balance in v1: %s", bal)
	}
	w.must(errOf(a.ch.send(a.key, nil, "Bank", "withdraw", a.id(), a.addr(), coins(5))))
	ups, err := a.govUpdates(false)
	w.must(err)
	if len(ups) != 1 || ups[0]["key"] != "Bank" {
		t.Fatalf("updates: %v", ups)
	}
	// The owner moves on.
	w.must(errOf(a.govUse("Bank", "")))
	if w.ch.dep.Contracts["Bank"] != bankV2.Hex() {
		t.Fatal("gov use did not switch")
	}
	w.must(a.deposit(coins(1)))
	out, err := w.ch.call("Bank", "balance", a.id())
	w.must(err)
	if out[0].(*big.Int).Cmp(coins(1)) != 0 || w.ch.c["Bank"].Address() != bankV2 {
		t.Fatal("deposit in v2")
	}
	// Pinned in the identity folder: a fresh client of the same identity keeps v2.
	ch3, _ := newChain(w.sim.Client(), Deployment{Contracts: map[string]string{"Directory": w.ch.dep.Contracts["Directory"], "Bank": oldBank}}, nil)
	a2, err := openClient(a.dir, ch3)
	w.must(err)
	if a2.ch.dep.Contracts["Bank"] != bankV2.Hex() {
		t.Fatal("the pin did not survive")
	}
}

// TestParamChangeByBothHouses: parameters go the same way (here the citizens' rules).
func TestParamChangeByBothHouses(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b := f["ana"], f["ben"]
	w.must(a.lockVotes(coins(30)))
	w.later(2 * time.Second)
	_, err := a.govProposeParam("Directory", "set", []string{"Bank", "0x0000000000000000000000000000000000000001", "x", "y"}, "")
	w.mustFail(err, "target") // upgrades go as upgrades, with a spec and an audit
	p, err := a.govProposeParam("Citizens", "setParams", []string{`{"vouches":2,"maxVouches":5,"quorumBps":2000,"revokeBps":6667,"minDeals":0,` +
		`"minAge":600,"voteWindow":600,"penalty":3600,"term":86400,"deals":"0x0000000000000000000000000000000000000000"}`}, "two vouches are enough for now")
	w.must(err)
	show, err := a.govShow(p)
	w.must(err)
	if call := show["call"].(map[string]any); call["target"] != "Citizens" || !strings.HasPrefix(call["method"].(string), "setParams") {
		t.Fatalf("show: %v", show)
	}
	w.pass(p, []*Client{a}, []*Client{a, b})
	cp, err := a.citizenParams()
	w.must(err)
	if cp.Vouches != 2 {
		t.Fatalf("params: %+v", cp)
	}
}

// TestGrantIntoEscrow: treasury money only by both houses, and into a lock
// that reviewers decide, not outright.
func TestGrantIntoEscrow(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	a, b := f["ana"], f["ben"]
	dev, _ := w.owner("dev")
	r1, _ := w.owner("rev1")
	r2, _ := w.owner("rev2")
	r3, _ := w.owner("rev3")
	treasury := common.HexToAddress(w.ch.dep.Contracts["Treasury"])
	opts, _ := w.ch.opts(w.faucet, coins(50)) // fees and donations land here (a plain transfer with gas for its receive)
	tx, err := w.ch.c["Treasury"].Transfer(opts)
	w.must(err)
	w.must(errOf(w.ch.wait(tx)))
	if inbox := mustCall(t, w.ch, "Inbox", "treasury").(common.Address); inbox != treasury {
		t.Fatalf("fees go to %s, not the treasury", inbox.Hex())
	}
	w.must(a.lockVotes(coins(30)))
	w.later(2 * time.Second)
	_, err = w.ch.send(w.faucet, nil, "Treasury", "grant", u(0), idOf("dev"), coins(5), uint64(time.Now().Unix()+1e6),
		common.HexToAddress(w.ch.dep.Contracts["PanelCondition"]), []byte{}, "")
	w.mustFail(err, "governance only")
	p, _, err := a.govProposeGrant("dev", coins(5), []string{"rev1", "rev2", "rev3"}, 2, 1, "a Notary primitive with tests and a spec")
	w.must(err)
	before, _ := w.ch.balance(treasury)
	w.pass(p, []*Client{a}, []*Client{a, b})
	show, err := a.govShow(p)
	w.must(err)
	g := show["grant"].(map[string]any)
	lock := bigID(g["lock"].(string)).Uint64()
	l, err := w.ch.call("Bank", "locks", u(lock))
	w.must(err)
	if common.Hash(l[0].([32]byte)) != idOf("tern-treasury") || common.Hash(l[2].([32]byte)) != idOf("dev") || l[3].(*big.Int).Cmp(coins(5)) != 0 {
		t.Fatalf("lock: %v", l)
	}
	if bal, _ := w.ch.balance(treasury); new(big.Int).Sub(before, bal).Cmp(coins(5)) != 0 {
		t.Fatalf("treasury: %s before, %s after", fmtCoins(before), fmtCoins(bal))
	}
	// The work is done; two of three reviewers rule; the money moves.
	w.later(25 * time.Hour)
	for _, r := range []*Client{r1, r2} {
		w.must(errOf(r.panelRule(lock, 10000, "delivered and reviewed")))
	}
	_ = r3
	w.must(dev.panelSettleAuto(lock))
	if bal, _ := dev.bankBalance("dev"); bal.Cmp(coins(5)) != 0 {
		t.Fatalf("dev got %s", fmtCoins(bal))
	}
}

// TestDeployerHasNoPowers: after deployment the deployer holds nothing: no
// role in the timelock, no Directory, no setters; and the coin house alone
// can't act through its own Governor.
func TestDeployerHasNoPowers(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	me := crypto.PubkeyToAddress(w.faucet.PublicKey)
	tl := common.HexToAddress(w.ch.dep.Contracts["TernTimelock"])
	for _, role := range []string{"DEFAULT_ADMIN_ROLE", "PROPOSER_ROLE", "CANCELLER_ROLE"} {
		r := mustCall(t, w.ch, "TernTimelock", role).([32]byte)
		if mustCall(t, w.ch, "TernTimelock", "hasRole", r, me).(bool) {
			t.Fatalf("the deployer holds %s", role)
		}
	}
	if mustCall(t, w.ch, "Directory", "owner").(common.Address) != tl || mustCall(t, w.ch, "FeeOracle", "governance").(common.Address) != tl ||
		mustCall(t, w.ch, "Citizens", "governance").(common.Address) != tl || mustCall(t, w.ch, "Treasury", "governance").(common.Address) != tl {
		t.Fatal("something is not owned by the timelock")
	}
	zero := common.Address{}
	tries := []struct {
		contract, method string
		args             []any
	}{
		{"Directory", "set", []any{"Bank", zero, [32]byte{}, ""}},
		{"Directory", "publish", []any{"Bank", zero, [32]byte{}, "", [32]byte{}, "", ""}},
		{"Directory", "transferOwnership", []any{me}},
		{"TernTimelock", "schedule", []any{zero, big.NewInt(0), []byte{}, [32]byte{}, [32]byte{}, big.NewInt(600)}},
		{"TernTimelock", "grantRole", []any{[32]byte{}, me}},
		{"TernTimelock", "updateDelay", []any{big.NewInt(0)}},
		{"Inbox", "setBank", []any{me}},
		{"FeeOracle", "setParams", []any{big.NewInt(0), uint16(1), big.NewInt(0), uint16(0), uint16(0), uint64(0), uint16(1)}},
		{"FeeOracle", "setRoundLength", []any{uint64(2)}},
		{"Citizens", "setSources", []any{[]common.Address{}, []uint16{}, uint16(1)}},
		{"Citizens", "setParams", []any{citizenParams{Vouches: 1, MaxVouches: 1, RevokeBps: 6000, VoteWindow: 1, Term: 1}}},
		{"Parliament", "setSettings", []any{big.NewInt(0), uint16(1), uint64(1)}},
		{"TernGovernor", "setVotingPeriod", []any{uint32(1)}},
		{"TernGovernor", "setQuorumFloor", []any{big.NewInt(0)}},
		{"Treasury", "grant", []any{big.NewInt(0), idOf("x"), big.NewInt(1), uint64(1 << 40), me, []byte{}, ""}},
	}
	for _, x := range tries {
		if _, err := w.ch.send(w.faucet, nil, x.contract, x.method, x.args...); err == nil {
			t.Fatalf("the deployer could still call %s.%s", x.contract, x.method)
		}
	}
	// Nobody can act through the coin house alone: a Governor proposal that
	// would rewrite the Directory passes there and changes nothing.
	a := f["ana"]
	w.must(a.lockVotes(coins(30)))
	w.later(2 * time.Second)
	data, _ := w.ch.abis["Directory"].Pack("set", "Bank", zero, [32]byte{}, "")
	dir := common.HexToAddress(w.ch.dep.Contracts["Directory"])
	r, err := a.ch.send(a.key, nil, "TernGovernor", "propose", []common.Address{dir}, []*big.Int{big.NewInt(0)}, [][]byte{data}, "take the Bank")
	w.must(err)
	ev := w.ch.abis["TernGovernor"].Events["ProposalCreated"]
	var gid *big.Int
	for _, l := range r.Logs {
		if l.Topics[0] == ev.ID {
			v, _ := ev.Inputs.NonIndexed().Unpack(l.Data)
			gid = v[0].(*big.Int)
		}
	}
	w.later(2 * time.Second)
	w.must(errOf(a.ch.send(a.key, nil, "TernGovernor", "castVote", gid, uint8(1))))
	w.later(11 * time.Minute)
	_, err = a.ch.send(a.key, nil, "TernGovernor", "execute", []common.Address{dir}, []*big.Int{big.NewInt(0)}, [][]byte{data},
		crypto.Keccak256Hash([]byte("take the Bank")))
	w.must(err) // it "executes": counting only
	if cur, _ := a.dirCurrent("Bank"); cur == zero {
		t.Fatal("the coin house alone changed the Directory")
	}
}

// TestGovCLI: the commands, as an owner types them.
func TestGovCLI(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	testChain = w.ch
	defer func() { testChain = nil }()
	os.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	dir := func(n string) string { return f[n].dir }
	gov := func(who string, args ...string) {
		t.Helper()
		if err := run(append(append([]string{"gov"}, args...), "--dir", dir(who), "--relay", w.relay)); err != nil { // secret votes: through a relayer
			t.Fatalf("gov %v: %v", args, err)
		}
	}
	w.owner("eve")
	gov("ana", "lock", "30")
	gov("ana", "citizens")
	gov("ana", "vouch", "eve")
	gov("ben", "vouch", "eve")
	gov("cai", "vouch", "eve")
	w.later(11 * time.Minute)
	gov("dov", "admit", "eve")
	if !f["ana"].isCitizen("eve") {
		t.Fatal("eve")
	}
	v2 := deployNew(w, "Bank", common.HexToAddress(w.ch.dep.Contracts["Registry"]), common.HexToAddress(w.ch.dep.Contracts["Inbox"]))
	doc := writeDoc(t, "spec.md", "Bank v2")
	gov("ana", "propose", "upgrade", "Bank", v2.Hex(), "--spec", doc, "--audit", "ipfs://bafy-audit", "--audit-hash",
		crypto.Keccak256Hash([]byte("audit")).Hex(), "--notes", "withdraw from v1, deposit to v2")
	gov("ana", "propose", "param", "FeeOracle", "setTarget", "envelope", "12", "--why", "envelopes cost a bit more")
	gov("ben", "propose", "grant", "eve", "1", "--reviewers", "cai,dov", "--k", "2", "--days", "3", "--why", "write the Notary spec")
	w.later(2 * time.Second)
	for _, id := range []string{"0", "1"} {
		gov("ana", "vote", id, "yes")
		gov("ben", "vote", id, "yes", "--house", "citizens")
		gov("cai", "vote", id, "yes", "--house", "citizens") // 5 citizens: a turnout of 3 at least (audit 4 GV-6)
	}
	gov("ana", "show", "0")
	gov("ana", "proposals")
	w.later(11 * time.Minute)
	gov("ana", "queue", "0")
	gov("ana", "queue", "1")
	w.later(11 * time.Minute)
	gov("ben", "execute", "0")
	w.later(20 * time.Minute) // a FeeOracle param is governance-critical: 3 exit windows (audit 6 FO-3)
	gov("ben", "execute", "1")
	if err := run([]string{"gov", "queue", "2", "--dir", dir("ana")}); err == nil || !strings.Contains(err.Error(), "defeated") {
		t.Fatalf("an unvoted grant queued: %v", err)
	}
	gov("ana", "versions", "Bank")
	gov("ana", "versions")
	gov("ana", "updates")
	gov("ana", "use", "Bank", "v2")
	if ppm := mustCall(t, w.ch, "FeeOracle", "target", envelopeKind).(*big.Int); ppm.Int64() != 12 {
		t.Fatalf("FeeOracle target: %s", ppm)
	}
	gov("dov", "revoke", "eve", "--sybil", "a duplicate of someone")
	gov("dov", "revoke-vote", "0", "no") // ben vouched for eve: not his vote (audit 4 GV-1)
}
