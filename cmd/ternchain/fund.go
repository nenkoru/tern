// All-or-nothing crowdfunding (an assurance contract): pledges are Bank
// locks to the campaign's creator under PledgeCondition, registered with it
// so they count. The creator (a person, an agency, a community's treasury)
// collects them all once the goal is met; below the goal at the deadline,
// each backer takes its pledge back. Registering a pledge holds its lock
// (Bank.hold): no give-back or release until the campaign resolves.
//
// What the backers paid for can be a sealed artifact (a build, an album, a
// dataset: artifact add, its key's hash on chain): once the goal is met,
// fund deliver sends the key sealed to every backer, each copy tied to the
// backer's pledge lock, so it arrives delivered (a deal of theirs) and
// artifact get LOCK FILE.enc opens it. Nobody who didn't back it gets the key
// from the creator; a backer can still pass it on, as with any key.
package main

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const collectWindow = 7 * 86400 // PledgeCondition.COLLECT_WINDOW

type Campaign struct {
	ID        uint64
	Creator   string
	By        string
	Goal      *big.Int
	Deadline  uint64
	ReachedAt uint64
	Backers   uint32
	Pledged   *big.Int
	Title     string
	Terms     common.Hash
}

func (c *Client) campaign(id uint64) (*Campaign, error) {
	if !c.ch.has("PledgeCondition") {
		return nil, errors.New("this network has no PledgeCondition (an older devnet)")
	}
	v, err := c.ch.call("PledgeCondition", "campaigns", u(id))
	if err != nil {
		return nil, fmt.Errorf("no campaign %d", id)
	}
	return &Campaign{ID: id, Creator: c.nameOrHex(common.Hash(v[0].([32]byte))), By: c.nameOrHex(common.Hash(v[1].([32]byte))),
		Goal: v[2].(*big.Int), Deadline: v[3].(uint64), ReachedAt: v[4].(uint64), Backers: v[5].(uint32), Pledged: v[6].(*big.Int),
		Title: v[7].(string), Terms: common.Hash(v[8].([32]byte))}, nil
}

func (k *Campaign) state(now uint64) string {
	switch {
	case k.ReachedAt != 0:
		return "succeeded"
	case now > k.Deadline:
		return "failed"
	}
	return "open"
}

// fundCreate announces a campaign for `to` (ourselves, a name, a split or a community).
func (c *Client) fundCreate(title string, goal *big.Int, deadline uint64, to, terms string) (uint64, error) {
	if to == "" {
		to = c.f.Name
	}
	var th common.Hash
	if terms != "" {
		th = crypto.Keccak256Hash([]byte(terms))
	}
	r, err := c.ch.send(c.key, nil, "PledgeCondition", "create", c.id(), payeeID(to), goal, deadline, title, th)
	if err != nil {
		return 0, err
	}
	return c.firstIndexed(r, "PledgeCondition", "CampaignCreated")
}

// fundPledge locks amount to the creator and registers the lock so it counts.
func (c *Client) fundPledge(id uint64, amount *big.Int) (uint64, error) {
	k, err := c.campaign(id)
	if err != nil {
		return 0, err
	}
	if c.chainNowU() > k.Deadline {
		return 0, fmt.Errorf("campaign %d is over (%s)", id, k.state(c.chainNowU()))
	}
	if ownerName(k.Creator) == c.f.Owner || ownerName(k.By) == c.f.Owner {
		return 0, fmt.Errorf("campaign %d is your own: your pledge wouldn't count toward its goal (audit 4 MO-7)", id)
	}
	d, err := c.ch.call("PledgeCondition", "demandOf", u(id))
	if err != nil {
		return 0, err
	}
	lock, err := c.lock(k.Creator, amount, k.Deadline+collectWindow+3600, "PledgeCondition", d[0].([]byte))
	if err != nil {
		return 0, err
	}
	if _, err := c.ch.send(c.key, nil, "PledgeCondition", "register", u(lock)); err != nil {
		return lock, fmt.Errorf("pledge lock %d opened but not counted (register it: anyone may): %w", lock, err)
	}
	return lock, nil
}

