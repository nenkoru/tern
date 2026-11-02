package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

const rekeyDay = 24 * time.Hour

func rekeySend(t *testing.T, from *Client, to, text string) string {
	t.Helper()
	id, err := from.send(SendOpts{To: to, Intent: "inform", Text: text})
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return id
}

// dirHas: any file under dir contains s.
func dirHas(dir, s string) bool {
	found := false
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(s)) {
				found = true
			}
		}
		return nil
	})
	return found
}

// imageOf: a copy of c's identity folder now (a seized device's image).
func imageOf(t *testing.T, c *Client) string {
	t.Helper()
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "image")
	err := filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(c.dir, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// imageOpens: do the epoch keys in an image open envelope id and show text?
func imageOpens(t *testing.T, dir string, c *Client, id, text string) bool {
	t.Helper()
	var ids []age.Identity
	ents, _ := os.ReadDir(filepath.Join(dir, "epochs"))
	for _, e := range ents {
		if b, err := os.ReadFile(filepath.Join(dir, "epochs", e.Name())); err == nil {
			if k, _, err := parseIdentity(string(b)); err == nil {
				ids = append(ids, k)
			}
		}
	}
	bs, err := c.batchesSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		envs, _ := decodeBatch(b.Data)
		for _, e := range envs {
			if e.hash().Hex() != id {
				continue
			}
			plain, err := e.open(ids)
			if err != nil {
				return false
			}
			var sg Signed
			var d Datagram
			if json.Unmarshal(plain, &sg) == nil && json.Unmarshal(sg.Payload, &d) == nil {
				return bytes.Contains(d.Body, []byte(text))
			}
			return bytes.Contains(plain, []byte(text))
		}
	}
	t.Fatalf("envelope %s not on chain", id)
	return false
}

// keyFile: an epoch key file's content ("" if gone).
func keyFile(c *Client, e uint64) string {
	b, _ := os.ReadFile(c.epochPath(e))
	return string(b)
}

// published: name's epoch recipient and view key for e, read afresh.
func published(t *testing.T, w *world, name string, e uint64) (string, []byte) {
	t.Helper()
	v, r, err := newResolver(w.ch).keysAt(name, e)
	if err != nil {
		t.Fatal(err)
	}
	return r, v.Bytes()
}

func mailText(items []*Item, text string) *Item {
	for _, it := range items {
		if it.Mail != nil && it.Mail.Text == text {
			return it
		}
	}
	return nil
}

// rekey: new keys from the next epoch on and a new view key, published;
// the old files are gone; senders use the new keys and the owner reads them.
func TestRekeyRegeneratesAndRepublishes(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.st.Contacts["alice"] = true
	e, _ := w.ch.epoch()
	if th, ok := bob.keysThrough("bob", e); !ok || th != e+defaultKeysAhead-1 {
		t.Fatalf("published through %d, want %d", th, e+defaultKeysAhead-1)
	}
	old0, old1, old5 := keyFile(bob, e), keyFile(bob, e+1), keyFile(bob, e+5)
	rec1, view := published(t, w, "bob", e+1)
	rekeySend(t, alice, "bob", "before") // alice's process has bob's old keys in its cache

	r, err := bob.rekey(false)
	if err != nil {
		t.Fatal(err)
	}
	if r["new_keys_from_epoch"] != e+1 {
		t.Fatalf("%v", r)
	}
	if keyFile(bob, e) != old0 {
		t.Fatal("rekey without --compromised replaced today's key (letters sealed to it today would not open)")
	}
	for _, k := range []string{old1, old5} {
		if k == "" || dirHas(bob.dir, strings.TrimSpace(k)) {
			t.Fatal("an old epoch key is still on disk")
		}
	}
	rec1n, viewN := published(t, w, "bob", e+1)
	if rec1n == rec1 || bytes.Equal(viewN, view) {
		t.Fatal("the published keys did not change")
	}
	if _, v0 := published(t, w, "bob", e); !bytes.Equal(v0, viewN) {
		t.Fatal("today's publish still carries the old view key")
	}
	if !bytes.Equal(bob.view.PublicKey().Bytes(), viewN) || bob.prevView == nil {
		t.Fatal("view.key not replaced, or the old one not kept for mail in flight")
	}

	// Today (alice's stale cache: old view key, today's key unchanged) and tomorrow (fresh keys).
	rekeySend(t, alice, "bob", "today")
	scan(t, bob)
	if mailText(bob.st.Items, "before") == nil || mailText(bob.st.Items, "today") == nil {
		t.Fatal("bob lost mail sent around the rekey")
	}
	w.later(rekeyDay)
	rekeySend(t, alice, "bob", "tomorrow")
	if v, rec, err := alice.res.keysAt("bob", e+1); err != nil || bytes.Equal(v.Bytes(), view) || rec != rec1n {
		t.Fatal("a day later alice still seals to bob's old keys")
	}
	if mailText(scan(t, bob), "tomorrow") == nil {
		t.Fatal("mail to the new keys did not open for bob")
	}
	// Past its day the old view key is gone.
	w.later(rekeyDay)
	scan(t, bob)
	if bob.prevView != nil {
		t.Fatal("the replaced view key outlived its day")
	}
	if _, err := os.Stat(filepath.Join(bob.dir, "view.prev")); !os.IsNotExist(err) {
		t.Fatal("view.prev still on disk")
	}
}

