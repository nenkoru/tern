// Communities that govern themselves (Commons), and blocklists anyone may
// follow. A community is a name and a set run by the Commons contract: its
// members elect admins and moderators for a term, veto what they do, and
// recall them by petition and vote. Its channel is a public group (keys
// published by an admin): anyone reads, members post, and readers hide what
// the community banned or hid.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	roleAdmin = 1
	roleMod   = 2
)

var actionKinds = []string{"none", "admit", "ban", "hide", "rules", "fee", "spend", "follow", "unban", "commission"}

// CommonsParams mirrors Commons.Params.
type CommonsParams struct {
	Term        uint64
	VoteWindow  uint64
	VetoWindow  uint64
	VetoBps     uint16
	PetitionBps uint16
	QuorumBps   uint16
	Budget      *big.Int
	AgentsVote  bool
}

// CommunityView mirrors Commons.Community.
type CommunityView struct {
	Id            [32]byte
	Set           [32]byte
	Created       uint64
	Members       uint32
	PostFee       *big.Int
	SpentThisTerm *big.Int
	BudgetFrom    uint64
	Rules         string
	P             CommonsParams
}

// ElectionView mirrors Commons.Election.
type ElectionView struct {
	Community  [32]byte
	Role       uint8
	Seats      uint8
	Ends       uint64
	Candidates [][32]byte
	Turnout    uint32
	Closed     bool
}

func roleOf(s string) (uint8, error) {
	switch s {
	case "admin":
		return roleAdmin, nil
	case "mod", "moderator":
		return roleMod, nil
	}
	return 0, fmt.Errorf("role %q: admin or mod", s)
}

func roleName(r uint8) string {
	if r == roleAdmin {
		return "admin"
	}
	return "mod"
}

func (c *Client) commonsBool(method string, args ...any) bool {
	if perfFixes && c.memoHead != 0 { // within one read pass (a scan, a refresh) at one head: each answer once, not 4–6 calls per post
		k := method + fmt.Sprint(args...)
		if m, ok := c.boolMemo[k]; ok && m.head == c.memoHead {
			return m.v
		}
		out, err := c.ch.call("Commons", method, args...)
		v := err == nil && out[0].(bool)
		if err == nil {
			if c.boolMemo == nil || len(c.boolMemo) > 20000 {
				c.boolMemo = map[string]memoBool{}
			}
			c.boolMemo[k] = memoBool{v, c.memoHead}
		}
		return v
	}
	out, err := c.ch.call("Commons", method, args...)
	return err == nil && out[0].(bool)
}

func (c *Client) community(name string) (*CommunityView, error) {
	if !c.ch.has("Commons") {
		return nil, errors.New("this network has no Commons contract (an older devnet)")
	}
	out, err := c.ch.call("Commons", "communities", idOf(name))
	if err != nil {
		return nil, err
	}
	v := CommunityView{Id: out[0].([32]byte), Set: out[1].([32]byte), Created: out[2].(uint64), Members: out[3].(uint32),
		PostFee: out[4].(*big.Int), SpentThisTerm: out[5].(*big.Int), BudgetFrom: out[6].(uint64), Rules: out[7].(string),
		P: *abi.ConvertType(out[8], new(CommonsParams)).(*CommonsParams)}
	if v.Created == 0 {
		return nil, fmt.Errorf("no community %s", name)
	}
	return &v, nil
}

// isCommunity: the group is a community (cached: that never changes).
func (c *Client) isCommunity(name string) bool {
	if c.comm == nil {
		c.comm = map[string]bool{}
	}
	if v, ok := c.comm[name]; ok {
		return v
	}
	_, err := c.community(name)
	c.comm[name] = err == nil
	return err == nil
}

// act sends a Commons power and returns the action's number.
func (c *Client) act(method string, args ...any) (uint64, error) {
	r, err := c.ch.send(c.key, nil, "Commons", method, args...)
	if err != nil {
		return 0, err
	}
	return c.firstIndexed(r, "Commons", "Acted")
}

// ---- founding and membership

// found: the community, with us as its first admin, and its channel keys published.
func (c *Client) found(name string, p CommonsParams, rules string) error {
	if _, err := c.ch.send(c.key, nil, "Commons", "found", name, c.id(), p, rules); err != nil {
		return err
	}
	e, err := c.ch.epoch()
	if err != nil {
		return err
	}
	g, err := newGroupKeys(name, e, keysAhead*4, true, c.f.PQ)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(g)
	if _, err := c.ch.send(c.key, nil, "Commons", "publish", idOf(name), c.id(), channelKeysSlot, b); err != nil {
		return err
	}
	c.st.Groups[name] = g
	c.enableRing() // the founder counts in the anonymity set of anonymous posts and votes
	return c.save()
}

// refOf: what a moderator hides: a post (its id or a prefix from inbox/group read) or a name's posts.
func (c *Client) refOf(s string) common.Hash {
	if strings.HasPrefix(s, "0x") && len(s) == 66 {
		return common.HexToHash(s)
	}
	if it := c.find1(s); it != nil {
		return common.HexToHash(it.ID)
	}
	return idOf(s)
}

// ---- reading a community

func (c *Client) nameOrHex(h common.Hash) string {
	if n, err := c.res.nameOf(h); err == nil {
		return n
	}
	return h.Hex()
}

// roleHolders: who holds a role now, with the term's end.
func (c *Client) roleHolders(cid common.Hash) []map[string]any {
	evs, _ := c.ch.events("Commons", "Elected", nil, []common.Hash{cid})
	seen := map[string]bool{}
	var out []map[string]any
	now := c.chainNow()
	for i := len(evs) - 1; i >= 0; i-- {
		who, role := common.Hash(evs[i].Args["who"].([32]byte)), evs[i].Args["role"].(uint8)
		k := who.Hex() + strconv.Itoa(int(role))
		if seen[k] {
			continue
		}
		seen[k] = true
		r, err := c.ch.call("Commons", "roleUntil", cid, who, role)
		if err != nil || int64(r[0].(uint64)) <= now {
			continue
		}
		until := int64(r[0].(uint64))
		v := map[string]any{"name": c.nameOrHex(who), "role": roleName(role), "term_ends": time.Unix(until, 0).UTC().Format(time.RFC3339),
			"term_left_s": until - now}
		if s, err := c.ch.call("Commons", "suspendedUntil", cid, who); err == nil && int64(s[0].(uint64)) > now {
			v["suspended"] = "a recall vote on it runs"
		}
		out = append(out, v)
	}
	return out
}