// pledges: the counted pledge locks of a campaign, with their backers.
func (c *Client) pledges(id uint64) ([]event, error) {
	return c.ch.events("PledgeCondition", "Pledged", []common.Hash{common.BigToHash(u(id))})
}

func (c *Client) fundView(id uint64) (map[string]any, error) {
	k, err := c.campaign(id)
	if err != nil {
		return nil, err
	}
	now := c.chainNowU()
	evs, _ := c.pledges(id)
	byBacker := map[string]*big.Int{}
	left := new(big.Int)
	for _, ev := range evs {
		b := c.nameOrHex(common.Hash(ev.Args["backer"].([32]byte)))
		if byBacker[b] == nil {
			byBacker[b] = new(big.Int)
		}
		byBacker[b].Add(byBacker[b], ev.Args["amount"].(*big.Int))
		if rem, err := c.ch.call("Bank", "remaining", ev.Args["lockId"].(*big.Int)); err == nil {
			left.Add(left, rem[0].(*big.Int))
		}
	}
	backers := map[string]string{}
	for b, a := range byBacker {
		backers[b] = c.fmtMoney(a)
	}
	m := map[string]any{"campaign": id, "title": c.outside(k.By, k.Title), "creator": k.Creator, "announced_by": k.By,
		"goal": c.fmtMoney(k.Goal), "pledged": c.fmtMoney(k.Pledged), "backers": k.Backers, "pledges": backers,
		"deadline": time.Unix(int64(k.Deadline), 0).UTC().Format(time.RFC3339), "state": k.state(now), "still_locked": c.fmtMoney(left)}
	if k.Terms != (common.Hash{}) {
		m["terms_hash"] = k.Terms.Hex()
	}
	if now <= k.Deadline {
		m["time_left_s"] = k.Deadline - now
	}
	if k.By != k.Creator && ownerName(k.By) != ownerName(k.Creator) {
		m["note"] = "announced by " + k.By + ", not by the creator: pledges go to " + k.Creator + " all the same"
	}
	switch k.state(now) {
	case "succeeded":
		m["next"] = fmt.Sprintf("fund collect %d (anyone; the money goes to %s)", id, k.Creator)
	case "failed":
		m["next"] = fmt.Sprintf("fund refund %d (each backer)", id)
	}
	return m, nil
}

// fundCollect claims every pledge left for the creator (anyone may call it).
func (c *Client) fundCollect(id uint64) (*big.Int, error) {
	k, err := c.campaign(id)
	if err != nil {
		return nil, err
	}
	if k.ReachedAt == 0 {
		return nil, fmt.Errorf("campaign %d hasn't reached its goal (%s of %s)", id, c.fmtMoney(k.Pledged), c.fmtMoney(k.Goal))
	}
	return c.eachPledge(id, nil, func(l uint64) error { return c.claimLock(l, nil) })
}

// fundRefund takes our pledges back after a failed deadline.
func (c *Client) fundRefund(id uint64) (*big.Int, error) {
	k, err := c.campaign(id)
	if err != nil {
		return nil, err
	}
	if s := k.state(c.chainNowU()); s != "failed" {
		return nil, fmt.Errorf("campaign %d is %s: pledges come back only after a deadline missed", id, s)
	}
	return c.eachPledge(id, c.mine(), func(l uint64) error { return c.refundEarly(l, nil) })
}

