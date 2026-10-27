// Thread limits between agents.
//
// With a stranger, an agent sends at most hop_limit agent messages in a row
// in one thread (a human's message resets the count): no endless agent
// loops. With a trusted correspondent (an agent of the same owner, or a
// lasting correspondent the owner approved: owner approve AGENT NAME, owner
// contact AGENT NAME) there is no hop limit, a daily budget instead: at most
// thread_per_day of this agent's messages in one thread a UTC day, counted in
// its local state.
//
// Neither refusal is a silent dead end: the client files an ask with the
// owner (ask-owner, carrying exactly this thread's limit), once per limit
// hit, and the owner's approval of it lets the thread go on: thread_per_day
// more messages today, or the in-a-row count reset once. Being trusted lifts
// only the hop limit: taint, approvals for new correspondents, payments and
// fan-out are guarded as before.
package main

import (
	"fmt"
	"strings"
	"time"
)

// ThreadDay: this agent's sends in one thread today (UTC).
type ThreadDay struct {
	Day    string            `json:"day"` // 2006-01-02, UTC
	To     string            `json:"to"`
	Sent   int               `json:"sent"`
	Extra  int               `json:"extra,omitempty"`  // budget extensions the owner approved today (+thread_per_day each)
	Resets int               `json:"resets,omitempty"` // hop-limit resets the owner approved today
	Asks   map[string]string `json:"asks,omitempty"`   // approval key => the ask filed for it (one ask per limit hit)
}

// today: the chain's UTC day.
func (c *Client) today() string {
	return time.Unix(c.chainNow(), 0).UTC().Format("2006-01-02")
}

// trustedPeer: a correspondent whose threads with this identity have no hop
// limit (a daily budget instead): the family (the owner and its agents), or,
// for an agent, a lasting correspondent its owner signed (for the name or its
// owner; the owner's latest word wins). Never a group. It lifts nothing else.
func (c *Client) trustedPeer(who string) bool {
	if who == "" || strings.HasPrefix(who, "group:") {
		return false
	}
	if c.family(who) {
		return true
	}
	if c.isOwner() {
		return false
	}
	if on, said := c.ownerSaid(who); said {
		return on
	}
	on, _ := c.ownerSaid(ownerName(who))
	return on
}

// threadDay: today's record of a thread (a new day starts afresh; records of
// earlier days are dropped).
func (c *Client) threadDay(thread, to string) *ThreadDay {
	day := c.today()
	if c.st.Threads == nil {
		c.st.Threads = map[string]*ThreadDay{}
	}
	td := c.st.Threads[thread]
	if td == nil || td.Day != day {
		for k, v := range c.st.Threads {
			if v == nil || v.Day != day {
				delete(c.st.Threads, k)
			}
		}
		td = &ThreadDay{Day: day, To: to}
		c.st.Threads[thread] = td
	}
	return td
}

// threadSent: one more message of this agent in the thread today.
func (c *Client) threadSent(thread, to string) {
	c.locked(func() { c.threadDay(thread, to).Sent++ })
}

// threadBudget: may this agent send one more message today in a thread with
// a trusted correspondent? Over thread_per_day (plus what the owner allowed
// today) it needs the owner's approval: thread_per_day more.
func (c *Client) threadBudget(thread, to string) error {
	s, err := c.scopes()
	if err != nil {
		return err
	}
	per := int(s.knob("thread_per_day"))
	c.syncGuard()
	td := c.threadDay(thread, to)
	limit := per * (1 + td.Extra)
	if td.Sent < limit {
		return nil
	}
	round := td.Extra + 1
	key := fmt.Sprintf("thread:%s:day:%s:%d", thread, td.Day, round)
	what := fmt.Sprintf("%d more messages today in thread %s with %s (the daily budget is spent: %d sent today, thread_per_day %d)", per, thread, to, td.Sent, per)
	ask := fmt.Sprintf("%s and %s hit the limit in this thread (%s: %d today / budget %d a day): allow more? Approving gives this thread %d more messages today.",
		c.f.Name, to, thread, td.Sent, limit, per)
	head := fmt.Sprintf("daily thread budget: %d messages to %s in this thread today (thread_per_day %d)", td.Sent, to, limit)
	if err := c.limitApproval(thread, to, key, what, ask, head); err != nil {
		return err
	}
	c.locked(func() {
		if td := c.threadDay(thread, to); td.Extra < round {
			td.Extra = round
		}
	})
	return nil
}

