package main

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// The bricks are not about GPUs: an API key and family backups are built
// from the same offer, presence, receipt, credits, pool and verify, with no
// code of their own (scripts/demo-quota.sh, scripts/demo-backup.sh).

// Flatmates split one API key: it stays with its holder, each member's
// tokens become receipts it countersigns from its own count, and the queue
// is a daily quota.
func TestSharedAPIQuota(t *testing.T) {
	w := newWorld(t)
	oscar, nina, ada, node := friends(t, w)
	os, err := parseOffers([]byte(`{"schema":"tern.offer/1","kind":"api-quota","title":"an LLM API key, at cost","amount":{"value":1000000,"unit":"token/month"},
		"price":{"coins":"0.002","per":1000,"unit":"token"},"free_for":["friends"],"settle":["credits","channel"],"protocol":"openai-compatible/1"}`))
	w.must(err)
	_, err = oscar.offerSet(os, false)
	w.must(err)
	_, err = oscar.announcePresence("friends", coopFlags{state: "online", ttl: 120, reach: "http://10.8.0.1:8080/v1"})
	w.must(err)
	on, _, _, err := nina.presence("friends")
	w.must(err)
	if len(on) != 1 || on[0].Offers[0].Kind != "api-quota" || on[0].Reach == "" {
		t.Fatalf("the key's offer: %+v", on)
	}
	w.must(oscar.creditsSetPolicyOK("friends", CreditsPolicy{Rates: map[string]float64{"token": 0.01}, Floor: -1000}))
	_, err = oscar.poolSetRule("friends", PoolRule{Rule: "pool-quota@1", QuotaDay: 5})
	w.must(err)
	for who, n := range map[*Client]uint64{nina: 560, ada: 155} {
		u := receipt(t, oscar, who, "friends", n, "")
		_, err := oscar.creditsPostUsage("friends", u)
		w.must(err)
	}
	w.must(node.flush())
	scan(t, ada)
	l, err := ada.ledger("friends")
	w.must(err)
	q, note := ada.order(l, map[string]float64{"nina": 0, "ada": 0})
	if note != "" || q[0].Name != "ada" || q[1].Name != "nina" || !q[1].Waits {
		t.Fatalf("the quota queue: %+v %s", q, note)
	}
}

// Households keep each other's backups: coverage per chunk, a chunk lost
// and caught by its hash, disk space credited, turns for who goes next.
func TestFamilyBackups(t *testing.T) {
	w := newWorld(t)
	oscar, nina, ada, _ := friends(t, w)
	store := func(c *Client, holds string) {
		os, err := parseOffers([]byte(`{"schema":"tern.offer/1","kind":"storage","title":"2 TB for the family","whole":"backup:oscar-photos","holds":"` +
			holds + `","of":8,"amount":{"value":2000,"unit":"gb"},"free_for":["friends"],"settle":["credits"],"protocol":"sftp"}`))
		w.must(err)
		_, err = c.offerSet(os, false)
		w.must(err)
		_, err = c.announcePresence("friends", coopFlags{state: "online", ttl: 120})
		w.must(err)
	}
	store(nina, "0-4")
	store(ada, "3-6")
	on, _, _, err := oscar.presence("friends")
	w.must(err)
	cov := presenceCoverage(on, 2)["backup:oscar-photos"].(map[string]any)
	if cov["missing"] != "7" || cov["thin"] != "0-2,5-6" {
		t.Fatalf("coverage: %v", cov)
	}

	// nina's copy of chunk 2 went bad: her hash is outvoted by the manifest's and ada's
	good, bad := sha256.Sum256([]byte("chunk 2")), sha256.Sum256(make([]byte, 7))
	in := common.Hash(sha256.Sum256([]byte("which chunk"))).Hex()
	var ss []WorkStatement
	for c, out := range map[*Client][32]byte{oscar: good, nina: bad, ada: good} {
		s, err := c.workStatement(WorkStatement{Job: "backup:oscar-photos:chunk-2", Input: in, Output: common.Hash(out).Hex(), Canon: "sha256/bytes"})
		w.must(err)
		ss = append(ss, s)
	}
	r, err := oscar.compareWork(ss, "")
	w.must(err)
	if r.Verdict != "mismatch" || strings.Join(r.Outvoted, ",") != "nina" {
		t.Fatalf("the lost chunk: %+v", r)
	}
}

// creditsSetPolicyOK: set a policy, only the error.
func (c *Client) creditsSetPolicyOK(group string, p CreditsPolicy) error {
	_, err := c.creditsSetPolicy(group, p)
	return err
}
