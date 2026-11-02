// A data node that lets a sender down (red team C2-WITHHOLD): it signs a
// receipt and either commits no batch with the envelope (a missed-receipt
// claim slashes it: 'da prove-missed'), or commits the batch and never serves
// its bytes ("commit and withhold"). Against the second a missed-receipt claim
// is the wrong tool: the node rebuts it with its commit and takes the claimant's
// bond. What works is the availability challenge (Inbox.challenge): the node
// must put the batch on chain within the resolve window or lose its stake to
// the challenger.
//
// So, client side, with the contracts as they are:
//   - the sender's alert tells the two cases apart, and 'da prove-missed'
//     refuses a receipt whose node committed a batch we can't fetch;
//   - the sender keeps the envelope's bytes until it is seen on chain and
//     re-posts the same envelope (the same id: recipients keep one copy)
//     through another staked data node when nothing was committed, or on chain
//     if the original batch was withheld or no other node takes it
//     (da_resend, default daResendDefault times);
//   - a batch that concerns us (our receipt's node committed it; an early item
//     of ours from that node never landed) and that no node serves, asked a few
//     times over a quarter hour or more, is challenged with our bond well within
//     the challenge window, and expired for the slash once the node lets the
//     resolve window pass (da_autochallenge: on, off, or all).
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const (
	daResendDefault = 2         // re-sends of one envelope, unless da_resend says otherwise
	daResendMax     = 5         // da_resend's ceiling: each one pays the fee (and gas) again
	daResendKeep    = 128 << 10 // an envelope larger than this isn't kept for a re-send ("send it again")
	daAllNodes      = 8         // the nodes asked for a batch before it is challenged
	// daChallengeMisses: a batch is challenged after this many tries in which
	// no node served it, each at least a gap (an eighth of the challenge
	// window: 7.5 min of the default hour) after the last, so at least a
	// quarter hour after the first; or after two when a third would come too
	// late. Never in the last twelfth of the window (the transaction must land).
	daChallengeMisses = 3
)

// Unserved: a committed batch that concerns us and that no node served yet.
type Unserved struct {
	Poster     string `json:"poster"`
	At         int64  `json:"at"`  // chain time it was committed
	Why        string `json:"why"` // how it concerns us
	Misses     int    `json:"misses,omitempty"`
	MissAt     int64  `json:"miss_at,omitempty"`    // chain time of the last try counted
	Challenged int64  `json:"challenged,omitempty"` // chain time we challenged it (our bond is on it)
	Told       bool   `json:"told,omitempty"`       // we said why we don't challenge it
}

// autoChallengeMode: on (batches that concern us), off, or all.
func (c *Client) autoChallengeMode() string {
	switch c.f.DAAutoChallenge {
	case "off", "all":
		return c.f.DAAutoChallenge
	}
	return "on"
}

// resendCap: how many times an envelope a data node let down is re-sent.
func (c *Client) resendCap() int {
	if c.f.DAResend == nil {
		return daResendDefault
	}
	return min(max(*c.f.DAResend, 0), daResendMax)
}

// setDAAuto: 'da auto --challenge on|off|all --resend N'.
func (c *Client) setDAAuto(challenge string, resend int) error {
	switch challenge {
	case "":
	case "on", "off", "all":
		c.f.DAAutoChallenge = challenge
		if challenge == "on" {
			c.f.DAAutoChallenge = ""
		}
	default:
		return fmt.Errorf("--challenge %q: on, off or all", challenge)
	}
	if resend >= 0 {
		if resend > daResendMax {
			return fmt.Errorf("--resend %d: at most %d (each re-send pays the fee again)", resend, daResendMax)
		}
		c.f.DAResend = &resend
	}
	return c.saveIdentityFile()
}

