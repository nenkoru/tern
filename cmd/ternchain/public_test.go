package main

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The public profile: what it ships and what it leaves out, nobody (the
// deployer least of all) holding a role, mail on chain with no coin, and the
// commands of modules it lacks refused.
func TestPublicProfile(t *testing.T) {
	w, _ := newHostedWorldWith(t, func(p *Params) { p.Profile = "public"; p.GasPaymaster = "test" })
	for _, n := range []string{"PrivacyPool", "AssociationSets", "PoolVerifier", "StealthPay", "ERC5564Announcer", "ERC6538Registry", "WrappedTern", "TernGovernor"} {
		if w.ch.has(n) {
			t.Fatalf("the public profile deployed %s", n)
		}
	}
	for _, n := range []string{"Random", "Clock", "Bank", "Inbox", "Parliament", "Citizens", "NoCoinHouse", "InboxForwarder", "ModuleRegistry", "TernJobs"} {
		if !w.ch.has(n) {
			t.Fatalf("the public profile lacks %s", n)
		}
	}
	// Roles: the deployer holds none; every governed contract is the timelock's,
	// whose only proposer is Parliament (and with no citizens, nothing passes).
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
	if n := mustCall(t, w.ch, "Citizens", "count").(uint32); n != 0 {
		t.Fatalf("%d citizens: the public profile starts with none", n)
	}
	zero := common.Address{}
	for _, x := range []struct {
		contract, method string
		args             []any
	}{
		{"Directory", "set", []any{"Bank", zero, [32]byte{}, ""}},
		{"Directory", "transferOwnership", []any{me}},
		{"TernTimelock", "grantRole", []any{[32]byte{}, me}},
		{"Inbox", "setBank", []any{me}},
		{"FeeOracle", "setRoundLength", []any{uint64(2)}},
		{"Treasury", "grant", []any{big.NewInt(0), idOf("x"), big.NewInt(1), uint64(1 << 40), me, []byte{}, ""}},
	} {
		if _, err := w.ch.send(w.faucet, nil, x.contract, x.method, x.args...); err == nil {
			t.Fatalf("the deployer could still call %s.%s", x.contract, x.method)
		}
	}
	rows, facts, err := w.ch.roleAudit(me)
	if err != nil || facts["deployer_roles"] != "0" || facts["citizens"] != uint32(0) {
		t.Fatalf("role audit: %v %v %+v", facts, err, rows)
	}
	// Mail on chain from accounts that hold no coin: the forwarder pays the fee's floor.
	usdc := common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])
	w.ch.settle = &Settlement{Token: usdc.Hex(), Symbol: "USDC", Decimals: 6}
	w.ch.gas = &GasConfig{Pays: "user", Paymaster: w.ch.dep.Contracts["TestTokenPaymaster"], Token: usdc.Hex()}
	mintTo := func(a common.Address) {
		if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", a, big.NewInt(5_000_000)); err != nil {
			t.Fatal(err)
		}
	}
	alice, bob := w.usdcOwner("alice", mintTo), w.usdcOwner("bob", mintTo)
	bob.setContact("alice", true)
	if _, err := alice.send(SendOpts{To: "bob", Text: "on chain, no coin, no relay"}); err != nil {
		t.Fatal(err)
	}
	if got := mailTo(scan(t, bob), tMessage); len(got) != 1 {
		t.Fatalf("bob got %d letters", len(got))
	}
	if b, _ := w.ch.balance(alice.me()); b.Sign() != 0 {
		t.Fatal("alice's account holds coin")
	}
	// An owner gives its agent's account USDC for gas (owner grant --gas).
	dir := w.dir + "/bot.alice"
	newIdentDir(dir, IdentityFile{Name: "bot.alice", Kind: "agent", Owner: "alice"})
	bot, err := openClient(dir, w.ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.enableAgentAccount(); err != nil {
		t.Fatal(err)
	}
	b, err := alice.grant(AgentRequest{Name: "bot", Owner: "alice", Address: bot.me().Hex(), Account: true}, 30, big.NewInt(1_000_000), `{}`, big.NewInt(500_000))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.Gas, "sent 0.5") {
		t.Fatalf("grant --gas: %q", b.Gas)
	}
	if err := bot.join(*b); err != nil { // its own gas, from what alice sent
		t.Fatal(err)
	}
	bob.setContact("bot.alice", true)
	if _, err := bot.send(SendOpts{To: "bob", Text: "an agent's letter, on chain"}); err != nil {
		t.Fatal(err)
	}
	// What the profile lacks, the client doesn't offer.
	pf := &DevnetFile{Profile: "public"}
	for _, cmd := range []string{"private send", "private pay", "asp list", "pot open", "fiat confirm", "gov lock", "commons fee"} {
		if err := netModuleRefusal(cmd, w.ch, pf); err == nil || !strings.Contains(err.Error(), "isn't on this network") {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	for _, cmd := range []string{"random draw", "clock show", "send", "job create"} {
		if err := netModuleRefusal(cmd, w.ch, pf); err != nil {
			t.Fatalf("%s refused: %v", cmd, err)
		}
	}
	// Anyone publishes a module in their own namespace; no gate, no endorsement.
	jobs := common.HexToAddress(w.ch.dep.Contracts["TernJobs"])
	if _, err := alice.ch.send(alice.key, nil, "ModuleRegistry", "publish", idOf("alice"), "jobs-copy", jobs, [32]byte{1}, "", "https://example.org/spec"); err != nil {
		t.Fatal(err)
	}
	if v := mustCall(t, w.ch, "ModuleRegistry", "versions", idOf("alice"), "jobs-copy").(*big.Int); v.Int64() != 1 {
		t.Fatalf("versions %v", v)
	}
	if _, err := bob.ch.send(bob.key, nil, "ModuleRegistry", "publish", idOf("alice"), "squat", jobs, [32]byte{}, "", ""); err == nil {
		t.Fatal("bob published in alice's namespace")
	}
}

// usdcOwner: a name held by a key-guarded account that pays its gas in USDC.
func (w *world) usdcOwner(name string, mint func(common.Address)) *Client {
	w.t.Helper()
	g, _ := newIdentDir(w.dir+"/guardian-"+name, IdentityFile{Name: name, Kind: "guardian"})
	newIdentDir(w.dir+"/"+name, IdentityFile{Name: name, Kind: "owner", Owner: name})
	c, err := openClient(w.dir+"/"+name, w.ch)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := c.enableKeyAccount(); err != nil {
		w.t.Fatal(err)
	}
	mint(c.me())
	if err := c.registerOwner(crypto.PubkeyToAddress(g.key.PublicKey)); err != nil {
		w.t.Fatal(err)
	}
	return c
}
