package main

import (
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// An agent pays an x402 endpoint that a ternchain service runs: 402, an
// EIP-3009 authorization signed by the agent's key, settled by the service;
// --max-pay, the owner's per-epoch token limit and scopes.to hold.
func TestX402PayAndServe(t *testing.T) {
	w := newWorld(t)
	usdc := common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])
	usd := func(s string) *big.Int { v, _ := parseUnits(s, 6); return v }
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	alice.asset, bob.asset = usdc, usdc
	if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", alice.addr(), usd("50")); err != nil {
		t.Fatal(err)
	}
	if err := alice.deposit(usd("20")); err != nil {
		t.Fatal(err)
	}
	bot := w.agent(alice, "bot", coins(1), `{"free_pay":"5"}`)
	bot.asset = usdc
	if err := alice.setTokenLimit("bot.alice", usd("3")); err != nil {
		t.Fatal(err)
	}
	if err := alice.x402Fund("bot.alice", usd("6")); err != nil {
		t.Fatal(err)
	}
	balance := func(a common.Address) *big.Int {
		erc20, _ := erc20ABI()
		data, _ := erc20.Pack("balanceOf", a)
		out, err := w.ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &usdc, Data: data}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return new(big.Int).SetBytes(out)
	}
	if b := balance(bot.addr()); b.Cmp(usd("6")) != 0 {
		t.Fatalf("the bot's key holds %v", b)
	}
	serve := func(price string) *httptest.Server {
		s := &x402Server{c: bob, price: usd(price), token: usdc, payTo: bob.me(), desc: "weather",
			content: func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("sunny")) }}
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		return srv
	}
	cheap := serve("0.25")
	if resp, _ := http.Get(cheap.URL); resp.StatusCode != http.StatusPaymentRequired || resp.Header.Get("PAYMENT-REQUIRED") == "" {
		t.Fatalf("no 402 with requirements: %v", resp.Status)
	}
	resp, body, info, err := bot.x402Pay(cheap.URL+"/today", http.MethodGet, nil, usd("1"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(body) != "sunny" || info["paid"] != "0.250000 USDC" {
		t.Fatalf("paid request: %v %q %v", resp.Status, body, info)
	}
	if st, ok := info["settlement"].(x402Settlement); !ok || !st.Success || st.Transaction == "" {
		t.Fatalf("settlement: %v", info["settlement"])
	}
	if b := balance(bob.me()); b.Cmp(usd("0.25")) != 0 {
		t.Fatalf("bob got %v", b)
	}
	if _, _, _, err := bot.x402Pay(cheap.URL, http.MethodGet, nil, usd("0.1")); err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("--max-pay: %v", err)
	}
	// The owner's limit (3 USDC an epoch) counts x402 payments: 0.25 + 2 fits, another 2 doesn't.
	dear := serve("2")
	if _, _, _, err := bot.x402Pay(dear.URL, http.MethodGet, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := bot.x402Pay(dear.URL, http.MethodGet, nil, nil); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over the token limit: %v", err)
	}
	// scopes.to: an agent allowed to pay only bob can't pay an x402 host.
	narrow := w.agent(alice, "narrow", coins(1), `{"to":["bob"]}`)
	narrow.asset = usdc
	if _, _, _, err := narrow.x402Pay(cheap.URL, http.MethodGet, nil, nil); err == nil || !strings.Contains(err.Error(), "scopes.to") {
		t.Fatalf("scopes.to: %v", err)
	}
}

// The EIP-712 domain the client signs under is Circle's USDC on Base Sepolia's
// (its DOMAIN_SEPARATOR(), read on chain 2026-10-28).
func TestEIP3009DomainBaseSepoliaUSDC(t *testing.T) {
	d := eip712DomainSeparator("USDC", "2", big.NewInt(baseSepoliaChainID), common.HexToAddress(baseSepoliaUSDC.Token))
	if d.Hex() != "0x71f17a3b2ff373b803d70a5a07c046c1a2bc8e89c09ef722fcb047abe94c9818" {
		t.Fatalf("domain separator %s", d.Hex())
	}
}
