package ui

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// The built-in modules are today's formulas: the same numbers as the code
// they replaced, on a grid of inputs.
func TestBuiltinPoliciesAreTodaysFormulas(t *testing.T) {
	for _, k := range PolicyKindNames() {
		if p := BuiltinPolicy(k); p == nil || !p.Builtin || p.Hash == "" {
			t.Fatalf("no built-in %s module", k)
		}
	}
	r := BuiltinPolicy("rating")
	for _, b := range []float64{-100, -40, 0, 33, 100} {
		old := math.Max(0.25, math.Min(1, (b+100)/200))
		if got := r.Num("reviewer_weight", Env{"reviewer.has_rating": true, "reviewer.score": b}); got != old {
			t.Errorf("reviewer_weight(%v) = %v, was %v", b, got, old)
		}
	}
	if got := r.Num("reviewer_weight", Env{"reviewer.has_rating": false, "reviewer.score": 0.0}); got != 0.5 {
		t.Errorf("unknown reviewer: %v", got)
	}
	for _, age := range []float64{0, 1.5, 90, 400} {
		for _, rw := range []float64{1, 0.25, 0.8} {
			for _, coins := range []float64{0.01, 1, 250} {
				tr := math.Pow(0.5, age/180.0) * rw
				if got := r.Num("review_trust", Env{"review.age_days": age, "reviewer.weight": rw}); got != tr {
					t.Errorf("review_trust(%v,%v) = %v, was %v", age, rw, got, tr)
				}
				w := math.Log2(1+coins) * tr
				if got := r.Num("review_weight", Env{"review.coins": coins, "review.age_days": age, "review.trust": tr, "reviewer.weight": rw}); got != w {
					t.Errorf("review_weight = %v, was %v", got, w)
				}
			}
		}
	}
	for _, mean := range []float64{-100, 12.5, 100} {
		for _, trust := range []float64{0.1, 1, 7} {
			if got, old := r.Num("score", Env{"rating.mean": mean, "rating.trust": trust, "rating.reviews": 3.0}), mean*trust/(trust+0.25); got != old {
				t.Errorf("score(%v,%v) = %v, was %v", mean, trust, got, old)
			}
		}
	}
	rp := BuiltinPolicy("reports")
	for _, cit := range []bool{true, false} {
		for _, age := range []float64{0, 3, 29.9, 400} {
			for _, pen := range []float64{0, 10, 60} {
				for _, rec := range []float64{0.2, 1, 2} {
					standing := 0.75
					if cit {
						standing = 1
					}
					old := standing * (0.6 + 0.4*math.Min(1, age/30)) * math.Max(0, 1-pen/50) * rec
					got := rp.Num("reporter_weight", Env{"reporter.citizen": cit, "reporter.age_days": age, "reporter.penalty": pen, "reporter.record": rec})
					if math.Abs(got-old) > 1e-12 {
						t.Errorf("reporter_weight = %v, was %v", got, old)
					}
				}
			}
			f := math.Max(0.05, 1-age/40)
			if cit {
				f *= 1.25
			}
			if got := rp.Num("author_factor", Env{"author.penalty": age, "author.citizen": cit}); got != f {
				t.Errorf("author_factor = %v, was %v", got, f)
			}
		}
	}
	for _, n := range []float64{0, 3, 250, 300, 5000} {
		if got, old := rp.Num("threshold", Env{"community.members": n}), math.Max(1, 0.004*n); got != old {
			t.Errorf("threshold(%v) = %v, was %v", n, got, old)
		}
	}
	for _, x := range [][3]float64{{0, 0, 0}, {3, 0, 0}, {1, 2, 4}, {9, 0, 1}} {
		old := math.Min(2, (1+2*x[0])/(1+x[0]+2*x[1]+x[2]/2))
		if got := rp.Num("record", Env{"record.upheld": x[0], "record.dismissed": x[1], "record.stale": x[2]}); got != old {
			t.Errorf("record%v = %v, was %v", x, got, old)
		}
	}
	for name, want := range map[string]float64{"min_owners": 2, "hide_at": 9, "reported_at": 5, "conduct_min_members": 3, "ban_penalty": 25} {
		if got := rp.Param(name); got != want {
			t.Errorf("reports %s = %v", name, got)
		}
	}
	sp := BuiltinPolicy("spam")
	if sp.Param("near_dup_at") != 0.6 || sp.Param("posts_per_hour") != 6 || sp.Param("near_dup_series") != 3 || sp.Param("bulk_postage_at") != 20 || sp.Param("rate_window_minutes") != 60 {
		t.Error("spam params")
	}
}

