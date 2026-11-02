// Keys ahead, and replacing them (red team B-4).
//
//	keys [--ahead N]        epochs of keys published in advance: 7 by default, 3 with --privacy strict
//	rekey                   new epoch keys from the next epoch on and a new view key, published now
//	rekey --compromised     an image of this folder is in someone else's hands (docs/RUNBOOK.md, "A seized device")
//
// A sender seals to the recipient's key for the epoch, published ahead on
// chain, so a device holds keys for mail not sent yet: an image taken today
// opens the letters of every epoch published ahead. Before, that was 14
// epochs, and 'keys' re-published the same files: nothing replaced them.
//
//   - The horizon is shorter: 7 epochs (strict privacy: 3). An image without
//     a rekey opens at most the next 6 (strict: 2) days of mail.
//   - Every run that reads mail (a scan, once an epoch) publishes again when
//     fewer than half the horizon is left, so a device that runs at least
//     every few days never runs out. One publish is one Registry call
//     (about 46k gas for 7 light keys, 575k for 7 post-quantum ones), about
//     every 4 days by default and every 2 days strict.
//   - A recipient with no key published for the epoch gets no mail: the
//     sender's command fails and says why, nothing is sealed to an older or
//     weaker key. An owner away for longer than the horizon sets keys
//     --ahead N first (at most 28): a longer horizon, a longer exposure.
//   - rekey deletes the key files from the next epoch on (--compromised:
//     from this one), makes new ones and a new view key, and publishes them
//     through the last epoch any earlier publish covered (senders take the
//     latest publish covering an epoch, so no old key stays in use). The
//     owner's agents get the family's new view key sealed to them; they
//     check it against the owner's publish and publish again. The old view
//     key stays a day to find mail already in flight (it opens nothing).
//   - --compromised also rotates the private groups this owner administers
//     and says what the client can't do alone: the image holds eth.key, so
//     the name itself must move to a new key (guardian recovery), and the
//     agents must be granted again under it.
//
// The publish is the same controller-signed Registry.publishKeys as ever:
// correspondents who pin keys see a re-publish by the same controller, a
// legitimate rotation; a thief's re-publish after a key change is the alarm.
package main

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	strictKeysAhead = 3               // epochs of keys published in advance, strict privacy
	maxKeysAhead    = 28              // keys --ahead at most
	tViewKey        = "tern.view-key" // an owner to its agents: the family's new view key (rekey)
	keysAlert       = "your mail keys: "
)

// ViewKeyMsg: the body of a tViewKey datagram.
type ViewKeyMsg struct {
	ViewKey string `json:"view_key"` // hex X25519 private key
}

// ahead: epochs of keys published in advance (strict privacy: at most 3).
func (i *Ident) ahead() uint64 {
	a := i.f.Ahead
	if a == 0 {
		a = defaultKeysAhead
	}
	if strict() {
		a = min(a, strictKeysAhead)
	}
	return a
}

// setAhead: keys --ahead N (identity.json).
func (c *Client) setAhead(n uint64) error {
	if n < 2 || n > maxKeysAhead {
		return fmt.Errorf("--ahead %d: 2 to %d epochs (a longer horizon means a seized device opens more of the mail to come)", n, maxKeysAhead)
	}
	c.f.Ahead = n
	return writeJSON(filepath.Join(c.dir, "identity.json"), c.f)
}

// ---- the view key a rekey replaced

// mineView: is this envelope tagged to our family (the view key, or the one
// a rekey replaced, for mail in flight)?
func (c *Client) mineView(e *Envelope) bool {
	return c.view != nil && e.mine(c.view) || c.prevView != nil && e.mine(c.prevView)
}

func (i *Ident) prevPath() string { return filepath.Join(i.dir, "view.prev") }

func (i *Ident) loadPrevView() {
	b, err := os.ReadFile(i.prevPath())
	if err != nil {
		return
	}
	k, until, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	raw, _ := hex.DecodeString(k)
	if v, err := ecdh.X25519().NewPrivateKey(raw); err == nil {
		i.prevView = v
		i.prevUntil, _ = strconv.ParseUint(strings.TrimSpace(until), 10, 64)
	}
}

