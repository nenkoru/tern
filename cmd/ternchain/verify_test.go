package main

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

func h(s string) string { return crypto.Keccak256Hash([]byte(s)).Hex() }

// Providers sign what they computed; a checker compares, by majority; the
// outvoted signed a wrong answer. Results are re-verified by every reader,
// scores count each job once, and a provider's bond goes to a panel with
// the result as evidence.
func TestVerify(t *testing.T) {
	noApprovalWait(t)
	w := newWorld(t)
	oscar, nina, ada, node := friends(t, w)
	stmt := func(c *Client, job, out, canon string) WorkStatement {
		t.Helper()
		s, err := c.workStatement(WorkStatement{Job: job, Input: h("prompt " + job), Output: h(out), Canon: canon, Resource: "tiny-32 0-31"})
		w.must(err)
		return s
	}
	so, sn, sa := stmt(oscar, "j1", "the answer", "tokens/greedy"), stmt(nina, "j1", "a lazy answer", "tokens/greedy"), stmt(ada, "j1", "the answer", "tokens/greedy")
	r, err := oscar.compareWork([]WorkStatement{so, sn, sa}, "")
	w.must(err)
	if r.Verdict != "mismatch" || len(r.Outvoted) != 1 || r.Outvoted[0] != "nina" || r.Majority != h("the answer") {
		t.Fatalf("three statements: %+v", r)
	}
	if r2, _ := oscar.compareWork([]WorkStatement{so, sn}, ""); r2.Verdict != "undecided" {
		t.Fatalf("two that disagree: %s", r2.Verdict)
	}
	if r2, _ := oscar.compareWork([]WorkStatement{so, sa}, ""); r2.Verdict != "match" {
		t.Fatalf("two that agree: %s", r2.Verdict)
	}
	gpu := w.agent(ada, "gpu", coins(1), `{}`)
	if _, err := oscar.compareWork([]WorkStatement{so, sa, stmt(gpu, "j1", "the answer", "tokens/greedy")}, ""); err == nil || !strings.Contains(err.Error(), "one family is one vote") {
		t.Fatalf("a family voting twice: %v", err)
	}
	if _, err := oscar.compareWork([]WorkStatement{so, stmt(ada, "j1", "the answer", "f16/round-3")}, ""); err == nil || !strings.Contains(err.Error(), "not comparable") {
		t.Fatalf("different canons: %v", err)
	}
	forged := sn
	forged.Output = h("the answer")
	if _, err := oscar.compareWork([]WorkStatement{so, forged}, ""); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("a statement changed after signing: %v", err)
	}
	if _, err := oscar.workStatement(WorkStatement{Job: "j2", Input: "the whole prompt", Output: h("x"), Canon: "tokens/greedy"}); err == nil {
		t.Fatal("a statement carrying data instead of a hash")
	}

	// sampling: the checker's seed picks the jobs, about the rate asked
	n := 0
	for i := 0; i < 2000; i++ {
		if sampled("secret", fmt.Sprint("job-", i), 0.1) {
			n++
		}
	}
	if n < 140 || n > 260 || sampled("secret", "j1", 0.1) != sampled("secret", "j1", 0.1) || !sampled("x", "y", 1) {
		t.Fatalf("sampled %d of 2000 at 10%%", n)
	}

	// reports: public claims and a sealed group post; scores re-verify both, one per job
	res, err := oscar.checkReport(r, "friends", true)
	w.must(err)
	if len(res["claims"].([]string)) != 2 {
		t.Fatalf("report: %v", res)
	}
	tampered := r
	tampered.Outvoted = []string{"ada"}
	_, err = nina.postGroup("friends", "spot check", jsonOf(checkPost{&tampered})) // the verdict is recomputed, not read
	w.must(err)
	w.must(node.flush())
	scan(t, ada)
	sn2, err := ada.checkScore("nina", "friends")
	w.must(err)
	sa2, err := ada.checkScore("ada", "friends")
	w.must(err)
	if sn2.Failed != 1 || sn2.Passed != 0 || sa2.Passed != 1 || sa2.Failed != 0 {
		t.Fatalf("scores: nina %+v ada %+v", sn2, sa2)
	}

	// a provider's bond: a panel decides with the result as evidence
	carol, _ := w.owner("carol")
	dave, _ := w.owner("dave")
	erin, _ := w.owner("erin")
	w.must(nina.deposit(coins(3)))
	bond, err := nina.checkBond("oscar", coins(2), []string{"carol", "dave", "erin"}, 2, 30, "friends")
	w.must(err)
	w.must(oscar.acceptLock(bond))
	if _, err := oscar.checkEscalate(bond, CheckResult{Schema: tCheck, Statements: []WorkStatement{so, sa}}); err == nil || !strings.Contains(err.Error(), "match") {
		t.Fatalf("escalating a match: %v", err)
	}
	if _, err := oscar.checkEscalate(bond, r); err != nil {
		t.Fatal(err)
	}
	w.later(5 * time.Minute)
	for _, p := range []*Client{carol, dave} {
		if _, err := p.panelRule(bond, 10000, "outvoted on a checked job"); err != nil {
			t.Fatal(err)
		}
	}
	bal := func(c *Client) *big.Int { b, _ := c.bankBalance(c.f.Name); return b }
	before := bal(oscar)
	w.must(erin.panelSettleAuto(bond))
	if got := new(big.Int).Sub(bal(oscar), before); got.Cmp(coins(2)) != 0 {
		t.Fatalf("the bond paid oscar %s", fmtCoins(got))
	}
}