func (c *Client) actionView(id uint64) map[string]any {
	a, err := c.ch.call("Commons", "actions", u(id))
	if err != nil {
		return nil
	}
	kind := actionKinds[min(int(a[1].(uint8)), len(actionKinds)-1)]
	target := common.Hash(a[3].([32]byte))
	by := c.nameOrHex(common.Hash(a[2].([32]byte)))
	text := func(t string) string { return c.outside(by, t) } // rules, memos, reasons: the actor's words
	v := map[string]any{"action": id, "kind": kind, "by": by,
		"vetoes": a[8].(uint32), "reversed": a[9].(bool)}
	switch kind {
	case "hide":
		v["target"] = target.Hex()
		if n, err := c.res.nameOf(target); err == nil {
			v["target"] = n + "'s posts"
		}
	case "rules":
		v["previous_rules"] = text(a[6].(string))
	case "fee":
		v["fee"] = fmtCoins(a[4].(*big.Int))
	case "spend":
		v["to"], v["amount"], v["memo"], v["paid"] = c.nameOrHex(target), fmtCoins(a[4].(*big.Int)), text(a[6].(string)), a[10].(bool)
	case "commission":
		v["to"], v["amount"], v["terms"], v["lock_opened"] = c.nameOrHex(target), fmtCoins(a[4].(*big.Int)), text(a[6].(string)), a[10].(bool)
		if m, err := c.ch.call("Commons", "commissions", u(id)); err == nil {
			cond := c.conditionName(m[0].(common.Address))
			v["condition"], v["refund_after"] = cond, m[1].(uint64)
			if l := m[2].(*big.Int); l.Sign() > 0 {
				v["lock"] = new(big.Int).Sub(l, big.NewInt(1)).Uint64()
			}
			switch cond {
			case "PanelCondition":
				if x, err := panelDemandType().Unpack(m[3].([]byte)); err == nil && len(x) == 1 {
					d := abiConvert[panelDemand](x[0])
					var ps []string
					for _, p := range d.Panel {
						ps = append(ps, c.nameOr(common.Hash(p)))
					}
					v["decided_by"] = map[string]any{"panel": ps, "k": d.K, "hearing_until": d.HearingUntil}
				}
			case "AttestationCondition":
				if d, ok := c.decodeAttestationDemand(m[3].([]byte)); ok {
					v["decided_by"] = d
				}
			case "HashlockCondition", "PledgeCondition", "RateCondition", "VoucherCondition", "RevealCondition":
			default:
				v["warning"] = "not one of the network's conditions: veto unless you have read it"
			}
		}
	case "follow":
		mode := a[4].(*big.Int).Uint64()
		v["list"], v["on"] = c.nameOrHex(target), mode > 0
		if mode == 2 {
			v["owner_wide"] = true
		}
	default:
		v["target"] = c.nameOrHex(target)
		if a[6].(string) != "" {
			v["reason"] = text(a[6].(string))
		}
	}
	return v
}

func (c *Client) showCommunity(name string) (map[string]any, error) {
	k, err := c.community(name)
	if err != nil {
		return nil, err
	}
	cid := idOf(name)
	acted, err := c.ch.events("Commons", "Acted", nil, []common.Hash{cid})
	if err != nil {
		return nil, err
	}
	var recent []map[string]any
	for i := len(acted) - 1; i >= 0 && len(recent) < 10; i-- {
		if v := c.actionView(acted[i].Args["action"].(*big.Int).Uint64()); v != nil {
			recent = append(recent, v)
		}
	}
	var follows []string
	if f, err := c.ch.call("Commons", "followingOf", cid); err == nil {
		for _, x := range f[0].([][32]byte) {
			follows = append(follows, c.nameOrHex(common.Hash(x)))
		}
	}
	treasury, _ := c.bankBalance(name)
	if treasury == nil {
		treasury = new(big.Int)
	}
	members, _ := c.setMembers(name)
	contrib := c.contributions(cid)
	// Names are free to register, so one person could hold many votes. The
	// contract can't weigh that (it is at its size limit); each member's name
	// age is shown so admins and voters can judge a vote, or an admission.
	ages, young := map[string]string{}, 0
	for _, m := range members {
		if m == name {
			continue
		}
		if t := c.registeredAt(m); t > 0 {
			d := float64(c.chainNow()-t) / 86400
			ages[m] = fmt.Sprintf("%.1f days", d)
			if d < 7 {
				young++
			}
		}
	}
	hv := map[string]any{"citizens_only": false}
	if h, ours := c.citizensHouse(name); h != (common.Address{}) {
		hv = map[string]any{"citizens_only": true, "house": h.Hex(), "note": "members and voters are citizens (owner names); agents read, they don't post or vote"}
		if !ours {
			hv["warning"] = "not this network's citizens' house: its 'humans only' is not verified"
		}
	}
	return map[string]any{
		"community": name, "members": k.Members, "humans": hv, "member_names": members, "roles": c.roleHolders(cid),
		"member_name_age": ages, "members_under_a_week_old": young, "announce_only": c.announceOnly(name),
		"recent_actions": recent, "follows_bans_of": follows, "rules": c.outside("the admins of "+name, k.Rules), "post_fee": fmtCoins(k.PostFee),
		"treasury": fmtCoins(treasury), "contributions": contrib, "spent_this_term": fmtCoins(k.SpentThisTerm), "budget_per_term": fmtCoins(k.P.Budget),
		"elections_open": c.openElections(cid), "recalls_open": c.openRecalls(cid),
		"params": map[string]any{"term_s": k.P.Term, "vote_window_s": k.P.VoteWindow, "veto_window_s": k.P.VetoWindow,
			"veto_bps": k.P.VetoBps, "petition_bps": k.P.PetitionBps, "quorum_bps": k.P.QuorumBps, "agents_vote": k.P.AgentsVote},
	}, nil
}

