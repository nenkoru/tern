package main

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// pinRPC: an RPC that answers honestly except for logs: it adds the ones add
// gives and hides the ones drop names (red team C-RPC).
type pinRPC struct {
	Backend
	add  []types.Log
	drop func(types.Log) bool
}

func (p pinRPC) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	logs, err := p.Backend.FilterLogs(ctx, q)
	if err != nil {
		return logs, err
	}
	var out []types.Log
	for _, l := range logs {
		if p.drop == nil || !p.drop(l) {
			out = append(out, l)
		}
	}
	for _, l := range p.add {
		if logMatches(q, l) {
			out = append(out, l)
		}
	}
	return out, nil
}

func logMatches(q ethereum.FilterQuery, l types.Log) bool {
	if len(q.Addresses) > 0 && !containsAddrs(q.Addresses, l.Address) {
		return false
	}
	for i, want := range q.Topics {
		if len(want) > 0 && (i >= len(l.Topics) || !containsHash(want, l.Topics[i])) {
			return false
		}
	}
	return (q.FromBlock == nil || q.FromBlock.Uint64() <= l.BlockNumber) && (q.ToBlock == nil || q.ToBlock.Sign() < 0 || q.ToBlock.Uint64() >= l.BlockNumber)
}

func containsAddrs(xs []common.Address, a common.Address) bool {
	for _, x := range xs {
		if x == a {
			return true
		}
	}
	return false
}

// pinLogOf: the last log of a Registry event about name, from the honest chain.
func pinLogOf(t *testing.T, w *world, event, name string) types.Log {
	t.Helper()
	ls, err := w.ch.b.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: big.NewInt(0), Addresses: []common.Address{w.ch.addr("Registry")},
		Topics: [][]common.Hash{{w.ch.abis["Registry"].Events[event].ID}, {idOf(name)}}})
	if err != nil || len(ls) == 0 {
		t.Fatalf("%s of %s: %v", event, name, err)
	}
	return ls[len(ls)-1]
}

// through: the identity in c's folder, as another process reading through b.
func through(t *testing.T, w *world, c *Client, b Backend) *Client {
	t.Helper()
	ch, err := newChain(b, w.ch.dep, w.ch.mine)
	w.must(err)
	ch.bundler = w.ch.bundler
	o, err := openClient(c.dir, ch)
	w.must(err)
	o.relay = c.relay
	return o
}

// First contact through an RPC that adds one forged KeysPublished for bob
// (mallory's real log, bob's id swapped in): its transaction is mallory's,
// so the log is never taken, the send is refused with an alert, and nothing
// is pinned; through an honest RPC bob's own keys are pinned.
func TestKeyPinForgedFirstContact(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.owner("mallory")
	forged := pinLogOf(t, w, "KeysPublished", "mallory")
	forged.Topics = []common.Hash{forged.Topics[0], idOf("bob")}
	forged.Index += 1000
	a2 := through(t, w, alice, pinRPC{Backend: w.ch.b, add: []types.Log{forged}})
	e, _ := a2.ch.epoch()
	_, got, err := a2.res.keysAt("bob", e)
	_, real, _ := bob.res.keysAt("bob", e)
	if err != nil || got != real {
		t.Fatalf("keys of bob through the lying RPC: %v (forged taken: %v)", err, got != real)
	}
	if _, err := a2.send(SendOpts{To: "bob", Text: "for bob"}); err == nil || !strings.Contains(err.Error(), "didn't sign") {
		t.Fatalf("send through a forging RPC: %v", err)
	}
	if !hasAlert(a2, "didn't sign") || a2.pin("bob") != nil {
		t.Fatalf("alerts %q, pin %+v", a2.st.Alerts, a2.pin("bob"))
	}
	a3 := through(t, w, alice, w.ch.b)
	if _, err := a3.send(SendOpts{To: "bob", Text: "for bob"}); err != nil {
		t.Fatal(err)
	}
	if p := a3.pin("bob"); p == nil || p.Controller != bob.addr().Hex() || p.Witnessed || p.Alarm != nil {
		t.Fatalf("pin %+v", p)
	}
}

