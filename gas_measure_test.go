package main

import (
	"context"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"math/big"
	"strings"
	"testing"
)

// Gas per message: one envelope, and a batch of fifty (what a relayer or bundler posts).
func TestMeasureMessageGas(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	alice.setContact("bob", true)
	text := strings.Repeat("x", 200) // a short message
	var envs []Envelope
	capture = func(e Envelope) { envs = append(envs, e) }
	for i := 0; i < 21; i++ {
		if _, err := alice.send(SendOpts{To: "bob", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	capture = nil
	post := func(es []Envelope) uint64 {
		fee, _ := alice.ch.fee()
		r, err := alice.ch.send(alice.key, new(big.Int).Mul(fee, big.NewInt(int64(len(es)))), "Inbox", "post", encodeBatch(es), big.NewInt(int64(len(es))))
		if err != nil {
			t.Fatal(err)
		}
		return r.GasUsed
	}
	one := post(envs[:1])
	fifty := post(envs[1:])
	t.Logf("envelope %d bytes; gas: 1 alone %d; 20 in a batch %d (%d each)", len(envs[0].encode()), one, fifty, fifty/20)
}

// Gas of operations through smart accounts (a passkey owner, a session-key agent).
func TestMeasureAccountGas(t *testing.T) {
	w := newWorld(t)
	alice := w.passkeyOwner("alice")
	w.owner("bob")
	bot := w.accountAgent(alice, "bot", coins(1), exact("0.05"))
	last := func() uint64 { h, _ := w.ch.b.HeaderByNumber(context.Background(), nil); return h.GasUsed }
	alice.deposit(coins(1))
	g1 := last()
	bot.pay("bob", exact("0.01"), "x")
	g2 := last()
	t.Logf("passkey owner deposit: %d gas; session-key agent payment: %d gas", g1, g2)
}

// Gas of a post priced by the FeeOracle, against the same Inbox with a fixed fee only.
func TestMeasureOracleGas(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	alice.setContact("bob", true)
	var envs []Envelope
	capture = func(e Envelope) { envs = append(envs, e) }
	for i := 0; i < 44; i++ {
		if _, err := alice.send(SendOpts{To: "bob", Text: strings.Repeat("x", 200)}); err != nil {
			t.Fatal(err)
		}
	}
	capture = nil
	a, _ := loadArtifact("Inbox")
	opts, _ := w.ch.opts(w.faucet, nil)
	addr, tx, bc, err := bind.DeployContract(opts, a.abi, a.bin, w.ch.b, big.NewInt(1e12), w.faucetAddr(), coins(1), coins(0.1), uint64(3600), uint64(1800), common.Address{}) // no Directory: the fixed fee only
	if err != nil {
		t.Fatal(err)
	}
	w.ch.wait(tx)
	w.ch.c["InboxPlain"], w.ch.abis["InboxPlain"], w.ch.dep.Contracts["InboxPlain"] = bc, a.abi, addr.Hex()
	post := func(inbox string, es []Envelope) uint64 {
		total, _ := w.ch.feeFor(len(es))
		r, err := w.ch.send(alice.key, total, inbox, "post", encodeBatch(es), big.NewInt(int64(len(es))))
		if err != nil {
			t.Fatal(err)
		}
		return r.GasUsed
	}
	// The first post on each warms the counters; the second is the steady state.
	plainFirst, plain1, plain20 := post("InboxPlain", envs[:1]), post("InboxPlain", envs[1:2]), post("InboxPlain", envs[2:22])
	oracleFirst, oracle1, oracle20 := post("Inbox", envs[22:23]), post("Inbox", envs[23:24]), post("Inbox", envs[24:44])
	t.Logf("gas, fixed fee: first %d, 1 envelope %d, 20 %d; oracle fee: first %d, 1 envelope %d (+%d), 20 %d (+%d; the calldata floor dominates)",
		plainFirst, plain1, plain20, oracleFirst, oracle1, oracle1-plain1, oracle20, int64(oracle20)-int64(plain20))
}

func (w *world) faucetAddr() common.Address { return crypto.PubkeyToAddress(w.faucet.PublicKey) }
