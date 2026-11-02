package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/rlp"
)

func someAddrs(n int) []common.Address {
	var out []common.Address
	for i := 0; i < n; i++ {
		k, _ := crypto.GenerateKey()
		out = append(out, crypto.PubkeyToAddress(k.PublicKey))
	}
	return out
}

// TestValidatorsGenesisStorage: the storage validator genesis writes is what
// the constructor leaves, slot by slot, and the contract reads it back.
func TestValidatorsGenesisStorage(t *testing.T) {
	vs := someAddrs(4)
	names := []common.Hash{idOf("ana"), idOf("ben"), {}, idOf("dov")} // one unbound
	boot := someAddrs(1)[0]
	deployer, _ := crypto.GenerateKey()
	code, err := validatorsRuntime()
	if err != nil {
		t.Fatal(err)
	}
	alloc := types.GenesisAlloc{
		crypto.PubkeyToAddress(deployer.PublicKey): {Balance: new(big.Int).Mul(coin, big.NewInt(10))},
		validatorsAddress:                          {Code: code, Storage: validatorsStorage(boot, vs, names, devnetRules), Balance: new(big.Int)},
	}
	sim := simulated.NewBackend(alloc, withOsaka)
	defer sim.Close()
	cl := sim.Client()
	a, _ := loadArtifact("Validators")
	// The same contract made by its constructor.
	opts, _ := bind.NewKeyedTransactorWithChainID(deployer, big.NewInt(1337))
	var nb [][32]byte
	for _, n := range names {
		nb = append(nb, n)
	}
	addr, _, _, err := bind.DeployContract(opts, a.abi, a.bin, cl, boot, vs, nb, devnetRules)
	if err != nil {
		t.Fatal(err)
	}
	sim.Commit()
	ctx := context.Background()
	built, _ := cl.CodeAt(ctx, addr, nil)
	if !bytes.Equal(built, code) {
		t.Fatal("runtime code in build/ differs from what the constructor leaves (immutables?)")
	}
	st := validatorsStorage(boot, vs, names, devnetRules)
	for slot, want := range st {
		got, err := cl.StorageAt(ctx, addr, slot, nil)
		if err != nil {
			t.Fatal(err)
		}
		if common.BytesToHash(got) != want {
			t.Fatalf("slot %s: constructor wrote %x, genesis writes %s", slot.Hex(), got, want.Hex())
		}
	}
	for _, s := range []int64{2, 4, 9, 12, 13, 14} { // governance, upgrades, changes, registry, citizens, challenges: zero at genesis
		got, _ := cl.StorageAt(ctx, addr, common.BigToHash(big.NewInt(s)), nil)
		if common.BytesToHash(got) != (common.Hash{}) {
			t.Fatalf("slot %d not zero after the constructor", s)
		}
	}
	for _, at := range []common.Address{addr, validatorsAddress} { // both read the same
		bc := bind.NewBoundContract(at, a.abi, cl, cl, cl)
		var out []any
		if err := bc.Call(nil, &out, "getValidators"); err != nil {
			t.Fatal(err)
		}
		got := out[0].([]common.Address)
		if len(got) != len(vs) {
			t.Fatalf("getValidators at %s: %v", at.Hex(), got)
		}
		for i := range vs {
			if got[i] != vs[i] {
				t.Fatalf("getValidators[%d] = %s, want %s", i, got[i].Hex(), vs[i].Hex())
			}
			out = nil
			if err := bc.Call(nil, &out, "isValidator", vs[i]); err != nil || !out[0].(bool) {
				t.Fatalf("isValidator(%s): %v %v", vs[i].Hex(), out, err)
			}
		}
		out = nil
		bc.Call(nil, &out, "bootstrap")
		if out[0].(common.Address) != boot {
			t.Fatalf("bootstrap: %v", out)
		}
		out = nil
		bc.Call(nil, &out, "citizenOf", vs[1])
		if common.Hash(out[0].([32]byte)) != idOf("ben") {
			t.Fatalf("citizenOf: %v", out)
		}
		out = nil
		bc.Call(nil, &out, "rules")
		if out[0].(uint64) != devnetRules.Window || out[3].(uint32) != devnetRules.MinSet || out[5].(uint16) != devnetRules.QuorumBps {
			t.Fatalf("rules: %v", out)
		}
	}
	// The genesis file: Besu's contract validator selection, an empty list in extraData.
	g, err := validatorGenesis(genesisOpts{chainID: 7357, period: 1, timeout: 4, validators: vs, deployer: boot, supply: coin, gasLimit: 60_000_000})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(g)
	if !strings.Contains(string(b), `"validatorcontractaddress":"`+strings.ToLower(validatorsAddress.Hex())+`"`) ||
		!strings.Contains(string(b), `"extraData":"0xe5a0`+strings.Repeat("0", 64)+`c0c080c0"`) {
		t.Fatalf("genesis: %s", b[:300])
	}
	if _, err := validatorGenesis(genesisOpts{validators: []common.Address{vs[0], vs[0]}}); err == nil {
		t.Fatal("a validator listed twice was accepted")
	}
	if _, err := validatorGenesis(genesisOpts{validators: vs[:2], names: []common.Hash{idOf("ana"), idOf("ana")}}); err == nil {
		t.Fatal("one citizen, two validators in genesis")
	}
}

