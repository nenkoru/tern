// Weighted reports: members' reports hide a community post when their weight,
// by each reporter's standing, reaches a threshold relative to the post's
// audience (the community's members). A fixed "3 reports hide a post" didn't
// scale (sim/scale/REPORT.md): in communities of ~300, a few long-standing
// sybils and honest readers' rare mistakes hid 28% of honest posts; scaling the
// count with the community let spam through instead.
//
// A reporter's weight (one per owner: its agents are the owner), from the chain, by the reader:
//
//	w = standing × age × conduct × record
//	standing  1 for a citizen (a verified human), 0.75 for another member
//	age       0.6 + 0.4 × min(1, days since the owner registered / 30)
//	conduct   max(0, 1 − penalty/50), penalty = 25 per community that banned one of its
//	          names + 10 per name hidden + 5 per post hidden by moderators + 3 per post
//	          that 5 or more members reported and no moderator there cleared (any
//	          community). Only what a community of at least conduct_min_members voting
//	          members still holds against a name that was its member counts, a hidden
//	          post only if the reader has it and its signer is the author named, and a
//	          report only if it counts here (below): a name anyone founds a community
//	          for, or writes into a claim, is not silenced by that (audit 5 HM-1, HM-2)
//	record    (1 + 2 × upheld) / (1 + upheld + 2 × dismissed + stale/2), at most 2: its
//	          reports in this community whose post moderators hid after the report (a
//	          moderator's own reports don't uphold themselves), whose reports they
//	          dismissed (a revoked report too), or that stand alone a day later
//
// Reports from the author's own family don't count, nor from members on
// probation (the veto window), nor after a moderator dismissed the reports.
// The weights don't come from the author's circle: who the author knows or
// vouched for neither raises nor lowers them.
//
// A post is hidden when at least reportMinOwners owners reported it and
// Σ w ≥ max(reportMinWeight, reportPerMember × members) × author,
// author = max(0.05, 1 − the author's own penalty/40), × 1.25 for a citizen:
// 3 new members in a small community, 2 established ones (never one alone: one aged
// citizen weighs the whole small-community threshold), 3–4 new ones in a community of 300 (a
// reader there who misreports 1 post in 300 adds ~1 report per post: well
// under the threshold), and a brigade of fresh or misbehaving names weighs
// little. A spammer moderators already caught elsewhere is hidden by two reports, and
// one whose penalty reached conductHideAt (9: a ban, two posts hidden by
// moderators, or three posts that 5+ members reported — an honest author's post
// draws 5 reports about once in 150 even in a community of 300) before anyone reports it: readers who read
// at once can't wait for reports (the simulation's first readers of a post saw
// it before any report landed), so the author's record decides; a moderator's
// dismissal shows the post.
//
// All of the above is the built-in reports policy module
// (internal/ui/builtin/policy-reports.json): a reader may use another
// (tern.policy/1, policy.go) — its weights, thresholds and penalties — but not
// below the floors: at least 2 owners, the family, probation and dismissals
// as said, bans only from communities of at least 3 voting members.
package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/ui"
)

// reportWeights: weighted reports (default); false: the old fixed count (reportHideAt), for comparisons.
var reportWeights = true

// The built-in reports module's reported_at (5): owners reporting a post that
// count it against its author's conduct. (Its other numbers: min_owners 2,
// never one reporter alone (audit 5 HM-11); hide_at 9, an author with this
// penalty has its new posts hidden before anyone reports them; a report
// alone for a day counts against its reporter's record.)

// reportWeightThreshold: the built-in module's weight that hides a post in a community of n members.
func reportWeightThreshold(n int) float64 {
	return ui.BuiltinPolicy("reports").Num("threshold", ui.Env{"community.members": float64(n)})
}

// reportsPolicy: the reports module in use.
func (c *Client) reportsPolicy() *ui.Policy { return c.policyOf("reports") }

// weightThreshold: the module in use's weight that hides a post in a community of n members.
func (c *Client) weightThreshold(n int) float64 {
	return c.reportsPolicy().Num("threshold", ui.Env{"community.members": float64(n)})
}

func (c *Client) communityMembers(group string) int {
	if c.commCount == nil {
		c.commCount = map[string]int{}
	}
	n, ok := c.commCount[group]
	if !ok {
		if cm, err := c.community(group); err == nil {
			n = int(cm.Members)
		}
		c.commCount[group] = n
	}
	return n
}

