// Group credits: a ledger without money, so a group can run on "who gave,
// gets first". Nobody keeps it: each reader computes it from what the
// group's channel holds, as it computes a rating from the chain.
//
//	credits post GROUP RECEIPT      put a countersigned usage receipt in the group (either party; on chain, sealed to the group)
//	credits show GROUP [NAME]       every member's balance (served minus used), or one member's with its sessions
//	credits priority GROUP          members by balance: who is served first, and who is past the floor and waits
//	credits policy GROUP JSON       (admins) the group's rates, floor, cap per pair a day and grants
//
// What counts: receipts both parties signed, naming this group, between
// members of different families (an owner and its agents are one member:
// credits go to the owner), the best count per session. A member's balance
// is what it served minus what it used, each unit at the group's rate,
// plus any grant in the policy. There is no transfer: credits come only
// from work someone else countersigned, so they can't be sold or moved.
// A pair of members moves at most pair_cap_day credits a UTC day between
// them (0: no cap). Posts are on chain, so the history can't be rewritten;
// a moderator hiding a post doesn't take credits away.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const tCreditsPolicy = "tern.credits-policy/1"

// CreditsPolicy: a group's terms for its ledger, set by its admins.
type CreditsPolicy struct {
	Schema     string             `json:"schema"`
	Rates      map[string]float64 `json:"rates,omitempty"`        // credits per unit (absent: 1)
	Floor      float64            `json:"floor"`                  // how far below zero a member may use (a newcomer starts at 0)
	PairCapDay float64            `json:"pair_cap_day,omitempty"` // credits a pair moves a UTC day at most (0: no cap)
	Grants     map[string]float64 `json:"grants,omitempty"`       // credits for what no receipt counts (hosting, electricity)
}

var defaultCreditsPolicy = CreditsPolicy{Schema: tCreditsPolicy, Floor: -1000}

func (p CreditsPolicy) rate(unit string) float64 {
	if r, ok := p.Rates[unit]; ok {
		return r
	}
	return 1
}

func (p CreditsPolicy) check() error {
	if p.Schema != tCreditsPolicy {
		return errors.New("not a tern.credits-policy/1")
	}
	if p.Floor > 0 || p.PairCapDay < 0 || math.IsNaN(p.Floor) {
		return errors.New("floor: 0 or below; pair_cap_day: 0 or above")
	}
	for u, r := range p.Rates {
		if !unitRE.MatchString(u) || r < 0 || math.IsInf(r, 0) || math.IsNaN(r) {
			return fmt.Errorf("rate for %q: a unit word and a number 0 or above", u)
		}
	}
	return nil
}

// creditsPost: what a ledger post carries in its data.
type creditsPost struct {
	Usage  *Usage         `json:"usage,omitempty"`
	Policy *CreditsPolicy `json:"credits_policy,omitempty"`
	Pool   *PoolRule      `json:"pool,omitempty"` // the group's queue rule (pool.go)
}

// Ledger: a group's credits as one reader computed them.
type Ledger struct {
	Group    string             `json:"group"`
	Policy   CreditsPolicy      `json:"policy"`
	PolicyBy string             `json:"policy_by,omitempty"`
	Balances map[string]float64 `json:"balances"` // member (owner name) => credits
	Served   map[string]float64 `json:"served"`
	Used     map[string]float64 `json:"used"`
	Sessions int                `json:"sessions"`
	// for the queue rule (pool.go)
	Pool      PoolRule           `json:"pool"`
	PoolBy    string             `json:"pool_by,omitempty"`
	UsedToday map[string]float64 `json:"used_today,omitempty"`
	LastUsed  map[string]int64   `json:"last_used,omitempty"` // unix seconds of the latest receipt a member countersigned
	Ignored   []string           `json:"ignored,omitempty"`   // what didn't count, and why
}

// ledger reads the group's posts (call scan first) and computes the credits.
func (c *Client) ledger(group string) (*Ledger, error) {
	if c.st.Groups[group] == nil {
		return nil, fmt.Errorf("no keys for group %s here: join it first", group)
	}
	l := &Ledger{Group: group, Policy: defaultCreditsPolicy, Balances: map[string]float64{}, Served: map[string]float64{}, Used: map[string]float64{},
		Pool: defaultPoolRule, UsedToday: map[string]float64{}, LastUsed: map[string]int64{}}
	best := map[string]Usage{}
	var order []string
	for _, it := range c.groupItems(group) {
		if it.Mail == nil || len(it.Mail.Data) == 0 || it.Status == "undelivered" {
			continue
		}
		var p creditsPost
		if json.Unmarshal(it.Mail.Data, &p) != nil {
			continue
		}
		if p.Policy != nil {
			if err := p.Policy.check(); err != nil || !c.hasRole(group, "admin", it.From) {
				l.Ignored = append(l.Ignored, fmt.Sprintf("a policy by %s: not an admin's, or not valid", it.From))
				continue
			}
			l.Policy, l.PolicyBy = *p.Policy, it.From // the latest admin's policy holds
			continue
		}
		if p.Pool != nil {
			if err := p.Pool.check(); err != nil || !c.hasRole(group, "admin", it.From) {
				l.Ignored = append(l.Ignored, fmt.Sprintf("a pool rule by %s: not an admin's, or not valid", it.From))
				continue
			}
			l.Pool, l.PoolBy = *p.Pool, it.From
			continue
		}
		if p.Usage == nil {
			continue
		}
		u := *p.Usage
		why := ""
		switch {
		case u.Group != group:
			why = "names another group"
		case ownerName(u.Provider) == ownerName(u.Consumer):
			why = "one family on both sides"
		case !c.groupMember(group, u.Provider) || !c.groupMember(group, u.Consumer):
			why = "not between members"
		}
		if why == "" {
			if err := c.receiptVerifyAt(u, true, it.Block); err != nil { // at the post's block (0 while pending: now)
				why = err.Error()
			}
		}
		if why != "" {
			l.Ignored = append(l.Ignored, fmt.Sprintf("session %.10s %s→%s: %s", u.Session, u.Provider, u.Consumer, why))
			continue
		}
		k := u.key()
		if b, ok := best[k]; !ok {
			order = append(order, k)
			best[k] = u
		} else if b.terms() == u.terms() && u.Count > b.Count {
			best[k] = u
		}
	}
	type pairDay struct {
		a, b string
		day  int64
	}
	moved := map[pairDay]float64{}
	for _, k := range order {
		u := best[k]
		p, q := ownerName(u.Provider), ownerName(u.Consumer)
		amt := float64(u.Count) * l.Policy.rate(u.Unit)
		if cp := l.Policy.PairCapDay; cp > 0 {
			a, b := min(p, q), max(p, q)
			pd := pairDay{a, b, time.Unix(u.At, 0).UTC().Unix() / 86400}
			if left := cp - moved[pd]; amt > left {
				l.Ignored = append(l.Ignored, fmt.Sprintf("session %.10s %s→%s: %.0f credits over the pair's cap a day", u.Session, p, q, amt-max(left, 0)))
				amt = max(left, 0)
			}
			moved[pd] += amt
		}
		l.Served[p] += amt
		l.Used[q] += amt
		if u.At/86400 == time.Now().Unix()/86400 {
			l.UsedToday[q] += amt
		}
		l.LastUsed[q] = max(l.LastUsed[q], u.At)
		l.Balances[p] += amt
		l.Balances[q] -= amt
		l.Sessions++
	}
	for n, g := range l.Policy.Grants {
		l.Balances[n] += g
	}
	return l, nil
}