// contributions: what each name paid into the treasury (Bank payments to the
// community's name, and locks claimed to it such as pledges), from Bank events.
func (c *Client) contributions(cid common.Hash) map[string]string {
	sum := map[string]*big.Int{}
	add := func(from common.Hash, a *big.Int) {
		n := c.nameOrHex(from)
		if sum[n] == nil {
			sum[n] = new(big.Int)
		}
		sum[n].Add(sum[n], a)
	}
	if evs, err := c.ch.events("Bank", "Paid", nil, []common.Hash{cid}); err == nil {
		for _, ev := range evs {
			add(common.Hash(ev.Args["from"].([32]byte)), ev.Args["amount"].(*big.Int))
		}
	}
	if evs, err := c.ch.events("Bank", "Locked", nil, nil, []common.Hash{cid}); err == nil {
		for _, ev := range evs {
			if l, err := c.ch.call("Bank", "locks", ev.Args["id"].(*big.Int)); err == nil && l[4].(*big.Int).Sign() > 0 {
				add(common.Hash(l[1].([32]byte)), l[4].(*big.Int))
			}
		}
	}
	out := map[string]string{}
	for n, a := range sum {
		out[n] = fmtCoins(a)
	}
	return out
}

// commission: an admin proposes hiring `payee` through an escrow paid from
// the treasury: a panel of k members (or an arbiter) decides, after a hearing.
// The lock opens on 'commons payout' once the veto window has passed.
func (c *Client) commission(name, payee string, amount *big.Int, panel []string, k uint8, arbiter string, hearingMin, days uint64, terms string) (uint64, error) {
	cm, err := c.community(name)
	if err != nil {
		return 0, err
	}
	opens := uint64(c.chainNow()) + cm.P.VetoWindow // the earliest payout
	refundAfter := opens + max(days, 1)*86400
	subj := dealSubject(name, payee, "commission", terms, fmt.Sprint(refundAfter))
	var cond string
	var d []byte
	switch {
	case arbiter != "":
		cond = "AttestationCondition"
		d, err = c.attestationDemand(arbiter, subj, "ruling", 0, 1)
	case len(panel) > 0:
		judges := 0
		for _, p := range panel {
			if !isParty(p, name, payee) {
				judges++
			}
		}
		if k == 0 || judges < int(k) {
			return 0, fmt.Errorf("k=%d but only %d of the panel are not the payee's side: it could never be decided", k, judges)
		}
		ids := make([][32]byte, len(panel))
		for i, p := range panel {
			ids[i] = idOf(p)
		}
		cond = "PanelCondition"
		d, err = panelDemandType().Pack(panelDemand{ids, k, c.schema(), subj, crypto.Keccak256Hash([]byte("ruling")), opens + hearingMin*60})
	default:
		return 0, errors.New("who decides: --panel a,b,c --k N, or --arbiter NAME (or --plain: a plain spend, no lock)")
	}
	if err != nil {
		return 0, err
	}
	c.noteDeal(subj, fmt.Sprintf("commission: %s hires %s for %q", name, payee, terms))
	return c.act("commission", idOf(name), c.id(), payeeID(payee), amount, refundAfter, common.HexToAddress(c.ch.dep.Contracts[cond]), d, terms)
}

// registeredAt: when a name was registered (an owner) or first granted (an
// agent), in chain time; 0 if unknown.
func (c *Client) registeredAt(name string) int64 {
	ev := "OwnerRegistered"
	if fromAgent(name) {
		ev = "AgentGranted"
	}
	evs, err := c.ch.events("Registry", ev, []common.Hash{idOf(name)})
	if err != nil || len(evs) == 0 {
		return 0
	}
	t, _ := c.ch.blockTime(evs[0].Block)
	return t
}

func (c *Client) openElections(cid common.Hash) []map[string]any {
	evs, _ := c.ch.events("Commons", "ElectionOpened", nil, []common.Hash{cid})
	var out []map[string]any
	for _, ev := range evs {
		id := ev.Args["election"].(*big.Int)
		x, err := c.ch.call("Commons", "electionOf", id)
		if err != nil {
			continue
		}
		e := *abi.ConvertType(x[0], new(ElectionView)).(*ElectionView)
		if e.Closed {
			continue
		}
		var cands []string
		for _, h := range e.Candidates {
			cands = append(cands, c.nameOrHex(common.Hash(h)))
		}
		out = append(out, map[string]any{"election": id.Uint64(), "role": roleName(e.Role), "seats": e.Seats, "candidates": cands,
			"turnout": e.Turnout, "ends_in_s": int64(e.Ends) - c.chainNow()})
	}
	return out
}

func (c *Client) openRecalls(cid common.Hash) []map[string]any {
	evs, _ := c.ch.events("Commons", "RecallOpened", nil, []common.Hash{cid})
	var out []map[string]any
	for _, ev := range evs {
		id := ev.Args["recall"].(*big.Int)
		x, err := c.ch.call("Commons", "recalls", id)
		if err != nil || x[8].(bool) {
			continue
		}
		v := map[string]any{"recall": id.Uint64(), "who": c.nameOrHex(common.Hash(x[1].([32]byte))), "role": roleName(x[2].(uint8)),
			"signatures": x[4].(uint32)}
		if ends := int64(x[5].(uint64)); ends != 0 {
			v["voting_ends_in_s"], v["yes"], v["no"] = ends-c.chainNow(), x[6].(uint32), x[7].(uint32)
		}
		out = append(out, v)
	}
	return out
}

// ---- blocklists: follow a community's bans in your own client

// blockedBy: the followed community that bans this author (or its owner), or "".
func (c *Client) blockedBy(from string) string {
	if len(c.st.Blocklists)+len(c.st.BlockOwners) == 0 || from == "" || strings.HasPrefix(from, "anon:") {
		return ""
	}
	if c.bl == nil {
		c.bl = map[string]string{}
	}
	if v, ok := c.bl[from]; ok {
		return v
	}
	v := ""
	for _, l := range c.st.Blocklists {
		if _, _, claims := c.claimList(l); claims { // any ICommunity's moderation claims (moderation.go)
			if c.claimBans(l, from) || c.claimBans(l, ownerName(from)) {
				v = l
				break
			}
			continue
		}
		if c.ch.has("Commons") && (c.settledBan(l, from) || c.settledBan(l, ownerName(from))) {
			v = l
			break
		}
	}
	for _, l := range c.st.BlockOwners { // owner-wide: an owner any of whose names (an agent) the list bans
		if v != "" {
			break
		}
		o := ownerName(from)
		if _, _, claims := c.claimList(l); claims {
			if c.claimBansOwnerWide(l, o) {
				v = l + " (owner-wide: it bans a name of " + o + ")"
			}
			continue
		}
		if !c.ch.has("Commons") {
			continue
		}
		if c.commonsBool("banned", idOf(l), idOf(o)) && !c.settledBan(l, o) {
			continue // the owner itself is banned there, but its ban can still be vetoed
		}
		if out, err := c.ch.call("Commons", "bannedNamesOf", idOf(l), idOf(o)); err == nil && out[0].(uint32) > 0 {
			v = l + " (owner-wide: it bans a name of " + ownerName(from) + ")"
		}
	}
	c.bl[from] = v
	return v
}

