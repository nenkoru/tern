package main

import (
	"fmt"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestParseUnits(t *testing.T) {
	for _, c := range []struct {
		s    string
		dec  uint8
		want string
		bad  bool
	}{{"0.5", 6, "500000", false}, {"20", 6, "20000000", false}, {"0.0000001", 6, "", true}, {"1.25", 18, "1250000000000000000", false}, {"-1", 6, "", true}} {
		v, err := parseUnits(c.s, c.dec)
		if (err != nil) != c.bad || (!c.bad && v.String() != c.want) {
			t.Fatalf("parseUnits(%q, %d) = %v, %v", c.s, c.dec, v, err)
		}
	}
	if s := fmtUnits(big.NewInt(1_500_000), 6); s != "1.500000" {
		t.Fatalf("fmtUnits: %s", s)
	}
	if s := fmtCoins(new(big.Int).Mul(big.NewInt(3), coinUnit)); s != "3.000000" { // the coin as before
		t.Fatalf("fmtCoins: %s", s)
	}
}

// A network that settles in USDC (6 decimals): deposits, an agent's limit,
// payments and ratings in USDC units; the coin stays for gas.
func TestUSDCSettlement(t *testing.T) {
	w := newWorld(t)
	usdc := common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])
	if usdc == (common.Address{}) {
		t.Fatal("no TestUSDC on the devnet")
	}
	w.ch.settle = &Settlement{Token: usdc.Hex(), Symbol: "USDC", Decimals: 6} // net.json "settle"
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	if a, err := alice.tokenAddr(""); err != nil || a != usdc { // no --token: the network's settlement token
		t.Fatalf("default asset %v %v", a, err)
	}
	if a, _ := alice.tokenAddr("coin"); a != (common.Address{}) {
		t.Fatal("--token coin is the network's coin")
	}
	alice.asset, bob.asset = usdc, usdc
	if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", alice.addr(), big.NewInt(100_000_000)); err != nil {
		t.Fatal(err)
	}
	fifty, err := alice.parseMoney("50")
	if err != nil || fifty.Cmp(big.NewInt(50_000_000)) != 0 {
		t.Fatalf("50 USDC = %v %v", fifty, err)
	}
	if err := alice.deposit(fifty); err != nil {
		t.Fatal(err)
	}
	if b, _ := alice.bankBalance("alice"); alice.fmtMoney(b) != "50.000000" {
		t.Fatalf("alice's bank: %v", b)
	}
	three, _ := alice.parseMoney("3")
	bot := w.agent(alice, "bot", three, `{}`)
	bot.asset = usdc
	if l, err := w.ch.call("Bank", "tokenLimit", idOf("bot.alice"), usdc); err != nil || l[0].(*big.Int).Cmp(three) != 0 {
		t.Fatalf("the agent's USDC limit: %v %v", l, err)
	}
	half, _ := bot.parseMoney("0.5")
	r, err := bot.payReceipt("bob", half, "coffee", false)
	if err != nil {
		t.Fatal(err)
	}
	if r["paid"] != "0.500000 USDC" {
		t.Fatalf("receipt: %v", r)
	}
	if b, _ := bob.bankBalance("bob"); b.Cmp(half) != 0 {
		t.Fatalf("bob's bank: %v", b)
	}
	if err := bot.pay("bob", three, "over the limit"); err == nil || !strings.Contains(err.Error(), "3.000000 USDC per epoch") {
		t.Fatalf("the USDC limit: %v", err)
	}
	// Ratings weigh a deal by whole units of its token.
	if a := bot.dealAmount(&dealLock{Paid: big.NewInt(1_500_000), Token: usdc}); a != 1.5 {
		t.Fatalf("a 1.5 USDC deal weighs %v", a)
	}
}

// A relay on a network that settles in USDC charges senders their postage in
// USDC by voucher (not in the coin, and the operator pays nothing for them):
// a channel in USDC is accepted, one in the coin refused.
func TestRelayChargesInUSDC(t *testing.T) {
	w := newWorld(t)
	usdc := common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])
	w.ch.settle = &Settlement{Token: usdc.Hex(), Symbol: "USDC", Decimals: 6}
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	nodeco, _ := w.owner("nodeco")
	bob.setContact("alice", true)
	alice.asset, nodeco.asset = usdc, usdc
	if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", alice.addr(), big.NewInt(10_000_000)); err != nil {
		t.Fatal(err)
	}
	if err := alice.deposit(big.NewInt(5_000_000)); err != nil {
		t.Fatal(err)
	}
	price, _ := parseUnits("0.001", 6)
	r := &Relay{ch: w.ch, key: nodeco.key, name: "nodeco", price: price, token: usdc}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	if ni, err := nodeInfo(srv.URL); err != nil || ni.Token != usdc.Hex() {
		t.Fatalf("node info: %+v %v", ni, err)
	}
	alice.relay = srv.URL
	for i := 0; i < 3; i++ {
		if _, err := alice.send(SendOpts{To: "bob", Text: "postage in USDC"}); err != nil {
			t.Fatal(err)
		}
	}
	ch := alice.st.Channels[srv.URL]
	if ch.Cumulative != "3000" { // 3 x 0.001 USDC
		t.Fatalf("vouchers: %+v", ch)
	}
	l, _ := w.ch.call("Bank", "locks", new(big.Int).SetUint64(ch.Lock))
	if l[9].(common.Address) != usdc {
		t.Fatalf("the channel is in %v, not USDC", l[9])
	}
	// A sender paying in the coin is refused: the node takes USDC.
	carol, _ := w.owner("carol")
	carol.deposit(coins(1))
	coinPrice := &Relay{ch: w.ch, key: nodeco.key, name: "nodeco", price: price}
	csrv := httptest.NewServer(coinPrice.Handler())
	defer csrv.Close()
	carol.relay = csrv.URL
	if _, err := carol.send(SendOpts{To: "bob", Text: "coin channel"}); err != nil {
		t.Fatal(err) // a coin node takes coin channels
	}
	cl := carol.st.Channels[csrv.URL].Lock
	if err := r.acceptVoucher(fmt.Sprint(cl), "1000", strings.Repeat("00", 65)); err == nil || !strings.Contains(err.Error(), "pays in coins") {
		t.Fatalf("a coin channel at a USDC node: %v", err)
	}
}