// rekey --compromised: today's key too; a same-day image opens nothing sent
// after it; mail sealed to the destroyed keys is an alert, never silence.
func TestRekeyCompromised(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.st.Contacts["alice"] = true
	e, _ := w.ch.epoch()
	scan(t, bob)
	snap := imageOf(t, bob)
	old0 := keyFile(bob, e)
	rekeySend(t, alice, "bob", "warm") // alice's process caches bob's keys
	scan(t, bob)

	view, recipient, err := alice.res.keysAt("bob", e)
	if err != nil {
		t.Fatal(err)
	}
	inFlight, err := seal([]byte("already sealed"), e, view, nil, []string{recipient}, w.ch.bucketBits())
	if err != nil {
		t.Fatal(err)
	}

	r, err := bob.rekey(true)
	if err != nil {
		t.Fatal(err)
	}
	if keyFile(bob, e) == old0 || keyFile(bob, e) == "" {
		t.Fatal("--compromised kept today's key")
	}
	if next, _ := r["next"].([]string); len(next) < 4 || !strings.Contains(strings.Join(next, "\n"), "recovery start --dir GUARDIAN-DIR bob") {
		t.Fatalf("no guardian-recovery steps: %v", r["next"])
	}
	// Already sealed before the rekey but delivered afterwards: report the loss.
	if err := alice.post([]Envelope{inFlight}); err != nil {
		t.Fatal(err)
	}
	got := scan(t, bob)
	if mailText(got, "in-flight") != nil {
		t.Fatal("a letter sealed to a destroyed key opened")
	}
	alerted := false
	for _, a := range bob.st.Alerts {
		alerted = alerted || strings.Contains(a, "tagged to your family but you can't open it")
	}
	if !alerted {
		t.Fatal("a letter sealed to the destroyed keys was lost silently")
	}
	// The same long-running sender must observe the replacement in this epoch.
	id := rekeySend(t, alice, "bob", "same-day")
	if imageOpens(t, snap, alice, id, "same-day") {
		t.Fatal("a cached correspondent key let the image open mail sent after rekey")
	}
	if mailText(scan(t, bob), "same-day") == nil {
		t.Fatal("a long-running sender used the destroyed epoch key")
	}
	// A fresh sender, same day: the new keys; the image opens nothing.
	carol, _ := w.owner("carol")
	bob.st.Contacts["carol"] = true
	id = rekeySend(t, carol, "bob", "after")
	if imageOpens(t, snap, carol, id, "after") {
		t.Fatal("the image opens a letter sent after rekey --compromised")
	}
	if mailText(scan(t, bob), "after") == nil {
		t.Fatal("bob can't read mail to his new keys")
	}
}