// settledBan: the list bans the name (here, not through lists it follows), and
// the ban is past the list's veto window: a list's new bans reach the people
// following it only once its members could have vetoed them.
func (c *Client) settledBan(list, name string) bool {
	cid, id := idOf(list), idOf(name)
	if !c.commonsBool("banned", cid, id) {
		return false
	}
	if abuseFixesOff {
		return true
	}
	b, err := c.ch.call("Commons", "banAction", cid, id)
	if err != nil || b[0].(*big.Int).Sign() == 0 {
		return true
	}
	a, err := c.ch.call("Commons", "actions", new(big.Int).Sub(b[0].(*big.Int), big.NewInt(1)))
	k, err2 := c.community(list)
	if err != nil || err2 != nil {
		return true
	}
	return int64(a[7].(uint64)+k.P.VetoWindow) <= c.chainNow()
}

// bans: the names a community bans now.
func (c *Client) bans(name string) []string {
	cid := idOf(name)
	evs, _ := c.ch.events("Commons", "Acted", nil, []common.Hash{cid})
	seen := map[common.Hash]bool{}
	out := []string{}
	for _, ev := range evs {
		if ev.Args["kind"].(uint8) != 2 {
			continue
		}
		t := common.Hash(ev.Args["target"].([32]byte))
		if !seen[t] && c.commonsBool("banned", cid, t) {
			seen[t] = true
			out = append(out, c.nameOrHex(t))
		}
	}
	return out
}

// ---- push events: roles, recalls, vetoes, bans

func (c *Client) commonsEvents(from uint64, now int64, w *WatchState, emit func(Event), once func(string, Event)) error {
	if !c.ch.has("Commons") {
		return nil
	}
	me := c.mine()
	in := map[common.Hash]bool{}
	member := func(cid common.Hash) bool {
		v, ok := in[cid]
		if !ok {
			v = c.commonsBool("isMember", cid, idOf(c.f.Name)) || c.commonsBool("isMember", cid, idOf(c.f.Owner))
			in[cid] = v
		}
		return v
	}
	cname := func(h common.Hash) string { return c.nameOrHex(h) }
	evs, err := c.ch.eventsFrom(from, "Commons", "Elected")
	if err != nil {
		return err
	}
	for _, ev := range evs {
		cid, who, role := common.Hash(ev.Args["community"].([32]byte)), common.Hash(ev.Args["who"].([32]byte)), ev.Args["role"].(uint8)
		until := ev.Args["until"].(uint64)
		ref := map[string]any{"community": cname(cid), "who": cname(who), "role": roleName(role), "until": until}
		if me[who] {
			w.Roles[cid.Hex()+"|"+who.Hex()+"|"+strconv.Itoa(int(role))] = until
			emit(Event{"role-elected", ev.Block, fmt.Sprintf("you are %s of %s until %s", roleName(role), cname(cid), time.Unix(int64(until), 0).UTC().Format(time.RFC3339)), ref})
		} else if member(cid) {
			emit(Event{"role-elected", ev.Block, fmt.Sprintf("%s is %s of %s now", cname(who), roleName(role), cname(cid)), ref})
		}
	}
	for k, until := range w.Roles { // our terms that ran out (a recall ends them too)
		p := strings.Split(k, "|")
		role, _ := strconv.Atoi(p[2])
		r, err := c.ch.call("Commons", "roleUntil", common.HexToHash(p[0]), common.HexToHash(p[1]), uint8(role))
		if err == nil && int64(r[0].(uint64)) <= now {
			once("expired:"+k+strconv.FormatUint(until, 10), Event{"role-expired", 0, fmt.Sprintf("your %s role in %s has ended", roleName(uint8(role)), cname(common.HexToHash(p[0]))),
				map[string]any{"community": cname(common.HexToHash(p[0])), "role": roleName(uint8(role))}})
			delete(w.Roles, k)
		}
	}
	for _, name := range []string{"RecallOpened", "RecallVoting", "RecallClosed"} {
		evs, err := c.ch.eventsFrom(from, "Commons", name)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			cid, who := common.Hash(ev.Args["community"].([32]byte)), common.Hash(ev.Args["who"].([32]byte))
			if !me[who] && !member(cid) {
				continue
			}
			r := ev.Args["recall"].(*big.Int).String()
			ref := map[string]any{"recall": r, "community": cname(cid), "who": cname(who)}
			switch name {
			case "RecallOpened":
				emit(Event{"recall-opened", ev.Block, fmt.Sprintf("petition %s to recall %s as %s of %s: 'commons sign %s'", r, cname(who), roleName(ev.Args["role"].(uint8)), cname(cid), r), ref})
			case "RecallVoting":
				emit(Event{"recall-voting", ev.Block, fmt.Sprintf("recall %s of %s in %s is put to a vote: 'commons recall-vote %s yes|no' within %d s", r, cname(who), cname(cid), r,
					int64(ev.Args["ends"].(uint64))-now), ref})
			default:
				what := "stays"
				if ev.Args["removed"].(bool) {
					what = "is removed"
				}
				emit(Event{"recall-closed", ev.Block, fmt.Sprintf("recall %s closed: %s %s (%d yes, %d no)", r, cname(who), what, ev.Args["yes"].(uint32), ev.Args["no"].(uint32)), ref})
			}
		}
	}
	vetoes, err := c.ch.eventsFrom(from, "Commons", "Vetoed")
	if err != nil {
		return err
	}
	for _, ev := range vetoes {
		if cid := common.Hash(ev.Args["community"].([32]byte)); member(cid) {
			id := ev.Args["action"].(*big.Int).String()
			emit(Event{"action-vetoed", ev.Block, fmt.Sprintf("action %s in %s has %d vetoes: 'commons veto %s' to join", id, cname(cid), ev.Args["vetoes"].(uint32), id),
				map[string]any{"action": id, "community": cname(cid)}})
		}
	}
	revs, err := c.ch.eventsFrom(from, "Commons", "Reversed")
	if err != nil {
		return err
	}
	for _, ev := range revs {
		cid, id := common.Hash(ev.Args["community"].([32]byte)), ev.Args["action"].(*big.Int)
		a, _ := c.ch.call("Commons", "actions", id)
		mine := len(a) > 3 && me[common.Hash(a[3].([32]byte))]
		if mine || member(cid) {
			v := c.actionView(id.Uint64())
			emit(Event{"action-reversed", ev.Block, fmt.Sprintf("members reversed action %s in %s (%v %v)", id, cname(cid), v["kind"], v["target"]),
				map[string]any{"action": id.String(), "community": cname(cid)}})
		}
	}
	acted, err := c.ch.eventsFrom(from, "Commons", "Acted")
	if err != nil {
		return err
	}
	for _, ev := range acted {
		// Only bans from communities we are in: anyone can found one and "ban" anyone.
		if cid := common.Hash(ev.Args["community"].([32]byte)); ev.Args["kind"].(uint8) == 2 && me[common.Hash(ev.Args["target"].([32]byte))] &&
			(c.st.Groups[cname(cid)] != nil || member(cid)) {
			id, by := ev.Args["action"].(*big.Int).String(), cname(common.Hash(ev.Args["by"].([32]byte)))
			emit(Event{"banned", ev.Block, fmt.Sprintf("you were banned from %s by %s, reason %s (action %s; its members may veto it)", cname(cid),
				by, c.outside(by, ev.Args["note"].(string)), id), map[string]any{"action": id, "community": cname(cid)}})
		}
	}
	return nil
}

