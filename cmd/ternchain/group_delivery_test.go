package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGroupAdmissionDeliveryRetry(t *testing.T) {
	for _, change := range []string{"restart", "direct add", "generation", "removed", "banned", "admin revoked"} {
		t.Run(change, func(t *testing.T) {
			w := newWorld(t)
			nina, _ := w.owner("nina")
			ada, _ := w.owner("ada")
			must(t, nina.createGroup("club", false))
			if change == "direct add" {
				_, err := w.ch.send(nina.key, nil, "Sets", "add", setID("club"), nina.id(), ada.id())
				must(t, err)
			} else {
				// Repeated requests must not duplicate either admission or delivery.
				for range 2 {
					if _, err := ada.joinGroup("club"); err != nil {
						t.Fatal(err)
					}
				}
			}
			w.later(8 * 24 * time.Hour)
			want := func(group, member string) bool { return group == "club" && member == "ada" }
			done, _, err := nina.admitPendingFor(want)
			if err == nil || !strings.Contains(err.Error(), "retry group admit club ada") || len(done) != 0 {
				t.Fatalf("expired recipient keys: done=%v err=%v", done, err)
			}
			out, err := w.ch.call("Sets", "members", setID("club"), ada.id())
			if err != nil || !out[0].(bool) {
				t.Fatalf("delivery failure rolled back membership: %v %v", out, err)
			}
			if ada.st.Groups["club"] != nil {
				t.Fatal("failed delivery gave the recipient channel keys")
			}
			switch change {
			case "generation":
				if r, err := nina.rotateGroup("club", "test newer generation before retry"); err == nil {
					t.Fatalf("rotation should fail to deliver to the offline member: %v", r)
				}
			case "removed", "banned":
				_, err := w.ch.send(nina.key, nil, "Sets", "remove", setID("club"), nina.id(), ada.id(), change == "banned", "removed before retry")
				must(t, err)
			case "admin revoked":
				// Retry from a former admin, with a durable local copy of the keys.
				bob, _ := w.owner("bob")
				_, err := w.ch.send(nina.key, nil, "Sets", "setRole", setID("club"), nina.id(), "admin", bob.id(), true)
				must(t, err)
				bob.st.Groups["club"] = nina.st.Groups["club"]
				must(t, bob.save())
				_, err = w.ch.send(nina.key, nil, "Sets", "setRole", setID("club"), nina.id(), "admin", bob.id(), false)
				must(t, err)
				nina = bob
			}
			must(t, ada.publishKeys())
			// Opening from disk discards all in-memory delivery and resolver state.
			nina, err = openClient(filepath.Join(w.dir, nina.f.Name), w.ch)
			must(t, err)
			added, err := w.ch.events("Sets", "Added", nil)
			must(t, err)
			for _, ok := range []func(string, string) bool{
				func(string, string) bool { return false }, // bare daily keeper
				nil, // --all is only everyone waiting
			} {
				done, _, err := nina.admitPendingFor(ok)
				if err != nil || len(done) != 0 {
					t.Fatalf("implicit retry: done=%v err=%v", done, err)
				}
			}
			done, _, err = nina.admitPendingFor(want)
			must(t, err)
			scan(t, ada)
			g := ada.st.Groups["club"]
			if change == "removed" || change == "banned" || change == "admin revoked" {
				if len(done) != 0 || g != nil {
					t.Fatalf("unauthorized retry delivered keys: done=%v group=%v", done, g)
				}
			} else {
				if len(done) != 1 || done[0] != "club: ada" || g == nil {
					t.Fatalf("explicit retry did not deliver: done=%v group=%v", done, g)
				}
				current := nina.st.Groups["club"]
				if g.Gen != current.Gen || g.ViewKey != current.ViewKey || len(g.Old) != 0 {
					t.Fatalf("retry must send only current generation: got gen=%d old=%d want gen=%d", g.Gen, len(g.Old), current.Gen)
				}
			}
			after, err := w.ch.events("Sets", "Added", nil)
			must(t, err)
			if len(after) != len(added) {
				t.Fatal("retry changed membership")
			}
		})
	}
}