// daAutoView: the settings, and the batches being chased.
func (c *Client) daAutoView() map[string]any {
	var chased []map[string]any
	for _, root := range sortedKeys(c.st.Unserved) {
		u := c.st.Unserved[root]
		v := map[string]any{"root": root, "poster": u.Poster, "why": u.Why, "tries": u.Misses}
		if u.Challenged != 0 {
			v["challenged_at"] = u.Challenged
		}
		chased = append(chased, v)
	}
	return map[string]any{"da_autochallenge": c.autoChallengeMode(), "da_resend": c.resendCap(), "unserved": chased}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// daWindows: the Inbox's challenge and resolve windows (seconds) and the bond.
func (c *Client) daWindows() (cw, rw int64, bond *big.Int, err error) {
	a, err := c.ch.callFixed("Inbox", "challengeWindow")
	if err != nil {
		return 0, 0, nil, err
	}
	b, err := c.ch.callFixed("Inbox", "resolveWindow")
	if err != nil {
		return 0, 0, nil, err
	}
	d, err := c.ch.callFixed("Inbox", "challengeBond")
	if err != nil {
		return 0, 0, nil, err
	}
	return int64(a[0].(uint64)), int64(b[0].(uint64)), d[0].(*big.Int), nil
}

// withheldRoot: a batch the receipt's node committed by the receipt's
// deadline that no node serves us ("" if none), and when it was committed.
// Our envelope may be in it; a missed-receipt claim would be rebutted with it.
func (c *Client) withheldRoot(rc *Receipt) (string, int64) {
	node := common.HexToAddress(rc.Node)
	from := c.blockAt(rc.Deadline - receiptMaxAhead - lostSlack) // the hand-over was at most receiptMaxAhead before the deadline
	evs, err := c.ch.eventsFrom(from, "Inbox", "Committed", nil, nil, []common.Hash{common.BytesToHash(node.Bytes())})
	if err != nil {
		return "", 0
	}
	var urls map[common.Address]string
	var others []string
	deadline := time.Now().Add(daBudget)
	for i, ev := range evs {
		bt, err := c.ch.blockTime(ev.Block)
		if err != nil || bt > rc.Deadline || i >= lostFetches {
			break
		}
		root := common.Hash(ev.Args["root"].([32]byte))
		st, err := c.ch.call("Inbox", "commitment", root)
		if err != nil || st[2].(uint8) == 3 || st[2].(uint8) == 4 {
			continue // resolved: read from the chain; lost: a claim can't be rebutted with it
		}
		if _, ok := c.st.PendingDA[root.Hex()]; ok { // this scan couldn't fetch it
			return root.Hex(), bt
		}
		if !time.Now().Before(deadline) {
			return root.Hex(), bt // not checked in time: not taken for served (a claim could cost the bond)
		}
		if urls == nil {
			urls = c.daURLs()
			others = sortedURLs(urls)
		}
		if _, _, err := c.committedEnvs(ev, urls, others); err != nil {
			return root.Hex(), bt
		}
	}
	return "", 0
}

// receiptMissed: the alert for an envelope a data node signed a receipt for
// and that isn't on chain past its deadline. Committed and not served: the
// availability challenge, never a missed-receipt claim (it would hand the
// node our bond). Not committed: the claim slashes it.
func (c *Client) receiptMissed(id string, r *SentRec) {
	again := "send the message again"
	if r.Env != "" && r.Resent < c.resendCap() {
		again = "it is re-sent automatically through another data node, or on chain if none takes it (da_resend)"
	}
	Again := strings.ToUpper(again[:1]) + again[1:]
	rc := r.Receipt
	if root, at := c.withheldRoot(rc); root != "" {
		if r.Env != "" && r.Resent < c.resendCap() {
			Again = "It is re-sent automatically on chain from your own account (da_resend), unless strict privacy forbids that"
		}
		r.Withheld = root
		c.markUnserved(root, rc.Node, at, "your message "+id[:18]+" was handed to its node")
		closes := ""
		if cw, _, _, err := c.daWindows(); err == nil {
			closes = " before " + time.Unix(at+cw, 0).UTC().Format(time.RFC3339)
		}
		how := "it is challenged automatically if it stays unavailable (da_autochallenge)"
		if c.autoChallengeMode() == "off" {
			how = "da_autochallenge is off"
		}
		c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("your message %s to %s: data node %s committed batch %s by its deadline but doesn't serve it; your message may be in it. "+
			"A missed-receipt claim would be rebutted with that commit and cost you the bond: challenge the batch instead ('ternchain da challenge %s'%s; %s). %s",
			id[:18], r.To, rc.Node, root[:10], root, closes, how, Again))
		return
	}
	c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("your message %s to %s: data node %s signed a receipt to commit it by %s and committed no batch we can't read since. "+
		"'ternchain da prove-missed %s' claims its stake (within the challenge window); %s",
		id[:18], r.To, rc.Node, time.Unix(rc.Deadline, 0).UTC().Format(time.RFC3339), id[:18], again))
}

