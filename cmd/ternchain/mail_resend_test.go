package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func queuedMail(t *testing.T, c *Client, o SendOpts) (string, []Envelope) {
	t.Helper()
	var queued []Envelope
	old := capture
	capture = func(e Envelope) { queued = append(queued, e) }
	id, err := c.send(o)
	capture = old
	if err != nil {
		t.Fatal(err)
	}
	return id, queued
}

func postedMail(t *testing.T, c *Client, id string) Envelope {
	t.Helper()
	evs, err := c.ch.events("Inbox", "Posted")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		es, err := decodeBatch(ev.Args["batch"].([]byte))
		if err != nil {
			t.Fatal(err)
		}
		for _, env := range es {
			if env.hash().Hex() == id {
				return env
			}
		}
	}
	t.Fatal("posted message not found")
	return Envelope{}
}

func openedMail(t *testing.T, c *Client, env Envelope) *Datagram {
	t.Helper()
	plain, err := env.open(c.identities())
	if err != nil {
		t.Fatal(err)
	}
	d, _, _, err := openSigned(plain)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestResendDelayedMailCurrentKeys(t *testing.T) {
	w := newWorld(t)
	h, err := w.ch.b.HeaderByNumber(context.Background(), nil)
	w.must(err)
	w.later(time.Duration(86400-int64(h.Time)%86400+23*3600) * time.Second)
	nina, _ := w.owner("nina")
	ag := w.agent(nina, "a", coins(1), `{}`)
	nina.relay = "https://relay.invalid"
	id, queued := queuedMail(t, nina, SendOpts{To: ag.f.Name, Text: "keep the lighthouse", Intent: "inform"})
	original := openedMail(t, nina, queued[0])
	w.later(2 * time.Hour)
	scan(t, nina) // an ordinary inbox/watch pass must preserve manual refresh
	if nina.st.Sent[id].Env == "" {
		t.Fatal("sender scan discarded delayed-mail refresh source")
	}
	w.must(nina.post(queued))
	got := scan(t, ag)
	if len(got) != 1 || got[0].Status != "refused" || !strings.Contains(got[0].Verified, "recipient's owner") {
		t.Fatalf("stale original should remain refused: %+v", got)
	}
	nina.relay = ""
	r, err := nina.resend(id)
	if err != nil {
		t.Fatal(err)
	}
	newID := r["sent"].(string)
	freshEnv := postedMail(t, nina, newID)
	fresh := openedMail(t, nina, freshEnv)
	if fresh.DeliveryID == "" || fresh.DeliveryID != original.DeliveryID || fresh.Created != original.Created || !bytes.Equal(fresh.Body, original.Body) {
		t.Fatal("refresh changed the logical message or its expiry")
	}
	got = scan(t, ag)
	if len(got) != 1 || got[0].Status != "delivered" || got[0].Mail == nil || got[0].Mail.Text != "keep the lighthouse" {
		t.Fatalf("current-key delivery: %+v", got)
	}
	got[0].Done = true
	ag.pruneItems(true)
	w.must(ag.save())
	restarted, err := openClient(ag.dir, w.ch)
	w.must(err)
	w.must(nina.post([]Envelope{freshEnv, queued[0]}))
	for _, it := range scan(t, restarted) {
		if it.Wake || it.Mail != nil {
			t.Fatalf("duplicate woke after purge: %+v", it)
		}
	}
	if nina.st.Sent[id].Replacement != newID || nina.st.Sent[id].Env != "" {
		t.Fatal("refresh did not retire the old retry ciphertext")
	}
}

func TestResendRefusesUnsafeOrExpiredMail(t *testing.T) {
	t.Run("paid", func(t *testing.T) {
		w := newWorld(t)
		a, _ := w.owner("alice")
		w.owner("bob")
		a.relay = "https://relay.invalid"
		id, _ := queuedMail(t, a, SendOpts{To: "bob", Text: "hello", Postage: true})
		if _, err := a.resend(id); err == nil {
			t.Fatal("postage-bearing operation was refreshed")
		}
	})
	t.Run("expired", func(t *testing.T) {
		w := newWorld(t)
		a, _ := w.owner("alice")
		w.owner("bob")
		a.relay = "https://relay.invalid"
		id, _ := queuedMail(t, a, SendOpts{To: "bob", Text: "hello"})
		w.later(8 * 24 * time.Hour)
		if _, err := a.resend(id); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expired mail: %v", err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		w := newWorld(t)
		a, _ := w.owner("alice")
		w.owner("bob")
		ag := w.agent(a, "a", coins(1), `{"to":["bob"]}`)
		ag.relay = "https://relay.invalid"
		id, _ := queuedMail(t, ag, SendOpts{To: "bob", Text: "hello"})
		w.must(a.revokeAgent(ag.f.Name, false))
		if _, err := ag.resend(id); err == nil || !strings.Contains(err.Error(), "no longer authorized") {
			t.Fatalf("revoked sender: %v", err)
		}
	})
}

func TestResendAttemptLimitSurvivesRestart(t *testing.T) {
	w := newWorld(t)
	a, _ := w.owner("alice")
	w.owner("bob")
	a.relay = "https://relay.invalid"
	id, _ := queuedMail(t, a, SendOpts{To: "bob", Text: "hello"})
	old := capture
	capture = func(Envelope) {}
	t.Cleanup(func() { capture = old })
	for i := 0; i < mailRefreshMax; i++ {
		if _, err := a.resend(id); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		var err error
		a, err = openClient(a.dir, w.ch)
		w.must(err)
		a.relay = "https://relay.invalid"
	}
	if _, err := a.resend(id); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("refresh limit reset: %v", err)
	}
}