// rotateView: v becomes the view key; the one it replaces finds mail in
// flight through epoch until.
func (c *Client) rotateView(v *ecdh.PrivateKey, until uint64) error {
	if c.view != nil {
		if err := os.WriteFile(c.prevPath(), []byte(hex.EncodeToString(c.view.Bytes())+"\n"+strconv.FormatUint(until, 10)+"\n"), 0o600); err != nil {
			return err
		}
		c.prevView, c.prevUntil = c.view, until
	}
	c.bkMemo = nil // the buckets we read change with it
	return c.setView(v)
}

// dropPrevView: past its day, the replaced view key goes.
func (c *Client) dropPrevView(e uint64) {
	if c.prevView != nil && e > c.prevUntil {
		wipeFile(c.prevPath())
		c.prevView, c.bkMemo = nil, nil
	}
}

// syncView: the view key on disk, if another process (rekey, an agent's
// scan) replaced the one this process holds.
func (c *Client) syncView() {
	b, err := os.ReadFile(filepath.Join(c.dir, "view.key"))
	if err != nil || c.view == nil {
		return
	}
	raw, _ := hex.DecodeString(strings.TrimSpace(string(b)))
	if bytes.Equal(raw, c.view.Bytes()) {
		return
	}
	if v, err := ecdh.X25519().NewPrivateKey(raw); err == nil {
		c.view, c.prevView, c.bkMemo = v, nil, nil
		c.loadPrevView()
	}
}

// wipeFile overwrites a key file before unlinking it (as destroyOld does).
func wipeFile(p string) error {
	if st, err := os.Stat(p); err == nil {
		os.WriteFile(p, make([]byte, st.Size()), 0o600)
	}
	return os.Remove(p)
}

// ---- publishing ahead

// keysThrough: the last epoch from e on that name has keys published for
// without a gap (false: none for e).
func (c *Client) keysThrough(name string, e uint64) (uint64, bool) {
	t := e
	for ; t < e+2*maxKeysAhead; t++ {
		if _, _, err := c.res.keysAt(name, t); err != nil {
			break
		}
	}
	if t == e {
		return 0, false
	}
	return t - 1, true
}

// publishedMax: the last epoch any publish of name covers.
func (c *Client) publishedMax(name string) uint64 {
	p, err := c.res.party(name)
	if err != nil {
		return 0
	}
	var m uint64
	for _, k := range p.keys {
		if n := uint64(len(k.keys)); n > 0 && k.first+n-1 > m {
			m = k.first + n - 1
		}
	}
	return m
}

// topUpKeys: publish again when fewer than half the horizon is left (once an
// epoch, from destroyExpired). A failure is an alert: senders find no key
// past the last epoch published.
func (c *Client) topUpKeys(e uint64) {
	if c.view == nil {
		return
	}
	lead := max(c.ahead()/2, 1)
	t, ok := c.keysThrough(c.f.Name, e)
	if !ok || t < e+lead {
		if err := c.publishFrom(e, c.ahead()); err != nil {
			left := fmt.Sprintf("published through epoch %d only (now %d)", t, e)
			if !ok {
				left = fmt.Sprintf("none published for this epoch (%d)", e)
			}
			c.retractAlerts(keysAlert)
			c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("%s%s, and publishing more failed: %v. Senders can't write to you past it: run keys",
				keysAlert, left, err))
			return
		}
	}
	c.retractAlerts(keysAlert)
	if !c.isOwner() && c.f.Owner != "" { // mail to and from a family needs the owner's keys too
		if t, ok := c.keysThrough(c.f.Owner, e); !ok || t <= e {
			c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("%syour owner %s's keys end this epoch (%d) or have ended: mail to and from you stops "+
				"until your owner runs any command that reads mail (inbox) or keys on its device; tell your owner (ask-owner)", keysAlert, c.f.Owner, e))
		}
	}
}

// noKeyErr: a recipient with no key for this epoch gets nothing, and the
// sender is told why (never sealed to an older key).
func noKeyErr(err error, name string) error {
	return fmt.Errorf("%w: nothing was sent. %s's device has not run since its keys ran out (they are published days ahead and "+
		"renewed by any command that reads mail); send it again once %s is back", err, name, name)
}

