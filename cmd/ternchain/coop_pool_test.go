package main

import (
	"strings"
	"testing"
	"time"
)

// One ledger, four rules: the group's pick decides the order, and a rule
// this client doesn't have falls back to the default, saying so.
func TestPoolRules(t *testing.T) {
	now := time.Now().Unix()
	l := &Ledger{Policy: CreditsPolicy{Floor: -5},
		Balances:  map[string]float64{"oscar": 20, "nina": -8, "ada": -1},
		UsedToday: map[string]float64{"nina": 25, "ada": 5},
		LastUsed:  map[string]int64{"oscar": now - 3600, "nina": now - 36000}}
	c := &Client{Ident: &Ident{dir: t.TempDir()}}
	names := func(q []QueueEntry) string {
		var s []string
		for _, e := range q {
			n := e.Name
			if e.Waits {
				n += "(waits)"
			}
			s = append(s, n)
		}
		return strings.Join(s, " ")
	}
	for rule, want := range map[string]string{
		"pool-default@1":     "oscar ada nina(waits)",
		"pool-round-robin@1": "ada nina oscar",
		"pool-quota@1":       "oscar ada nina(waits)",
	} {
		l.Pool = PoolRule{Schema: tPool, Rule: rule, QuotaDay: 20}
		q, note := c.order(l, nil)
		if got := names(q); got != want || note != "" {
			t.Errorf("%s: %s, want %s (%s)", rule, got, want, note)
		}
	}
	l.Pool = PoolRule{Schema: tPool, Rule: "pool-fifo@1"}
	if q, _ := c.order(l, map[string]float64{"oscar": 1, "nina": 9, "ada": 3}); names(q) != "nina ada oscar" {
		t.Errorf("fifo: %s", names(q))
	}
	l.Pool = PoolRule{Schema: tPool, Rule: "pool-lottery@3"}
	if q, note := c.order(l, nil); names(q) != "oscar ada nina(waits)" || !strings.Contains(note, "not installed here") {
		t.Errorf("an unknown rule: %s %q", names(q), note)
	}
}

// Only an admin's rule counts; members read it from the group's posts.
func TestPoolRuleInGroup(t *testing.T) {
	w := newWorld(t)
	oscar, nina, ada, node := friends(t, w)
	if _, err := ada.poolSetRule("friends", PoolRule{Rule: "pool-round-robin@1"}); err == nil || !strings.Contains(err.Error(), "only an admin") {
		t.Fatalf("a member's rule: %v", err)
	}
	_, err := ada.postGroup("friends", "pool rule", jsonOf(creditsPost{Pool: &PoolRule{Schema: tPool, Rule: "pool-fifo@1"}}))
	w.must(err)
	_, err = oscar.poolSetRule("friends", PoolRule{Rule: "pool-quota@1", QuotaDay: 50})
	w.must(err)
	w.must(node.flush())
	scan(t, nina)
	l, err := nina.ledger("friends")
	w.must(err)
	if l.Pool.Rule != "pool-quota@1" || l.Pool.QuotaDay != 50 || l.PoolBy != "oscar" || !strings.Contains(strings.Join(l.Ignored, "\n"), "a pool rule by ada") {
		t.Fatalf("pool rule: %+v %v", l.Pool, l.Ignored)
	}
}
