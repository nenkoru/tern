package main

import (
	"math/big"
	"strings"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ERC-8183 jobs over Bank locks: funded from the client's bank (the agent's
// limit applies), paid on completion, refunded on rejection; a tender picks
// its provider later; a provider that is no ternchain name is paid plainly;
// a completed job is a paid deal for ratings.
func TestERC8183Jobs(t *testing.T) {
	w := newWorld(t)
	if !w.ch.has("TernJobs") {
		t.Fatal("no TernJobs on the devnet")
	}
	usdc := common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	for _, c := range []*Client{alice, bob, carol} {
		c.asset = usdc
	}
	mint := func(to common.Address, n int64) {
		t.Helper()
		if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", to, big.NewInt(n)); err != nil {
			t.Fatal(err)
		}
	}
	usd := func(s string) *big.Int { v, _ := parseUnits(s, 6); return v }
	mint(alice.addr(), 100_000_000)
	if err := alice.deposit(usd("50")); err != nil {
		t.Fatal(err)
	}
	// A job for bob, judged by carol: funded from alice's bank, paid on completion.
	r, err := alice.jobCreate("bob", "carol", usd("5"), 7, "a logo")
	if err != nil {
		t.Fatal(err)
	}
	id := r["job"].(uint64)
	f, err := alice.jobFund(id, usd("5"), "")
	if err != nil {
		t.Fatal(err)
	}
	lock := f["lock"].(uint64)
	if s, _ := alice.jobShow(id); s["status"] != "funded" || s["lock"] != lock {
		t.Fatalf("after fund: %v", s)
	}
	if _, err := alice.ch.send(alice.key, nil, "Bank", "release", new(big.Int).SetUint64(lock), usd("5")); err == nil {
		t.Fatal("the payer released a lock the job holds")
	}
	if _, err := carol.jobAct(id, "complete", "too early"); err == nil {
		t.Fatal("completed before a submission")
	}
	if _, err := bob.jobAct(id, "submit", "ipfs://logo-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.jobAct(id, "complete", "I like it"); err == nil || !strings.Contains(err.Error(), "evaluator only") {
		t.Fatalf("the provider completed its own job: %v", err)
	}
	if s, err := carol.jobAct(id, "complete", "good"); err != nil || s["status"] != "completed" {
		t.Fatalf("complete: %v %v", s, err)
	}
	if b, _ := bob.tokenBalance("bob"); b.Cmp(usd("5")) != 0 {
		t.Fatalf("bob was paid %v", b)
	}
	if _, err := alice.reviewData("bob", lock, "great logo"); err != nil { // a completed job is a paid deal
		t.Fatal(err)
	}
	// Rejected: the money goes back to alice's bank.
	r, _ = alice.jobCreate("bob", "carol", usd("4"), 7, "a second logo")
	id2 := r["job"].(uint64)
	if _, err := alice.jobFund(id2, usd("4"), ""); err != nil {
		t.Fatal(err)
	}
	before, _ := alice.tokenBalance("alice")
	if s, err := carol.jobAct(id2, "reject", "not what was asked"); err != nil || s["status"] != "rejected" {
		t.Fatalf("reject: %v %v", s, err)
	}
	if after, _ := alice.tokenBalance("alice"); new(big.Int).Sub(after, before).Cmp(usd("4")) != 0 {
		t.Fatalf("refund: %v -> %v", before, after)
	}
	// A tender: no provider until alice picks one.
	r, _ = alice.jobCreate("-", "carol", usd("1"), 7, "a tender")
	id3 := r["job"].(uint64)
	if _, err := alice.jobFund(id3, usd("1"), ""); err == nil {
		t.Fatal("funded a tender with no provider")
	}
	if err := alice.jobProvider(id3, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.jobFund(id3, usd("1"), ""); err != nil {
		t.Fatal(err)
	}
	// A provider that is no ternchain name: the standard's plain funding from alice's own tokens.
	k, _ := crypto.GenerateKey()
	outsider := crypto.PubkeyToAddress(k.PublicKey)
	r, err = alice.jobCreate(outsider.Hex(), "carol", usd("2"), 7, "outside work")
	if err != nil {
		t.Fatal(err)
	}
	id4 := r["job"].(uint64)
	if _, err := alice.jobFund(id4, usd("2"), ""); err != nil {
		t.Fatal(err)
	}
	if err := w.ch.transfer(w.faucet, outsider, coins(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ch.send(k, nil, "TernJobs", "submit", new(big.Int).SetUint64(id4), jobDeliverable("done"), []byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := carol.jobAct(id4, "complete", "ok"); err != nil {
		t.Fatal(err)
	}
	erc20, _ := erc20ABI()
	data, _ := erc20.Pack("balanceOf", outsider)
	if out, err := w.ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &usdc, Data: data}, nil); err != nil || new(big.Int).SetBytes(out).Cmp(usd("2")) != 0 {
		t.Fatalf("the outsider was paid %x %v", out, err)
	}
	// An agent's job is within the limit its owner set for the token.
	bot := w.agent(alice, "bot", coins(1), `{}`)
	bot.asset = usdc
	if err := alice.setTokenLimit("bot.alice", usd("3")); err != nil {
		t.Fatal(err)
	}
	r, err = bot.jobCreate("bob", "carol", usd("5"), 7, "too big for the bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bot.jobFund(r["job"].(uint64), usd("5"), ""); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the agent funded a job over its limit: %v", err)
	}
}