// ---- CLI

type commonsFlags struct {
	term, voteWindow, vetoWindow    uint64
	vetoBps, petitionBps, quorumBps uint
	budget, rules                   string
	agentsVote, off                 bool
	citizensOnly, owners            bool
	announce                        bool // commons found --announce: readers show only its admins' and moderators' posts (news.go)
	panel, arbiter                  string
	k, hearing, days                uint64
	plain                           bool // commons commission --plain: a spend, no lock
}

func runCommons(c *Client, cmd string, pos []string, f commonsFlags) error {
	at := func(i int) string { return firstOr(pos, i, "") }
	num := func(i int) (uint64, error) {
		n, err := strconv.ParseUint(at(i), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad number %q", at(i))
		}
		return n, nil
	}
	rest := func(i int) string { return strings.Join(pos[min(i, len(pos)):], " ") }
	acted := func(what string, id uint64, err error, extra ...any) error {
		if err != nil {
			return err
		}
		v := map[string]any{what: true, "action": id, "note": fmt.Sprintf("members may veto it: 'commons veto %d'", id)}
		for i := 0; i+1 < len(extra); i += 2 {
			v[extra[i].(string)] = extra[i+1]
		}
		out(v)
		return nil
	}
	switch cmd {
	case "commons found":
		if err := need(pos, 1, "commons found NAME [--term S] [--vote-window S] [--veto-window S] [--veto-bps N] [--petition-bps N] [--quorum-bps N] [--budget COINS] [--agents-vote] [--citizens-only] [--announce] [--rules TEXT]"); err != nil {
			return err
		}
		budget, err := parseCoins(f.budget)
		if err != nil {
			return err
		}
		p := CommonsParams{Term: f.term, VoteWindow: f.voteWindow, VetoWindow: f.vetoWindow, VetoBps: uint16(f.vetoBps),
			PetitionBps: uint16(f.petitionBps), QuorumBps: uint16(f.quorumBps), Budget: budget, AgentsVote: f.agentsVote}
		if f.citizensOnly {
			if fromAgent(c.f.Name) || !c.isCitizen(c.f.Name) {
				return fmt.Errorf("a citizens-only community is founded by a citizen, as itself")
			}
		}
		if err := c.found(pos[0], p, f.rules); err != nil {
			return err
		}
		v := map[string]any{"founded": pos[0], "admin": c.f.Name, "term_s": p.Term, "note": "you are its first admin for one term; people ask with 'commons request " + pos[0] + "'"}
		if f.citizensOnly {
			if err := c.makeCitizensOnly(pos[0]); err != nil {
				return fmt.Errorf("founded %s, but not citizens only: %w", pos[0], err)
			}
			v["citizens_only"] = "members and voters must be citizens; agents read, they don't post or vote"
		}
		if f.announce {
			if err := c.makeAnnounceOnly(pos[0]); err != nil {
				return fmt.Errorf("founded %s, but not announce-only: %w", pos[0], err)
			}
			v["announce"] = "announce-only: readers show posts by its admins and moderators only; members join and read. " +
				"Keep an admin in office (--term sets the first term): when no admin or moderator holds a role, readers show nothing. " +
				"To make it the network's news, set \"announce\": \"" + pos[0] + "\" in net.json"
		}
		out(v)
	case "commons request":
		if err := need(pos, 1, "commons request NAME"); err != nil {
			return err
		}
		msg, err := c.joinGroup(pos[0])
		if err != nil {
			return err
		}
		out(map[string]any{"community": pos[0], "status": msg})
	case "commons requests":
		if err := need(pos, 1, "commons requests NAME   (who asked to join and waits for a moderator)"); err != nil {
			return err
		}
		rs, err := c.joinRequests(at(0))
		if err != nil {
			return err
		}
		out(map[string]any{"community": at(0), "waiting": rs, "admit": "commons admit " + at(0) + " WHO"})
	case "commons report":
		if err := need(pos, 2, "commons report NAME POST [REASON…]   (members flag a post; moderators are told)"); err != nil {
			return err
		}
		ref, err := c.report(at(0), at(1), rest(2), "report")
		if err != nil {
			return err
		}
		out(map[string]any{"reported": ref.Hex(), "community": at(0), "note": "moderators get a 'report' event; members' reports, weighed by each reporter's standing (citizen, account age, conduct, record), hide the post for readers once they reach " + c.reportNeed(at(0)) + ", until a moderator decides"})
	case "commons dismiss":
		if err := need(pos, 2, "commons dismiss NAME POST [REASON…]   (a moderator: the reports on this post no longer hide it)"); err != nil {
			return err
		}
		if !c.commonsBool("hasRole", idOf(at(0)), c.id(), uint8(roleMod)) {
			return fmt.Errorf("only a moderator or admin of %s dismisses reports", at(0))
		}
		ref, err := c.report(at(0), at(1), rest(2), "report-dismissed")
		if err != nil {
			return err
		}
		out(map[string]any{"dismissed": ref.Hex(), "community": at(0)})
	case "commons admit":
		if err := need(pos, 2, "commons admit NAME WHO"); err != nil {
			return err
		}
		id, err := c.act("admit", idOf(at(0)), c.id(), idOf(at(1)))
		return acted("admitted", id, err, "who", at(1))
	case "commons ban":
		if err := need(pos, 2, "commons ban NAME WHO [REASON…]"); err != nil {
			return err
		}
		id, err := c.act("ban", idOf(at(0)), c.id(), idOf(at(1)), rest(2))
		return acted("banned", id, err, "who", at(1))
	case "commons unban":
		if err := need(pos, 2, "commons unban NAME WHO [REASON…]"); err != nil {
			return err
		}
		id, err := c.act("unban", idOf(at(0)), c.id(), idOf(at(1)), rest(2))
		return acted("unbanned", id, err, "who", at(1))
	case "commons hide":
		if err := need(pos, 2, "commons hide NAME POST-ID|NAME [REASON…]"); err != nil {
			return err
		}
		ref := c.refOf(at(1))
		reason := rest(2)
		if it := c.find1(at(1)); it != nil && it.From != "" && !strings.HasPrefix(it.From, "anon:") {
			reason = strings.TrimSpace(reason + " [author " + it.From + "]") // the owner's conduct counts it (rating)
		}
		id, err := c.act("hide", idOf(at(0)), c.id(), ref, reason)
		return acted("hidden", id, err, "ref", ref.Hex())
	case "commons rules":
		if err := need(pos, 2, "commons rules NAME TEXT…"); err != nil {
			return err
		}
		id, err := c.act("setRules", idOf(at(0)), c.id(), rest(1))
		return acted("rules_set", id, err)
	case "commons fee":
		if err := need(pos, 2, "commons fee NAME AMOUNT   (paid to the community per group post)"); err != nil {
			return err
		}
		fee, err := parseCoins(at(1))
		if err != nil {
			return err
		}
		id, err := c.act("setPostFee", idOf(at(0)), c.id(), fee)
		return acted("post_fee_set", id, err, "fee", at(1))
	case "commons follow":
		if err := need(pos, 2, "commons follow NAME OTHER [--owners] [--off]   (NAME follows OTHER's bans; --owners: also every name of an owner any of whose agents OTHER bans)"); err != nil {
			return err
		}
		mode := uint8(1)
		if f.owners {
			mode = 2
		}
		if f.off {
			mode = 0
		}
		id, err := c.act("follow", idOf(at(0)), c.id(), idOf(at(1)), mode)
		return acted("follows", id, err, "list", at(1), "on", !f.off, "owner_wide", mode == 2)
	case "commons spend":
		if err := need(pos, 3, "commons spend NAME TO AMOUNT [MEMO…]"); err != nil {
			return err
		}
		amt, err := parseCoins(at(2))
		if err != nil {
			return err
		}
		id, err := c.act("spend", idOf(at(0)), c.id(), payeeID(at(1)), amt, rest(3))
		if err != nil {
			return err
		}
		k, _ := c.community(at(0))
		out(map[string]any{"spend": id, "to": at(1), "amount": at(2), "note": fmt.Sprintf("paid after the veto window (%d s): 'commons payout %d'; members may veto it meanwhile", k.P.VetoWindow, id)})
	case "commons commission":
		if f.plain { // a commission without a condition: a plain spend from the treasury after the veto window
			if err := need(pos, 3, "commons commission NAME TO AMOUNT [MEMO…] --plain"); err != nil {
				return err
			}
			amt, err := parseCoins(at(2))
			if err != nil {
				return err
			}
			id, err := c.act("spend", idOf(at(0)), c.id(), payeeID(at(1)), amt, rest(3))
			if err != nil {
				return err
			}
			k, _ := c.community(at(0))
			v := c.actionView(id)
			v["note"] = fmt.Sprintf("paid after the veto window (%d s): 'commons payout %d'; members may veto it meanwhile", k.P.VetoWindow, id)
			out(v)
			return nil
		}
		if err := need(pos, 4, "commons commission NAME PAYEE AMOUNT --panel a,b,c --k N [--hearing-minutes N] [--days D] TERMS…   (or --arbiter NAME; or --plain: no lock, a spend)"); err != nil {
			return err
		}
		amt, err := parseCoins(at(2))
		if err != nil {
			return err
		}
		id, err := c.commission(at(0), at(1), amt, splitList(f.panel), uint8(f.k), f.arbiter, f.hearing, f.days, rest(3))
		if err != nil {
			return err
		}
		v := c.actionView(id)
		v["note"] = fmt.Sprintf("after the veto window, 'commons payout %d' opens the lock from the treasury; members may veto it meanwhile", id)
		out(v)
	case "commons fund":
		if err := need(pos, 2, "commons fund NAME AMOUNT   (anyone pays into a community's treasury)"); err != nil {
			return err
		}
		amt, err := parseCoins(at(1))
		if err != nil {
			return err
		}
		if _, err := c.community(at(0)); err != nil {
			return err
		}
		if err := c.pay(at(0), amt, "fund "+at(0)); err != nil {
			return err
		}
		t, _ := c.bankBalance(at(0))
		out(map[string]any{"funded": at(0), "amount": at(1), "treasury": fmtCoins(t)})
	case "commons payout":
		id, err := num(0)
		if err != nil {
			return err
		}
		r, err := c.ch.send(c.key, nil, "Commons", "payOut", u(id))
		if err != nil {
			return err
		}
		v := map[string]any{"paid_out": id}
		ev := c.ch.abis["Commons"].Events["Commissioned"]
		for _, l := range r.Logs {
			if len(l.Topics) > 3 && l.Topics[0] == ev.ID {
				lock := new(big.Int).SetBytes(l.Topics[3].Bytes()).Uint64()
				v = map[string]any{"commission": id, "lock": lock, "note": fmt.Sprintf("the payee accepts with 'lock accept %d'; the panel rules with 'panel rule %d …' (lock show %d)", lock, lock, lock)}
			}
		}
		out(v)
	case "commons veto":
		id, err := num(0)
		if err != nil {
			return err
		}
		if _, err := c.ch.send(c.key, nil, "Commons", "veto", u(id), c.id()); err != nil {
			return err
		}
		out(c.actionView(id))
	case "commons elect":
		if err := need(pos, 3, "commons elect NAME admin|mod SEATS"); err != nil {
			return err
		}
		role, err := roleOf(at(1))
		if err != nil {
			return err
		}
		seats, err := num(2)
		if err != nil {
			return err
		}
		r, err := c.ch.send(c.key, nil, "Commons", "callElection", idOf(at(0)), c.id(), role, uint8(seats))
		if err != nil {
			return err
		}
		e, err := c.firstIndexed(r, "Commons", "ElectionOpened")
		if err != nil {
			return err
		}
		v := map[string]any{"election": e, "community": at(0), "role": at(1), "seats": seats,
			"note": fmt.Sprintf("candidates first ('commons stand %d'); voting opens once someone stands ('commons vote %d CANDIDATE'); then 'commons close %d'", e, e, e)}
		if cm, err := c.community(at(0)); err == nil {
			v["voting_ends_in_s"] = int64(cm.P.VoteWindow)
			v["quorum"] = fmt.Sprintf("%d votes of %d members (%d bps) or nobody is elected", quorumOf(cm.Members, cm.P.QuorumBps), cm.Members, cm.P.QuorumBps)
		}
		out(v)
	case "commons stand":
		e, err := num(0)
		if err != nil {
			return err
		}
		if _, err := c.ch.send(c.key, nil, "Commons", "stand", u(e), c.id()); err != nil {
			return err
		}
		out(map[string]any{"standing": c.f.Name, "election": e})
	case "commons vote":
		if err := need(pos, 2, "commons vote E CANDIDATE"); err != nil {
			return err
		}
		e, err := num(0)
		if err != nil {
			return err
		}
		if x, err := c.election(e); err == nil {
			cands := c.candidates(x)
			switch {
			case x.Closed || int64(x.Ends) <= c.chainNow():
				return fmt.Errorf("election %d is over: voting has ended ('commons close %d' counts it)", e, e)
			case len(cands) == 0:
				return fmt.Errorf("voting in election %d opens when candidates stood: nobody stands yet ('commons stand %d'; %d s left)", e, e, int64(x.Ends)-c.chainNow())
			case !contains(cands, at(1)):
				return fmt.Errorf("%s does not stand in election %d: candidates are %s (a candidate stands with 'commons stand %d')", at(1), e, strings.Join(cands, ", "), e)
			}
		}
		if _, err := c.ch.send(c.key, nil, "Commons", "vote", u(e), c.id(), idOf(at(1))); err != nil {
			return err
		}
		out(map[string]any{"voted": at(1), "election": e})
	case "commons close":
		e, err := num(0)
		if err != nil {
			return err
		}
		x, xerr := c.election(e)
		if xerr == nil { // one clock: the chain's, as ends_in_s in 'commons show'
			if x.Closed {
				return fmt.Errorf("election %d is already closed: see 'commons show' for who holds the roles", e)
			}
			if left := int64(x.Ends) - c.chainNow(); left > 0 {
				return fmt.Errorf("election %d: voting is open for %d more seconds (chain time); close it after", e, left)
			}
		}
		r, err := c.ch.send(c.key, nil, "Commons", "closeElection", u(e))
		if err != nil {
			return err
		}
		elected := []string{}
		ev := c.ch.abis["Commons"].Events["Elected"]
		for _, l := range r.Logs {
			if len(l.Topics) > 3 && l.Topics[0] == ev.ID {
				elected = append(elected, c.nameOrHex(l.Topics[3]))
			}
		}
		v := map[string]any{"election": e, "elected": elected}
		if len(elected) == 0 {
			v["note"] = "nobody elected: turnout under the quorum, or no votes"
			if xerr == nil {
				if cm, err := c.ch.call("Commons", "communities", x.Community); err == nil {
					members, bps := cm[3].(uint32), abi.ConvertType(cm[8], new(CommonsParams)).(*CommonsParams).QuorumBps
					v["note"] = fmt.Sprintf("nobody elected: %d voted, the quorum is %d of %d members; a candidate also needs more than turnout/(seats+1) votes", x.Turnout, quorumOf(members, bps), members)
				}
			}
		}
		out(v)
	case "commons recall":
		if err := need(pos, 3, "commons recall NAME WHO admin|mod"); err != nil {
			return err
		}
		role, err := roleOf(at(2))
		if err != nil {
			return err
		}
		r, err := c.ch.send(c.key, nil, "Commons", "petition", idOf(at(0)), c.id(), idOf(at(1)), role)
		if err != nil {
			return err
		}
		id, err := c.firstIndexed(r, "Commons", "RecallOpened")
		if err != nil {
			return err
		}
		out(map[string]any{"recall": id, "who": at(1), "role": at(2), "note": fmt.Sprintf("others sign with 'commons sign %d'; enough signatures open the vote", id)})
	case "commons sign":
		id, err := num(0)
		if err != nil {
			return err
		}
		r, err := c.ch.send(c.key, nil, "Commons", "sign", u(id), c.id())
		if err != nil {
			return err
		}
		v := map[string]any{"signed": id}
		if _, err := c.firstIndexed(r, "Commons", "RecallVoting"); err == nil {
			v["note"] = fmt.Sprintf("enough signatures: the vote is open ('commons recall-vote %d yes|no')", id)
		}
		out(v)
	case "commons recall-vote":
		if err := need(pos, 2, "commons recall-vote R yes|no"); err != nil {
			return err
		}
		id, err := num(0)
		if err != nil {
			return err
		}
		if at(1) != "yes" && at(1) != "no" {
			return fmt.Errorf("%s: vote yes or no (got %q)", cmd, at(1))
		}
		if _, err := c.ch.send(c.key, nil, "Commons", "voteRecall", u(id), c.id(), at(1) == "yes"); err != nil {
			return err
		}
		out(map[string]any{"recall": id, "voted": at(1)})
	case "commons recall-close":
		id, err := num(0)
		if err != nil {
			return err
		}
		r, err := c.ch.send(c.key, nil, "Commons", "closeRecall", u(id))
		if err != nil {
			return err
		}
		ev := c.ch.abis["Commons"].Events["RecallClosed"]
		for _, l := range r.Logs {
			if len(l.Topics) > 3 && l.Topics[0] == ev.ID {
				xs, err := ev.Inputs.NonIndexed().Unpack(l.Data)
				if err != nil {
					return err
				}
				out(map[string]any{"recall": id, "who": c.nameOrHex(l.Topics[3]), "removed": xs[0].(bool), "yes": xs[1].(uint32), "no": xs[2].(uint32)})
			}
		}
	case "commons show":
		if err := need(pos, 1, "commons show NAME"); err != nil {
			return err
		}
		v, err := c.showCommunity(at(0))
		if err != nil {
			return err
		}
		out(v)
	default:
		return unknownCommand(cmd)
	}
	return nil
}

