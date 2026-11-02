package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A member's tern.group-key item keeps no keys (they live in st.Groups, where
// retention destroys them), and a new member gets the current generation
// only, not the admin's history (redteam A-1, E2).
func TestGroupKeyItemsKeepNoKeys(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	eve, _ := w.owner("eve")
	must(t, nina.createGroup("club", false))
	for _, c := range []*Client{ada, eve} {
		if _, err := c.joinGroup("club"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := nina.admitPending(); err != nil {
		t.Fatal(err)
	}
	scan(t, ada)
	nina.postGroup("club", "gen0 secret", nil)
	must(t, nina.moderate("club", "ban", "eve", "leaks"))
	late, _ := w.owner("late")
	if _, err := late.joinGroup("club"); err != nil {
		t.Fatal(err)
	}
	if _, err := nina.admitPending(); err != nil {
		t.Fatal(err)
	}
	scan(t, ada)
	scan(t, late)
	g := late.st.Groups["club"]
	if g == nil || g.Gen != 1 || len(g.Old) != 0 {
		t.Fatalf("new member's keys: %+v", g)
	}
	for _, it := range late.groupItems("club") {
		if it.Mail != nil && strings.Contains(it.Mail.Text, "gen0 secret") {
			t.Fatal("a new member reads a post of a generation rotated out before it joined")
		}
	}
	for _, c := range []*Client{ada, late} {
		var secrets []string
		for _, gk := range append([]*GroupKeys{c.st.Groups["club"]}, c.st.Groups["club"].Old...) {
			secrets = append(append(secrets, gk.ViewKey), gk.Keys...)
		}
		n := 0
		for _, it := range c.st.Items {
			if it.Type != tGroupKey {
				continue
			}
			n++
			for _, s := range secrets {
				if strings.Contains(string(it.Body), s) {
					t.Fatalf("%s: a group-key item keeps a key", c.f.Name)
				}
			}
		}
		if n == 0 {
			t.Fatalf("%s: no group-key item", c.f.Name)
		}
	}
	// An item an older client kept with its keys is cleaned when the state is opened.
	full, _ := json.Marshal(ada.st.Groups["club"])
	for _, it := range ada.st.Items {
		if it.Type == tGroupKey {
			it.Body = full
		}
	}
	must(t, ada.save())
	again, err := openClient(filepath.Join(w.dir, "ada"), w.ch)
	if err != nil {
		t.Fatal(err)
	}
	must(t, again.save())
	b, _ := os.ReadFile(again.statePath())
	var st State
	must(t, json.Unmarshal(b, &st))
	for _, it := range st.Items {
		if it.Type == tGroupKey && strings.Contains(string(it.Body), ada.st.Groups["club"].ViewKey) {
			t.Fatal("an old group-key item keeps its keys after reopening")
		}
	}
}