func (c *Client) eachPledge(id uint64, backers map[common.Hash]bool, f func(uint64) error) (*big.Int, error) {
	evs, err := c.pledges(id)
	if err != nil {
		return nil, err
	}
	moved := new(big.Int)
	for _, ev := range evs {
		if backers != nil && !backers[common.Hash(ev.Args["backer"].([32]byte))] {
			continue
		}
		l := ev.Args["lockId"].(*big.Int)
		rem, err := c.ch.call("Bank", "remaining", l)
		if err != nil || rem[0].(*big.Int).Sign() == 0 {
			continue
		}
		if err := f(l.Uint64()); err != nil {
			return moved, fmt.Errorf("pledge lock %s: %w", l, err)
		}
		moved.Add(moved, rem[0].(*big.Int))
	}
	if moved.Sign() == 0 {
		return moved, errors.New("nothing left to move: the pledges were collected or refunded already (fund show CAMPAIGN)")
	}
	return moved, nil
}

// fundDeliver: once the goal is met, the artifact's key to every backer,
// sealed, one copy per backer tied to its first pledge lock. The key must
// match the artifact's commitment on chain.
func (c *Client) fundDeliver(id, art uint64, key []byte) (map[string]any, error) {
	k, err := c.campaign(id)
	if err != nil {
		return nil, err
	}
	if k.ReachedAt == 0 {
		return nil, fmt.Errorf("campaign %d hasn't met its goal: backers pay only if it does, so they get the key only then", id)
	}
	if ownerName(c.f.Name) != ownerName(k.Creator) && ownerName(c.f.Name) != ownerName(k.By) {
		return nil, fmt.Errorf("only campaign %d's creator delivers to its backers", id)
	}
	_, _, _, _, commit, err := c.artifact(art)
	if err != nil {
		return nil, err
	}
	if commitOf(key) != commit {
		return nil, fmt.Errorf("that key doesn't match artifact %d's commitment: backers' clients would refuse it", art)
	}
	evs, err := c.pledges(id)
	if err != nil {
		return nil, err
	}
	sent, failed := []string{}, []string{}
	seen := map[common.Hash]bool{}
	for _, ev := range evs {
		b := common.Hash(ev.Args["backer"].([32]byte))
		if seen[b] {
			continue
		}
		seen[b] = true
		name, err := c.res.nameOf(b)
		if err != nil {
			failed = append(failed, b.Hex()[:10]+" (no name)")
			continue
		}
		lock := ev.Args["lockId"].(*big.Int).Uint64()
		if _, err := c.sendDatagram(tKey, name, KeyDelivery{Lock: lock, Artifact: art, Key: key}); err != nil {
			failed = append(failed, name+" ("+err.Error()+")")
			continue
		}
		sent = append(sent, name)
	}
	res := map[string]any{"campaign": id, "artifact": art, "delivered_to": sent,
		"next": "each backer opens it with 'artifact get LOCK FILE.enc --out DIR' (LOCK: its pledge)"}
	if len(failed) > 0 {
		res["not_delivered"] = failed
		return res, fmt.Errorf("the key reached %d backers, not %d: run fund deliver again later", len(sent), len(sent)+len(failed))
	}
	return res, nil
}