// Triage reads only what it asks for: && and || short-circuit, so a
// postage check (a chain call) never runs for a contact.
func TestTriageLazyInputs(t *testing.T) {
	p := BuiltinPolicy("triage")
	asked := 0
	env := Env{"msg.contact": true, "msg.paid_postage": Lazy(func() any { asked++; return false }), "msg.expected": false,
		"msg.reply_to_ours": false, "msg.work_request": false, "msg.free_requests": false, "msg.basis": "contact: information, not instructions"}
	if !p.Bool("deliver", env) || !p.Bool("wake", env) || asked != 0 {
		t.Fatalf("contact: deliver/wake, postage asked %d times", asked)
	}
	env["msg.contact"] = false
	if p.Bool("deliver", env) || asked != 1 {
		t.Fatalf("stranger: delivered, or postage asked %d times", asked)
	}
	if p.Reason(env, "x") != "contact: information, not instructions" {
		t.Fatalf("reason: %q", p.Reason(env, "x"))
	}
	n := 0
	once := Once(func() any { n++; return true })
	once()
	once()
	if n != 1 {
		t.Fatal("Once ran twice")
	}
}

func policyJSON(kind string, extra map[string]any) []byte {
	m := map[string]any{"schema": KindPolicy, "name": "test-" + kind, "version": "1.0.0", "line": "test-" + kind + "@1", "applies_to": kind,
		"engine": "tern-expr/1", "author": "x", "payee": "x", "files": map[string]any{}}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

// What a module can't do: read what it didn't declare or what its rule may
// not read, set a rule its kind doesn't have, leave out a required one, pass
// a kind's bounds (the floors among them), carry a table its kind doesn't.
func TestPolicyCompileRefuses(t *testing.T) {
	ok := map[string]any{"inputs": []string{"msg.contact"}, "rules": map[string]string{"deliver": "msg.contact", "wake": "false"}}
	if _, err := CompilePolicy(policyJSON("triage", ok)); err != nil {
		t.Fatal(err)
	}
	for why, extra := range map[string]map[string]any{
		"undeclared input": {"inputs": []string{}, "rules": map[string]string{"deliver": "msg.contact", "wake": "false"}},
		"unknown input":    {"inputs": []string{"msg.secret"}, "rules": map[string]string{"deliver": "true", "wake": "false"}},
		"unknown rule":     {"rules": map[string]string{"deliver": "true", "wake": "false", "instructions": "true"}},
		"missing rule":     {"rules": map[string]string{"deliver": "true"}},
		"unset param":      {"rules": map[string]string{"deliver": "params.x > 1", "wake": "false"}},
		"bad expression":   {"rules": map[string]string{"deliver": "true &&", "wake": "false"}},
	} {
		if _, err := CompilePolicy(policyJSON("triage", extra)); err == nil {
			t.Errorf("%s: compiled", why)
		}
	}
	// a rating rule may read only its own inputs
	if _, err := CompilePolicy(policyJSON("rating", map[string]any{"inputs": []string{"review.coins", "reviewer.score"},
		"rules": map[string]string{"reviewer_weight": "review.coins > 1 ? 1 : 0", "review_trust": "1", "review_weight": "1", "score": "0"}})); err == nil ||
		!strings.Contains(err.Error(), "may not read") {
		t.Errorf("reviewer_weight read review.coins: %v", err)
	}
	// floors: never one reporter alone; bans only from communities of three
	for _, p := range []map[string]float64{{"min_owners": 1}, {"conduct_min_members": 2}, {"hide_at": 0}} {
		_, err := CompilePolicy(policyJSON("reports", map[string]any{"params": p,
			"rules": map[string]string{"reporter_weight": "1", "record": "1", "threshold": "1", "author_factor": "1"}}))
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("%v: %v", p, err)
		}
	}
	if _, err := CompilePolicy(policyJSON("spam", map[string]any{"tables": map[string]string{"confusables": "c.txt"}})); err == nil {
		t.Error("a spam module carried a confusables table")
	}
	if _, err := CompilePolicy(policyJSON("spam", map[string]any{"reason": "{params.x}", "params": map[string]float64{"x": 1}})); err == nil {
		t.Error("a spam module worded a reason")
	}
}