// After a pin: a forged controller change (its transaction isn't bob's) and a
// genuine guardian recovery both stop sealing with an alert; an agent can't
// accept; the owner accepts from another process, and a long-running one that
// held the alarm doesn't undo it; an agent takes its owner's approval of
// exactly that change.
func TestKeyPinChangeRefusedUntilOwnerAccepts(t *testing.T) {
	w := newWorld(t)
	old := approvalWait
	approvalWait = 0
	t.Cleanup(func() { approvalWait = old })
	alice, _ := w.owner("alice")
	_, bobG := w.owner("bob")
	ada := w.agent(alice, "ada", coins(1), `{"to":["bob"]}`)
	mallory, _ := w.owner("mallory")
	for _, c := range []*Client{alice, ada} {
		if _, err := c.send(SendOpts{To: "bob", Text: "hello"}); err != nil {
			t.Fatal(err)
		}
	}
	// 1. A forged ControllerChanged (no recovery) moving bob to mallory.
	ml := pinLogOf(t, w, "KeysPublished", "mallory")
	cc := w.ch.abis["Registry"].Events["ControllerChanged"]
	data, err := cc.Inputs.NonIndexed().Pack(mallory.addr(), uint32(0), false)
	w.must(err)
	fake := types.Log{Address: w.ch.addr("Registry"), Topics: []common.Hash{cc.ID, idOf("bob")}, Data: data, BlockNumber: ml.BlockNumber,
		BlockHash: ml.BlockHash, TxHash: ml.TxHash, TxIndex: ml.TxIndex, Index: ml.Index + 1000}
	a2 := through(t, w, alice, pinRPC{Backend: w.ch.b, add: []types.Log{fake}})
	if _, err := a2.send(SendOpts{To: "bob", Text: "x"}); err == nil || !strings.Contains(err.Error(), "KEY CHANGE") ||
		!strings.Contains(err.Error(), "no transaction of its old key proves") {
		t.Fatalf("forged change: %v", err)
	}
	if !hasAlert(a2, pinAlertPrefix("bob")) || a2.pin("bob").Alarm == nil {
		t.Fatal("no alarm for a forged key change")
	}
	// The honest RPC shows the pinned key: the alarm goes by itself.
	a3 := through(t, w, alice, w.ch.b)
	if _, err := a3.send(SendOpts{To: "bob", Text: "x"}); err != nil || a3.pin("bob").Alarm != nil || hasAlert(a3, pinAlertPrefix("bob")) {
		t.Fatalf("after the lie: %v", err)
	}

	// 2. A genuine guardian recovery: no transaction of bob's key proves it.
	nk, _ := crypto.GenerateKey()
	w.must(bobG.startRecovery("bob", crypto.PubkeyToAddress(nk.PublicKey)))
	w.later(73 * time.Hour)
	w.must(bobG.finishRecovery("bob"))
	alice.syncGuard()
	alice.res.forget()
	if _, err := alice.send(SendOpts{To: "bob", Text: "x"}); err == nil || !strings.Contains(err.Error(), "guardian recovery") ||
		!strings.Contains(err.Error(), "pins accept") {
		t.Fatalf("after a recovery: %v", err)
	}
	if l := alice.pinsList(); l["key_changes"] != 1 {
		t.Fatalf("pins: %v", l)
	}
	if _, err := ada.acceptPin("bob"); err == nil || !strings.Contains(err.Error(), "an agent can't accept") {
		t.Fatalf("an agent accepted: %v", err)
	}
	cli := through(t, w, alice, w.ch.b) // the owner's CLI, beside a long-running alice
	if _, err := cli.acceptPin("bob"); err != nil {
		t.Fatal(err)
	}
	w.must(alice.save()) // the long-running process writes its copy, alarm and all
	var disk State
	w.must(readJSON(alice.statePath(), &disk))
	if p := disk.KeyPins["bob"]; p == nil || p.Alarm != nil || p.Controller != crypto.PubkeyToAddress(nk.PublicKey).Hex() {
		t.Fatalf("the acceptance was undone: %+v", p)
	}
	if _, err := alice.send(SendOpts{To: "bob", Text: "after checking with bob"}); err != nil {
		t.Fatal(err)
	}
	if hasAlert(alice, pinAlertPrefix("bob")) {
		t.Fatal("the alert outlived the acceptance")
	}

	// 3. The agent: refused, its owner asked; the owner approves exactly this change.
	ada.res.forget()
	if _, err := ada.send(SendOpts{To: "bob", Text: "x"}); err == nil || !strings.Contains(err.Error(), "KEY CHANGE") ||
		!strings.Contains(err.Error(), "owner approve") {
		t.Fatalf("agent after a recovery: %v", err)
	}
	al := ada.pin("bob").Alarm
	if al == nil || al.Ask == "" {
		t.Fatalf("the owner wasn't asked: %+v", al)
	}
	w.must(alice.approveAction(ada.f.Name, "keys:bob:"+al.ID, nil))
	if _, err := ada.send(SendOpts{To: "bob", Text: "approved"}); err != nil {
		t.Fatal(err)
	}
	if ada.pin("bob").Alarm != nil {
		t.Fatal("the agent's alarm stayed")
	}
}

