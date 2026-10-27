package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"

	"github.com/nenkoru/tern/internal/artifact"
)

var coin = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1 coin = 1e18 wei

func coins(f float64) *big.Int {
	v, _ := new(big.Float).Mul(big.NewFloat(f), new(big.Float).SetInt(coin)).Int(nil)
	return v
}

type world struct {
	t      *testing.T
	sim    *simulated.Backend
	ch     *Chain
	dir    string
	faucet *ecdsa.PrivateKey
	relay  string // a relayer's URL new owners use (secret votes, pool sends), when set
}

func newWorld(t *testing.T) *world {
	t.Helper()
	faucet, _ := crypto.GenerateKey()
	alloc := types.GenesisAlloc{crypto.PubkeyToAddress(faucet.PublicKey): {Balance: new(big.Int).Mul(coin, big.NewInt(1_000_000))}}
	sim := simulated.NewBackend(alloc, withOsaka)
	t.Cleanup(func() { sim.Close() })
	treasury, _ := crypto.GenerateKey()
	ch, err := deploy(sim.Client(), faucet, Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12),
		Treasury: crypto.PubkeyToAddress(treasury.PublicKey)}, "", func() { sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	ch.bundler = faucet // a node runner submitting user operations
	return &world{t: t, sim: sim, ch: ch, dir: t.TempDir(), faucet: faucet}
}

// passkeyOwner: a name held by a smart account guarded by a passkey.
func (w *world) passkeyOwner(name string) *Client {
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
	if err := c.enablePasskeyAccount(); err != nil {
		w.t.Fatal(err)
	}
	// The account's own coins (for Bank deposits); the mail key gets nothing.
	if err := w.ch.transfer(w.faucet, c.me(), coins(100)); err != nil {
		w.t.Fatal(err)
	}
	if err := c.registerOwner(crypto.PubkeyToAddress(g.key.PublicKey)); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.ch.send(w.faucet, coins(5), "TernPaymaster", "fund", idOf(name)); err != nil {
		w.t.Fatal(err)
	}
	return c
}

// accountAgent: an agent whose key is a session key of its own account; no ETH.
func (w *world) accountAgent(owner *Client, label string, spend, gasBudget *big.Int) *Client {
	w.t.Helper()
	full := label + "." + owner.f.Name
	dir := filepath.Join(w.dir, full)
	if _, err := newIdentDir(dir, IdentityFile{Name: full, Kind: "agent", Owner: owner.f.Name}); err != nil {
		w.t.Fatal(err)
	}
	a, err := openClient(dir, w.ch)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := a.enableAgentAccount(); err != nil {
		w.t.Fatal(err)
	}
	b, err := owner.grant(AgentRequest{Name: label, Owner: owner.f.Name, Address: a.me().Hex(), Account: true}, 30, spend, `{}`, gasBudget)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := a.join(*b); err != nil {
		w.t.Fatal(err)
	}
	return a
}

func (w *world) fund(a *Client, amount *big.Int) {
	if err := w.ch.transfer(w.faucet, a.addr(), amount); err != nil {
		w.t.Fatal(err)
	}
}

// owner registers a name; its guardian key is kept apart (returned).
func (w *world) owner(name string) (*Client, *Client) {
	return w.ownerWith(name, false)
}

// ownerWith: pq publishes post-quantum (hybrid) epoch keys.
func (w *world) ownerWith(name string, pq bool) (*Client, *Client) {
	w.t.Helper()
	g, err := newIdentDir(filepath.Join(w.dir, "guardian-"+name), IdentityFile{Name: name, Kind: "guardian"})
	if err != nil {
		w.t.Fatal(err)
	}
	gc := &Client{Ident: g, ch: w.ch, res: newResolver(w.ch), st: &State{Contacts: map[string]bool{}, Groups: map[string]*GroupKeys{}}}
	if _, err := newIdentDir(filepath.Join(w.dir, name), IdentityFile{Name: name, Kind: "owner", Owner: name, PQ: pq}); err != nil {
		w.t.Fatal(err)
	}
	c, err := openClient(filepath.Join(w.dir, name), w.ch)
	if err != nil {
		w.t.Fatal(err)
	}
	c.relay = w.relay
	w.fund(c, coins(100))
	w.fund(gc, coins(1))
	if err := c.registerOwner(g.addr()); err != nil {
		w.t.Fatal(err)
	}
	return c, gc
}

func (w *world) agent(owner *Client, label string, spend *big.Int, scopes string) *Client {
	w.t.Helper()
	full := label + "." + owner.f.Name
	dir := filepath.Join(w.dir, full)
	if _, err := newIdentDir(dir, IdentityFile{Name: full, Kind: "agent", Owner: owner.f.Name}); err != nil {
		w.t.Fatal(err)
	}
	a, err := openClient(dir, w.ch)
	if err != nil {
		w.t.Fatal(err)
	}
	b, err := owner.grant(AgentRequest{Name: label, Owner: owner.f.Name, Address: a.addr().Hex()}, 30, spend, scopes, coins(5))
	if err != nil {
		w.t.Fatal(err)
	}
	if err := a.join(*b); err != nil {
		w.t.Fatal(err)
	}
	return a
}

func (w *world) later(d time.Duration) {
	err := w.sim.AdjustTime(d)
	// A transaction another goroutine (a relay, a data node) just sent may be
	// in the pool: mine it, then jump.
	for i := 0; err != nil && strings.Contains(err.Error(), "non-empty") && i < 50; i++ {
		w.sim.Commit()
		err = w.sim.AdjustTime(d)
	}
	if err != nil {
		w.t.Fatal(err)
	}
	w.sim.Commit()
}

