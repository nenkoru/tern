// What a seized device holds: keys past their retention and read mail past
// its keep are deleted by every client run, not only by 'keys'.
//
//	keys [--retain N] [--keep-days D]   epochs an epoch key outlives its epoch (default 7);
//	                                    days read mail stays in state.json (default 30)
//	keys --ahead N, rekey               the keys published ahead, and replacing them (rekey.go)
//	inbox --purge                       drop handled mail and group posts now
//
// Epoch keys are the per-session keys of this design: a sender seals each
// envelope with a fresh ephemeral X25519 key to the recipient's key for that
// epoch (one a day), published ahead on chain. Once an epoch's key is
// destroyed, its mail opens for nobody, wherever copies are kept; a group's
// generations go the same way (rotate.go). What stays is what the owner keeps:
// the current keys, and mail not yet past its keep.
package main

import "path/filepath"

const defaultKeepDays = 30 // days handled mail and group posts stay on disk

func (c *Client) keepDays() int {
	d := defaultKeepDays
	if c.f.KeepDays > 0 {
		d = c.f.KeepDays
	}
	if strict() {
		d = min(d, strictKeepDays)
	}
	return d
}

// retain: epochs an epoch key outlives its epoch (strict privacy: at most 2).
func (i *Ident) retain() uint64 {
	r := i.f.Retain
	if r == 0 {
		r = defaultRetainEp
	}
	if strict() {
		r = min(r, strictRetain)
	}
	return r
}

// destroyExpired deletes expired epoch keys and group keys, once per epoch
// per process; then it publishes keys ahead before they run out (rekey.go).
func (c *Client) destroyExpired() {
	e, err := c.ch.epoch()
	if err != nil || c.destroyedAt == e+1 {
		return
	}
	c.destroyedAt = e + 1
	c.destroyOld(e)
	for _, g := range c.st.Groups {
		g.destroyOld(e, c.retain())
	}
	c.dropPrevView(e)
	c.res.forget()
	c.topUpKeys(e)
}

// pruneItems drops read mail past its keep (all: handled mail and every group post, now).
func (c *Client) pruneItems(all bool) int {
	c.st.seedMailReceipts()
	now := c.chainNow()
	var keep []*Item
	n := 0
	for _, it := range c.st.Items {
		days := c.keepDays()
		group := ""
		if len(it.To) > 6 && it.To[:6] == "group:" {
			group = it.To[6:]
			if g := c.st.Groups[group]; g != nil && g.KeepDays > 0 && g.KeepDays < days {
				days = g.KeepDays // the group asks for less
			}
		}
		old := it.Time > 0 && it.Time < now-int64(days)*86400 // no time yet (pending): not old
		drop := old && (it.Done || group != "") || all && (it.Done || group != "")
		if drop {
			c.st.purgeDirectMail(it)
			n++
			continue
		}
		keep = append(keep, it)
	}
	c.st.Items = keep
	return n
}

// setRetention: keys --retain / --keep-days (identity.json).
func (c *Client) setRetention(retain uint64, keepDays int) error {
	if retain > 0 {
		c.f.Retain = retain
	}
	if keepDays > 0 {
		c.f.KeepDays = keepDays
	}
	return writeJSON(filepath.Join(c.dir, "identity.json"), c.f)
}