// resendLost: an envelope a data node signed for and let down (not committed,
// or committed and not served) goes out again, the same bytes: through a
// staked data node we haven't used for it, or on chain from our own account
// when none takes it (never under strict privacy). Each re-send pays the
// node or the Inbox fee and gas again; postage paid for it stays paid (tied to
// the envelope's hash). At most resendCap re-sends.
func (c *Client) resendLost() {
	n := c.resendCap()
	now := c.chainNow()
	var urls []string
	for _, id := range sortedKeys(c.st.Sent) {
		r := c.st.Sent[id]
		if !r.Lost || r.Block != 0 || r.Env == "" || r.Replacement != "" {
			continue
		}
		if r.Resent >= n || now-r.Time > alertTTL || r.Receipt == nil && len(r.Earlier) == 0 { // a plain relay's loss: only the alert
			if !r.Refreshable || r.MailCreated <= now-mailFreshness {
				r.Env, r.SenderCopy = "", ""
			}
			continue
		}
		b, err := hex.DecodeString(r.Env)
		es, derr := decodeBatch(b)
		if err != nil || derr != nil || len(es) != 1 || es[0].hash().Hex() != id {
			r.Env = ""
			continue
		}
		e := es[0]
		used := map[string]bool{strings.TrimRight(r.Via, "/"): true}
		for _, h := range r.Earlier {
			used[strings.TrimRight(h.Via, "/")] = true
		}
		if urls == nil {
			urls = sortedURLs(c.daURLs())
		}
		via := ""
		var rc *Receipt
		// Re-sending the same withheld envelopes can reconstruct their already
		// committed root, even for a batch of several envelopes. Another node
		// cannot commit that root or honor its new receipts. Post on chain instead.
		committed := r.Withheld != ""
		if st, err := c.ch.call("Inbox", "commitment", common.HexToHash(id)); err != nil || st[2].(uint8) != 0 {
			committed = true
		}
		for _, u := range urls {
			if committed || used[strings.TrimRight(u, "/")] {
				continue
			}
			if rc, err = c.handTo(u, e); err == nil {
				via = u
				break
			}
		}
		r.Resent++ // tries, not successes: a failing path isn't retried every scan
		if via == "" {
			if strict() {
				c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("your message %s to %s: automatic re-send needs an on-chain post, but strict privacy never posts from your own account: send it again through another relay (--relay)", id[:18], r.To))
				if !r.Refreshable {
					r.Env = ""
				}
				continue
			}
			if err := c.post([]Envelope{e}); err != nil {
				c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("your message %s to %s: re-sending it on chain failed (%v)", id[:18], r.To, err))
				continue
			}
			via = "chain"
		}
		r.Earlier = append(r.Earlier, Handover{Via: r.Via, Time: r.Time, Receipt: r.Receipt, Withheld: r.Withheld})
		r.Via, r.Time, r.Receipt, r.Withheld, r.Lost = via, now, rc, "", false
		where := "through data node " + via
		if via == "chain" {
			why := "no other data node took it"
			if committed {
				why = "a batch may already hold it: another node could reconstruct a root it cannot commit again"
			}
			where = "on chain from your own account (" + why + "; the Inbox fee and gas paid again)"
		}
		c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("your message %s to %s re-sent %s (re-send %d of at most %d; the same envelope: the recipient keeps one copy)",
			id[:18], r.To, where, r.Resent, n))
	}
}