// Different GPUs and kernels don't give the same bits: a comparator
// within a tolerance decides, exact hashes would cry wolf. A comparator
// this client lacks is refused, never guessed.
func TestVerifyTolerance(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	st := func(c *Client, out string, vec ...float64) WorkStatement {
		s, err := c.workStatement(WorkStatement{Job: "p7", Input: h("prompt"), Output: h(out), Canon: "logits/top8", Vec: vec})
		w.must(err)
		return s
	}
	a := st(oscar, "3090 bits", 9.1, 7.2, 5.0, 3.3, 1.0, 0.2, 0.1, 0.0)
	b := st(nina, "m2 bits", 9.1004, 7.1998, 5.0002, 3.2999, 1.0001, 0.2, 0.1, 0.0)
	lazy := st(ada, "lazy bits", 0.1, 0.0, 3.3, 9.1, 1.0, 7.2, 0.2, 5.0)
	if r, _ := oscar.compareWork([]WorkStatement{a, b, lazy}, ""); r.Verdict != "undecided" {
		t.Fatalf("exact hashes on different GPUs: %s", r.Verdict)
	}
	for _, line := range []string{"compare-cosine@1", "compare-topk@1", "compare-abs@1"} {
		r, err := oscar.compareWork([]WorkStatement{a, b, lazy}, line)
		w.must(err)
		if r.Verdict != "mismatch" || len(r.Outvoted) != 1 || r.Outvoted[0] != "ada" || r.Comparator != line {
			t.Fatalf("%s: %+v", line, r)
		}
		if again, err := nina.recheck(r, 0); err != nil || again.Verdict != "mismatch" {
			t.Fatalf("%s: a reader rechecks with the comparator the result names: %v %v", line, again.Verdict, err)
		}
	}
	if _, err := oscar.compareWork([]WorkStatement{a, b}, "compare-vibes@1"); err == nil || !strings.Contains(err.Error(), "not built in") {
		t.Fatalf("an unknown comparator: %v", err)
	}
}

// Red team H8/H10: a score counts every reported result per job, not the
// first. Two Sybil owners who report first that an honest provider was
// outvoted don't fail it once honest checkers report a match: the job is
// disputed (no two-thirds side). Results that agree keep their outcome.
// Tolerance comparators don't match vectors too short to mean anything.
func TestVerifyScoreSybilsReportFirst(t *testing.T) {
	noApprovalWait(t)
	w := newWorld(t)
	oscar, _, ada, _ := friends(t, w)
	prov, _ := w.owner("prov")
	s1, _ := w.owner("sybil1")
	s2, _ := w.owner("sybil2")
	carol, _ := w.owner("carol")
	stmt := func(c *Client, job, out string, vec ...float64) WorkStatement {
		t.Helper()
		s, err := c.workStatement(WorkStatement{Job: job, Input: h("prompt " + job), Output: h(out), Canon: "tokens/greedy", Vec: vec})
		w.must(err)
		return s
	}
	report := func(by *Client, ss ...WorkStatement) {
		t.Helper()
		r, err := by.compareWork(ss, "")
		w.must(err)
		_, err = by.checkReport(r, "", true)
		w.must(err)
	}
	honest := stmt(prov, "j1", "the answer")
	report(s1, honest, stmt(s1, "j1", "framed"), stmt(s2, "j1", "framed"))
	if sc, err := oscar.checkScore("prov", ""); err != nil || sc.Failed != 1 {
		t.Fatalf("one report, outvoted: %+v %v", sc, err)
	}
	report(oscar, honest, stmt(ada, "j1", "the answer"), stmt(oscar, "j1", "the answer"))
	sc, err := oscar.checkScore("prov", "")
	w.must(err)
	if sc.Failed != 0 || sc.Passed != 0 || sc.Disputed != 1 || sc.Jobs[0].Outcome != "disputed" ||
		fmt.Sprint(sc.Jobs[0].Agree) != "[ada oscar]" || fmt.Sprint(sc.Jobs[0].Disagree) != "[sybil1 sybil2]" {
		t.Fatalf("conflicting reports: %+v", sc)
	}
	// More honest families: 4 of 6 is exactly two thirds (still disputed); 5 of 7 is more (passed).
	report(carol, honest, stmt(carol, "j1", "the answer"))
	if sc, _ := oscar.checkScore("prov", ""); sc.Disputed != 1 {
		t.Fatalf("4 of 6 families: %+v", sc)
	}
	dave, _ := w.owner("dave")
	report(dave, honest, stmt(dave, "j1", "the answer"))
	if sc, _ := oscar.checkScore("prov", ""); sc.Passed != 1 || sc.Disputed != 0 {
		t.Fatalf("5 of 7 families: %+v", sc)
	}
	// Tolerance comparators: 5 numbers (top-5 is every entry) and 1 (cosine of two positives is 1) never agree.
	a := stmt(oscar, "v1", "real", 0.9, 0.1, 0.0, -0.2, 0.3)
	b := stmt(prov, "v1", "lazy", -5, 7, 100, 3, 2)
	if r, _ := oscar.compareWork([]WorkStatement{a, b}, "compare-topk@1"); r.Verdict == "match" {
		t.Fatal("top-k on 5 numbers matched")
	}
	c1, c2 := stmt(oscar, "v2", "real", 0.7), stmt(prov, "v2", "lazy", 4242)
	if r, _ := oscar.compareWork([]WorkStatement{c1, c2}, "compare-cosine@1"); r.Verdict == "match" {
		t.Fatal("cosine on 1 number matched")
	}
	if r, _ := oscar.compareWork([]WorkStatement{stmt(oscar, "v3", "r", 41.0), stmt(prov, "v3", "l", 41.0)}, "compare-abs@1"); r.Verdict != "match" {
		t.Fatalf("one reading under compare-abs@1: %s", r.Verdict)
	}
}