// ---- rekey

// rekey replaces this identity's epoch keys and (an owner's) the family's
// view key, and publishes them. compromised: an image of this folder is in
// someone else's hands (the owner only).
func (c *Client) rekey(compromised bool) (map[string]any, error) {
	if compromised && !c.isOwner() {
		return nil, errors.New("rekey --compromised is the owner's: run it with the owner's --dir; for a seized agent folder, the owner runs owner revoke AGENT --compromised and grants a new one")
	}
	if c.view == nil {
		return nil, errNotJoined
	}
	r := map[string]any{"identity": c.f.Name, "compromised": compromised}
	// Mail sealed to the keys about to go: read it first.
	if _, err := c.scan(); err != nil {
		r["scan"] = "failed (" + err.Error() + "): mail sent to the old keys and not read yet won't open"
	}
	e, err := c.ch.epoch()
	if err != nil {
		return nil, err
	}
	from := e + 1 // today's key stays: letters sealed to it today still open
	if compromised {
		from = e // the image holds it: today's letters from now on go to a new one
	}
	through := max(e+c.ahead()-1, c.publishedMax(c.f.Name)) // no epoch left to an earlier publish
	gone, err := c.destroyFrom(from)
	if err != nil {
		return nil, err
	}
	if c.isOwner() {
		v, err := newViewKey()
		if err != nil {
			return nil, err
		}
		if err := c.rotateView(v, e+1); err != nil {
			return nil, err
		}
	}
	if err := c.publishFrom(e, through-e+1); err != nil {
		return r, fmt.Errorf("new keys made but not published (%w): run rekey again", err)
	}
	r["epoch"], r["new_keys_from_epoch"], r["published_through"], r["destroyed_epochs"] = e, from, through, gone
	if c.isOwner() {
		r["view_key"] = "new (the old one finds mail in flight until epoch " + fmt.Sprint(e+1) + ", then is deleted)"
		r["agents"] = c.sendViewKey()
	}
	if compromised {
		r["groups_rotated"], r["next"] = c.seizureSteps()
	}
	return r, c.save()
}

// destroyFrom deletes the epoch keys of epochs from on.
func (c *Client) destroyFrom(from uint64) ([]uint64, error) {
	var gone []uint64
	ents, _ := os.ReadDir(filepath.Join(c.dir, "epochs"))
	for _, ent := range ents {
		n, err := strconv.ParseUint(strings.TrimSuffix(ent.Name(), ".key"), 10, 64)
		if err != nil || n < from {
			continue
		}
		if err := wipeFile(filepath.Join(c.dir, "epochs", ent.Name())); err != nil {
			return gone, err
		}
		gone = append(gone, n)
	}
	sort.Slice(gone, func(a, b int) bool { return gone[a] < gone[b] })
	return gone, nil
}