// An owner's rekey reaches its agents: the agent installs the family's new
// view key and publishes again; a bundle from before the rekey is refused.
func TestRekeyAgentsGetViewKey(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	oscar, _ := w.owner("oscar")
	ada := w.agent(oscar, "ada", coins(1), `{}`)
	dir := filepath.Join(w.dir, "bo.oscar")
	if _, err := newIdentDir(dir, IdentityFile{Name: "bo.oscar", Kind: "agent", Owner: "oscar"}); err != nil {
		t.Fatal(err)
	}
	bo, err := openClient(dir, w.ch)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := oscar.grant(AgentRequest{Name: "bo", Owner: "oscar", Address: bo.addr().Hex()}, 30, coins(1), `{}`, coins(1))
	if err != nil {
		t.Fatal(err)
	}

	r, err := oscar.rekey(false)
	if err != nil {
		t.Fatal(err)
	}
	if ag, _ := r["agents"].(map[string]string); !strings.HasPrefix(ag["ada.oscar"], "sent") {
		t.Fatalf("agents: %v", r["agents"])
	}
	scan(t, ada)
	if !bytes.Equal(ada.view.Bytes(), oscar.view.Bytes()) {
		t.Fatal("ada did not install the family's new view key")
	}
	e, _ := w.ch.epoch()
	if _, v := published(t, w, "ada.oscar", e); !bytes.Equal(v, oscar.view.PublicKey().Bytes()) {
		t.Fatal("ada did not publish again with the new view key")
	}
	for _, it := range ada.st.Items {
		if it.Type == tViewKey && bytes.Contains(it.Body, []byte(stale.ViewKey)) {
			t.Fatal("a view key kept in state.json")
		}
	}
	if err := bo.join(*stale); err == nil || !strings.Contains(err.Error(), "re-keyed") {
		t.Fatalf("a bundle from before the rekey joined: %v", err)
	}
	ada.st.Contacts["alice"] = true
	rekeySend(t, alice, "ada.oscar", "to-ada")
	if mailText(scan(t, ada), "to-ada") == nil {
		t.Fatal("ada can't read mail after the rekey")
	}
}

// The destructive mode is the owner's: an agent can't run it, and no MCP tool does.
func TestRekeyCompromisedOwnerOnly(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	ada := w.agent(oscar, "ada", coins(1), `{}`)
	e, _ := w.ch.epoch()
	k := keyFile(ada, e)
	if _, err := ada.rekey(true); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("an agent ran rekey --compromised: %v", err)
	}
	if keyFile(ada, e) != k {
		t.Fatal("the refused rekey touched the agent's keys")
	}
	for _, tl := range mcpTools {
		if strings.Contains(tl.Name, "rekey") || strings.Contains(tl.Name, "keys") {
			t.Fatalf("MCP exposes %s", tl.Name)
		}
	}
	if _, err := runTool(ada, "tern_rekey", map[string]any{"compromised": true}); err == nil {
		t.Fatal("tern_rekey ran")
	}
	// A plain rekey of the agent's own epoch keys is allowed; the family view key stays the owner's.
	if _, err := ada.rekey(false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ada.view.Bytes(), oscar.view.Bytes()) {
		t.Fatal("an agent's rekey changed the family view key")
	}
}

// Horizon: 7 epochs by default, 3 strict; any scan publishes again before it runs out.
func TestKeysAheadRenewedBeforeTheEnd(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	bob.st.Contacts["alice"] = true
	e, _ := w.ch.epoch()
	through := func(c *Client) uint64 {
		c.res.forget()
		th, _ := c.keysThrough(c.f.Name, e)
		return th
	}
	if through(bob) != e+6 {
		t.Fatalf("published through %d", through(bob))
	}
	w.later(3 * rekeyDay)
	scan(t, bob)
	if through(bob) != e+6 {
		t.Fatal("published again with half the horizon left")
	}
	w.later(rekeyDay)
	scan(t, bob) // e+4: e+7 not covered
	if through(bob) != e+4+6 {
		t.Fatalf("not renewed before the end: through %d", through(bob))
	}

	// Offline past the horizon: the sender is told, nothing is sealed to an old key; after a scan it goes.
	w.later(12 * rekeyDay) // e+16
	if _, err := alice.send(SendOpts{To: "bob", Intent: "inform", Text: "late"}); err == nil || !strings.Contains(err.Error(), "nothing was sent") {
		t.Fatalf("send to a recipient with no key: %v", err)
	}
	scan(t, bob)
	rekeySend(t, alice, "bob", "late")
	if mailText(scan(t, bob), "late") == nil {
		t.Fatal("mail after the renewal did not arrive")
	}

	// Strict: 3 ahead, renewed every 2 epochs.
	old := privacyLevel
	privacyLevel = privacyStrict
	defer func() { privacyLevel = old }()
	carol, _ := w.owner("carol")
	e, _ = w.ch.epoch()
	if through(carol) != e+strictKeysAhead-1 {
		t.Fatalf("strict: published through %d", through(carol))
	}
	w.later(rekeyDay)
	scan(t, carol)
	if through(carol) != e+2 {
		t.Fatal("strict: published again too early")
	}
	w.later(rekeyDay)
	scan(t, carol)
	if through(carol) != e+4 {
		t.Fatalf("strict: not renewed: through %d", through(carol))
	}
	if err := carol.setAhead(maxKeysAhead + 1); err == nil {
		t.Fatal("--ahead past the maximum")
	}
	if err := carol.setAhead(14); err != nil || carol.ahead() != strictKeysAhead {
		t.Fatal("strict caps --ahead")
	}
}