func TestResendEnforcesShorterKeyRetention(t *testing.T) {
	w := newWorld(t)
	a, _ := w.owner("alice")
	w.owner("bob")
	w.must(a.setRetention(1, 0))
	a.relay = "https://relay.invalid"
	id, _ := queuedMail(t, a, SendOpts{To: "bob", Text: "hello"})
	w.later(3 * 24 * time.Hour)
	if _, err := a.resend(id); err == nil || !strings.Contains(err.Error(), "no longer retained") {
		t.Fatalf("refresh used an epoch key beyond retention: %v", err)
	}
}

func TestRefreshRecordsExpireWithoutStaleRestore(t *testing.T) {
	w := newWorld(t)
	a, _ := w.owner("alice")
	w.owner("bob")
	a.relay = "https://relay.invalid"
	id, _ := queuedMail(t, a, SendOpts{To: "bob", Text: "hello"})
	w.later(8 * 24 * time.Hour)
	scan(t, a)
	w.must(a.save())
	restarted, err := openClient(a.dir, w.ch)
	w.must(err)
	if r := restarted.st.Sent[id]; r != nil && (r.Env != "" || r.SenderCopy != "") {
		t.Fatal("expired outgoing ciphertext restored by state merge")
	}
	if r := restarted.st.Sent[id]; r != nil {
		r.Block = 1
		w.must(restarted.save())
		restarted.checkSent()
		w.must(restarted.save())
		if restarted.st.Sent[id] != nil {
			t.Fatal("pruned confirmed record restored by state merge")
		}
	}
}

func TestResendMixedKeyKindsAndSenderCopyBinding(t *testing.T) {
	w := newWorld(t)
	a, _ := w.owner("alice")
	w.ownerWith("bob", true)
	a.relay = "https://relay.invalid"
	id, queued := queuedMail(t, a, SendOpts{To: "bob", Text: "first"})
	other, _ := queuedMail(t, a, SendOpts{To: "bob", Text: "second"})
	if len(queued) != 2 || a.st.Sent[id].SenderCopy == "" {
		t.Fatal("mixed-key send did not retain sealed sender copy")
	}
	senderCopy := a.st.Sent[id].SenderCopy
	a.st.Sent[id].SenderCopy = a.st.Sent[other].SenderCopy
	if _, err := a.resend(id); err == nil {
		t.Fatal("swapped sender copy refreshed a different logical message")
	}
	a.st.Sent[id].SenderCopy = senderCopy
	a.relay = ""
	if _, err := a.resend(id); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRootSurvivesInterruptedRetirement(t *testing.T) {
	w := newWorld(t)
	a, _ := w.owner("alice")
	w.owner("bob")
	a.relay = "https://relay.invalid"
	id, queued := queuedMail(t, a, SendOpts{To: "bob", Text: "hello"})
	d := openedMail(t, a, queued[0])
	w.must(a.locked(func() { a.st.Sent[id].Refreshes = mailRefreshMax }))
	old := capture
	capture = func(Envelope) {}
	t.Cleanup(func() { capture = old })
	// Simulate interruption after the replacement's first save, before the
	// old record is retired. The reservation must remain attached to its root.
	child, err := a.sendDatagramDelivery(d.Type, d.To, d.Body, false, d.DeliveryID, d.Created, id)
	w.must(err)
	a, err = openClient(a.dir, w.ch)
	w.must(err)
	a.relay = "https://relay.invalid"
	if _, err := a.resend(child); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("interrupted retirement reset attempt cap: %v", err)
	}
}

func TestResendEntryPoints(t *testing.T) {
	for _, surface := range []string{"cli", "mcp"} {
		t.Run(surface, func(t *testing.T) {
			w := newWorld(t)
			a, _ := w.owner("alice")
			w.owner("bob")
			a.relay = "https://relay.invalid"
			id, _ := queuedMail(t, a, SendOpts{To: "bob", Text: "hello"})
			a.relay = ""
			var result map[string]any
			if surface == "cli" {
				out, err := cli(t, w, "resend", "--dir", a.dir, id)
				w.must(err)
				w.must(json.Unmarshal([]byte(out), &result))
			} else {
				out, err := runTool(a, "tern_read", map[string]any{"id": id})
				w.must(err)
				if out.(map[string]any)["can_refresh"] != true {
					t.Fatal("MCP outgoing status did not expose refresh")
				}
				out, err = runTool(a, "tern_resend", map[string]any{"id": id})
				w.must(err)
				result = out.(map[string]any)
			}
			if result["replaces"] != id || result["sent"] == id {
				t.Fatalf("wrong replacement result: %+v", result)
			}
		})
	}
}