// reportHides: do members' reports hide this post? And how many owners reported it, with what weight.
func (c *Client) reportHides(cid, post common.Hash, author, group string) (bool, int, float64) {
	if !reportWeights {
		n := c.reportsAgainst(cid, post, author)
		return n >= c.reportThreshold(group), n, float64(n)
	}
	w, n := c.reportWeight(cid, post, author)
	if n < 0 { // a moderator cleared it
		return false, 0, 0
	}
	pol := c.reportsPolicy()
	if author != "" && !strings.HasPrefix(author, "anon:") && c.conductPenalty(ownerName(author)) >= pol.Param("hide_at") {
		return true, n, w // its record speaks: shown only once a moderator clears the post
	}
	return float64(n) >= pol.Param("min_owners") && w >= c.postThreshold(group, author), n, w
}

// postThreshold: the weight that hides this author's post here: the
// community's (its audience) × the author's standing: a post by a name
// whose names moderators banned or hid elsewhere needs less, a citizen's more.
func (c *Client) postThreshold(group, author string) float64 {
	t := c.weightThreshold(c.communityMembers(group))
	if author == "" || strings.HasPrefix(author, "anon:") {
		return t
	}
	o := ownerName(author)
	f := c.reportsPolicy().Num("author_factor", ui.Env{"author.penalty": c.conductPenalty(o), "author.citizen": c.citizenCached(o)})
	return t * f
}

// reportWeight: Σ of the reporters' weights (one per owner), 0 once a moderator dismissed the reports.
func (c *Client) reportWeight(cid, post common.Hash, author string) (float64, int) {
	by, cleared := c.reportersOf(cid, post, author)
	if cleared {
		return 0, -1
	}
	sum := 0.0
	for o := range by {
		sum += c.reporterWeight(cid, o)
	}
	return sum, len(by)
}

// reportersOf: the owners whose reports of this post count in community cid
// (members there, past their probation, not the author's family; one per
// owner), or cleared: a moderator of cid dismissed the reports. author is the
// post's verified signer, never what a report says.
func (c *Client) reportersOf(cid, post common.Hash, author string) (map[string]bool, bool) {
	by := map[string]bool{}
	for _, cl := range c.flags[post] {
		var d struct {
			Community string `json:"community"`
		}
		if json.Unmarshal([]byte(cl.Data), &d) != nil || idOf(d.Community) != cid {
			continue
		}
		switch cl.Kind {
		case "report-dismissed":
			if c.commonsBool("hasRole", cid, idOf(cl.Issuer), uint8(roleMod)) {
				return nil, true
			}
		case "report":
			o := ownerName(cl.Issuer)
			if o == ownerName(author) || by[o] {
				continue
			}
			seasoned := func(n string) bool { return abuseFixesOff || c.commonsBool("seasoned", cid, idOf(n)) }
			if c.commonsBool("isMember", cid, idOf(cl.Issuer)) && seasoned(cl.Issuer) || c.commonsBool("isMember", cid, idOf(o)) && seasoned(o) {
				by[o] = true
			}
		}
	}
	return by, false
}

// reporterWeight: an owner's weight as a reporter in community cid (kept for the read pass).
func (c *Client) reporterWeight(cid common.Hash, owner string) float64 {
	key := cid.Hex() + "|" + owner
	if c.repW == nil || c.repWHead != c.memoHead || c.memoHead == 0 {
		c.repW, c.repWHead = map[string]float64{}, c.memoHead
	}
	if w, ok := c.repW[key]; ok {
		return w
	}
	ageDays := 0.0
	if at := c.registeredAt(owner); at > 0 {
		ageDays = float64(c.chainNow()-at) / 86400
	}
	w := c.reportsPolicy().Num("reporter_weight", ui.Env{"reporter.citizen": c.citizenCached(owner), "reporter.age_days": ageDays,
		"reporter.penalty": c.conductPenalty(owner), "reporter.record": c.reportRecord(cid, owner)})
	c.repW[key] = w
	return w
}

func (c *Client) citizenCached(name string) bool {
	return c.ch.has("Citizens") && c.citizenMemo(name)
}

// humanMemo: human mode's caches, each for one read pass (one head).
type humanMemo struct {
	citHead  uint64               // the pass c.cit was read in
	hideHead uint64               // the pass hides was read in
	hides    map[string][]hideRec // community|ref => moderators' hides of it
}

type hideRec struct {
	action *big.Int
	block  uint64
	by     string // the hider's owner
}

// citizenMemo: is this name a citizen now? Kept for one read pass at one
// head; outside a pass it asks the chain, so a long-lived process (an MCP
// server, a relay) never keeps a citizenship that ended (audit 5 HM-5).
func (c *Client) citizenMemo(name string) bool {
	if c.memoHead == 0 {
		return c.isCitizen(name)
	}
	if c.cit == nil || c.hm.citHead != c.memoHead {
		c.cit, c.hm.citHead = map[string]bool{}, c.memoHead
	}
	if v, ok := c.cit[name]; ok {
		return v
	}
	v := c.isCitizen(name)
	c.cit[name] = v
	return v
}