// TestQBFTSealRecovery: status reads who sealed a block from its commit seals.
func TestQBFTSealRecovery(t *testing.T) {
	h := &types.Header{Number: big.NewInt(7), Time: 100, GasLimit: 1, Difficulty: big.NewInt(1), BaseFee: big.NewInt(7), Coinbase: common.HexToAddress("0x01")}
	vanity := make([]byte, 32)
	enc := func(seals [][]byte) []byte {
		b, _ := rlp.EncodeToBytes([]any{vanity, []common.Address{}, []any{}, uint(2), seals})
		return b
	}
	noSeals, _ := rlp.EncodeToBytes([]any{vanity, []common.Address{}, []any{}, uint(2), [][]byte{}})
	hh := types.CopyHeader(h)
	hh.Extra = noSeals
	var seals [][]byte
	var want []common.Address
	for i := 0; i < 3; i++ {
		k, _ := crypto.GenerateKey()
		sig, _ := crypto.Sign(hh.Hash().Bytes(), k)
		seals, want = append(seals, sig), append(want, crypto.PubkeyToAddress(k.PublicKey))
	}
	h.Extra = enc(seals)
	p, got, round, err := qbftSigners(h)
	if err != nil || p != h.Coinbase || round != 2 || len(got) != 3 {
		t.Fatalf("signers: %v %v %d %v", p, got, round, err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seal %d: %s, want %s", i, got[i].Hex(), want[i].Hex())
		}
	}
}

// TestValidatorSetByBothHouses: a chain whose genesis holds the Validators
// contract; deploy hands it to the timelock; the deployer then can't touch it;
// a validator joins only by a proposal both houses pass, after the exit window;
// a removal by vote waits the removal delay, then anyone applies it.
func TestValidatorSetByBothHouses(t *testing.T) {
	w, cs, vs := newValWorld(t, launchRules, []string{"ana", "ben", "cai", "dov", "eve"}, 4)
	ch, faucet := w.ch, w.faucet
	boot := crypto.PubkeyToAddress(faucet.PublicKey)
	if got := mustCall(t, ch, "Validators", "governance").(common.Address); got.Hex() != ch.dep.Contracts["TernTimelock"] {
		t.Fatalf("governance %s, timelock %s", got.Hex(), ch.dep.Contracts["TernTimelock"])
	}
	if got := mustCall(t, ch, "Validators", "bootstrap").(common.Address); got != (common.Address{}) {
		t.Fatalf("bootstrap kept: %s", got.Hex())
	}
	if a, err := cs["ana"].dirCurrent("Validators"); err != nil || a != validatorsAddress {
		t.Fatalf("Directory: %v %v", a, err)
	}
	newV := someAddrs(1)[0]
	_, err := ch.send(faucet, nil, "Validators", "addValidator", newV, idOf("eve"))
	w.mustFail(err, "governance only")
	_, err = ch.send(faucet, nil, "Validators", "setGovernance", boot, boot, boot)
	w.mustFail(err, "bootstrap only")
	a, b, e := cs["ana"], cs["ben"], cs["eve"]
	w.must(a.lockVotes(coins(30)))
	w.later(2 * time.Second)
	w.must(runGovValidator(e, []string{"offer", newV.Hex()}, govFlags{})) // eve consents to this key (VA-3)
	p, err := e.govProposeParam("Validators", "addValidator", []string{newV.Hex(), idOf("eve").Hex()}, "a fifth validator")
	w.must(err)
	w.pass(p, []*Client{a}, []*Client{a, b, cs["cai"]})
	if !mustCall(t, ch, "Validators", "isValidator", newV).(bool) || mustCall(t, ch, "Validators", "count").(*big.Int).Int64() != 5 {
		t.Fatal("the passed proposal didn't add the validator")
	}
	if mustCall(t, ch, "Validators", "validatorOf", idOf("eve")).(common.Address) != newV {
		t.Fatal("not bound to eve")
	}
	// Removal by the same path: scheduled, then applied after the removal delay.
	p, err = a.govProposeParam("Validators", "removeValidator", []string{vs[0].Hex()}, "")
	w.must(err)
	w.pass(p, []*Client{a}, []*Client{a, b, cs["cai"]})
	if !mustCall(t, ch, "Validators", "isValidator", vs[0]).(bool) {
		t.Fatal("removed at once: a removal waits the removal delay")
	}
	_, err = ch.send(faucet, nil, "Validators", "applyRemoval", vs[0])
	w.mustFail(err, "no removal due")
	w.later(time.Duration(launchRules.RemovalDelay) * time.Second)
	_, err = ch.send(faucet, nil, "Validators", "applyRemoval", vs[0]) // anyone
	w.mustFail(err, "rolling cap")                                     // 5 before the window: one change per 30 days, the add used it
	w.later(time.Duration(launchRules.Window) * time.Second)
	w.must(errOf(ch.send(faucet, nil, "Validators", "applyRemoval", vs[0])))
	if mustCall(t, ch, "Validators", "isValidator", vs[0]).(bool) {
		t.Fatal("not removed")
	}
}
