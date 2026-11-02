package main

import (
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Users pay their own gas in USDC through an ERC-20 paymaster (Circle
// Paymaster's look-alike): an owner's first operation (making its account,
// taking the name) and an agent's payments are paid from their accounts'
// USDC; no key or account holds ETH, and the network pays nothing.
func TestUsersPayGasInUSDC(t *testing.T) {
	w, _ := newHostedWorldWith(t, func(p *Params) { p.GasPaymaster = "test" })
	usdc := common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])
	pm := w.ch.dep.Contracts["TestTokenPaymaster"]
	if pm == "" {
		t.Fatal("no TestTokenPaymaster")
	}
	w.ch.settle = &Settlement{Token: usdc.Hex(), Symbol: "USDC", Decimals: 6}
	w.ch.gas = &GasConfig{Pays: "user", Paymaster: pm, Token: usdc.Hex()}
	mint := func(to common.Address, n int64) {
		t.Helper()
		if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", to, big.NewInt(n)); err != nil {
			t.Fatal(err)
		}
	}
	owner := func(name string) *Client {
		t.Helper()
		g, _ := newIdentDir(filepath.Join(w.dir, "guardian-"+name), IdentityFile{Name: name, Kind: "guardian"})
		newIdentDir(filepath.Join(w.dir, name), IdentityFile{Name: name, Kind: "owner", Owner: name})
		c, err := openClient(filepath.Join(w.dir, name), w.ch)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.enableKeyAccount(); err != nil {
			t.Fatal(err)
		}
		if err := c.registerOwner(crypto.PubkeyToAddress(g.key.PublicKey)); err == nil {
			t.Fatal("an account with no USDC took a name: who paid its gas?")
		}
		mint(c.me(), 20_000_000)
		if err := c.registerOwner(crypto.PubkeyToAddress(g.key.PublicKey)); err != nil {
			t.Fatal(err)
		}
		c.asset = usdc
		return c
	}
	alice, bob := owner("alice"), owner("bob")
	before, _ := alice.tokenOf(usdc, alice.me())
	if err := alice.deposit(big.NewInt(10_000_000)); err != nil {
		t.Fatal(err)
	}
	after, _ := alice.tokenOf(usdc, alice.me())
	gas := new(big.Int).Sub(new(big.Int).Sub(before, after), big.NewInt(10_000_000))
	if gas.Sign() <= 0 || gas.Cmp(big.NewInt(100_000)) > 0 { // paid, and a sliver of a dollar
		t.Fatalf("alice paid %v USDC units of gas for a deposit", gas)
	}
	dir := filepath.Join(w.dir, "bot.alice")
	newIdentDir(dir, IdentityFile{Name: "bot.alice", Kind: "agent", Owner: "alice"})
	bot, err := openClient(dir, w.ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.enableAgentAccount(); err != nil {
		t.Fatal(err)
	}
	b, err := alice.grant(AgentRequest{Name: "bot", Owner: "alice", Address: bot.me().Hex(), Account: true}, 30, big.NewInt(3_000_000), `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.join(*b); err == nil || !strings.Contains(err.Error(), "pays its own gas in USDC") {
		t.Fatalf("an agent with no USDC of its own paid its gas: %v", err)
	}
	mint(bot.me(), 2_000_000)
	if err := bot.join(*b); err != nil {
		t.Fatal(err)
	}
	bot.asset = usdc
	if err := bot.pay("bob", big.NewInt(500_000), "gas in USDC"); err != nil {
		t.Fatal(err)
	}
	if b, _ := bob.bankBalance("bob"); b.Cmp(big.NewInt(500_000)) != 0 {
		t.Fatalf("bob's bank %v", b)
	}
	for _, a := range []common.Address{alice.addr(), alice.me(), bot.addr(), bot.me(), bob.addr(), bob.me()} {
		if b, _ := w.ch.balance(a); b.Sign() != 0 {
			t.Fatalf("%s holds %v wei", a.Hex(), b)
		}
	}
}
