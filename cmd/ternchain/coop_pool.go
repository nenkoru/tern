// Pools: who in a group is served first when something shared is busy (a
// GPU cluster, the API key's rate limit, the car on Saturday, tonight's
// game server). The rule is a policy module of kind pool (tern.policy/1,
// tern-expr/1: pure, cost-bounded, explainable) that the group's admins
// pick; every member computes the same order from the same ledger.
//
//	pool rule GROUP LINE [--quota-day N]   (admins) the group's rule, posted to the group (tern.pool/1)
//	pool order GROUP [NAME[:WAITED-MIN] …]  the members (or those named) in the group's order: priority, waits
//
// Built in: pool-default@1 (credits first; past the floor, wait),
// pool-round-robin@1 (whoever used it longest ago), pool-quota@1 (least used
// today; past quota_day, wait), pool-fifo@1 (longest waiting). Any other is
// a module someone published (ui publish) and the member installed (policy
// install): a group can adopt a new rule with no change to this client.
package main

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nenkoru/tern/internal/ui"
)

const tPool = "tern.pool/1"

// PoolRule: a group's queue rule, set by its admins.
type PoolRule struct {
	Schema   string  `json:"schema"`
	Rule     string  `json:"rule"`                // a pool module's line
	QuotaDay float64 `json:"quota_day,omitempty"` // credits a member uses a UTC day before it waits (pool-quota@1)
}

var defaultPoolRule = PoolRule{Schema: tPool, Rule: "pool-default@1"}

var lineRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}@[0-9]+$`)

func (r PoolRule) check() error {
	if r.Schema != tPool || !lineRE.MatchString(r.Rule) || r.QuotaDay < 0 {
		return errors.New("not a tern.pool/1 rule (a module line like pool-quota@1, quota_day 0 or above)")
	}
	return nil
}

// poolModule: the group's rule as this client can run it: built in, or
// installed; otherwise the default, with a note saying what to install.
func (c *Client) poolModule(line string) (*ui.Policy, string) {
	if p := ui.BuiltinPolicyLine(line); p != nil {
		return p, ""
	}
	if p, err := c.loadPolicyLine(line); err == nil && p.M.AppliesTo == "pool" {
		return p, ""
	}
	return ui.BuiltinPolicy("pool"), fmt.Sprintf("the group's rule is %s, not installed here: ordered by pool-default@1 instead "+
		"(policy install the module to follow the group)", line)
}

// QueueEntry: one member's place under the group's rule.
type QueueEntry struct {
	Name     string  `json:"name"`
	Priority float64 `json:"priority"`
	Waits    bool    `json:"waits,omitempty"`
	Balance  float64 `json:"balance"`
	Rank     int     `json:"rank"`
}

// order: members (owner names; with minutes waited) in the group's order.
func (c *Client) order(l *Ledger, waited map[string]float64) ([]QueueEntry, string) {
	p, note := c.poolModule(l.Pool.Rule)
	names := map[string]bool{}
	for n := range waited {
		names[n] = true
	}
	if len(waited) == 0 {
		for n := range l.Balances {
			names[n] = true
		}
	}
	now := time.Now().Unix()
	var out []QueueEntry
	for n := range names {
		since := 1e6
		if t := l.LastUsed[n]; t > 0 {
			since = float64(now-t) / 3600
		}
		env := ui.Env{"member.balance": l.Balances[n], "member.served": l.Served[n], "member.used": l.Used[n],
			"member.used_today": l.UsedToday[n], "member.hours_since_used": since, "member.waiting_minutes": waited[n],
			"pool.floor": l.Policy.Floor, "pool.quota_day": l.Pool.QuotaDay}
		out = append(out, QueueEntry{Name: n, Priority: p.Num("priority", env), Waits: p.Bool("waits", env), Balance: l.Balances[n]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Waits != out[j].Waits {
			return !out[i].Waits
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out, note
}

func (c *Client) poolSetRule(group string, r PoolRule) (string, error) {
	r.Schema = tPool
	if err := r.check(); err != nil {
		return "", err
	}
	if !c.hasRole(group, "admin", c.f.Name) {
		return "", fmt.Errorf("only an admin of %s sets its pool rule (readers ignore anyone else's)", group)
	}
	return c.postGroup(group, "pool rule: "+r.Rule, jsonOf(creditsPost{Pool: &r}))
}

func runPool(c *Client, cmd string, pos []string, f coopFlags) error {
	if len(pos) < 1 {
		return fmt.Errorf("usage: %s GROUP …", cmd)
	}
	group := pos[0]
	switch cmd {
	case "pool rule":
		if len(pos) < 2 {
			return errors.New("usage: pool rule GROUP LINE [--quota-day N] (pool-default@1, pool-round-robin@1, pool-quota@1, pool-fifo@1, or one you installed)")
		}
		id, err := c.poolSetRule(group, PoolRule{Rule: pos[1], QuotaDay: f.quotaDay})
		if err != nil {
			return err
		}
		out(map[string]any{"posted": id, "group": group, "rule": pos[1]})
		return nil
	case "pool order":
		if _, err := c.scan(); err != nil {
			return err
		}
		l, err := c.ledger(group)
		if err != nil {
			return err
		}
		waited := map[string]float64{}
		for _, a := range pos[1:] {
			n, m, _ := strings.Cut(a, ":")
			var x float64
			fmt.Sscan(m, &x)
			waited[ownerName(n)] = x
		}
		q, note := c.order(l, waited)
		res := map[string]any{"group": group, "rule": l.Pool.Rule, "queue": q,
			"note": "serve in this order; a member that waits is served only when nobody else waits"}
		if note != "" {
			res["warning"] = note
		}
		out(res)
		return nil
	}
	return unknownCommand(cmd)
}