// fundEvents: pledges to campaigns we run or back, goals reached, deadlines missed.
func (c *Client) fundEvents(from uint64, now uint64, w *WatchState, emit func(Event), once func(string, Event)) error {
	if !c.ch.has("PledgeCondition") {
		return nil
	}
	if w.Campaigns == nil {
		w.Campaigns = map[string]bool{}
	}
	me := c.mine()
	created, err := c.ch.eventsFrom(from, "PledgeCondition", "CampaignCreated")
	if err != nil {
		return err
	}
	for _, ev := range created {
		if me[common.Hash(ev.Args["creator"].([32]byte))] || me[common.Hash(ev.Args["by"].([32]byte))] {
			w.Campaigns[ev.Args["campaign"].(*big.Int).String()] = true
		}
	}
	pledged, err := c.ch.eventsFrom(from, "PledgeCondition", "Pledged")
	if err != nil {
		return err
	}
	for _, ev := range pledged {
		id := ev.Args["campaign"].(*big.Int).String()
		if me[common.Hash(ev.Args["backer"].([32]byte))] {
			w.Campaigns[id] = true
		}
		if w.Campaigns[id] {
			emit(Event{"fund-pledged", ev.Block, fmt.Sprintf("campaign %s: %s pledged %s (%s in all)", id, c.nameOr(common.Hash(ev.Args["backer"].([32]byte))),
				c.fmtMoney(ev.Args["amount"].(*big.Int)), c.fmtMoney(ev.Args["pledged"].(*big.Int))), map[string]any{"campaign": id, "lock": ev.Args["lockId"].(*big.Int).String()}})
		}
	}
	reached, err := c.ch.eventsFrom(from, "PledgeCondition", "GoalReached")
	if err != nil {
		return err
	}
	for _, ev := range reached {
		if id := ev.Args["campaign"].(*big.Int).String(); w.Campaigns[id] {
			emit(Event{"fund-goal-reached", ev.Block, fmt.Sprintf("campaign %s reached its goal with %s: 'fund collect %s'", id, c.fmtMoney(ev.Args["pledged"].(*big.Int)), id),
				map[string]any{"campaign": id}})
		}
	}
	for id := range w.Campaigns {
		if k, err := c.campaign(atoi(id)); err == nil && k.state(now) == "failed" {
			once("fund-failed:"+id, Event{"fund-failed", 0, fmt.Sprintf("campaign %s missed its goal (%s of %s): backers 'fund refund %s'", id, c.fmtMoney(k.Pledged), c.fmtMoney(k.Goal), id),
				map[string]any{"campaign": id}})
		}
	}
	return nil
}

func runFund(c *Client, cmd string, pos []string, days, minutes int, to, terms string) error {
	switch cmd {
	case "fund create":
		if err := need(pos, 2, "fund create TITLE GOAL --days N [--to NAME|COMMUNITY] [--uri TERMS]"); err != nil {
			return err
		}
		goal, err := c.parseMoney(pos[1])
		if err != nil {
			return err
		}
		secs := uint64(max(days, 1)) * 86400
		if minutes > 0 {
			secs = uint64(minutes) * 60
		}
		deadline := c.chainNowU() + secs
		id, err := c.fundCreate(pos[0], goal, deadline, to, terms)
		if err != nil {
			return err
		}
		v, _ := c.fundView(id)
		v["note"] = fmt.Sprintf("backers: 'fund pledge %d AMOUNT'; all or nothing by the deadline", id)
		out(v)
	case "fund pledge":
		if err := need(pos, 2, "fund pledge CAMPAIGN AMOUNT"); err != nil {
			return err
		}
		amt, err := c.parseMoney(pos[1])
		if err != nil {
			return err
		}
		lock, err := c.fundPledge(atoi(pos[0]), amt)
		if err != nil {
			return err
		}
		v, _ := c.fundView(atoi(pos[0]))
		v["your_pledge_lock"] = lock
		out(v)
	case "fund deliver":
		if err := need(pos, 3, "fund deliver CAMPAIGN ARTIFACT KEY-FILE"); err != nil {
			return err
		}
		key, err := os.ReadFile(pos[2])
		if err != nil {
			return err
		}
		res, err := c.fundDeliver(atoi(pos[0]), atoi(pos[1]), key)
		if res != nil {
			out(res)
		}
		return err
	case "fund show":
		if err := need(pos, 1, "fund show CAMPAIGN"); err != nil {
			return err
		}
		v, err := c.fundView(atoi(pos[0]))
		if err != nil {
			return err
		}
		out(v)
	case "fund collect", "fund refund":
		if err := need(pos, 1, cmd+" CAMPAIGN"); err != nil {
			return err
		}
		f := c.fundCollect
		if cmd == "fund refund" {
			f = c.fundRefund
		}
		moved, err := f(atoi(pos[0]))
		if err != nil {
			return err
		}
		out(map[string]any{"campaign": pos[0], cmd[5:] + "ed": c.fmtMoney(moved)})
	default:
		return unknownCommand(cmd)
	}
	return nil
}
