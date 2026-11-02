//go:build redteam || redteam_d

// Red team, scenario D: on-chain graph analysis. The adversary has only the
// chain (every log of every contract, every transaction's sender and
// calldata) and, in one part, a seat in an anonymous group. It rebuilds who
// dealt with whom, who reviewed whom, who is in which group and who mailed
// whom, and we measure what share of the true edges it recovers.
//
// Run: go test -tags redteam_d -run TestRedTeamD -count=1 -v ./cmd/ternchain
package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ---- the adversary's view: logs, transactions and their senders only

type rtdView struct {
	t        *testing.T
	w        *world
	byAddr   map[common.Address]string // a key's address => the name it acts for (Registry events)
	byID     map[common.Hash]string    // a name's id => name
	envFrom  map[common.Hash]common.Address
	envBlock map[common.Hash]uint64
	txFrom   map[common.Hash]common.Address
}

func rtdObserve(t *testing.T, w *world) *rtdView {
	t.Helper()
	v := &rtdView{t: t, w: w, byAddr: map[common.Address]string{}, byID: map[common.Hash]string{},
		envFrom: map[common.Hash]common.Address{}, envBlock: map[common.Hash]uint64{}, txFrom: map[common.Hash]common.Address{}}
	owners, err := w.ch.events("Registry", "OwnerRegistered")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range owners {
		name, _ := e.Args["name"].(string)
		if a, ok := e.Args["controller"].(common.Address); ok {
			v.byAddr[a] = name
		}
		v.byID[idOf(name)] = name
	}
	agents, _ := w.ch.events("Registry", "AgentGranted")
	for _, e := range agents {
		name, _ := e.Args["name"].(string)
		var owner string
		if o, ok := e.Args["owner"].([32]byte); ok {
			owner = v.byID[common.Hash(o)]
		}
		full := name
		if owner != "" && !strings.Contains(name, ".") {
			full = name + "." + owner
		}
		if a, ok := e.Args["agent"].(common.Address); ok {
			v.byAddr[a] = full
		}
		v.byID[idOf(full)] = full
	}
	// Every envelope on chain, with the sender of the transaction that carried it.
	posted, err := w.ch.events("Inbox", "Posted")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range posted {
		b, _ := e.Args["batch"].([]byte)
		envs, err := decodeBatch(b)
		if err != nil {
			continue
		}
		from := v.sender(e.Tx)
		for _, env := range envs {
			v.envFrom[env.hash()] = from
			v.envBlock[env.hash()] = e.Block
		}
	}
	return v
}

func (v *rtdView) sender(tx common.Hash) common.Address {
	if a, ok := v.txFrom[tx]; ok {
		return a
	}
	x, _, err := v.w.sim.Client().TransactionByHash(context.Background(), tx)
	if err != nil {
		v.t.Fatalf("tx %s: %v", tx, err)
	}
	a, err := types.Sender(types.LatestSignerForChainID(x.ChainId()), x)
	if err != nil {
		v.t.Fatal(err)
	}
	v.txFrom[tx] = a
	return a
}

func (v *rtdView) name(a common.Address) string {
	if n, ok := v.byAddr[a]; ok {
		return n
	}
	return a.Hex()[:10]
}

func (v *rtdView) idName(x any) string {
	if b, ok := x.([32]byte); ok {
		if n, ok := v.byID[common.Hash(b)]; ok {
			return n
		}
		return common.Hash(b).Hex()[:10]
	}
	return fmt.Sprint(x)
}

func rtdFrac(got, want int) string {
	if want == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", got, want, 100*float64(got)/float64(want))
}