// conductPenalty: what moderators did to an owner's names, all communities
// (standing bans, hidden names, hidden posts we can check) and posts many
// members reported: one pass over the history per read pass.
func (c *Client) conductPenalty(owner string) float64 {
	if c.conduct == nil || c.conductHead != c.memoHead || c.memoHead == 0 {
		c.conduct, c.conductHead = map[string]float64{}, c.memoHead
		pol := c.reportsPolicy()
		banned := map[string]bool{}
		for _, a := range c.conductActs("", true) {
			o := ownerName(a.name)
			switch {
			case a.kind == 2:
				if k := o + "|" + a.cid.Hex(); !banned[k] {
					banned[k] = true
					c.conduct[o] += pol.Param("ban_penalty")
				}
			case !a.post:
				c.conduct[o] += pol.Param("name_hidden_penalty")
			default:
				c.conduct[o] += pol.Param("post_hidden_penalty")
			}
		}
		reportedAtN := int(pol.Param("reported_at"))
		// posts that many members reported (and no moderator there cleared): what
		// moderators didn't get to, because the reports hid them first. Only posts
		// we hold, by their signer, and only the reports that count there.
		for post := range c.flags {
			it := c.item(post.Hex())
			if it == nil || it.From == "" || strings.HasPrefix(it.From, "anon:") || !strings.HasPrefix(it.To, "group:") {
				continue
			}
			if by, cleared := c.reportersOf(idOf(strings.TrimPrefix(it.To, "group:")), post, it.From); !cleared && len(by) >= reportedAtN {
				c.conduct[ownerName(it.From)] += pol.Param("reported_penalty")
			}
		}
	}
	return c.conduct[owner]
}

// conductAct: a moderator's action that counts against the name it targets.
type conductAct struct {
	kind   uint8 // 2 ban, 3 hide
	cid    common.Hash
	target common.Hash
	name   string // the banned or hidden name, or a hidden post's author
	post   bool   // a hidden post
	known  bool   // a hidden post the reader holds, signed by name
	note   string
}

