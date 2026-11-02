package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/tern/internal/zk"
)

// paidLock: payer pays payee through an escrow it releases.
func paidLock(t *testing.T, w *world, payer *Client, payee, arbiter string, amount float64) uint64 {
	t.Helper()
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	l, _, err := payer.openEscrow(payee, arbiter, coins(amount), head.Time+7*86400, "work")
	if err != nil {
		t.Fatal(err)
	}
	must(t, payer.releaseLock(l, nil))
	return l
}

func TestPrivateReputation(t *testing.T) {
	t.Setenv("TERNCHAIN_ZK_DEVNET_DIR", t.TempDir())
	w := newWorld(t)
	bob, _ := w.owner("bob")
	w.owner("guild")
	var payers []*Client
	scores := []int{90, 80, 100, 70}
	for i, n := range []string{"alice", "carol", "dave", "erin"} {
		c, _ := w.owner(n)
		must(t, c.deposit(coins(20)))
		payers = append(payers, c)
		l := paidLock(t, w, c, "bob", "guild", 2)
		if _, err := bob.repTagSend(l); err != nil {
			t.Fatal(err)
		}
		scan(t, c)
		if c.st.RepTags[l] == "" {
			t.Fatalf("%s got no tag", n)
		}
		must(t, c.repReview(l, scores[i]))
		if err := c.repReview(l, scores[i]); err == nil {
			t.Fatal("a second review of one lock")
		}
	}
	// The chain shows leaves: no subject, no score.
	evs, _ := w.ch.events("PrivateReviews", "Review")
	b, _ := json.Marshal(evs)
	if len(evs) != 4 || strings.Contains(string(b), "bob") {
		t.Fatalf("private reviews on chain: %d %s", len(evs), b)
	}
	// Bob proves 4 deals averaging >= 85 (90+80+100+70 = 340 / 4) for nina.
	start := time.Now()
	p, err := bob.repProve("nina 2026-10-29 n1", 0, -101)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rep proof: %v (%d deals, avg >= %d)", time.Since(start), p.Count, p.AvgAtLeast)
	if p.Count != 4 || p.AvgAtLeast != 85 {
		t.Fatalf("proof: %+v", p)
	}
	nina, _ := w.owner("nina")
	start = time.Now()
	r, err := nina.repVerify(p, "nina 2026-10-29 n1")
	if err != nil || r["deals"] != 4 {
		t.Fatalf("verify: %v %v", r, err)
	}
	t.Logf("rep verify: %v", time.Since(start))
	// The proof names no lock and no counterpart.
	pb, _ := json.Marshal(p)
	for _, n := range []string{"alice", "carol", "dave", "erin", "bob"} {
		if strings.Contains(string(pb), n) {
			t.Fatalf("the proof names %s", n)
		}
	}
	// Another verifier's scope: refused (no replay); a raised claim: refused.
	if _, err := nina.repVerify(p, "eve 2026-10-29"); err == nil {
		t.Fatal("a proof for nina accepted by eve")
	}
	forged := *p
	forged.MinTotal += 40
	if _, err := nina.repVerify(&forged, p.Scope); err == nil {
		t.Fatal("a raised average verified")
	}
	more := *p
	more.Count = 5
	if _, err := nina.repVerify(&more, p.Scope); err == nil {
		t.Fatal("a raised count verified")
	}
	// Best 2 deals, at least 90; the same scope gives the same nullifier (one prover).
	start = time.Now()
	p2, err := bob.repProve("nina 2026-10-29 n1", 2, 90)
	t.Logf("rep proof with the keys loaded: %v (prove %v)", time.Since(start), zk.LastProveTime)
	if err != nil || p2.Count != 2 || p2.AvgAtLeast != 90 || p2.Nullifier != p.Nullifier {
		t.Fatalf("best two: %+v %v", p2, err)
	}
	if _, err := bob.repProve("x", 4, 90); err == nil {
		t.Fatal("claimed an average above the deals'")
	}
	// Someone else can't use bob's reviews: carol has none of her own.
	if _, err := payers[1].repProve("x", 0, -101); err == nil {
		t.Fatal("a payer proved the payee's reputation")
	}
	// Only the payer's side posts a lock's leaf, only for a paid lock.
	head, _ := w.ch.b.HeaderByNumber(context.Background(), nil)
	open, _, err := payers[0].openEscrow("bob", "guild", coins(1), head.Time+7*86400, "unpaid")
	must(t, err)
	if _, err := nina.ch.send(nina.key, nil, "PrivateReviews", "review", u(open), nina.id(), zk.HashN(u(1))); err == nil {
		t.Fatal("a stranger reviewed a lock")
	}
	if _, err := payers[0].ch.send(payers[0].key, nil, "PrivateReviews", "review", u(open), payers[0].id(), zk.HashN(u(1))); err == nil {
		t.Fatal("an unpaid lock reviewed")
	}
	// The gas a private review costs the payer.
	l := paidLock(t, w, payers[0], "bob", "guild", 1)
	if r, err := payers[0].ch.send(payers[0].key, nil, "PrivateReviews", "review", u(l), payers[0].id(), zk.HashN(u(7))); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("PrivateReviews.review: %d gas", r.GasUsed)
	}
	// The CLI's proof file round-trips.
	f := filepath.Join(t.TempDir(), "rep.json")
	bb, _ := json.Marshal(p)
	must(t, os.WriteFile(f, bb, 0o600))
	if q, err := readRepProof(f); err != nil || q.Nullifier != p.Nullifier {
		t.Fatal(err)
	}
}
