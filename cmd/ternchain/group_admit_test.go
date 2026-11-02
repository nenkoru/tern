package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// Anyone can ask to join a private group on chain. group admit (the daily
// keeper run of FOR-AGENTS rule 21) lets in only the names it is given, or
// everyone with --all; a bare run lists who waits (redteam E1).
func TestGroupAdmitOnlyByName(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	spy, _ := w.owner("spy")
	must(t, nina.createGroup("club", false))
	for _, c := range []*Client{ada, spy} {
		if _, err := c.joinGroup("club"); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(w.dir, "nina")
	admit := func(args ...string) (admitted, waiting []string) {
		t.Helper()
		o, err := cli(t, w, append([]string{"group", "admit", "--dir", dir}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Admitted []string `json:"admitted"`
			Waiting  []string `json:"waiting"`
		}
		if err := json.Unmarshal([]byte(o), &r); err != nil {
			t.Fatalf("%v: %s", err, o)
		}
		return r.Admitted, r.Waiting
	}
	if a, wt := admit(); len(a) != 0 || len(wt) != 2 {
		t.Fatalf("a bare group admit: admitted %v, waiting %v", a, wt)
	}
	if a, wt := admit("club", "ada"); len(a) != 1 || a[0] != "club: ada" || len(wt) != 1 || wt[0] != "club: spy" {
		t.Fatalf("group admit club ada: admitted %v, waiting %v", a, wt)
	}
	scan(t, ada)
	scan(t, spy)
	if ada.st.Groups["club"] == nil || spy.st.Groups["club"] != nil {
		t.Fatal("keys went to the wrong names")
	}
	if a, wt := admit("club", "ada"); len(a) != 1 || a[0] != "club: ada" || len(wt) != 1 || wt[0] != "club: spy" {
		t.Fatalf("explicit key delivery retry: admitted %v, waiting %v", a, wt)
	}
	if a, _ := admit("--all"); len(a) != 1 || a[0] != "club: spy" {
		t.Fatalf("group admit --all: %v", a)
	}
}