func rtdHead(t *testing.T, w *world) uint64 {
	h, err := w.ch.b.HeaderByNumber(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return h.Time
}

// ---- D1: mail graph, deals, reviews, Sets groups, agents

func TestRedTeamD_GraphAnalysis(t *testing.T) {
	t.Setenv("TERNCHAIN_ZK_DEVNET_DIR", t.TempDir())
	w := newWorld(t)
	people := map[string]*Client{}
	for _, n := range []string{"alice", "bob", "carol", "dave", "erin", "frank", "guild"} {
		people[n], _ = w.owner(n)
	}
	al, bo, ca, da, er, fr := people["alice"], people["bob"], people["carol"], people["dave"], people["erin"], people["frank"]
	helper := w.agent(al, "helper", coins(5), `{}`)
	for _, c := range []*Client{al, ca, da, er} {
		must(t, c.deposit(coins(30)))
	}

	// True mail edges (sender => recipient). bob asks strangers for postage.
	must(t, bo.setCard(`{"about":"bob's shop","postage":"0.1"}`))
	type edge struct{ from, to, env string }
	var mail []edge
	sendM := func(c *Client, o SendOpts) {
		t.Helper()
		id, err := c.send(o)
		if err != nil {
			t.Fatalf("%s -> %s: %v", c.f.Name, o.To, err)
		}
		mail = append(mail, edge{c.f.Name, o.To, id})
	}
	sendM(al, SendOpts{To: "carol", Text: "lunch?"})
	sendM(ca, SendOpts{To: "dave", Text: "the report"})
	sendM(er, SendOpts{To: "bob", Text: "a quote please", Intent: "request", Postage: true})
	sendM(fr, SendOpts{To: "erin", Text: "hi"})
	sendM(helper, SendOpts{To: "alice", Text: "done, boss"})
	sendM(da, SendOpts{To: "bob", Text: "price?", Intent: "request", Postage: true})

	// Deals: a public review, two private ones.
	l1 := paidLock(t, w, al, "bob", "guild", 2)
	rd, err := al.reviewData("bob", l1, "good")
	must(t, err)
	if _, err := al.attestScored("bob", "review", rd, 90); err != nil {
		t.Fatal(err)
	}
	type deal struct {
		lock         uint64
		payer, payee string
	}
	priv := []deal{{paidLock(t, w, ca, "dave", "guild", 1), "carol", "dave"}, {paidLock(t, w, er, "bob", "guild", 1), "erin", "bob"}}
	for i, d := range priv {
		payee := people[d.payee]
		if _, err := payee.repTagSend(d.lock); err != nil {
			t.Fatal(err)
		}
		payer := people[d.payer]
		scan(t, payer)
		must(t, payer.repReview(d.lock, []int{40, -20}[i]))
	}

	// A private Sets group.
	must(t, al.createGroup("club", false))
	for _, c := range []*Client{bo, ca} {
		if _, err := c.joinGroup("club"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := al.admitPending(); err != nil {
		t.Fatal(err)
	}
	clubMembers := []string{"alice", "bob", "carol"}

	// ---- the adversary
	v := rtdObserve(t, w)

	// Mail: who sent each envelope (the transaction's sender), and to whom
	// (a Bank payment whose memo is the envelope's hash: paid postage).
	memoTo := map[common.Hash]string{}
	for _, ev := range []string{"Paid", "TokenPaid"} {
		ps, _ := w.ch.events("Bank", ev)
		for _, p := range ps {
			if m, ok := p.Args["memo"].([32]byte); ok && m != [32]byte{} {
				memoTo[common.Hash(m)] = v.idName(p.Args["to"])
			}
		}
	}
	sendersOK, recipOK := 0, 0
	for _, e := range mail {
		h := common.HexToHash(e.env)
		from, ok := v.envFrom[h]
		if ok && v.name(from) == e.from {
			sendersOK++
		}
		if memoTo[h] == e.to {
			recipOK++
		}
		t.Logf("mail %s->%s: on chain from %q, postage memo names %q", e.from, e.to, v.name(from), memoTo[h])
	}
	// FINDING D-1: every envelope's sender is the transaction's sender (no relay, default privacy).
	if sendersOK != len(mail) {
		t.Fatalf("senders recovered %d/%d", sendersOK, len(mail))
	}
	// FINDING D-2: paid postage names the recipient and the exact envelope (memo = envelope hash).
	if recipOK != 2 {
		t.Fatalf("recipients through postage: %d, want the 2 paid ones", recipOK)
	}
	t.Logf("RESULT D-1 mail senders recovered: %s (tx sender of the Inbox batch)", rtdFrac(sendersOK, len(mail)))
	t.Logf("RESULT D-2 mail recipients recovered: %s (all paid-postage ones: memo = envelope hash)", rtdFrac(recipOK, len(mail)))

	// Agent <=> owner: grants are public (by design).
	agOK := 0
	if v.name(helper.addr()) == "helper.alice" {
		agOK++
	}
	t.Logf("RESULT D-3 agent->owner links: %s (AgentGranted; by design)", rtdFrac(agOK, 1))

	// Deals (Bank, public by design) and reviews.
	locked, _ := w.ch.events("Bank", "Locked")
	lockParties := map[uint64][2]string{}
	for _, e := range locked {
		id := e.Args["id"].(*big.Int).Uint64()
		lockParties[id] = [2]string{v.idName(e.Args["from"]), v.idName(e.Args["payee"])}
	}
	t.Logf("RESULT D-4 deals (Bank.Locked, by design): %d locks, payer and payee of each", len(lockParties))

	pub := 0
	for n := range people {
		as, _ := al.attestationsAbout(n, nil)
		for _, a := range as {
			if a.Kind == "review" && a.Score == 90 && ownerName(a.Subject) == "bob" {
				pub++
			}
		}
	}
	t.Logf("RESULT D-5 public reviews recovered with score: %s (EAS, by design)", rtdFrac(pub, 1))

	// Private reviews: the event carries the lock; the lock names both sides.
	revs, _ := w.ch.events("PrivateReviews", "Review")
	linked := 0
	for _, e := range revs {
		id := e.Args["lock"].(*big.Int).Uint64()
		p := lockParties[id]
		for _, d := range priv {
			if d.lock == id && p[0] == d.payer && p[1] == d.payee && v.name(v.sender(e.Tx)) == d.payer {
				linked++
			}
		}
	}
	// FINDING D-5b: a "private" review is linked to payer and payee by its lock id
	// (docs/PRIVACY.md: "not the score, not whose it is"; THREATS-STATE V2: "the leaf hides the payee").
	if linked != len(priv) {
		t.Fatalf("private reviews linked: %d/%d", linked, len(priv))
	}
	t.Logf("RESULT D-6 private reviews linked to (payer, payee): %s; scores recovered 0/%d", rtdFrac(linked, len(priv)), len(priv))

	// A reputation proof for a verifier: its count against the payee's public set of reviewed locks.
	p, err := da.repProve("nina 2026-10-29 d1", 0, -101)
	must(t, err)
	reviewedOfDave := 0
	for _, e := range revs {
		if lockParties[e.Args["lock"].(*big.Int).Uint64()][1] == "dave" {
			reviewedOfDave++
		}
	}
	// FINDING D-6b: when the count equals the prover's public number of reviewed locks,
	// the verifier knows exactly which deals (and counterparts) back the proof.
	if p.Count != reviewedOfDave {
		t.Fatalf("proof count %d, dave's reviewed locks on chain %d", p.Count, reviewedOfDave)
	}
	t.Logf("RESULT D-7 rep proof by dave: %d deals; the chain lists exactly %d reviewed locks with dave as payee => the deals behind the proof are identified", p.Count, reviewedOfDave)

	// Sets groups: Added events name every member (by design for Sets).
	added, _ := w.ch.events("Sets", "Added")
	got := map[string]bool{}
	for _, e := range added {
		if common.Hash(e.Args["id"].([32]byte)) == setID("club") {
			got[v.idName(e.Args["member"])] = true
		}
	}
	mem := 0
	for _, m := range clubMembers {
		if got[m] {
			mem++
		}
	}
	t.Logf("RESULT D-8 private Sets group members recovered: %s (by design: Sets names members; use --anonymous)", rtdFrac(mem, len(clubMembers)))
	if mem != len(clubMembers) {
		t.Fatalf("Sets members %v", got)
	}
}

// ---- D2: anonymous groups

// An adversary in the group (any member, or the admin) maps each post's
// envelope to the transaction that carried it: the pseudonym gets a name.
// An outsider maps join envelopes to the admin's Leaves batch by timing.
func TestRedTeamD_AnonGroupDeanonymisation(t *testing.T) {
	w := newWorld(t)
	dave, _ := w.owner("dave")
	erin, _ := w.owner("erin")
	frank, _ := w.owner("frank") // the infiltrator, an ordinary member
	noise := []*Client{}
	for _, n := range []string{"alice", "bob", "carol"} {
		c, _ := w.owner(n)
		noise = append(noise, c)
	}
	// Background traffic: the noise names mail each other around the joins.
	chat := func(round int) {
		for i, c := range noise {
			if _, err := c.send(SendOpts{To: noise[(i+1)%len(noise)].f.Name, Text: fmt.Sprint("round ", round)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	chat(0)
	start := rtdBlock(t, w)
	// anonCell's steps, with the noise names mailing in between each of them.
	oscar, _ := w.owner("oscar")
	if _, err := oscar.createAnonGroup("cell", false, 0); err != nil {
		t.Fatal(err)
	}
	for i, m := range []*Client{dave, erin, frank} {
		chat(10 + i)
		if _, err := oscar.inviteAnon("cell", m.f.Name); err != nil {
			t.Fatal(err)
		}
		scan(t, m)
		chat(20 + i)
		if _, err := m.joinGroup("cell"); err != nil {
			t.Fatal(err)
		}
		chat(30 + i)
	}
	scan(t, oscar)
	if done, err := oscar.admitPending(); err != nil || len(done) != 3 {
		t.Fatalf("admit: %v %v", done, err)
	}
	for _, m := range []*Client{dave, erin, frank} {
		scan(t, m)
	}
	chat(1)
	members := map[string]bool{"dave": true, "erin": true, "frank": true}

	// Outsider: the group's Leaves batch, and who posted envelopes between the group's
	// creation and the batch (the invitations come from the admin; the joins from members).
	v := rtdObserve(t, w)
	leaves, _ := w.ch.events("AnonGroups", "Leaves")
	created, _ := w.ch.events("AnonGroups", "Created")
	if len(leaves) == 0 || len(created) == 0 {
		t.Fatal("no anonymous group on chain")
	}
	admin := v.name(created[0].Args["admin"].(common.Address))
	lb := leaves[len(leaves)-1].Block
	cand := map[string]bool{}
	for h, from := range v.envFrom {
		if b := v.envBlock[h]; b > start && b < lb && v.name(from) != admin {
			cand[v.name(from)] = true
		}
	}
	tp := 0
	for n := range cand {
		if members[n] {
			tp++
		}
	}
	t.Logf("RESULT D-9 outsider, join timing: admin %s; candidates %v; members recovered %s, precision %s",
		admin, rtdKeys(cand), rtdFrac(tp, len(members)), rtdFrac(tp, len(cand)))

	// Insider: posts under pseudonyms.
	// FIXED D-10 (redteam/fix-anon-relay): from a member's own account an
	// anonymous post is refused; through a relay its carrier is the relay.
	if _, err := dave.postGroup("cell", "I can bring the documents", nil); !errors.Is(err, errAnonDirect) {
		t.Fatalf("an anonymous post from dave's own account: %v", err)
	}
	r := w.startRelay()
	dave.relay, erin.relay, frank.relay = w.relay, w.relay, w.relay
	for _, p := range []struct {
		c    *Client
		text string
	}{{dave, "I can bring the documents"}, {erin, "meet at 6"}, {dave, "careful with phones"}} {
		if _, err := p.c.postGroup("cell", p.text, nil); err != nil {
			t.Fatal(err)
		}
	}
	must(t, r.flush())
	w.sim.Commit()
	v = rtdObserve(t, w)
	posts := 0
	named := 0
	for _, it := range anonPosts(t, frank, "cell") {
		if it.Mail == nil || !strings.HasPrefix(it.From, "member:") {
			continue
		}
		posts++
		from, ok := v.envFrom[common.HexToHash(it.ID)]
		if ok && members[v.name(from)] {
			named++
		}
		t.Logf("post %q by %s: carried by a transaction from %s", it.Mail.Text, it.From[:min(len(it.From), 20)], v.name(from))
	}
	if posts != 3 || named != 0 {
		t.Fatalf("posts %d, named %d", posts, named)
	}
	t.Logf("RESULT D-10 FIXED (was high): own-account anonymous posts refused; through the relay %s posts named; timing to the relay remains", rtdFrac(named, posts))
	_ = oscar
}

func rtdBlock(t *testing.T, w *world) uint64 {
	h, err := w.ch.b.HeaderByNumber(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return h.Number.Uint64()
}

func rtdKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