// runBlocklist: owners (blocklist follow --owners) follows a list owner-wide:
// an owner any of whose names (an agent) it bans is blocked with all its names.
func runBlocklist(c *Client, cmd string, pos []string, owners ...bool) error {
	switch cmd {
	case "blocklist follow", "blocklist unfollow":
		if err := need(pos, 1, cmd+" COMMUNITY [--owners]"); err != nil {
			return err
		}
		l := pos[0]
		drop := func(xs []string) []string {
			keep := []string{}
			for _, x := range xs {
				if x != l {
					keep = append(keep, x)
				}
			}
			return keep
		}
		keep, wide := drop(c.st.Blocklists), drop(c.st.BlockOwners)
		if cmd == "blocklist follow" {
			if strings.Contains(l, ":") { // CONTRACT:NAME: any ICommunity's moderation claims (moderation.go)
				if _, _, ok := c.claimList(l); !ok {
					return fmt.Errorf("%s: CONTRACT:NAME, CONTRACT a deployment name (Sets, Commons) or an 0x address of an ICommunity", l)
				}
			} else if _, err := c.community(l); err != nil {
				return err
			}
			keep = append(keep, l)
			if len(owners) > 0 && owners[0] {
				wide = append(wide, l)
			}
		}
		sort.Strings(keep)
		sort.Strings(wide)
		c.st.Blocklists, c.st.BlockOwners = keep, wide
		c.bl = nil
		if err := c.save(); err != nil {
			return err
		}
		out(map[string]any{"following_bans_of": keep, "owner_wide": wide, "note": "mail and posts from names these communities ban are dropped (shown with --all)"})
	case "blocklist show":
		v := map[string]any{}
		for _, l := range c.st.Blocklists {
			if a, id, ok := c.claimList(l); ok {
				_, banned := c.modState(a, id)
				names := []string{}
				for h := range banned {
					names = append(names, c.nameOr(h))
				}
				sort.Strings(names)
				v[l] = names
				continue
			}
			v[l] = c.bans(l)
		}
		out(map[string]any{"following_bans_of": c.st.Blocklists, "banned": v})
	default:
		return unknownCommand(cmd)
	}
	return nil
}