func scan(t *testing.T, c *Client) []*Item {
	t.Helper()
	got, err := c.scan()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mailTo(items []*Item, typ string) []*Item {
	var out []*Item
	for _, it := range items {
		if it.Type == typ {
			out = append(out, it)
		}
	}
	return out
}

// ---- 1:1 mail, policy, taint, owner oversight

func TestMailBetweenPeopleOnChain(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	ada := w.agent(oscar, "ada", coins(1), `{"wake_siblings":false}`)
	eve := w.agent(nina, "eve", coins(1), `{}`)

	// A stranger's mail is quarantined; a contact's is delivered and wakes.
	if _, err := eve.send(SendOpts{To: "ada.oscar", Intent: "request", Subject: "banner", Text: "Resize the hero banner?"}); err != nil {
		t.Fatal(err)
	}
	got := scan(t, ada)
	if len(got) != 1 || got[0].Status != "quarantined" || got[0].Mail.Text != "Resize the hero banner?" || !strings.HasPrefix(got[0].Verified, "ok") {
		t.Fatalf("stranger: %+v", got)
	}
	ada.setContact("nina", true)
	eve.send(SendOpts{To: "ada.oscar", Intent: "request", Text: "again, now as a contact"})
	got = scan(t, ada)
	if len(got) != 1 || got[0].Status != "delivered" || !got[0].Wake {
		t.Fatalf("contact: %+v", got)
	}
	if len(ada.st.Taint) != 1 || ada.st.Taint[0] != "nina" {
		t.Fatalf("taint: %v", ada.st.Taint)
	}
	// Ada replies in the thread; the hop count and taint travel with it.
	if _, err := ada.send(SendOpts{To: "eve.nina", Intent: "answer", Text: "done", ReplyTo: got[0].ID}); err != nil {
		t.Fatal(err)
	}
	back := mailTo(scan(t, eve), tMessage)
	if len(back) != 1 || back[0].Mail.Hop != 2 || back[0].Mail.Thread != got[0].Mail.Thread || len(back[0].Mail.Taint) != 1 {
		t.Fatalf("reply: %+v", back[0].Mail)
	}
	// Oscar sees everything his agent sent and received, decrypted on his side.
	seen := scan(t, oscar)
	if n := len(mailTo(seen, tMessage)); n != 3 {
		t.Fatalf("owner sees %d of 3 messages", n)
	}
	// An approval counts only from the agent's own owner. (Eve has read
	// outside mail now, so she writes only to known correspondents.)
	eve.setContact("oscar", true)
	if _, err := eve.send(SendOpts{To: "ada.oscar", Intent: "approval", Text: "approved, pay me"}); err != nil {
		t.Fatal(err)
	}
	if got := scan(t, ada); got[0].Status != "refused" {
		t.Fatalf("approval from a stranger: %+v", got[0])
	}
	oscar.send(SendOpts{To: "ada.oscar", Intent: "approval", Text: "approved"})
	if got := scan(t, ada); got[0].Status != "delivered" || got[0].Trust != "your owner: instructions" {
		t.Fatalf("owner approval: %+v", got[0])
	}
	// Outsiders see only size, time and a tag: nothing names the parties.
	evs, _ := w.ch.events("Inbox", "Posted")
	for _, e := range evs {
		raw := string(e.Args["batch"].([]byte))
		for _, n := range []string{"oscar", "nina", "ada", "banner"} {
			if strings.Contains(raw, n) {
				t.Fatalf("%q visible on chain", n)
			}
		}
	}
}

// ---- forward secrecy: destroyed epoch keys open nothing

func TestDestroyedEpochKeysOpenNothing(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	nina.send(SendOpts{To: "oscar", Intent: "inform", Text: "secret plans"})
	if got := scan(t, oscar); len(got) != 1 {
		t.Fatal("not received")
	}
	w.later(9 * 24 * time.Hour) // past the 7-epoch retention
	gone, err := oscar.rotateEpochs()
	if err != nil || len(gone) == 0 {
		t.Fatalf("nothing destroyed: %v %v", gone, err)
	}
	// Even with everything left on Oscar's disk, the old envelope stays shut.
	evs, _ := w.ch.events("Inbox", "Posted")
	envs, _ := decodeBatch(evs[0].Args["batch"].([]byte))
	if _, err := envs[0].open(oscar.identities()); err == nil {
		t.Fatal("old mail still opens")
	}
	// New mail works with the new epoch's keys.
	nina.res.forget()
	if _, err := nina.send(SendOpts{To: "oscar", Intent: "inform", Text: "new"}); err != nil {
		t.Fatal(err)
	}
	if got := scan(t, oscar); len(got) != 1 || got[0].Mail.Text != "new" {
		t.Fatalf("%+v", got)
	}
}

// ---- one-to-many: a public city group with moderation, a private group

func TestMersinGroupAndModeration(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	spammer, _ := w.owner("spammer")
	tourist := w.agent(oscar, "guide", coins(1), `{}`)
	local := w.agent(nina, "mersin", coins(1), `{}`)
	if err := nina.createGroup("mersin", true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Client{tourist, local, spammer} {
		if _, err := c.joinGroup("mersin"); err != nil {
			t.Fatal(err)
		}
	}
	tourist.postGroup("mersin", "Two of us arrive Friday. Anyone up for a walk in the old town and board games?", nil)
	spammer.postGroup("mersin", "CHEAP FOLLOWERS", nil)
	got := scan(t, local)
	if len(got) != 2 {
		t.Fatalf("group posts: %d", len(got))
	}
	// A latecomer who has been reading its mail for a while joins: it still
	// sees what was posted before it joined (the rules of a game, a listing).
	late := w.agent(nina, "late", coins(1), `{}`)
	scan(t, late)
	if _, err := late.joinGroup("mersin"); err != nil {
		t.Fatal(err)
	}
	if n := len(scan(t, late)); n != 2 {
		t.Fatalf("latecomer sees %d earlier posts, want 2", n)
	}
	// The local agent answers one-to-one: groups are for finding each other.
	local.setContact("oscar", true)
	if _, err := local.send(SendOpts{To: "guide.oscar", Intent: "offer", Text: "My owner hosts a games night on Friday. Join?"}); err != nil {
		t.Fatal(err)
	}
	if got := mailTo(scan(t, tourist), tMessage); len(got) < 1 {
		t.Fatal("offer not received")
	}
	// The admin bans the spammer and hides the post; every client applies it.
	var spam *Item
	for _, it := range got {
		if it.From == "spammer" {
			spam = it
		}
	}
	if err := nina.moderate("mersin", "hide", spam.ID, "spam"); err != nil {
		t.Fatal(err)
	}
	if err := nina.moderate("mersin", "ban", "spammer", "spam"); err != nil {
		t.Fatal(err)
	}
	spammer.postGroup("mersin", "MORE SPAM", nil)
	fresh, _ := openClient(filepath.Join(w.dir, "mersin.nina"), w.ch)
	fresh.st = &State{Contacts: map[string]bool{}, Groups: map[string]*GroupKeys{}}
	fresh.joinGroup("mersin")
	for _, it := range scan(t, fresh) {
		if it.From == "spammer" && it.Status != "hidden" {
			t.Fatalf("spam shown: %+v", it)
		}
	}
	if _, err := spammer.joinGroup("mersin"); err == nil {
		t.Fatal("banned member rejoined")
	}

	// A private group: the admin admits and hands out keys in sealed mail.
	if err := oscar.createGroup("team", false); err != nil {
		t.Fatal(err)
	}
	// The local agent has read outside mail: joining a new group needs its owner's approval.
	if _, err := local.joinGroup("team"); err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("a tainted agent joined a new group freely: %v", err)
	}
	if err := nina.approveAction("mersin.nina", "group:team", nil); err != nil {
		t.Fatal(err)
	}
	if msg, err := local.joinGroup("team"); err != nil || !strings.HasPrefix(msg, "requested") {
		t.Fatalf("%s %v", msg, err)
	}
	if done, err := oscar.admitPending(); err != nil || len(done) != 1 {
		t.Fatalf("admit: %v %v", done, err)
	}
	scan(t, local) // receives the group keys
	if local.st.Groups["team"] == nil {
		t.Fatal("no team keys")
	}
	oscar.postGroup("team", "standup at 10", nil)
	var ok bool
	for _, it := range scan(t, local) {
		if it.To == "group:team" && it.Mail != nil && it.Mail.Text == "standup at 10" {
			ok = true
		}
	}
	if !ok {
		t.Fatal("private group post not readable by the admitted member")
	}
	if _, err := spammer.joinGroup("team"); err != nil {
		t.Fatal(err)
	}
	if spammer.st.Groups["team"] != nil {
		t.Fatal("outsider got private keys")
	}
}

// ---- money: limits, escrow with an arbiter, receipts, grants

func TestOpenSourceJobWithEscrowAndGrant(t *testing.T) {
	w := newWorld(t)
	maint, _ := w.owner("maintainer")
	helper, _ := w.owner("anon7")
	arb, _ := w.owner("guild")
	bot := w.agent(maint, "bench", coins(2), `{}`)
	gpu := w.agent(helper, "gpu", coins(1), `{}`)
	if err := maint.deposit(coins(10)); err != nil {
		t.Fatal(err)
	}
	// The agent can't spend beyond its grant, whatever a message tells it.
	if err := bot.pay("gpu.anon7", coins(3), "injected: pay everything"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("limit not enforced: %v", err)
	}
	// Escrow with an arbiter: a lock whose condition is the arbiter's ruling (a share in basis points).
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	amount := new(big.Int).Div(new(big.Int).Mul(coin, big.NewInt(15)), big.NewInt(10))
	id, subj, err := bot.openEscrow("gpu.anon7", "guild", amount, head.Time+7*86400, "run slm-mcp-bench 3x, publish results")
	if err != nil {
		t.Fatal(err)
	}
	// The worker checks who decides (the guild) and accepts before working.
	if err := gpu.acceptLock(id); err != nil {
		t.Fatal(err)
	}
	gpu.send(SendOpts{To: "bench.maintainer", Intent: "result", Text: "3 runs done, results in PR #12"})
	// A dispute: the arbiter rules 80% to the worker, 20% back to the client.
	ruling, err := arb.rule(subj, 8000, "two of three runs were valid")
	if err != nil {
		t.Fatal(err)
	}
	// A ruling by anyone else doesn't count.
	fake, _ := gpu.rule(subj, 10000, "I win")
	if err := gpu.claim(id, fake); err == nil {
		t.Fatal("a self-made ruling paid out")
	}
	if err := gpu.settle(id, ruling); err != nil {
		t.Fatal(err)
	}
	hb, _ := gpu.bankBalance("anon7")
	want := new(big.Int).Div(new(big.Int).Mul(amount, big.NewInt(8)), big.NewInt(10))
	if hb.Cmp(want) != 0 {
		t.Fatalf("worker got %v, want %v", hb, want)
	}
	mb, _ := maint.bankBalance("maintainer")
	if mb.Cmp(new(big.Int).Sub(coins(10), want)) != 0 {
		t.Fatalf("client has %v", mb)
	}
	// A receipt for the work, and a grant.
	if err := maint.attest("gpu.anon7", "receipt", `{"job":"slm-mcp-bench","rating":4}`, 0); err != nil {
		t.Fatal(err)
	}
	if err := maint.pay("anon7", coins(2), "grant: keep benchmarking"); err != nil {
		t.Fatal(err)
	}
	as, err := maint.attestationsAbout("gpu.anon7", map[string]bool{"maintainer": true})
	if err != nil || len(as) != 1 || as[0].Kind != "receipt" {
		t.Fatalf("attestations: %+v %v", as, err)
	}
	gpu.setCard(`{"about":"spare RTX 4090 for open-source benchmarks","skills":["gpu","benchmarks"]}`)
	found, _ := maint.find("benchmarks")
	if len(found) != 1 || found[0].Name != "gpu.anon7" {
		t.Fatalf("find: %+v", found)
	}
}

// ---- recovery: a stolen guardian key is vetoed; an unvetoed one completes

func TestRecoveryVetoAndCompletion(t *testing.T) {
	w := newWorld(t)
	oscar, guardian := w.owner("oscar")
	ada := w.agent(oscar, "ada", coins(1), `{}`)
	thief, _ := crypto.GenerateKey()
	if err := guardian.startRecovery("oscar", crypto.PubkeyToAddress(thief.PublicKey)); err != nil {
		t.Fatal(err)
	}
	if err := oscar.cancelRecovery(); err != nil {
		t.Fatal(err)
	}
	w.later(73 * time.Hour)
	if err := guardian.finishRecovery("oscar"); err == nil {
		t.Fatal("vetoed recovery completed")
	}
	// A genuine recovery: the guardian moves the name to a new key after 72 h.
	newKey, _ := crypto.GenerateKey()
	guardian.startRecovery("oscar", crypto.PubkeyToAddress(newKey.PublicKey))
	if err := guardian.finishRecovery("oscar"); err == nil {
		t.Fatal("recovery completed before the delay")
	}
	w.later(73 * time.Hour)
	if err := guardian.finishRecovery("oscar"); err != nil {
		t.Fatal(err)
	}
	// Grants made under the lost key no longer count.
	out, _ := w.ch.call("Registry", "grantValid", idOf("ada.oscar"))
	if out[0].(bool) {
		t.Fatal("old grant still valid after recovery")
	}
	if _, err := ada.send(SendOpts{To: "oscar", Intent: "inform", Text: "x"}); err != nil {
		t.Logf("send after recovery: %v", err)
	}
}

// ---- the six primitives, through one recipe: an agency fine-tunes a model

func TestPrimitivesFineTuneMarket(t *testing.T) {
	w := newWorld(t)
	firm, _ := w.owner("lawfirm")
	agency, _ := w.owner("tuneshop")
	evals, _ := w.owner("evalhub")
	arbiter, _ := w.owner("guild")
	buyer := w.agent(firm, "procure", coins(3), `{}`)
	seller := w.agent(agency, "sales", coins(1), `{}`)
	firm.deposit(coins(20))
	agency.deposit(coins(5))
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	week := head.Time + 7*86400

	// Bond: the agency stakes 1 coin; evalhub may give part of it to the firm.
	bondID, bondSubj, err := seller.bond("evalhub", "lawfirm", coins(1), week, "legal fine-tune quality")
	if err != nil {
		t.Fatal(err)
	}
	// Conditional payment: 5 coins, released when evalhub scores the job ≥ 80.
	job := "0x" + strings.Repeat("ab", 32)
	cond, err := firm.openConditional("tuneshop", "evalhub", job, "eval:legal-qa", 80, coins(5), week)
	if err != nil {
		t.Fatal(err)
	}
	// Artifact: the tuned weights, encrypted, off chain; bought through an escrow, key sent sealed.
	weights := []byte("pretend these are LoRA weights for legal QA")
	ct, hash, key, _ := artifact.SealFile(weights)
	art, err := seller.registerArtifact(hash, uint64(len(ct)), "ipfs://bafy-pretend", "commercial, single org", coins(1), key)
	if err != nil {
		t.Fatal(err)
	}
	firm.setContact("tuneshop", true)
	sale, _, err := buyer.buy(art, "guild", week)
	if err != nil {
		t.Fatal(err)
	}
	seller.st.Arbiters = []string{"guild"} // the seller's own list of arbiters (MO-5)
	if err := seller.deliverKey(sale, art, "procure.lawfirm", key); err != nil {
		t.Fatal(err)
	}
	buyer.setContact("tuneshop", true)
	_, plain, err := buyer.receiveKey(sale, ct)
	if err != nil || !bytes.Equal(plain, weights) {
		t.Fatalf("key exchange: %v", err)
	}
	if err := buyer.releaseLock(sale, nil); err != nil {
		t.Fatal(err)
	}
	// A low score releases nothing; a passing one does; anyone may present it.
	low, _ := evals.attestScored(job, "eval:legal-qa", `{"suite":"legal-qa-v2"}`, 61)
	if err := seller.claim(cond, low); err == nil {
		t.Fatal("low score paid")
	}
	high, _ := evals.attestScored(job, "eval:legal-qa", `{"suite":"legal-qa-v2"}`, 87)
	if err := seller.claim(cond, high); err != nil {
		t.Fatal(err)
	}
	fake, _ := arbiter.attestScored(job, "eval:legal-qa", "{}", 100)
	cond2, _ := firm.openConditional("tuneshop", "evalhub", job, "eval:legal-qa", 80, coins(1), week)
	if err := seller.claim(cond2, fake); err == nil {
		t.Fatal("an attestation from the wrong verifier released money")
	}
	bal, _ := agency.bankBalance("tuneshop")
	if want := coins(10); bal.Cmp(want) != 0 { // 5 deposit - 1 bond + 5 job + 1 sale
		t.Fatalf("agency balance %v", bal)
	}
	// The verifier finds a regression later and gives half the bond to the firm
	// (a slash names its bond: audit 4 MO-6).
	slash, _ := evals.decideSubject(bondSubj, "slash", []byte("regression on held-out set"), 5000)
	if err := firm.settle(bondID, slash); err != nil {
		t.Fatal(err)
	}
	selfSlash, _ := seller.easAttest(bondSubj, "slash", nil, 0, 0, common.Hash{})
	if err := seller.settle(bondID, selfSlash); err == nil {
		t.Fatal("only the slasher may decide")
	}

	// Subscription: 0.25 per day for 4 days; the agent's limit counts the whole lock.
	if _, err := buyer.subscribe("tuneshop", coins(1), 10, 86400); err == nil {
		t.Fatal("agent subscribed above its limit")
	}
	sub, err := buyer.subscribe("tuneshop", coins(0.25), 4, 86400)
	if err != nil {
		t.Fatal(err)
	}
	w.later(2 * 24 * time.Hour)
	if err := seller.collect(sub); err != nil {
		t.Fatal(err)
	}
	if err := seller.collect(sub); err == nil {
		t.Fatal("collected twice for the same periods")
	}
	if err := buyer.cancelSub(sub); err != nil {
		t.Fatal(err)
	}
	if err := seller.collect(sub); err == nil {
		t.Fatal("collected after cancellation")
	}

	// Payment channel: stream payments as tokens arrive, one claim at the end.
	ch, err := firm.openChannel("tuneshop", coins(2), week+86400*7)
	if err != nil {
		t.Fatal(err)
	}
	var last []byte
	for _, n := range []float64{0.1, 0.2, 0.35} { // per batch of tokens served
		last, _ = firm.voucher(ch, coins(n))
	}
	if err := seller.claimVoucher(ch, coins(0.35), last); err != nil {
		t.Fatal(err)
	}
	if err := seller.claimVoucher(ch, coins(1.9), last); err == nil {
		t.Fatal("a voucher for more than signed")
	}

	// Allowance: the firm lets someone else's contract (here any address) move up to 1 coin.
	spender, _ := crypto.GenerateKey()
	w.ch.transfer(w.faucet, crypto.PubkeyToAddress(spender.PublicKey), coins(1))
	if err := firm.approve(crypto.PubkeyToAddress(spender.PublicKey), coins(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ch.send(spender, nil, "Bank", "pull", idOf("lawfirm"), idOf("guild"), coins(0.4), common.Hash{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ch.send(spender, nil, "Bank", "pull", idOf("lawfirm"), idOf("guild"), coins(0.7), common.Hash{}); err == nil {
		t.Fatal("pulled beyond the allowance")
	}
	if err := buyer.approve(crypto.PubkeyToAddress(spender.PublicKey), coins(1)); err == nil {
		t.Fatal("an agent granted an allowance")
	}
}

// ---- social recipes: dating with private photos, paid reach, an emergency at night

func TestDatingAdsAndEmergency(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	aliceAgent := w.agent(alice, "match", coins(1), `{}`)
	bobAgent := w.agent(bob, "match", coins(1), `{}`)

	// Profiles in an open set; photos are artifacts, encrypted, off chain.
	if err := alice.createGroup("dating.mersin", true); err != nil {
		t.Fatal(err)
	}
	bobAgent.joinGroup("dating.mersin")
	aliceAgent.joinGroup("dating.mersin")
	photo := []byte("pretend this is a JPEG")
	ct, hash, photoKey, _ := artifact.SealFile(photo)
	art, err := aliceAgent.registerArtifact(hash, uint64(len(ct)), "ipfs://bafy-alice-photo", "private", big.NewInt(0), photoKey)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(map[string]any{"seeking": "man", "height_min": 180, "photo": map[string]any{"artifact": art, "encrypted": true}})
	if _, err := aliceAgent.postGroup("dating.mersin", "Looking for someone tall who likes hiking.", profile); err != nil {
		t.Fatal(err)
	}
	posts := scan(t, bobAgent)
	if len(posts) == 0 || posts[0].Mail == nil || len(posts[0].Mail.Data) == 0 {
		t.Fatal("bob's agent sees no profile")
	}

	// Secret mutual like: nothing shows until both sides like.
	if err := bobAgent.like("match.alice", "dating.mersin"); err != nil {
		t.Fatal(err)
	}
	bobAgent.like("match.alice", "dating.mersin") // liking twice from one side
	if m, _ := aliceAgent.mutual("match.bob", "dating.mersin"); m {
		t.Fatal("a one-sided like looked mutual")
	}
	if m, _ := carol.mutual("match.alice", "dating.mersin"); m {
		t.Fatal("an outsider computed the marker")
	}
	aliceAgent.like("match.bob", "dating.mersin")
	if m, _ := bobAgent.mutual("match.alice", "dating.mersin"); !m {
		t.Fatal("mutual like not detected")
	}
	// After the match, alice's agent sends the photo key, sealed.
	aliceAgent.setContact("bob", true)
	keyMsg, _ := json.Marshal(map[string]any{"artifact": art, "key": hex.EncodeToString(photoKey)})
	if _, err := aliceAgent.send(SendOpts{To: "match.bob", Intent: "offer", Text: "It's a match! Here is my photo.", Data: keyMsg}); err != nil {
		t.Fatal(err)
	}
	bobAgent.setContact("alice", true)
	var got struct {
		Key string `json:"key"`
	}
	for _, it := range scan(t, bobAgent) {
		if it.Mail != nil && it.Mail.Intent == "offer" {
			json.Unmarshal(it.Mail.Data, &got)
		}
	}
	k, _ := hex.DecodeString(got.Key)
	if plain, err := artifact.OpenFile(ct, k); err != nil || !bytes.Equal(plain, photo) {
		t.Fatalf("photo after match: %v", err)
	}

	// Paid reach: a vet clinic asks strangers for postage; a paying stranger gets through and wakes it.
	vet, _ := w.owner("vetclinic")
	vetAgent := w.agent(vet, "night", coins(1), `{}`)
	vet.setCard(`{"about":"Veterinary clinic, Mersin, 24/7 emergencies","postage":"0.2"}`)
	carol.deposit(coins(2))
	found, _ := carol.find("emergencies")
	if len(found) != 1 || found[0].Name != "vetclinic" {
		t.Fatalf("find: %+v", found)
	}
	carol.send(SendOpts{To: "night.vetclinic", Intent: "request", Text: "My cat is vomiting blood.", Data: json.RawMessage(`{"species":"cat","symptom":"hematemesis"}`)})
	unpaid := scan(t, vetAgent)
	if len(unpaid) != 1 || unpaid[0].Status != "quarantined" {
		t.Fatalf("unpaid: %+v", unpaid)
	}
	if _, err := carol.send(SendOpts{To: "night.vetclinic", Intent: "request", Text: "My cat is vomiting blood. Please help.", Postage: true}); err != nil {
		t.Fatal(err)
	}
	paid := scan(t, vetAgent)
	if len(paid) != 1 || paid[0].Status != "delivered" || !paid[0].Wake {
		t.Fatalf("paid: %+v", paid)
	}
	// The clinic's agent wakes its human, who answers with a phone number, sealed.
	if _, err := vetAgent.send(SendOpts{To: "vetclinic", Intent: "question", Text: "Emergency: cat vomiting blood (carol). Call her?"}); err != nil {
		t.Fatal(err)
	}
	vet.setContact("carol", true)
	if _, err := vet.send(SendOpts{To: "carol", Intent: "answer", Text: "Dr. Aylin here. Come now, or call +90 555 000 00 00."}); err != nil {
		t.Fatal(err)
	}
	carol.setContact("vetclinic", true)
	var answered bool
	for _, it := range scan(t, carol) {
		if it.From == "vetclinic" && it.Mail != nil && strings.Contains(it.Mail.Text, "+90") && it.Status == "delivered" {
			answered = true
		}
	}
	if !answered {
		t.Fatal("carol did not get the clinic's answer")
	}

	// Commit-reveal: a sealed bid stays sealed until revealed.
	hb, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	id, pre, err := carol.commitValue("auction:old-guitar", []byte("12.5"), hb.Time+3600)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := carol.revealed("auction:old-guitar"); len(r) != 0 {
		t.Fatal("bid visible before reveal")
	}
	carol.revealValue(id, pre)
	if r, _ := carol.revealed("auction:old-guitar"); len(r) != 1 || string(r[0]) != "12.5" {
		t.Fatalf("reveal: %q", r)
	}
}

// ---- anonymous by default, disclosure by choice

func TestAnonymousByDefault(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	kyc, _ := w.owner("kyc")
	app, _ := w.owner("datingapp")

	// 1. Private payments: the chain shows coins going to a new address, not to bob.
	if err := bob.enableStealth(); err != nil {
		t.Fatal(err)
	}
	if err := alice.payStealth("bob", coins(2), common.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	notes, err := bob.notes()
	if err != nil || len(notes) != 1 || notes[0].Amount.Cmp(coins(2)) != 0 {
		t.Fatalf("bob's private coins: %+v %v", notes, err)
	}
	if notes[0].Address == bob.addr() {
		t.Fatal("paid to bob's known address")
	}
	if others, _ := carol.enableStealth(), 0; others == nil {
		if ns, _ := carol.notes(); len(ns) != 0 {
			t.Fatal("carol found bob's payment")
		}
	}
	// Bob spends from the private coin: the payer of that transaction is the one-time address.
	fresh, _ := crypto.GenerateKey()
	if err := bob.sweep(notes[0], crypto.PubkeyToAddress(fresh.PublicKey)); err != nil {
		t.Fatal(err)
	}

	// 2. Private postage, paid from a private coin: nothing links carol to the clinic.
	vet, _ := w.owner("vetclinic")
	vet.enableStealth()
	night := w.agent(vet, "night", coins(1), `{}`) // the grant carries the view key for payments
	vet.setCard(`{"about":"24/7 vet","postage":"0.2"}`)
	carol.shield(coins(1))
	if _, err := carol.send(SendOpts{To: "night.vetclinic", Intent: "request", Text: "cat vomiting blood", Postage: true}); err != nil {
		t.Fatal(err)
	}
	got := scan(t, night)
	if len(got) != 1 || got[0].Status != "delivered" || !got[0].Wake {
		t.Fatalf("private postage: %+v", got)
	}
	if paid, _ := w.ch.events("Bank", "Paid"); len(paid) != 0 {
		t.Fatal("a public payment links the parties")
	}

	// 3. A relayer posts for us: our address never appears.
	rk, _ := crypto.GenerateKey()
	w.ch.transfer(w.faucet, crypto.PubkeyToAddress(rk.PublicKey), coins(10))
	relay := &Relay{ch: w.ch, key: rk}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	nonce, _ := w.ch.b.NonceAt(context.Background(), alice.addr(), nil)
	alice.relay = srv.URL
	alice.setContact("bob", true)
	bob.setContact("alice", true)
	if _, err := alice.send(SendOpts{To: "bob", Intent: "inform", Text: "via a relayer"}); err != nil {
		t.Fatal(err)
	}
	if err := relay.flush(); err != nil {
		t.Fatal(err)
	}
	if n, _ := w.ch.b.NonceAt(context.Background(), alice.addr(), nil); n != nonce {
		t.Fatal("alice sent a transaction herself")
	}
	if got := scan(t, bob); len(got) == 0 || got[len(got)-1].Mail.Text != "via a relayer" {
		t.Fatalf("relayed mail: %+v", got)
	}

	// 4. Anonymous membership: posts and votes by "a member", one vote each.
	alice.relay = ""
	alice.createGroup("site", true)
	for _, c := range []*Client{bob, carol} {
		if _, err := c.joinGroup("site"); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []*Client{alice, bob, carol} {
		if err := c.enableRing(); err != nil {
			t.Fatal(err)
		}
	}
	vote := func(c *Client, v string) {
		t.Helper()
		if _, err := c.postAnon("site", "poll:sunbeds", "my vote", json.RawMessage(`{"vote":"`+v+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	vote(alice, "yes")
	vote(bob, "no")
	vote(carol, "yes")
	vote(alice, "no") // the same member again: same key image, not counted twice
	scan(t, bob)
	tally := bob.tally("site", "poll:sunbeds")
	if tally["yes"] != 2 || tally["no"] != 1 {
		t.Fatalf("tally %v", tally)
	}
	for _, it := range bob.groupItems("site") {
		if it.Mail != nil && it.Mail.Thread == "poll:sunbeds" && !strings.HasPrefix(it.From, "anon:") {
			t.Fatalf("author revealed: %s", it.From)
		}
	}
	dave.enableRing()
	if _, err := dave.joinGroup("nonexistent-x"); err == nil {
		t.Fatal("joined a group that does not exist")
	}
	if _, err := dave.ringSign("site", "site|x", []byte("m")); err == nil {
		t.Fatal("a non-member made a ring signature for the group")
	}

	// 5. Disclosure by choice: an ID service vouches "over 18"; carol shows only that, only to the app.
	kyc.setContact("carol", true)
	carol.setContact("kyc", true)
	credHash, err := kyc.issue("carol", map[string]string{"name": "Carol X", "passport": "X1234567", "over_18": "true"}, 365)
	if err != nil {
		t.Fatal(err)
	}
	scan(t, carol)
	held := carol.heldCredentials()
	if len(held) != 1 {
		t.Fatalf("carol holds %d credentials", len(held))
	}
	p, err := carol.present(held[0], []string{"over_18"}, "datingapp")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := app.verifyPresentation(p, map[string]bool{"kyc": true})
	if err != nil || claims["over_18"] != "true" || claims["name"] != "" || claims["passport"] != "" {
		t.Fatalf("presentation: %v %v", claims, err)
	}
	if _, err := app.verifyPresentation(p, map[string]bool{"someone-else": true}); err == nil {
		t.Fatal("accepted an issuer the app does not trust")
	}
	if _, err := dave.verifyPresentation(p, map[string]bool{"kyc": true}); err == nil {
		t.Fatal("a presentation for the app worked for dave")
	}
	stolen := p[:len(p)-5] + "AAAAA"
	if _, err := app.verifyPresentation(stolen, map[string]bool{"kyc": true}); err == nil {
		t.Fatal("a tampered presentation verified")
	}
	if err := kyc.revokeCredential(credHash); err != nil {
		t.Fatal(err)
	}
	if _, err := app.verifyPresentation(p, map[string]bool{"kyc": true}); err == nil {
		t.Fatal("a revoked credential verified")
	}
}

// The network grows by vote (a new primitive, a new version): governance_test.go.

// A game with a pot for a winner not known in advance, reveal bonds that
// make walking away cost money without an arbiter, an open bounty, and a
// payee who gives money back early.
func TestOpenPotRevealBondGiveBack(t *testing.T) {
	w := newWorld(t)
	host, _ := w.owner("kemal")
	ref, _ := w.owner("hakem")
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	for _, c := range []*Client{alice, bob} {
		if err := c.deposit(coins(2)); err != nil {
			t.Fatal(err)
		}
	}
	now := func() uint64 { return uint64(alice.chainNow()) }
	// Both commit a number, post a reveal bond to the host, and stake into an open pot.
	ca, preA, err := alice.commitValue("arena:G3", []byte("2"), now()+600)
	if err != nil {
		t.Fatal(err)
	}
	cb, _, err := bob.commitValue("arena:G3", []byte("5"), now()+600)
	if err != nil {
		t.Fatal(err)
	}
	bondA, _, err := alice.revealBond(ca, "kemal", coins(0.1))
	if err != nil {
		t.Fatal(err)
	}
	bondB, _, err := bob.revealBond(cb, "kemal", coins(0.1))
	if err != nil {
		t.Fatal(err)
	}
	potA, subjA, err := alice.openBounty("hakem", coins(0.5), now()+86400, "arena G3 stake")
	if err != nil {
		t.Fatal(err)
	}
	potB, subjB, err := bob.openBounty("hakem", coins(0.5), now()+86400, "arena G3 stake")
	if err != nil {
		t.Fatal(err)
	}
	// An open pot can't be released by the payer: there's no payee yet.
	if err := alice.releaseLock(potA, nil); err == nil {
		t.Fatal("released a pot with no payee")
	}
	// Alice reveals; Bob, seeing he'd lose, walks away.
	if err := alice.revealValue(ca, preA); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.settleRevealBond(bondA); err != nil {
		t.Fatalf("revealed bond not returned: %v", err)
	}
	if _, err := host.settleRevealBond(bondB); err == nil {
		t.Fatal("bond forfeited before the deadline")
	}
	w.later(11 * time.Minute)
	if res, err := host.settleRevealBond(bondB); err != nil || !strings.Contains(res, "payee") {
		t.Fatalf("unrevealed bond: %v %v", res, err)
	}
	// The referee awards both pots to the winner; nobody else can name a payee.
	fake, _ := bob.award(subjB, "bob", 10000, "I win")
	if err := bob.settle(potB, fake); err == nil {
		t.Fatal("a self-made award paid out")
	}
	for _, p := range []struct {
		id   uint64
		subj common.Hash
	}{{potA, subjA}, {potB, subjB}} {
		uid, err := ref.award(p.subj, "alice", 10000, "lowest unique 2; bob did not reveal")
		if err != nil {
			t.Fatal(err)
		}
		if err := host.settle(p.id, uid); err != nil {
			t.Fatal(err)
		}
	}
	// The payee is fixed now: a second award to someone else moves nothing.
	again, _ := ref.award(subjA, "bob", 10000, "changed my mind")
	if err := bob.settle(potA, again); err == nil {
		t.Fatal("the payee of a pot changed after it was paid")
	}
	ab, _ := alice.bankBalance("alice")
	bb, _ := bob.bankBalance("bob")
	kb, _ := host.bankBalance("kemal")
	// alice: 2 - 0.5 + 1.0 (both pots) ; bob: 2 - 0.5 - 0.1 ; host: bob's bond 0.1
	wantB := new(big.Int).Sub(new(big.Int).Sub(coins(2), coins(0.5)), coins(0.1))
	if ab.Cmp(coins(2.5)) != 0 || bb.Cmp(wantB) != 0 || kb.Cmp(coins(0.1)) != 0 {
		t.Fatalf("balances alice %v bob %v kemal %v", ab, bb, kb)
	}

	// An open bounty: whoever solves it, named by the evaluator, gets paid.
	bounty, bsubj, err := alice.openBounty("hakem", coins(1), now()+86400, "fix issue #3")
	if err != nil {
		t.Fatal(err)
	}
	win, _ := ref.award(bsubj, "bob", 10000, "PR by bob passes the tests")
	if err := bob.settle(bounty, win); err != nil {
		t.Fatal(err)
	}
	// Give-back: the payee returns an unused deposit right away, no deadline.
	dep, err := alice.lock("bob", coins(0.4), now()+30*86400, "AttestationCondition", mustDemand(t, alice, "hakem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.giveBack(dep, nil); err == nil {
		t.Fatal("the payer gave back to itself")
	}
	if err := bob.giveBack(dep, coins(0.4)); err != nil {
		t.Fatal(err)
	}
	ab2, _ := alice.bankBalance("alice")
	if ab2.Cmp(coins(1.5)) != 0 { // 2.5 - 1 (bounty) - 0.4 + 0.4
		t.Fatalf("after give-back alice has %v", ab2)
	}
}

func mustDemand(t *testing.T, c *Client, arbiter string) []byte {
	d, err := c.attestationDemand(arbiter, dealSubject("deposit"), "ruling", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Money paid outside the network: claimed by the payer, confirmed by the
// payee, and able to release a lock (coins for a bank transfer).
func TestFiatPaymentConfirmed(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	if err := bob.deposit(coins(3)); err != nil {
		t.Fatal(err)
	}
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	// Bob sells 1 coin for 300 TRY: the coin waits in a lock for his own confirmation.
	sale, err := bob.openConditional("alice", "bob", "alice", "fiat:received", 1, coins(1), head.Time+86400)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := alice.fiatPaid(FiatPayment{To: "bob", Amount: "300", Currency: "TRY", Method: "bank transfer", Ref: "TR-4471"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := carol.fiatAnswer(claim, true, "sure"); err == nil {
		t.Fatal("a third party confirmed someone else's payment")
	}
	l, _ := bob.fiatList("alice")
	if len(l) != 1 || l[0]["status"] != "claimed, not confirmed yet" {
		t.Fatalf("list before: %v", l)
	}
	ans, err := bob.fiatAnswer(claim, true, "arrived")
	if err != nil {
		t.Fatal(err)
	}
	l, _ = alice.fiatList("bob")
	if len(l) != 1 || l[0]["status"] != "confirmed by bob" {
		t.Fatalf("list after: %v", l)
	}
	if err := alice.claim(sale, ans); err != nil {
		t.Fatalf("confirmation did not release the coin: %v", err)
	}
	if b, _ := alice.bankBalance("alice"); b.Cmp(coins(1)) != 0 {
		t.Fatalf("alice has %v", b)
	}
}

// Push: what concerns you arrives as events, so agents wait instead of polling.
func TestPushEvents(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	if err := alice.deposit(coins(5)); err != nil {
		t.Fatal(err)
	}
	types := func(evs []Event) map[string]int {
		m := map[string]int{}
		for _, e := range evs {
			m[e.Type]++
		}
		return m
	}
	poll := func(c *Client, groups ...string) map[string]int {
		g := map[string]bool{}
		for _, x := range groups {
			g[x] = true
		}
		evs, err := c.pollEvents(g, false)
		if err != nil {
			t.Fatal(err)
		}
		return types(evs)
	}
	poll(bob)   // the first call only learns the past
	poll(alice) // likewise
	alice.send(SendOpts{To: "bob", Text: "TV still for sale?"})
	if err := alice.pay("bob", coins(0.5), "deposit"); err != nil {
		t.Fatal(err)
	}
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	lockID, subj, err := alice.openEscrow("bob", "carol", coins(1), head.Time+600, "fix the bike")
	if err != nil {
		t.Fatal(err)
	}
	carol.createGroup("bike-club", true)
	got := poll(bob, "bike-club")
	for _, want := range []string{"mail", "paid", "lock-opened", "group-created"} {
		if got[want] == 0 {
			t.Fatalf("bob missed %s: %v", want, got)
		}
	}
	// The arbiter rules: both sides hear of it, with the command to apply it.
	if _, err := carol.rule(subj, 10000, "done"); err != nil {
		t.Fatal(err)
	}
	if got := poll(bob); got["decision"] != 1 {
		t.Fatalf("bob: %v", got)
	}
	if got := poll(alice); got["decision"] != 1 || got["lock-opened"] != 1 {
		t.Fatalf("alice: %v", got)
	}
	// Deadlines come by the chain clock: a reveal is due, a refund is available.
	if _, _, err := bob.commitValueKept("game", "7", uint64(bob.chainNow())+120); err != nil {
		t.Fatal(err)
	}
	if got := poll(bob); got["reveal-due"] != 1 {
		t.Fatalf("no reveal reminder: %v", got)
	}
	if got := poll(bob); got["reveal-due"] != 0 {
		t.Fatal("reminded twice")
	}
	w.later(15 * time.Minute)
	if got := poll(alice); got["refund-available"] != 1 {
		t.Fatalf("no refund notice for lock %d: %v", lockID, got)
	}
	// Nothing that isn't yours: carol sees none of bob's mail or money.
	if got := poll(carol); got["mail"] != 0 || got["paid"] != 0 {
		t.Fatalf("carol saw others' events: %v", got)
	}
}

// Primitives for worlds: randomness, a clock, items, tokens, splits, panels.
func TestWorldPrimitives(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	erin, _ := w.owner("erin")
	for _, c := range []*Client{alice, bob, carol, dave} {
		if err := c.deposit(coins(5)); err != nil {
			t.Fatal(err)
		}
	}
	now := func() uint64 { return uint64(alice.chainNow()) }
	bal := func(c *Client) *big.Int { b, _ := c.bankBalance(c.f.Name); return b }
	delta := func(before *big.Int, c *Client) *big.Int { return new(big.Int).Sub(bal(c), before) }

	// Randomness: entries made after the cut-off don't count, nobody draws early.
	if err := alice.openRound("dice:1", now()+60, now()+600); err != nil {
		t.Fatal(err)
	}
	ca, pa, _ := alice.commitValue("dice:1", []byte("alice-seed"), now()+500)
	cb, pb, _ := bob.commitValue("dice:1", []byte("bob-seed"), now()+500)
	w.later(70 * time.Second)
	cc, pc, _ := carol.commitValue("dice:1", []byte("late"), now()+300)
	carol.revealValue(cc, pc)
	alice.revealValue(ca, pa)
	if _, _, err := alice.drawRound("dice:1"); err == nil {
		t.Fatal("drew before every entry was revealed")
	}
	bob.revealValue(cb, pb)
	seed, n, err := carol.drawRound("dice:1")
	if err != nil || n != 2 || seed == (common.Hash{}) {
		t.Fatalf("draw: %v entries %d (the late one must not count)", err, n)
	}
	if r, err := bob.roll("dice:1", 6, 0); err != nil || r > 5 {
		t.Fatalf("roll %d %v", r, err)
	}

	// The clock: bob stops moving; his stake goes to alice, hers comes back.
	g, err := alice.startGame([]string{"alice", "bob"}, "", 60)
	if err != nil {
		t.Fatal(err)
	}
	ba, _ := alice.clockBond(g, "bob", exact("0.2"), now()+86400)
	bb, _ := bob.clockBond(g, "alice", exact("0.2"), now()+86400)
	if err := bob.clockCall("move", g, []byte("e4")); err == nil {
		t.Fatal("moved out of turn")
	}
	bob.pollEvents(nil, false)
	if err := alice.clockCall("move", g, []byte("e4")); err != nil {
		t.Fatal(err)
	}
	if evs, _ := bob.pollEvents(nil, false); len(evs) == 0 || evs[len(evs)-1].Type != "your-move" {
		t.Fatalf("bob wasn't told it's his move: %+v", evs)
	}
	if err := carol.clockCall("timeout", g); err == nil {
		t.Fatal("timed out early")
	}
	w.later(61 * time.Second)
	if err := carol.clockCall("timeout", g); err != nil {
		t.Fatal(err)
	}
	a0 := bal(alice)
	if err := alice.claimLock(bb, nil); err != nil {
		t.Fatalf("timeout stake: %v", err)
	}
	if err := alice.refundEarly(ba, nil); err != nil {
		t.Fatalf("own stake back: %v", err)
	}
	if d := delta(a0, alice); d.Cmp(new(big.Int).Add(exact("0.2"), exact("0.2"))) != 0 {
		t.Fatalf("alice got %v", d)
	}

	// Items: an atomic sale for a stablecoin; soulbound items stay put.
	sword, err := alice.createItem("ipfs://sword.json", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.mintItem(sword, "alice", 2); err != nil {
		t.Fatal(err)
	}
	diploma, _ := carol.createItem("ipfs://diploma-bob.json", 1, true)
	if err := carol.mintItem(diploma, "bob", 1); err != nil {
		t.Fatal(err)
	}
	if err := bob.giveItem(diploma, "dave", 1); err == nil {
		t.Fatal("a soulbound item moved")
	}
	usd, _ := alice.tokenAddr("usd")
	alice.asset, bob.asset = usd, usd
	if err := bob.mintUSD(bob.addr(), coins(20)); err != nil {
		t.Fatal(err)
	}
	if err := bob.depositToken("bob", coins(20)); err != nil {
		t.Fatal(err)
	}
	offer, err := alice.offerItem(sword, 1, coins(5), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.buyItem(offer); err != nil {
		t.Fatal(err)
	}
	if n, _ := bob.itemBalance("bob", sword); n != 1 {
		t.Fatalf("bob has %d swords", n)
	}
	if b, _ := alice.tokenBalance("alice"); b.Cmp(coins(5)) != 0 {
		t.Fatalf("alice was paid %v tUSD", b)
	}
	// An agent can't move tokens until its owner sets a limit for that token.
	shop := w.agent(bob, "shop", coins(1), `{}`)
	shop.asset = usd
	if err := shop.pay("alice", coins(1), "tip"); err == nil {
		t.Fatal("an agent spent tokens without a limit")
	}
	if err := bob.setTokenLimit("shop.bob", coins(2)); err != nil {
		t.Fatal(err)
	}
	if err := shop.pay("alice", coins(1), "tip"); err != nil {
		t.Fatal(err)
	}
	if err := shop.pay("alice", coins(1.5), "more"); err == nil {
		t.Fatal("over the token limit")
	}
	alice.asset, bob.asset = common.Address{}, common.Address{}

	// A split: what is paid to it is shared at once.
	split, err := alice.createSplit([]string{"alice", "carol"}, []uint16{7000, 3000})
	if err != nil {
		t.Fatal(err)
	}
	a1, c1 := bal(alice), bal(carol)
	if err := dave.pay(split.Hex(), coins(1), "game sales"); err != nil {
		t.Fatal(err)
	}
	if delta(a1, alice).Cmp(exact("0.7")) != 0 || delta(c1, carol).Cmp(exact("0.3")) != 0 {
		t.Fatalf("split: alice %v carol %v", delta(a1, alice), delta(c1, carol))
	}

	// A panel: 2 of 3 decide, after a hearing; evidence is sealed to the panel.
	lock, subj, err := alice.openPanel("bob", coins(1), now()+86400, 2, []string{"carol", "dave", "erin"}, "build the castle", now()+300)
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.acceptLock(lock); err != nil { // bob accepts the panel as his judges
		t.Fatal(err)
	}
	proof := filepath.Join(t.TempDir(), "photos.txt")
	os.WriteFile(proof, []byte("castle photo: only two towers of four"), 0o600)
	if _, err := alice.panelEvidence(lock, proof, "the castle is half done"); err != nil {
		t.Fatal(err)
	}
	early, _ := carol.rule(subj, 10000, "before hearing anyone")
	if _, err := carol.panelRule(lock, 10000, "too early"); err == nil {
		t.Fatal("the client let a panelist rule during the hearing")
	}
	view, err := dave.panelView(lock)
	if err != nil {
		t.Fatal(err)
	}
	ev := view["evidence"].([]map[string]any)
	if len(ev) == 0 || !strings.Contains(fmt.Sprint(ev[0]["text"]), "two towers") {
		t.Fatalf("the panelist can't read the evidence: %+v", ev)
	}
	w.later(301 * time.Second)
	if _, err := bob.panelEvidence(lock, proof, "too late"); err == nil {
		t.Fatal("evidence taken after the hearing")
	}
	r2, _ := dave.panelRule(lock, 6000, "mostly done")
	if err := bob.settlePanel(lock, []common.Hash{early, r2}); err == nil {
		t.Fatal("a ruling made during the hearing counted")
	}
	if _, err := carol.panelRule(lock, 10000, "done"); err != nil {
		t.Fatal(err)
	}
	b2, a2 := bal(bob), bal(alice)
	if err := erin.panelSettleAuto(lock); err != nil {
		t.Fatal(err)
	}
	if delta(b2, bob).Cmp(exact("0.6")) != 0 || delta(a2, alice).Cmp(exact("0.4")) != 0 {
		t.Fatalf("panel: bob %v alice %v", delta(b2, bob), delta(a2, alice))
	}
}

func exact(s string) *big.Int { v, _ := parseCoins(s); return v }

// Smart accounts: the owner signs with a passkey, the agent with a session
// key, and the agent's gas is paid by the owner's paymaster deposit, within a
// budget. Neither key holds ETH.
func TestSmartAccountsPasskeys(t *testing.T) {
	w := newWorld(t)
	alice := w.passkeyOwner("alice")
	bob, _ := w.owner("bob")
	if code, _ := w.ch.b.CodeAt(context.Background(), alice.me(), nil); len(code) == 0 {
		t.Fatal("alice's account was not deployed by her first operation")
	}
	if err := alice.deposit(coins(5)); err != nil {
		t.Fatal(err)
	}
	if b, _ := alice.bankBalance("alice"); b.Cmp(coins(5)) != 0 {
		t.Fatalf("alice's bank: %v", b)
	}
	bot := w.accountAgent(alice, "bot", coins(1), exact("0.01"))
	if err := bot.pay("bob", exact("0.5"), "within the limit"); err != nil {
		t.Fatal(err)
	}
	if err := bot.pay("bob", exact("0.6"), "over the limit"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the Bank's limit was not enforced through the account: %v", err)
	}
	// Mail from an agent account is verified as the agent's.
	bob.setContact("alice", true)
	if _, err := bot.send(SendOpts{To: "bob", Text: "hello from an agent with no ETH"}); err != nil {
		t.Fatal(err)
	}
	got := mailTo(scan(t, bob), tMessage)
	if len(got) != 1 || !strings.HasPrefix(got[0].Verified, "ok") {
		t.Fatalf("mail from the agent account: %+v", got)
	}
	// A claim made by the owner's account counts as alice's.
	if _, err := alice.attestScored("bob", "reference", "reliable", 90); err != nil {
		t.Fatal(err)
	}
	if cls, _ := bob.attestationsAbout("bob", map[string]bool{"alice": true}); len(cls) != 1 {
		t.Fatalf("alice's claim through her account: %+v", cls)
	}
	// The agent's message fee came from alice's Bank balance, within its limit.
	if b, _ := w.ch.balance(bot.me()); b.Sign() != 0 {
		t.Fatalf("the agent's account holds %v wei: it needs none", b)
	}
	// No signing key ever held ETH: gas came from accounts or the paymaster.
	for _, k := range []common.Address{alice.addr(), bot.addr()} {
		if b, _ := w.ch.balance(k); b.Sign() != 0 {
			t.Fatalf("a signing key holds %v wei", b)
		}
	}
	// The agent's gas budget runs out: it stops, whatever it tries.
	stopped := false
	for i := 0; i < 80 && !stopped; i++ {
		if err := bot.pay("bob", exact("0.001"), "spam"); err != nil {
			if !strings.Contains(err.Error(), "gas budget") {
				t.Fatalf("unexpected: %v", err)
			}
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("the agent's gas budget never ran out")
	}
	// Someone with another passkey can't act for alice's account.
	thief, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	r := *w.ch.routes[alice.addr()]
	r.Passkey = thief
	cd, _ := w.ch.executeCallData(w.ch.addr("Bank"), nil, mustPack(t, w.ch, "Bank", "withdraw", idOf("alice"), crypto.PubkeyToAddress(thief.PublicKey), coins(1)))
	if _, err := w.ch.userOp(&r, cd); err == nil {
		t.Fatal("a stranger's passkey moved alice's money")
	}
}

func mustPack(t *testing.T, ch *Chain, contract, method string, args ...any) []byte {
	b, err := ch.abis[contract].Pack(method, args...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Light (X25519) and post-quantum (hybrid) readers mix: each sender seals to
// the keys its readers published. Post-quantum costs bytes, not compatibility.
func TestLightAndPostQuantumMail(t *testing.T) {
	w := newWorld(t)
	carol, _ := w.ownerWith("carol", true) // post-quantum keys
	dave, _ := w.owner("dave")             // light keys
	carol.setContact("dave", true)
	dave.setContact("carol", true)
	sendSized := func(from *Client, to, text string) []int {
		var envs []Envelope
		capture = func(e Envelope) { envs = append(envs, e) }
		_, err := from.send(SendOpts{To: to, Text: text})
		capture = nil
		if err != nil {
			t.Fatal(err)
		}
		if err := from.post(envs); err != nil {
			t.Fatal(err)
		}
		var sizes []int
		for _, e := range envs {
			sizes = append(sizes, len(e.encode()))
		}
		return sizes
	}
	toPQ := sendSized(dave, "carol", "hello from a light key")
	toLight := sendSized(carol, "dave", "hello from a post-quantum key")
	if len(toPQ) != 2 || len(toLight) != 2 {
		t.Fatalf("mixed kinds should give one envelope per family: %v %v", toPQ, toLight)
	}
	byFrom := func(items []*Item) map[string]string {
		m := map[string]string{}
		for _, it := range items {
			m[it.From] = it.Mail.Text
		}
		return m
	}
	c := byFrom(mailTo(scan(t, carol), tMessage))
	d := byFrom(mailTo(scan(t, dave), tMessage))
	if c["dave"] != "hello from a light key" || d["carol"] != "hello from a post-quantum key" {
		t.Fatalf("received: carol %v, dave %v", c, d)
	}
	if c["carol"] == "" || d["dave"] == "" {
		t.Fatalf("a sender lost its own copy: carol %v, dave %v", c, d)
	}
	// Each side still sees its own sent copy.
	t.Logf("envelopes (theirs, ours): light->pq %v bytes, pq->light %v bytes", toPQ, toLight)
}

// startRelay: a relayer (pool withdrawals, secret votes) on a test server; new owners use it.
func (w *world) startRelay() *Relay {
	w.t.Helper()
	rk, _ := crypto.GenerateKey()
	if err := w.ch.transfer(w.faucet, crypto.PubkeyToAddress(rk.PublicKey), coins(100)); err != nil {
		w.t.Fatal(err)
	}
	r := &Relay{ch: w.ch, key: rk}
	srv := httptest.NewServer(r.Handler())
	w.t.Cleanup(srv.Close)
	w.relay = srv.URL
	return r
}