// Re-publishing keys (a new epoch, twice in one epoch) and a key change the
// old key signed change nothing a sender has to approve; nor do smart
// accounts' publishes (a passkey's user operation, an agent account's).
func TestKeyPinLegitimateRotationNoAlarm(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	if _, err := alice.send(SendOpts{To: "bob", Text: "1"}); err != nil {
		t.Fatal(err)
	}
	b0 := alice.pin("bob").Block
	w.later(25 * time.Hour)
	w.must(bob.publishKeys())
	w.must(bob.publishKeys()) // a rekey: the same first epoch again
	alice.res.forget()
	if _, err := alice.send(SendOpts{To: "bob", Text: "2"}); err != nil {
		t.Fatal(err)
	}
	if p := alice.pin("bob"); p.Block <= b0 || p.Alarm != nil || len(alice.st.Alerts) > 0 {
		t.Fatalf("after a re-publish: %+v %q", p, alice.st.Alerts)
	}
	fresh, _ := crypto.GenerateKey()
	if _, err := w.ch.send(bob.key, nil, "Registry", "setController", idOf("bob"), crypto.PubkeyToAddress(fresh.PublicKey), false); err != nil {
		t.Fatal(err)
	}
	alice.res.forget()
	if _, err := alice.send(SendOpts{To: "bob", Text: "3"}); err != nil {
		t.Fatal(err)
	}
	if p := alice.pin("bob"); p.Controller != crypto.PubkeyToAddress(fresh.PublicKey).Hex() || p.Alarm != nil || len(alice.st.Alerts) > 0 {
		t.Fatalf("after a signed key change: %+v %q", p, alice.st.Alerts)
	}
	carol := w.passkeyOwner("carol")
	w.accountAgent(carol, "x", coins(1), coins(1))
	for _, to := range []string{"carol", "x.carol"} {
		if _, err := alice.send(SendOpts{To: to, Text: "hi"}); err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}
	if p := alice.pin("carol"); p == nil || !strings.HasPrefix(p.AccountKey, "p256:") || p.Controller != carol.me().Hex() {
		t.Fatalf("carol's pin: %+v", p)
	}
	if alice.pin("x.carol") == nil || len(alice.st.Alerts) > 0 {
		t.Fatalf("x.carol: %q", alice.st.Alerts)
	}
}