// Priority: one member's place in the queue.
type Priority struct {
	Name    string  `json:"name"`
	Balance float64 `json:"balance"`
	Rank    int     `json:"rank"`
	Waits   bool    `json:"waits,omitempty"` // at or below the floor: served only when nobody else waits
}

// priority: the members with a balance, highest first.
func (l *Ledger) priority() []Priority {
	out := make([]Priority, 0, len(l.Balances))
	for n, b := range l.Balances {
		out = append(out, Priority{Name: n, Balance: b, Waits: b <= l.Policy.Floor})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Balance != out[j].Balance {
			return out[i].Balance > out[j].Balance
		}
		return out[i].Name < out[j].Name
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func (c *Client) creditsPostUsage(group string, u Usage) (string, error) {
	if c.st.Groups[group] == nil {
		return "", fmt.Errorf("no keys for group %s here: join it first", group)
	}
	if u.Group != group {
		return "", fmt.Errorf("the receipt names group %q, not %s: it wouldn't count there", u.Group, group)
	}
	if err := c.receiptVerify(u, true); err != nil {
		return "", err
	}
	return c.postGroup(group, fmt.Sprintf("credits: %s served %s %d %s", u.Provider, u.Consumer, u.Count, u.Unit), jsonOf(creditsPost{Usage: &u}))
}

func (c *Client) creditsSetPolicy(group string, p CreditsPolicy) (string, error) {
	p.Schema = tCreditsPolicy
	if err := p.check(); err != nil {
		return "", err
	}
	if !c.hasRole(group, "admin", c.f.Name) {
		return "", fmt.Errorf("only an admin of %s sets its credits policy (readers ignore anyone else's)", group)
	}
	return c.postGroup(group, "credits policy", jsonOf(creditsPost{Policy: &p}))
}

func runCredits(c *Client, cmd string, pos []string, f coopFlags) error {
	if len(pos) < 1 {
		return fmt.Errorf("usage: %s GROUP …", cmd)
	}
	group := pos[0]
	switch cmd {
	case "credits post":
		if len(pos) < 2 {
			return errors.New("usage: credits post GROUP RECEIPT (a countersigned tern.receipt/1: JSON, @FILE or -)")
		}
		u, err := parseReceipt(pos[1])
		if err != nil {
			return err
		}
		id, err := c.creditsPostUsage(group, u)
		if err != nil {
			return err
		}
		out(map[string]any{"posted": id, "group": group, "session": u.Session, "count": u.Count})
		return nil
	case "credits policy":
		if len(pos) < 2 {
			return errors.New(`usage: credits policy GROUP '{"rates":{"token":0.001},"floor":-100,"pair_cap_day":500,"grants":{"oscar":200}}'`)
		}
		raw, err := coopInput(pos[1])
		if err != nil {
			return err
		}
		var p CreditsPolicy
		if err := jsonStrict(raw, &p); err != nil {
			return fmt.Errorf("policy: %v", err)
		}
		id, err := c.creditsSetPolicy(group, p)
		if err != nil {
			return err
		}
		out(map[string]any{"posted": id, "group": group})
		return nil
	case "credits show", "credits priority":
		if _, err := c.scan(); err != nil {
			return err
		}
		l, err := c.ledger(group)
		if err != nil {
			return err
		}
		if cmd == "credits priority" {
			out(map[string]any{"group": group, "floor": l.Policy.Floor, "queue": l.priority(),
				"note": "serve in this order; a member that waits is past the floor: it gives before it takes again"})
			return nil
		}
		if len(pos) > 1 {
			n := ownerName(pos[1])
			out(map[string]any{"group": group, "member": n, "balance": l.Balances[n], "served": l.Served[n], "used": l.Used[n],
				"floor": l.Policy.Floor, "may_use": l.Balances[n] > l.Policy.Floor})
			return nil
		}
		out(l)
		return nil
	}
	return unknownCommand(cmd)
}