// Handover: one time an envelope was handed over (a re-send keeps the earlier ones).
type Handover struct {
	Via      string   `json:"via"`
	Time     int64    `json:"time"`
	Receipt  *Receipt `json:"receipt,omitempty"`
	Withheld string   `json:"withheld,omitempty"`
}

// landedAfterResend: a re-sent envelope is on chain. An earlier node that
// signed for it and committed nothing we couldn't read can still be claimed.
func (c *Client) landedAfterResend(id string, r *SentRec) {
	for i := len(r.Earlier) - 1; i >= 0; i-- {
		h := r.Earlier[i]
		if h.Receipt == nil || h.Withheld != "" {
			continue
		}
		c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("your message %s to %s reached the chain after a re-send (block %d); data node %s signed a receipt for it and didn't commit it: "+
			"'ternchain da prove-missed %s' claims its stake (within the challenge window after %s)",
			id[:18], r.To, r.Block, h.Receipt.Node, id[:18], time.Unix(h.Receipt.Deadline, 0).UTC().Format(time.RFC3339)))
		return
	}
}

// markUnserved: a committed batch that concerns us is chased (chaseUnserved).
func (c *Client) markUnserved(root, poster string, at int64, why string) {
	if c.st.Unserved == nil {
		c.st.Unserved = map[string]*Unserved{}
	}
	if c.st.Unserved[root] == nil {
		c.st.Unserved[root] = &Unserved{Poster: common.HexToAddress(poster).Hex(), At: at, Why: why}
	}
}

// markEarlyMissed: an early item from data node url never landed by its
// deadline: a batch that node committed meanwhile and we couldn't fetch may
// hold it, so it concerns us.
func (c *Client) markEarlyMissed(it *Item) {
	if len(c.st.PendingDA) == 0 || it.Node == "" {
		return
	}
	var node string
	for a, u := range c.daURLs() {
		if strings.EqualFold(strings.TrimRight(u, "/"), strings.TrimRight(it.Node, "/")) {
			node = a.Hex()
		}
	}
	if node == "" {
		return
	}
	for root, p := range c.st.PendingDA {
		if !strings.EqualFold(p.Poster, node) {
			continue
		}
		if bt, err := c.ch.blockTime(p.Block); err == nil && bt >= it.Deadline-receiptMaxAhead-lostSlack && bt <= it.Deadline {
			c.markUnserved(root, node, bt, it.Type+" "+it.ID[:18]+" came early from its node and never landed")
		}
	}
}

// fetchAnywhere: asks our relay, the poster's node and every other staked
// node (at most daAllNodes) for a batch. served: one had it; answered: some
// node answered without it (we are online: a try that counts).
func (c *Client) fetchAnywhere(root common.Hash, poster common.Address) (served, answered bool) {
	urls := c.daURLs()
	order := []string{c.relay, urls[poster]}
	order = append(order, sortedURLs(urls)...)
	seen := map[string]bool{"": true}
	asked := 0
	for _, u := range order {
		k := strings.TrimRight(u, "/")
		if seen[k] || asked >= daAllNodes {
			continue
		}
		seen[k] = true
		asked++
		_, err := fetchBatchFn(u, root)
		if err == nil {
			return true, true
		}
		var se statusErr
		if errors.As(err, &se) || errors.Is(err, errWrongBytes) {
			answered = true
		}
	}
	return false, answered
}