// A second RPC: a first pin it confirms is witnessed; one it contradicts (the
// log RPC hides bob's newest keys) is refused; strict privacy takes no first
// pin from one RPC.
func TestKeyPinTwoRPCs(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.owner("carol")
	w.later(25 * time.Hour)
	w.must(bob.publishKeys())
	newest := pinLogOf(t, w, "KeysPublished", "bob")
	hide := pinRPC{Backend: w.ch.b, drop: func(l types.Log) bool { return l.TxHash == newest.TxHash }}
	a2 := through(t, w, alice, hide)
	a2.ch.witness = []Backend{w.ch.b}
	if _, err := a2.send(SendOpts{To: "bob", Text: "x"}); err == nil || !strings.Contains(err.Error(), "RPCs disagree") {
		t.Fatalf("disagreeing RPCs: %v", err)
	}
	if !hasAlert(a2, "RPCs disagree") || a2.pin("bob") != nil {
		t.Fatal("no alert, or pinned anyway")
	}
	a3 := through(t, w, alice, w.ch.b)
	a3.ch.witness = []Backend{w.ch.b}
	if _, err := a3.send(SendOpts{To: "bob", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if p := a3.pin("bob"); p == nil || !p.Witnessed {
		t.Fatalf("pin %+v", p)
	}
	old := privacyLevel
	privacyLevel = privacyStrict
	t.Cleanup(func() { privacyLevel = old })
	a4 := through(t, w, alice, w.ch.b)
	if err := a4.sealCheck("carol"); err == nil || !strings.Contains(err.Error(), "--privacy strict") {
		t.Fatalf("strict with one RPC: %v", err)
	}
	a4.ch.witness = []Backend{w.ch.b}
	if err := a4.sealCheck("carol"); err != nil {
		t.Fatal(err)
	}
}

// An RPC that hides bob's newest keys from a sender that has seen them
// (serving older ones, whose secrets may be gone) is refused.
func TestKeyPinRollbackRefused(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.later(25 * time.Hour)
	w.must(bob.publishKeys())
	if _, err := alice.send(SendOpts{To: "bob", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	newest := pinLogOf(t, w, "KeysPublished", "bob")
	a2 := through(t, w, alice, pinRPC{Backend: w.ch.b, drop: func(l types.Log) bool { return l.TxHash == newest.TxHash }})
	if _, err := a2.send(SendOpts{To: "bob", Text: "x"}); err == nil || !strings.Contains(err.Error(), "older than the ones already seen") {
		t.Fatalf("rollback: %v", err)
	}
	if !hasAlert(a2, "hiding newer keys") {
		t.Fatal("no alert")
	}
}

// verify_blocks: the pinned log's block must be the second RPC's at that
// height, and its transaction and receipt in it by the header's roots.
func TestKeyPinVerifyBlocks(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	w.owner("carol")
	a2 := through(t, w, alice, w.ch.b)
	a2.ch.verifyBlocks = true
	if err := a2.sealCheck("bob"); err == nil || !strings.Contains(err.Error(), "needs a second RPC") {
		t.Fatalf("no second RPC: %v", err)
	}
	a2.ch.witness = []Backend{otherBlocks{w.ch.b}}
	if err := a2.sealCheck("bob"); err == nil || !strings.Contains(err.Error(), "RPCs disagree") {
		t.Fatalf("another chain's blocks: %v", err)
	}
	a3 := through(t, w, alice, w.ch.b)
	a3.ch.verifyBlocks = true
	a3.ch.witness = []Backend{w.ch.b}
	if err := a3.sealCheck("carol"); err != nil {
		t.Fatal(err)
	}
	f, err := a3.res.facts("carol")
	w.must(err)
	a3.ch.witness = []Backend{otherBlocks{w.ch.b}}
	if err := a3.res.blockCheck(f.newest.src); err == nil || !strings.Contains(err.Error(), "the second RPC has") {
		t.Fatalf("another chain's block: %v", err)
	}
	a3.ch.witness = []Backend{w.ch.b}
	ev := f.newest.src
	ev.data = append([]byte{}, ev.data...)
	ev.data[len(ev.data)-1] ^= 1
	if err := a3.res.blockCheck(ev); err == nil || !strings.Contains(err.Error(), "not in transaction") {
		t.Fatalf("a log not in its receipt: %v", err)
	}
}

// otherBlocks: an RPC whose headers aren't the chain's.
type otherBlocks struct{ Backend }

type unavailablePinWitness struct {
	Backend
	account bool
}

func (w unavailablePinWitness) CodeAt(ctx context.Context, addr common.Address, at *big.Int) ([]byte, error) {
	if w.account {
		return nil, errors.New("account state unavailable")
	}
	return w.Backend.CodeAt(ctx, addr, at)
}

func (w unavailablePinWitness) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return nil, errors.New("header unavailable")
}

func TestKeyPinIncompleteWitness(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	f, err := alice.res.facts("bob")
	w.must(err)
	old := privacyLevel
	privacyLevel = privacyStrict
	t.Cleanup(func() { privacyLevel = old })
	for _, account := range []bool{false, true} {
		alice.ch.witness = []Backend{unavailablePinWitness{Backend: w.ch.b, account: account}}
		if agreed, why, err := alice.res.witness(f); err != nil || agreed || why == "" {
			t.Fatalf("incomplete witness: agreed=%v why=%q err=%v", agreed, why, err)
		}
		if err := alice.sealCheck("bob"); err == nil || !strings.Contains(err.Error(), "--privacy strict") {
			t.Fatalf("strict first contact with incomplete witness: %v", err)
		}
		if alice.pin("bob") != nil {
			t.Fatal("incomplete witness established a pin")
		}
	}
}

func (o otherBlocks) HeaderByNumber(ctx context.Context, n *big.Int) (*types.Header, error) {
	h, err := o.Backend.HeaderByNumber(ctx, n)
	if err != nil {
		return nil, err
	}
	h = types.CopyHeader(h)
	h.Extra = append(h.Extra, 1)
	return h, nil
}

func TestKeyPinReplayedPublishAfterRotation(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	old := pinLogOf(t, w, "KeysPublished", "bob")
	e, err := w.ch.epoch()
	w.must(err)
	w.must(os.Remove(bob.epochPath(e)))
	w.must(bob.publishKeys()) // replace today's key, as rekey --compromised does
	alice.ch.witness = []Backend{w.ch.b}
	if err := alice.sealCheck("bob"); err != nil {
		t.Fatal(err)
	}
	pinned := alice.pin("bob").Block
	w.owner("carol") // a later genuine block to attach the replay to
	h, err := w.ch.b.HeaderByNumber(context.Background(), nil)
	w.must(err)
	old.BlockNumber, old.BlockHash, old.Index = h.Number.Uint64(), h.Hash(), old.Index+1000
	a2 := through(t, w, alice, pinRPC{Backend: w.ch.b, add: []types.Log{old}})
	a2.ch.witness = []Backend{w.ch.b}
	if err := a2.sealCheck("bob"); err == nil || !strings.Contains(err.Error(), "RPCs disagree") {
		t.Fatalf("replayed signed publish accepted: %v", err)
	}
	if a2.pin("bob").Block != pinned {
		t.Fatal("the refused replay replaced the pin")
	}
}

type pinAccountRPC struct {
	pinRPC
	account common.Address
	signer  common.Address
	tx      *types.Transaction
}

func (p pinAccountRPC) CallContract(ctx context.Context, m ethereum.CallMsg, at *big.Int) ([]byte, error) {
	if m.To != nil && *m.To == p.account && bytes.Equal(m.Data, crypto.Keccak256([]byte("signer()"))[:4]) {
		return common.LeftPadBytes(p.signer.Bytes(), 32), nil
	}
	return p.Backend.CallContract(ctx, m, at)
}

func (p pinAccountRPC) TransactionByHash(ctx context.Context, h common.Hash) (*types.Transaction, bool, error) {
	if p.tx != nil && p.tx.Hash() == h {
		return p.tx, false, nil
	}
	return p.Backend.TransactionByHash(ctx, h)
}

func TestKeyPinAgentAccountSignerChange(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob := w.passkeyOwner("bob")
	agent := w.accountAgent(bob, "x", coins(1), coins(1))
	if err := alice.sealCheck("x.bob"); err != nil {
		t.Fatal(err)
	}
	mallory, _ := w.owner("mallory")
	p, err := mallory.res.party("mallory")
	w.must(err)
	keys := p.keys[len(p.keys)-1]
	publish, err := w.ch.abis["Registry"].Pack("publishKeys", idOf("x.bob"), keys.first, keys.view, keys.raw)
	w.must(err)
	execution := append(append(w.ch.addr("Registry").Bytes(), make([]byte, 32)...), publish...)
	call, err := w.ch.abis["OwnerAccount"].Pack("execute", [32]byte{}, execution)
	w.must(err)
	op := packedUserOp{Sender: agent.me(), Nonce: new(big.Int), PreVerificationGas: new(big.Int), CallData: call}
	hash := userOpHash(op, w.ch.addr("EntryPoint"), w.ch.chainID)
	op.Signature, err = crypto.Sign(hash[:], mallory.key)
	w.must(err)
	data, err := w.ch.abis["EntryPoint"].Pack("handleOps", []packedUserOp{op}, mallory.addr())
	w.must(err)
	ep := w.ch.addr("EntryPoint")
	tx, err := types.SignNewTx(mallory.key, types.LatestSignerForChainID(w.ch.chainID), &types.LegacyTx{To: &ep, Data: data, Gas: 1000000, GasPrice: big.NewInt(1)})
	w.must(err)
	forged := pinLogOf(t, w, "KeysPublished", "mallory")
	forged.Topics = []common.Hash{forged.Topics[0], idOf("x.bob")}
	forged.TxHash, forged.Index = tx.Hash(), forged.Index+1000
	rpc := pinAccountRPC{pinRPC: pinRPC{Backend: w.ch.b, add: []types.Log{forged}}, account: agent.me(), signer: mallory.addr(), tx: tx}
	a2 := through(t, w, alice, rpc)
	if err := a2.sealCheck("x.bob"); err == nil || !strings.Contains(err.Error(), "agent account") || !strings.Contains(err.Error(), "KEY CHANGE") {
		t.Fatalf("changed agent-account signer accepted: %v", err)
	}
	if p := a2.pin("x.bob"); p == nil || p.Alarm == nil {
		t.Fatal("no alarm for the changed agent-account signer")
	}
}

func TestKeyPinAgentAccountWitness(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob := w.passkeyOwner("bob")
	agent := w.accountAgent(bob, "x", coins(1), coins(1))
	mallory, _ := w.owner("mallory")
	alice.ch.witness = []Backend{pinAccountRPC{pinRPC: pinRPC{Backend: w.ch.b}, account: agent.me(), signer: mallory.addr()}}
	if err := alice.sealCheck("x.bob"); err == nil || !strings.Contains(err.Error(), "agent account's key") {
		t.Fatalf("agent signer disagreement was not checked: %v", err)
	}
	if alice.pin("x.bob") != nil {
		t.Fatal("pinned an agent despite signer disagreement")
	}
}

func TestKeyPinOwnerSignedAgentReplacement(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.agent(bob, "x", coins(1), `{}`)
	if err := alice.sealCheck("x.bob"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_, err := newIdentDir(dir, IdentityFile{Name: "x.bob", Owner: "bob", Kind: "agent"})
	w.must(err)
	next, err := openClient(dir, w.ch)
	w.must(err)
	bundle, err := bob.grant(AgentRequest{Name: "x", Owner: "bob", Address: next.addr().Hex()}, 30, coins(1), `{}`, coins(1))
	w.must(err)
	w.must(next.join(*bundle))
	alice.res.forget()
	if err := alice.sealCheck("x.bob"); err != nil {
		t.Fatalf("owner-signed replacement refused: %v", err)
	}
	if p := alice.pin("x.bob"); p.Agent != next.addr().Hex() || p.Alarm != nil {
		t.Fatalf("replacement not pinned: %+v", p)
	}
}