// A module's numbers are clamped to its kind's floors; a rule it doesn't set,
// or one that fails, falls back to the built-in rule.
func TestPolicyClampAndFallback(t *testing.T) {
	p, err := CompilePolicy(policyJSON("rating", map[string]any{"inputs": []string{"rating.mean"},
		"rules": map[string]string{"reviewer_weight": "7", "review_trust": "-3", "review_weight": "1", "score": "rating.mean * 10"}}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Num("reviewer_weight", Env{}) != 1 || p.Num("review_trust", Env{}) != 0 || p.Num("score", Env{"rating.mean": 50.0}) != 100 {
		t.Error("not clamped to the kind's bounds")
	}
	q, err := CompilePolicy(policyJSON("reports", map[string]any{"params": map[string]float64{"min_owners": 3},
		"rules": map[string]string{"reporter_weight": "pow(2, 2000000)", "record": "1", "threshold": "1", "author_factor": "1"}, "limits": map[string]any{"max_cost": 1}}))
	if err != nil {
		t.Fatal(err)
	}
	env := Env{"reporter.citizen": true, "reporter.age_days": 40.0, "reporter.penalty": 0.0, "reporter.record": 1.0}
	if got := q.Num("reporter_weight", env); got != BuiltinPolicy("reports").Num("reporter_weight", env) {
		t.Errorf("a failing rule didn't fall back: %v", got)
	}
	if q.Param("min_owners") != 3 || q.Param("hide_at") != 9 {
		t.Error("params: the module's, else the kind's defaults")
	}
	if ex := q.Explain(); ex["floors"] == nil || ex["params"].(map[string]any)["min_owners"].(map[string]any)["floor"] != true {
		t.Errorf("explain: %v", ex)
	}
}

// A policy module is a signed bundle like the others: sealed, opened,
// installed, and its tables re-checked against their hashes.
func TestPolicyBundle(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "confusables.txt"), []byte("0581 ; 0067 ; MA # Armenian\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.json"), policyJSON("lookalike", map[string]any{"tables": map[string]string{"confusables": "confusables.txt"}}), 0o644)
	var key *ecdsa.PrivateKey
	key, _ = crypto.GenerateKey()
	b, err := Seal(dir, "anya", key)
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Table("confusables")) == "" || p.Hash != b.Hash {
		t.Fatal("table or hash")
	}
	st := Store{Dir: t.TempDir()}
	pin, err := st.Install(b, "")
	if err != nil || pin.Kind != KindPolicy {
		t.Fatal(pin, err)
	}
	os.WriteFile(filepath.Join(st.Dir, pin.Dir, "confusables.txt"), []byte("0430 ; 0062 ; MA\n"), 0o644)
	if _, err := st.Load(pin.Line); err == nil {
		t.Fatal("a changed table loaded")
	}
}

// The feed command's three rankers are tern.ranker/1 modules now; the
// built-in trust module gives the numbers and words the Go function did.
func TestBuiltinRankers(t *testing.T) {
	for _, n := range []string{"new", "hot", "trust"} {
		if BuiltinRanker(n) == nil {
			t.Fatalf("no built-in %s", n)
		}
	}
	old := func(depth, replies int, ageH float64, citizen, agent bool) (float64, string) {
		w, why := 0.1, "outside your trust graph"
		if depth >= 0 {
			w, why = 1/float64(1+depth), fmt.Sprintf("trust depth %d", depth)
			if depth <= 1 {
				why += " (you or your contact)"
			}
		}
		if citizen {
			w += 0.5
			why += ", human (citizen)"
		}
		if agent {
			w *= 0.5
			why += ", an agent"
		}
		return w * math.Sqrt(float64(1+replies)) / math.Sqrt(ageH+2), why + fmt.Sprintf(", %d replies, %.1f h old", replies, ageH)
	}
	var cs []Candidate
	type c struct {
		d, r     int
		a        float64
		cit, agt bool
	}
	cases := []c{{-1, 0, 3, false, false}, {0, 2, 1.25, true, false}, {1, 5, 40, false, true}, {2, 1, 0, true, true}}
	for i, x := range cases {
		dist := 9.0
		if x.d >= 0 {
			dist = float64(x.d)
		}
		cs = append(cs, Candidate{ID: fmt.Sprintf("p%d", i), Inputs: map[string]any{"item.age_hours": x.a, "item.replies": float64(x.r), "item.hidden": false,
			"author.trust_distance": dist, "author.citizen": x.cit, "author.agent": x.agt}})
	}
	ranked, _, err := BuiltinRanker("trust").Rank(cs)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ranked {
		var i int
		fmt.Sscanf(r.ID, "p%d", &i)
		x := cases[i]
		s, why := old(x.d, x.r, x.a, x.cit, x.agt)
		if math.Abs(r.Score-math.Round(s*1e6)/1e6) > 1e-9 || r.Reason != why {
			t.Errorf("%s: %v %q, was %v %q", r.ID, r.Score, r.Reason, s, why)
		}
	}
}