// chaseUnserved: each batch that concerns us and no node served is asked for
// again, at every node we know, once a gap; one that stays unserved is
// challenged with our bond (da_autochallenge), and expired once its node let
// the resolve window pass: its stake is slashed to us and the bond comes back.
// A batch some node serves is never challenged; one already challenged (by
// anyone) isn't challenged again.
func (c *Client) chaseUnserved() {
	mode := c.autoChallengeMode()
	if mode == "all" { // every batch we couldn't fetch, ours or not
		for root, p := range c.st.PendingDA {
			if p.Tries+p.Down > 0 {
				if bt, err := c.ch.blockTime(p.Block); err == nil {
					c.markUnserved(root, p.Poster, bt, "da_autochallenge all")
				}
			}
		}
	}
	if len(c.st.Unserved) == 0 {
		return
	}
	cw, rw, bond, err := c.daWindows()
	if err != nil {
		return
	}
	gap, last := max(cw/8, 1), cw/12
	now := c.chainNow()
	deadline := time.Now().Add(daBudget)
	for _, root := range sortedKeys(c.st.Unserved) {
		u := c.st.Unserved[root]
		h := common.HexToHash(root)
		st, err := c.ch.call("Inbox", "commitment", h)
		if err != nil {
			continue
		}
		switch st[2].(uint8) {
		case 1: // committed: below
		case 2: // challenged: once the resolve window passed, expire it (the challenger is paid, whoever calls)
			at, err := c.ch.call("Inbox", "challengedAt", h)
			if err != nil || now <= int64(at[0].(uint64))+rw {
				continue
			}
			if _, err := c.ch.send(c.key, nil, "Inbox", "expire", h); err == nil && u.Challenged != 0 {
				c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("batch %s: data node %s didn't put it on chain after our challenge: its stake was slashed and paid to us with the bond back",
					root[:10], u.Poster))
			}
			if s, err := c.ch.call("Inbox", "commitment", h); err == nil && s[2].(uint8) != 2 {
				delete(c.st.Unserved, root)
			}
			continue
		default: // resolved (on chain: read), lost, or unknown
			if st[2].(uint8) == 3 && u.Challenged != 0 {
				c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("batch %s was put on chain after our challenge: it is read; our bond of %s paid whoever resolved it", root[:10], fmtCoins(bond)))
			}
			delete(c.st.Unserved, root)
			continue
		}
		if mode == "off" || now > u.At+cw {
			if now > u.At+cw {
				delete(c.st.Unserved, root) // past the window: reading gives it up in time (batchesSince)
			}
			continue
		}
		if u.MissAt != 0 && now < u.MissAt+gap || !time.Now().Before(deadline) {
			continue
		}
		served, answered := c.fetchAnywhere(h, st[0].(common.Address))
		if served { // a node has it: the reading path takes it, never a challenge
			delete(c.st.Unserved, root)
			continue
		}
		if !answered { // no node answered at all: we may be the ones offline
			continue
		}
		u.Misses, u.MissAt = u.Misses+1, now
		if u.Misses < daChallengeMisses && !(u.Misses >= 2 && now+gap > u.At+cw-last) {
			continue
		}
		if now > u.At+cw-last || !c.isOwner() && len(c.st.Taint) > 0 {
			if !u.Told {
				u.Told = true
				who := "too close to the end of its challenge window to challenge it safely"
				if now <= u.At+cw-last {
					who = "not challenged by this agent (it read outside mail: its owner decides)"
				}
				c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("batch %s (data node %s) stays unavailable: %s; 'ternchain da challenge %s' before %s",
					root[:10], u.Poster, who, root, time.Unix(u.At+cw, 0).UTC().Format(time.RFC3339)))
			}
			continue
		}
		if _, err := c.ch.send(c.key, bond, "Inbox", "challenge", h); err != nil {
			if !u.Told {
				u.Told = true
				c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("batch %s (data node %s) stays unavailable; challenging it failed (%v): 'ternchain da challenge %s' before %s",
					root[:10], u.Poster, err, root, time.Unix(u.At+cw, 0).UTC().Format(time.RFC3339)))
			}
			continue
		}
		u.Challenged = now
		c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("batch %s (data node %s) stayed unavailable (%d tries, the last %d min after it was committed, every known node asked): challenged with a bond of %s (da_autochallenge). "+
			"Unless the node puts it on chain within %d min, its stake is slashed to you with the bond back (expired automatically); if it does, the batch is read and the bond pays whoever resolved it",
			root[:10], u.Poster, u.Misses, (now-u.At)/60, fmtCoins(bond), rw/60))
	}
}
