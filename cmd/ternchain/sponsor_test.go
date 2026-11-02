package main

import (
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// keyOwner: a name held by an account guarded by the owner's own key (what
// owner init makes on a network whose paymaster pays accounts' gas).
func (w *world) keyOwner(name string) *Client {
	w.t.Helper()
	g, err := newIdentDir(filepath.Join(w.dir, "guardian-"+name), IdentityFile{Name: name, Kind: "guardian"})
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := newIdentDir(filepath.Join(w.dir, name), IdentityFile{Name: name, Kind: "owner", Owner: name}); err != nil {
		w.t.Fatal(err)
	}
	c, err := openClient(filepath.Join(w.dir, name), w.ch)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := c.enableKeyAccount(); err != nil {
		w.t.Fatal(err)
	}
	if err := c.registerOwner(crypto.PubkeyToAddress(g.key.PublicKey)); err != nil {
		w.t.Fatal(err)
	}
	return c
}

// Base Sepolia's set-up: the canonical EntryPoint, a remote bundler, USDC
// settlement and the network's paymaster. Nobody but the operator and the
// bundler holds ETH: owners and agents take names, deposit, grant and pay
// with their accounts, and the paymaster stops a name at its daily cap.
func TestSponsoredAccountsNeedNoETH(t *testing.T) {
	perName := coins(0.05)
	w, _ := newHostedWorldWith(t, func(p *Params) {
		p.Sponsor = &SponsorParams{PerName: perName, PerDay: coins(10), Deposit: coins(5)}
	})
	if !w.ch.has("SponsorPaymaster") {
		t.Fatal("no SponsorPaymaster deployed")
	}
	if b, _ := w.ch.call("SponsorPaymaster", "balance"); b[0].(*big.Int).Cmp(coins(5)) != 0 {
		t.Fatalf("the paymaster's deposit: %v", b[0])
	}
	usdc := common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])
	w.ch.settle = &Settlement{Token: usdc.Hex(), Symbol: "USDC", Decimals: 6}
	alice := w.keyOwner("alice")
	bob := w.keyOwner("bob")
	alice.asset, bob.asset = usdc, usdc
	if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", alice.me(), big.NewInt(20_000_000)); err != nil {
		t.Fatal(err)
	}
	ten, _ := alice.parseMoney("10")
	if err := alice.deposit(ten); err != nil { // approve + deposit, both from her account, gas sponsored
		t.Fatal(err)
	}
	three, _ := alice.parseMoney("3")
	bot := w.accountAgent(alice, "bot", three, nil)
	bot.asset = usdc
	half, _ := bot.parseMoney("0.5")
	if err := bot.pay("bob", half, "through the sponsored account"); err != nil {
		t.Fatal(err)
	}
	if b, _ := bob.bankBalance("bob"); b.Cmp(half) != 0 {
		t.Fatalf("bob's bank: %v", b)
	}
	for _, a := range []common.Address{alice.addr(), alice.me(), bob.addr(), bob.me(), bot.addr(), bot.me()} {
		if b, _ := w.ch.balance(a); b.Sign() != 0 {
			t.Fatalf("%s holds %v wei: nobody needs ETH here", a.Hex(), b)
		}
	}
	if l, err := w.ch.call("SponsorPaymaster", "left", idOf("bot.alice")); err != nil || l[0].(*big.Int).Cmp(perName) >= 0 {
		t.Fatalf("the bot's sponsored gas today: %v %v", l, err)
	}
	// The operator lowers the cap below one operation: the bot is stopped.
	if _, err := w.ch.send(w.faucet, nil, "SponsorPaymaster", "setCaps", big.NewInt(1), coins(10)); err != nil {
		t.Fatal(err)
	}
	if err := bot.pay("bob", half, "over the cap"); err == nil || !strings.Contains(err.Error(), "sponsored gas") {
		t.Fatalf("the daily cap: %v", err)
	}
	// Only the operator sets caps.
	if _, err := alice.ch.send(alice.key, nil, "SponsorPaymaster", "setCaps", coins(1), coins(1)); err == nil {
		t.Fatal("a tester set the paymaster's caps")
	}
}

// A client that connects with net.json (not the deployer's own Chain) sees
// the contracts/ext contracts it names.
func TestNetFileBindsExtContracts(t *testing.T) {
	w, _ := newHostedWorldWith(t, func(p *Params) {
		p.Sponsor = &SponsorParams{PerName: coins(1), PerDay: coins(1), Deposit: coins(1)}
	})
	ch, err := newChain(w.sim.Client(), w.ch.dep, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"SponsorPaymaster", "TestUSDC", "Bank"} {
		if !ch.has(n) {
			t.Fatalf("%s not bound from net.json's contracts", n)
		}
	}
	if v, err := ch.call("SponsorPaymaster", "perName"); err != nil || v[0].(*big.Int).Cmp(coins(1)) != 0 {
		t.Fatalf("SponsorPaymaster.perName: %v %v", v, err)
	}
}

// A smart account registers as an ERC-8004 agent: the registry mints with
// _safeMint, which an account (no onERC721Received) refuses; the registrar
// takes the id and hands it over. (Found on Base Sepolia's registry.)
func TestERC8004FromAccount(t *testing.T) {
	w, _ := newHostedWorldWith(t, func(p *Params) {
		p.Sponsor = &SponsorParams{PerName: coins(1), PerDay: coins(10), Deposit: coins(5)}
	})
	bob := w.keyOwner("bob")
	alice := w.keyOwner("alice")
	if _, err := bob.ch.send(bob.key, nil, "ERC8004IdentityRegistry", "register1", "x"); err == nil || !strings.Contains(err.Error(), "ERC721InvalidReceiver") {
		t.Fatalf("an account took a safe-minted id directly: %v", err)
	}
	r, err := bob.erc8004Register("logos", false)
	if err != nil {
		t.Fatal(err)
	}
	id, err := alice.erc8004AgentOf("bob")
	if err != nil || id.String() != r["agent_id"] {
		t.Fatalf("bob's agent id %v %v, registered %v", id, err, r["agent_id"])
	}
	if o, _ := w.ch.call("ERC8004IdentityRegistry", "ownerOf", id); o[0].(common.Address) != bob.me() {
		t.Fatalf("the id is held by %v, not bob's account", o[0])
	}
}
