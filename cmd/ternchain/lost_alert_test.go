package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// skipPast: the client's scan cursor moves past everything on chain now, as
// an older client's did when it skipped a batch: the scan won't read it.
func skipPast(t *testing.T, w *world, c *Client) {
	t.Helper()
	head, err := w.ch.b.BlockNumber(ctxb())
	w.must(err)
	c.st.Cursor = head + 1
	c.st.PendingDA = nil
}

// A message sent through a relay, committed on chain as a batch of its own,
// but in a block the scan skipped: called lost, then found by its id
// (Inbox.commitment) on the next scan and the alert taken back.
func TestLostSendFoundAsOwnBatch(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	node, srv := newNode(w)
	alice.relay = srv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "through the relay"})
	w.must(err)
	alice.st.Sent[id].Receipt = nil // the relay path's alert (no receipt kept, as an older client)
	w.must(node.flush())
	skipPast(t, w, alice)
	w.later(6 * time.Minute)
	alice.checkSent()
	if r := alice.st.Sent[id]; !r.Lost || !hasAlert(alice, id[:18]) {
		t.Fatalf("not called lost: %+v %v", r, alice.st.Alerts)
	}
	scan(t, alice)
	r := alice.st.Sent[id]
	if r.Lost || r.Block == 0 || r.Batch != id || r.Node != srv.URL {
		t.Fatalf("not found on chain as its own batch: %+v", r)
	}
	if hasAlert(alice, id[:18]) {
		t.Fatalf("the alert stayed: %v", alice.st.Alerts)
	}
}

// In a batch with someone else's envelope (its root is not our id): found
// by reading the batches committed since it was handed over.
func TestLostSendFoundInSharedBatch(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	carol, _ := w.owner("carol")
	w.owner("bob")
	node, srv := newNode(w)
	alice.relay, carol.relay = srv.URL, srv.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "mine"})
	w.must(err)
	_, err = carol.send(SendOpts{To: "bob", Text: "carol's"})
	w.must(err)
	alice.st.Sent[id].Receipt = nil
	w.must(node.flush())
	skipPast(t, w, alice)
	w.later(6 * time.Minute)
	alice.checkSent()
	if !hasAlert(alice, id[:18]) {
		t.Fatalf("no alert: %v", alice.st.Alerts)
	}
	scan(t, alice)
	r := alice.st.Sent[id]
	if r.Lost || r.Block == 0 || r.Batch == "" || r.Batch == id || r.Node != srv.URL || hasAlert(alice, id[:18]) {
		t.Fatalf("not found in the shared batch: %+v %v", r, alice.st.Alerts)
	}
}

// A relay that never posts: the alert stays across scans; the search moves on.
func TestLostSendStillMissingKeepsAlert(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	black := httptest.NewServer((&Relay{ch: w.ch}).Handler())
	defer black.Close()
	alice.relay = black.URL
	id, err := alice.send(SendOpts{To: "bob", Text: "into the void"})
	w.must(err)
	w.later(6 * time.Minute)
	scan(t, alice)
	scan(t, alice)
	r := alice.st.Sent[id]
	if !r.Lost || r.Block != 0 || r.Sought == 0 || !hasAlert(alice, id[:18]) {
		t.Fatalf("a missing message lost its alert: %+v %v", r, alice.st.Alerts)
	}
}

// Alerts expire after 7 days; 'done --alerts' (and tern_done alerts:true) clears them.
func TestAlertsExpireAndClear(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	alice.st.Alerts = append(alice.st.Alerts, "old news")
	scan(t, alice)
	if !hasAlert(alice, "old news") || alice.st.AlertsAt["old news"] == 0 {
		t.Fatalf("not stamped: %v %v", alice.st.Alerts, alice.st.AlertsAt)
	}
	w.later(6 * 24 * time.Hour)
	alice.st.Alerts = append(alice.st.Alerts, "new news")
	scan(t, alice)
	w.later(2 * 24 * time.Hour)
	scan(t, alice)
	if hasAlert(alice, "old news") || !hasAlert(alice, "new news") || len(alice.st.AlertsAt) != 1 {
		t.Fatalf("expiry: %v %v", alice.st.Alerts, alice.st.AlertsAt)
	}
	// the CLI
	w.must(alice.save())
	testChain = w.ch
	defer func() { testChain = nil }()
	os.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	w.must(run([]string{"done", "--dir", alice.dir, "--alerts"}))
	again, err := openClient(alice.dir, w.ch)
	w.must(err)
	if len(again.st.Alerts) != 0 || len(again.st.AlertsAt) != 0 {
		t.Fatalf("done --alerts left %v", again.st.Alerts)
	}
	// the MCP tool
	again.st.Alerts = []string{"one more"}
	res, err := runTool(again, "tern_done", map[string]any{"alerts": true})
	w.must(err)
	if m, _ := res.(map[string]any); m["alerts_cleared"] != 1 || len(again.st.Alerts) != 0 {
		t.Fatalf("tern_done alerts: %v %v", res, again.st.Alerts)
	}
	if _, err := runTool(again, "tern_done", map[string]any{}); err == nil {
		t.Fatal("tern_done with neither an id nor alerts")
	}
}

// A state file from before alerts had times: they load, count from now.
func TestOldStateAlertsLoad(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.must(alice.save())
	path := filepath.Join(alice.dir, "state.json")
	var raw map[string]any
	w.must(readJSON(path, &raw))
	raw["alerts"] = []string{"from an old client"}
	delete(raw, "alerts_at")
	w.must(writeJSON(path, raw))
	c, err := openClient(alice.dir, w.ch)
	w.must(err)
	if len(c.st.Alerts) != 1 || c.st.AlertsAt != nil {
		t.Fatalf("old state: %v %v", c.st.Alerts, c.st.AlertsAt)
	}
	scan(t, c)
	if !hasAlert(c, "from an old client") || c.st.AlertsAt["from an old client"] < chainTime(w.ch)-60 {
		t.Fatalf("not counted from now: %v %v", c.st.Alerts, c.st.AlertsAt)
	}
	w.later(8 * 24 * time.Hour)
	scan(t, c)
	if len(c.st.Alerts) != 0 {
		t.Fatalf("an old alert never expires: %v", c.st.Alerts)
	}
}