// myAgents: the owner's agents whose grants are valid now.
func (c *Client) myAgents() []string {
	evs, err := c.ch.events("Registry", "AgentGranted", nil, []common.Hash{c.id()})
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, ev := range evs {
		n, _ := ev.Args["name"].(string)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		if v, err := c.ch.call("Registry", "grantValid", idOf(n)); err == nil && v[0].(bool) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// sendViewKey: the family's new view key, sealed to each agent (its keys and
// the owner's new ones).
func (c *Client) sendViewKey() map[string]string {
	out := map[string]string{}
	for _, a := range c.myAgents() {
		if _, err := c.sendDatagram(tViewKey, a, ViewKeyMsg{ViewKey: hex.EncodeToString(c.view.Bytes())}); err != nil {
			out[a] = "not sent (" + err.Error() + "): run rekey again, or grant it again (the bundle carries the new view key)"
			continue
		}
		out[a] = "sent: it installs it and publishes again at its next scan"
	}
	return out
}

// currentFamilyView: a view key from the owner must be the one the owner
// publishes now (a bundle or message from before a rekey is refused).
func (c *Client) currentFamilyView(v *ecdh.PrivateKey) error {
	e, err := c.ch.epoch()
	if err != nil {
		return err
	}
	c.res.forget()
	pub, _, err := c.res.keysAt(c.f.Owner, e)
	if err != nil {
		return nil // the owner's keys ran out: nothing to compare with
	}
	if !bytes.Equal(pub.Bytes(), v.PublicKey().Bytes()) {
		return fmt.Errorf("this view key is not the one %s publishes now (it re-keyed since): ask %s for a new grant (owner grant)", c.f.Owner, c.f.Owner)
	}
	return nil
}

// acceptViewKey: an agent installs its owner's new view key and publishes again.
func (c *Client) acceptViewKey(it *Item, d *Datagram) {
	var m ViewKeyMsg
	it.Body, it.Done = json.RawMessage(`{"view_key":"(kept in the identity folder, never shown)"}`), true
	if c.isOwner() || d.From != c.f.Owner || json.Unmarshal(d.Body, &m) != nil {
		it.Verified += " (ignored: only your owner sends your family's view key)"
		return
	}
	raw, _ := hex.DecodeString(m.ViewKey)
	v, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return
	}
	if c.view != nil && bytes.Equal(v.Bytes(), c.view.Bytes()) {
		return
	}
	if err := c.currentFamilyView(v); err != nil {
		it.Verified += " (ignored: " + err.Error() + ")"
		return
	}
	e, err := c.ch.epoch()
	if err != nil || c.rotateView(v, e+1) != nil {
		return
	}
	if err := c.publishKeys(); err != nil {
		c.st.Alerts = append(c.st.Alerts, keysAlert+"your owner re-keyed; publishing your keys with the new view key failed ("+err.Error()+"): run keys")
	}
}

// seizureSteps: what --compromised does about groups, and what it can't do alone.
func (c *Client) seizureSteps() ([]string, []string) {
	var rotated, member []string
	names := make([]string, 0, len(c.st.Groups))
	for n := range c.st.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g := c.st.Groups[n]
		if g.IsPublic {
			continue
		}
		if r, err := c.rotateGroup(n, "rekey --compromised"); err == nil {
			rotated = append(rotated, fmt.Sprintf("%s: generation %v", n, r["generation"]))
		} else {
			member = append(member, n)
		}
	}
	hours := uint64(72)
	if v, err := c.ch.call("Registry", "recoveryDelay"); err == nil {
		hours = v[0].(uint64) / 3600
	}
	o := c.f.Name
	next := []string{
		"The image also holds eth.key: whoever has it acts as " + o + " (spends your Bank balance, grants agents, publishes keys of its own) until the name moves to a new key. These new keys protect mail sealed to them, not mail already sealed to the old keys or your name itself. Upgrade or restart correspondents using older clients with cached keys.",
		"On a clean device: ternchain owner keygen --out NEW.key (prints NEW-ADDRESS).",
		"On your guardian's device (kept apart; it is not in the image): ternchain recovery start --dir GUARDIAN-DIR " + o + " NEW-ADDRESS, then after " + fmt.Sprint(hours) + " h: ternchain recovery finish --dir GUARDIAN-DIR " + o + ". The thief can veto it with the stolen key (owner cancel-recovery): if it does, the name is lost; tell your correspondents out of band.",
		"After the recovery, on the clean device: ternchain owner init --dir id/" + o + " --name " + o + " --key NEW.key --guardian-dir GUARDIAN-DIR (it takes up the name with new view and epoch keys); never use this folder again.",
		"Agents: the recovery voids their grants. An agent whose folder was on the seized device: ternchain owner revoke --dir id/" + o + " AGENT --compromised now. Each agent then: agent init (a new folder), owner grant, agent join.",
		"Ask your correspondents to send again what they sent you in the last hour: letters sealed to the destroyed keys show as an alert, \"tagged to your family but you can't open it\".",
	}
	if len(member) > 0 {
		next = append(next, "Private groups not rotated here (you are not their admin, or it failed; the image holds their current keys until the next rotation): ask their admins to run group rotate: "+strings.Join(member, ", ")+".")
	}
	next = append(next, "docs/RUNBOOK.md, \"A seized device\".")
	return rotated, next
}
