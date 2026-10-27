package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lockOut runs one lock-grammar call and returns what it printed.
func lockOut(t *testing.T, f func() error) map[string]any {
	t.Helper()
	r, err := cliResult(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func lockPaid(w *world, id uint64) *big.Int {
	l, err := w.ch.call("Bank", "locks", u(id))
	w.must(err)
	return l[4].(*big.Int)
}

// One grammar: lock open KIND, lock decide, lock settle, whatever the recipe.
func TestLockGrammar(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	w.owner("dave")
	w.owner("erin")
	w.must(alice.deposit(coins(10)))
	open := func(args ...string) uint64 {
		t.Helper()
		m := lockOut(t, func() error { return runLockOpen(alice, args, lockFlags{prim: primFlags{days: 30}}) })
		return uint64(m["lock"].(float64))
	}

	// An escrow: the arbiter decides 70% to the worker; anyone settles, finding the ruling itself.
	esc := open("escrow", "bob", "carol", "1", "translate", "the", "site")
	if _, err := cliResult(func() error { return runLockDecide(bob, []string{fmt.Sprint(esc), "10000", "mine"}) }); err == nil || !strings.Contains(err.Error(), "you don't decide") {
		t.Fatalf("the worker decided its own escrow: %v", err)
	}
	if _, err := cliResult(func() error { return runLockSettle(bob, []string{fmt.Sprint(esc)}) }); err == nil || !strings.Contains(err.Error(), "waits for carol") {
		t.Fatalf("settled before a decision: %v", err)
	}
	d := lockOut(t, func() error { return runLockDecide(carol, []string{fmt.Sprint(esc), "7000", "partly", "done"}) })
	if d["decision"] == "" || d["share_bps"].(float64) != 7000 {
		t.Fatalf("decide: %v", d)
	}
	s := lockOut(t, func() error { return runLockSettle(bob, []string{fmt.Sprint(esc)}) })
	if lockPaid(w, esc).Cmp(exact("0.7")) != 0 || !strings.Contains(fmt.Sprint(s["kind"]), "escrow") {
		t.Fatalf("escrow settled: %v, paid %s", s, lockPaid(w, esc))
	}

	// A bounty, opened by its old name (pot): the award names the payee.
	b := open("pot", "carol", "0.5", "fix", "issue", "#3")
	lockOut(t, func() error { return runLockDecide(carol, []string{fmt.Sprint(b), "bob", "10000", "first", "fix"}) })
	s = lockOut(t, func() error { return runLockSettle(bob, []string{fmt.Sprint(b)}) })
	if s["done"] != "paid to you" || lockPaid(w, b).Cmp(exact("0.5")) != 0 {
		t.Fatalf("bounty: %v", s)
	}

	// Pay unless disputed: nothing to settle in the window; after it, paid.
	m := lockOut(t, func() error {
		return runLockOpen(alice, []string{"optimistic", "bob", "1", "deliver", "the", "logs"}, lockFlags{prim: primFlags{days: 3, hours: 1, arbiter: "carol"}})
	})
	opt := uint64(m["lock"].(float64))
	if _, err := cliResult(func() error { return runLockSettle(bob, []string{fmt.Sprint(opt)}) }); err == nil || !strings.Contains(err.Error(), "window is still open") {
		t.Fatalf("settled in the window: %v", err)
	}
	w.later(2 * time.Hour)
	lockOut(t, func() error { return runLockSettle(bob, []string{fmt.Sprint(opt)}) })
	if lockPaid(w, opt).Cmp(exact("1")) != 0 {
		t.Fatalf("pay unless disputed: paid %s", lockPaid(w, opt))
	}

	// A disputed one: the arbiter rules, settle finds the ruling made after the dispute.
	m = lockOut(t, func() error {
		return runLockOpen(alice, []string{"optimistic", "bob", "1", "second", "batch"}, lockFlags{prim: primFlags{days: 3, hours: 1, arbiter: "carol"}})
	})
	opt2 := uint64(m["lock"].(float64))
	lockOut(t, func() error {
		return runPrimitive(alice, "lock dispute", []string{fmt.Sprint(opt2), "half", "missing"}, primFlags{days: 1})
	})
	lockOut(t, func() error { return runLockDecide(carol, []string{fmt.Sprint(opt2), "5000", "half", "delivered"}) })
	w.later(2 * time.Hour)
	lockOut(t, func() error { return runLockSettle(alice, []string{fmt.Sprint(opt2)}) })
	if lockPaid(w, opt2).Cmp(exact("0.5")) != 0 {
		t.Fatalf("disputed: paid %s", lockPaid(w, opt2))
	}

	// A panel in the flag form, the same shape as commissions.
	m = lockOut(t, func() error {
		return runLockOpen(alice, []string{"panel", "bob", "1", "the", "case"}, lockFlags{prim: primFlags{days: 30, panel: "carol,dave,erin", k: 2}, hearing: 20})
	})
	if m["k"] != "2" || m["panel"] != "carol,dave,erin" {
		t.Fatalf("panel: %v", m)
	}
	if _, err := cliResult(func() error { return runLockOpen(alice, []string{"bogus"}, lockFlags{}) }); err == nil || !strings.Contains(err.Error(), "lock open escrow") {
		t.Fatalf("an unknown kind: %v", err)
	}

	// MCP: the same grammar.
	r, err := runTool(alice, "tern_lock_open", map[string]any{"kind": "escrow", "args": "bob carol 0.25 a small job"})
	w.must(err)
	var o map[string]any
	bs, _ := json.Marshal(r)
	json.Unmarshal(bs, &o)
	if o["arbiter"] != "carol" {
		t.Fatalf("tern_lock_open: %v", o)
	}
}

// The old names keep working and say what replaced them, on stderr only.
func TestLockOldNamesNote(t *testing.T) {
	for old, now := range deprecated {
		if !strings.HasPrefix(now, "lock ") && !strings.HasPrefix(now, "commons commission") {
			t.Errorf("%s: %s", old, now)
		}
	}
	w := newWorld(t)
	w.owner("alice")
	testChain = w.ch
	defer func() { testChain = nil }()
	os.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	defer os.Unsetenv("TERNCHAIN_NET")
	r, wr, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = wr
	run([]string{"pot", "open", "--dir", filepath.Join(w.dir, "alice")})
	os.Stderr = saved
	wr.Close()
	b, _ := io.ReadAll(r)
	if !strings.Contains(string(b), "'pot open' is an old name, now 'lock open bounty") || strings.Count(string(b), "\n") != 1 {
		t.Fatalf("note: %q", b)
	}
}

// commons commission --plain is a spend: the treasury pays after the veto
// window, no lock; without --plain or a decider it is refused as before.
func TestCommissionPlainIsSpend(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	alice, _ := w.owner("alice")
	w.owner("worker")
	const C = "garden"
	w.must(nina.found(C, CommonsParams{Term: 30 * 86400, VoteWindow: 3600, VetoWindow: 3600, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 3000, Budget: coins(10)}, "grow things"))
	w.join(C, nina, alice)
	w.must(alice.deposit(coins(5)))
	w.must(alice.pay(C, coins(2), "fund"))
	if _, err := cliResult(func() error {
		return runCommons(nina, "commons commission", []string{C, "worker", "1", "seeds"}, commonsFlags{days: 1})
	}); err == nil || !strings.Contains(err.Error(), "--plain") {
		t.Fatalf("a commission with nobody deciding: %v", err)
	}
	v := lockOut(t, func() error {
		return runCommons(nina, "commons commission", []string{C, "worker", "1", "seeds"}, commonsFlags{days: 1, plain: true})
	})
	if v["kind"] != "spend" || v["to"] != "worker" || v["amount"] != "1.000000" {
		t.Fatalf("plain commission: %v", v)
	}
	w.later(2 * time.Hour)
	lockOut(t, func() error {
		return runCommons(alice, "commons payout", []string{fmt.Sprint(v["action"])}, commonsFlags{days: 1})
	})
	if tr := w.treasury(alice, C); tr.Cmp(coins(1)) != 0 {
		t.Fatalf("treasury after the spend: %v", tr)
	}
}