// hopApproved: a stranger's thread is past the hop limit; the owner's
// approval resets the in-a-row count once.
func (c *Client) hopApproved(thread, to string, hl, inRow int) error {
	c.syncGuard()
	td := c.threadDay(thread, to)
	round := td.Resets + 1
	key := fmt.Sprintf("thread:%s:hop:%s:%d", thread, td.Day, round)
	what := fmt.Sprintf("going on in thread %s with %s after %d agent messages in a row (hop limit %d): resets the count once", thread, to, inRow, hl)
	ask := fmt.Sprintf("%s and %s hit the limit in this thread (%s: %d today / %d in a row, hop limit %d): allow more? Approving resets the in-a-row count once.",
		c.f.Name, to, thread, td.Sent, inRow, hl)
	head := fmt.Sprintf("hop limit: %d agent messages in a row in this thread with %s (hop_limit %d)", inRow, to, hl)
	if err := c.limitApproval(thread, to, key, what, ask, head); err != nil {
		return err
	}
	c.locked(func() {
		if td := c.threadDay(thread, to); td.Resets < round {
			td.Resets = round
		}
	})
	return nil
}

// limitApproval: an owner approval for exactly this thread's limit (key), or
// a refusal that files an ask with the owner (once per key) and prints the
// owner's command.
func (c *Client) limitApproval(thread, to, key, what, ask, head string) error {
	err := c.useApproval(key, nil, what) // the guard's path: it records what an ask carries
	if err == nil {
		return nil
	}
	if !strings.HasPrefix(err.Error(), "blocked:") {
		return err
	}
	dir := c.ownerDirWords()
	direct := fmt.Sprintf("ternchain owner approve --dir %s %s %s", dir, c.f.Name, shellQuote(key))
	if id := c.threadDay(thread, to).Asks[key]; id != "" {
		return fmt.Errorf("%s; your owner was asked already (ask %s; not asked again): they approve with "+
			"'ternchain owner approve --dir %s --ask %s' (or: %s), then send again", head, id, dir, id, direct)
	}
	r, aerr := c.askOwner("limit in thread "+thread, ask)
	if aerr != nil {
		return fmt.Errorf("%s; asking your owner failed (%v): ask with ask-owner (it carries this request), or your owner runs: %s", head, aerr, direct)
	}
	id, _ := r["sent"].(string)
	if len(id) > 18 {
		id = id[:18]
	}
	c.locked(func() {
		td := c.threadDay(thread, to)
		if td.Asks == nil {
			td.Asks = map[string]string{}
		}
		td.Asks[key] = id
	})
	return fmt.Errorf("%s; an ask was filed with your owner (ask %s): they approve with "+
		"'ternchain owner approve --dir %s --ask %s' (or: %s), then send again", head, id, dir, id, direct)
}

// mergeThreads: another process's counts for today only ever grow.
func (s *State) mergeThreads(d *State) {
	for k, dt := range d.Threads {
		if dt == nil {
			continue
		}
		if s.Threads == nil {
			s.Threads = map[string]*ThreadDay{}
		}
		mt := s.Threads[k]
		switch {
		case mt == nil || dt.Day > mt.Day:
			cp := *dt
			cp.Asks = map[string]string{}
			for a, id := range dt.Asks {
				cp.Asks[a] = id
			}
			s.Threads[k] = &cp
		case dt.Day == mt.Day:
			mt.Sent, mt.Extra, mt.Resets = max(mt.Sent, dt.Sent), max(mt.Extra, dt.Extra), max(mt.Resets, dt.Resets)
			for a, id := range dt.Asks {
				if mt.Asks == nil {
					mt.Asks = map[string]string{}
				}
				if mt.Asks[a] == "" {
					mt.Asks[a] = id
				}
			}
		}
	}
}