// quorumOf: votes needed, as Commons counts it (rounded up).
func quorumOf(members uint32, bps uint16) uint32 {
	n := uint64(members) * uint64(bps)
	q := n / 10000
	if n%10000 != 0 {
		q++
	}
	return uint32(q)
}

func (c *Client) election(e uint64) (*ElectionView, error) {
	x, err := c.ch.call("Commons", "electionOf", u(e))
	if err != nil {
		return nil, fmt.Errorf("no election %d", e)
	}
	return abi.ConvertType(x[0], new(ElectionView)).(*ElectionView), nil
}

func (c *Client) candidates(x *ElectionView) []string {
	var out []string
	for _, h := range x.Candidates {
		out = append(out, c.nameOrHex(common.Hash(h)))
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// joinRequests: names that asked to join a community (or a group by
// approval) and wait to be let in.
func (c *Client) joinRequests(name string) ([]map[string]any, error) {
	sid := setID(name)
	community := c.isCommunity(name)
	if community {
		cm, err := c.community(name)
		if err != nil {
			return nil, err
		}
		sid = common.Hash(cm.Set)
	}
	evs, err := c.ch.events("Sets", "Requested", []common.Hash{sid})
	if err != nil {
		return nil, err
	}
	now := c.chainNow()
	seen := map[common.Hash]bool{}
	out := []map[string]any{}
	for _, e := range evs {
		mid := common.Hash(e.Args["member"].([32]byte))
		if seen[mid] {
			continue
		}
		seen[mid] = true
		if r, err := c.ch.call("Sets", "requested", sid, mid); err != nil || !r[0].(bool) {
			continue
		}
		if community && c.commonsBool("isMember", idOf(name), mid) {
			continue
		}
		v := map[string]any{"name": c.nameOrHex(mid)}
		if t, err := c.ch.blockTime(e.Block); err == nil {
			v["waiting_s"] = now - t
		}
		if community && c.commonsBool("isBanned", idOf(name), mid) {
			v["banned"] = true
		}
		out = append(out, v)
	}
	return out, nil
}

// report: a member's flag on a community post (kind "report"), or a
// moderator's dismissal of the flags (kind "report-dismissed"); a claim
// about the post, naming the community.
func (c *Client) report(name, post, reason, kind string) (common.Hash, error) {
	if !c.isCommunity(name) {
		return common.Hash{}, fmt.Errorf("no community %s", name)
	}
	cid := idOf(name)
	if kind == "report" && !c.commonsBool("isMember", cid, c.id()) && !c.commonsBool("isMember", cid, idOf(c.f.Owner)) {
		return common.Hash{}, fmt.Errorf("only members of %s report its posts (readers count members' reports only)", name)
	}
	var ref common.Hash
	if strings.HasPrefix(post, "0x") && len(post) == 66 {
		ref = common.HexToHash(post)
	} else {
		it := c.find1(post)
		if it == nil {
			c.scan()
			it = c.find1(post)
		}
		if it == nil || it.To != "group:"+name {
			return common.Hash{}, fmt.Errorf("no post %s in %s (ids from 'group read %s')", post, name, name)
		}
		ref = common.HexToHash(it.ID)
	}
	d := map[string]string{"community": name, "reason": reason}
	if it := c.item(ref.Hex()); it != nil && it.From != "" && !strings.HasPrefix(it.From, "anon:") {
		d["author"] = it.From // the owner's conduct counts reported posts (rating)
	}
	b, _ := json.Marshal(d)
	_, err := c.easAttest(ref, kind, b, 0, 0, common.Hash{})
	return ref, err
}

// moderates: this identity (or its owner) is a moderator or admin of the community now.
func (c *Client) moderates(name string) bool {
	return c.commonsBool("hasRole", idOf(name), idOf(c.f.Name), uint8(roleMod)) || c.commonsBool("hasRole", idOf(name), idOf(c.f.Owner), uint8(roleMod))
}