// conductActs: the moderators' bans and hides (all communities) that count
// against the names they target, for the read-time penalty and the rating
// (ownerConduct). An action counts only if:
//   - the members didn't reverse it, and it still holds (an unban, a reversed hide end it);
//   - its community had at least conduct_min_members voting members when it was
//     taken (anyone founds a community of one and "bans" anyone);
//   - it targets a member there: a ban of a name that was a member, a hide of a
//     name, or of a post by a name, admitted there before the hide;
//   - a hidden post's [author] tag, which the moderator writes, isn't
//     contradicted by the post's signer (known: the reader holds the post and
//     its signer is that author; knownOnly drops the posts we can't check).
//
// owner: "" for every owner, else only the actions about that owner's names.
func (c *Client) conductActs(owner string, knownOnly bool) []conductAct {
	if !c.ch.has("Commons") {
		return nil
	}
	evs, _ := c.ch.events("Commons", "Acted")
	var out []conductAct
	for _, ev := range evs {
		kind := ev.Args["kind"].(uint8)
		if kind != 2 && kind != 3 {
			continue
		}
		a := conductAct{kind: kind, cid: common.Hash(ev.Args["community"].([32]byte)), target: common.Hash(ev.Args["target"].([32]byte)), note: ev.Args["note"].(string)}
		if tn, _ := c.res.nameOf(a.target); tn != "" {
			a.name = tn
		} else if mt := authorTag.FindStringSubmatch(a.note); kind == 3 && mt != nil {
			a.name, a.post = mt[1], true
		} else {
			continue
		}
		// (checked before the calls below: the network's moderators hide thousands of posts)
		if owner != "" && ownerName(a.name) != owner {
			continue
		}
		if a.post {
			if it := c.item(a.target.Hex()); it != nil {
				if ownerName(it.From) != ownerName(a.name) || idOf(strings.TrimPrefix(it.To, "group:")) != a.cid {
					continue // the tag names someone else than the post's signer
				}
				a.known = true
			} else if knownOnly {
				continue
			}
		}
		info, ok := c.actionInfo(ev.Args["action"].(*big.Int))
		if !ok || info.rev || float64(info.electorate) < c.reportsPolicy().Param("conduct_min_members") {
			continue
		}
		if kind == 2 {
			if !info.member || !c.commonsBool("banned", a.cid, a.target) {
				continue
			}
		} else if !c.commonsBool("hidden", a.cid, a.target) || !c.admittedBefore(a.cid, a.name, info.when) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// admittedBefore: was this name (or its owner) admitted to the community at or before t?
func (c *Client) admittedBefore(cid common.Hash, name string, t uint64) bool {
	for _, n := range uniq([]string{name, ownerName(name)}) {
		if out, err := c.ch.call("Commons", "admittedAt", cid, idOf(n)); err == nil {
			if at := out[0].(uint64); at > 0 && at <= t {
				return true
			}
		}
	}
	return false
}

// actionInfo: a Commons action's fixed facts, and whether the members
// reversed it (that part is read again after a minute).
func (c *Client) actionInfo(id *big.Int) (revAt, bool) {
	k := id.String()
	if v, ok := c.ch.reversed.Load(k); ok && time.Since(v.(revAt).at) < time.Minute {
		return v.(revAt), true
	}
	a, err := c.ch.call("Commons", "actions", id)
	if err != nil {
		return revAt{}, false
	}
	r := revAt{rev: a[9].(bool), at: time.Now(), member: a[5].([32]byte) != [32]byte{}, when: a[7].(uint64), electorate: a[11].(uint32)}
	c.ch.reversed.Store(k, r)
	return r, true
}

// actionReversed: did the members reverse this moderator action? (Read again after a minute.)
func (c *Client) actionReversed(id *big.Int) bool {
	r, ok := c.actionInfo(id)
	return ok && r.rev
}

type revAt struct {
	rev        bool
	at         time.Time
	member     bool   // a ban: its target was a member
	when       uint64 // when it was taken
	electorate uint32 // the voting members then
}

// reportRecord: how the owner's reports in this community fared (see the top).
func (c *Client) reportRecord(cid common.Hash, owner string) float64 {
	now, staleAfter := c.chainNow(), int64(c.reportsPolicy().Param("record_stale_hours")*3600)
	upheld, dismissed, stale := 0.0, 0.0, 0.0
	for _, cl := range c.reportsBy[owner] {
		var d struct {
			Community string `json:"community"`
		}
		if json.Unmarshal([]byte(cl.Data), &d) != nil || idOf(d.Community) != cid {
			continue
		}
		switch {
		case cl.Revoked: // revoking a report the moderators dismissed doesn't erase it (audit 5 HM-13)
			if c.dismissedPost(cl.Subject, cid) {
				dismissed++
			}
		case c.commonsBool("hidden", cid, cl.Subject):
			if c.hiddenAfter(cid, cl) {
				upheld++
			}
		case c.dismissedPost(cl.Subject, cid):
			dismissed++
		case now-int64(cl.Time) > staleAfter && c.reporters(cl.Subject, cid) <= 1:
			stale++
		}
	}
	return c.reportsPolicy().Num("record", ui.Env{"record.upheld": upheld, "record.dismissed": dismissed, "record.stale": stale})
}

// hiddenAfter: a moderator other than the reporter hid the post after this
// report: only that upholds it (reporting what is already hidden doesn't farm a
// record, audit 5 HM-3).
func (c *Client) hiddenAfter(cid common.Hash, cl *Claim2) bool {
	if c.hm.hides == nil || c.hm.hideHead != c.memoHead || c.memoHead == 0 {
		c.hm.hides, c.hm.hideHead = map[string][]hideRec{}, c.memoHead
		evs, _ := c.ch.events("Commons", "Acted")
		for _, ev := range evs {
			if ev.Args["kind"].(uint8) != 3 {
				continue
			}
			k := common.Hash(ev.Args["community"].([32]byte)).Hex() + common.Hash(ev.Args["target"].([32]byte)).Hex()
			by, _ := c.res.nameOf(common.Hash(ev.Args["by"].([32]byte)))
			c.hm.hides[k] = append(c.hm.hides[k], hideRec{ev.Args["action"].(*big.Int), ev.Block, ownerName(by)})
		}
	}
	for _, h := range c.hm.hides[cid.Hex()+cl.Subject.Hex()] {
		if cl.Block < h.block && h.by != ownerName(cl.Issuer) && !c.actionReversed(h.action) {
			return true
		}
	}
	return false
}

// dismissedPost: a moderator of cid dismissed the reports of this post there.
func (c *Client) dismissedPost(post, cid common.Hash) bool {
	for _, cl := range c.flags[post] {
		var d struct {
			Community string `json:"community"`
		}
		if cl.Kind == "report-dismissed" && json.Unmarshal([]byte(cl.Data), &d) == nil && idOf(d.Community) == cid &&
			c.commonsBool("hasRole", cid, idOf(cl.Issuer), uint8(roleMod)) {
			return true
		}
	}
	return false
}

// reporters: how many owners reported this post.
func (c *Client) reporters(post, cid common.Hash) int {
	by := map[string]bool{}
	for _, cl := range c.flags[post] {
		if cl.Kind == "report" {
			by[ownerName(cl.Issuer)] = true
		}
	}
	return len(by)
}

// reportNeed: what hides a post here, in words.
func (c *Client) reportNeed(group string) string {
	if !reportWeights {
		return fmt.Sprintf("%d reports", c.reportThreshold(group))
	}
	return fmt.Sprintf("%.1f needed for %d members", c.weightThreshold(c.communityMembers(group)), c.communityMembers(group))
}
