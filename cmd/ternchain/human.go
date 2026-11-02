// Human mode: what a person sees, and who wrote it.
//
//   - Citizens-only communities: members (and voters) are citizens of the
//     network's citizens' house, by their owner names; agents read, they don't
//     post or vote. Readers check citizenship again at read time.
//   - Author labels on every post and letter: "human (citizen)", "agent of X",
//     "unverified owner", "anonymous member".
//   - Look-alike names: a name whose skeleton (confusables folded: Cyrillic а
//     as Latin a, rn as m, 0 as o, …) equals a contact's or a known name's is
//     flagged when it writes, and at registration.
//   - Owner conduct: the bans, hidden posts and reported posts of an owner's
//     names (its agents too) count in its rating.
//   - The feed: posts across the communities you joined, filtered by your trust
//     graph and ranked by a ranking module (tern.ranker/1: the built-in new, hot
//     and trust, or one installed) that says why each post is there.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/confuse"
	"github.com/nenkoru/tern/internal/ui"
)

// ---- citizens and author labels

// citizenNow: is this owner name a citizen of the network's house now (kept for one read pass).
func (c *Client) citizenNow(name string) bool {
	if !c.ch.has("Citizens") || fromAgent(name) || name == "" || strings.Contains(name, ":") {
		return false
	}
	return c.citizenMemo(name)
}

// authorLabel: who wrote it, as a person should read it.
func (c *Client) authorLabel(from string) string {
	switch {
	case from == "":
		return ""
	case strings.HasPrefix(from, "anon:"):
		return "anonymous member"
	case fromAgent(from):
		return "agent of " + ownerName(from)
	case c.citizenNow(from):
		return "human (citizen)"
	}
	return "unverified owner"
}

// citizensHouse: the citizens' house a community admits from (zero: anyone),
// and whether it is this network's own house.
func (c *Client) citizensHouse(group string) (common.Address, bool) {
	if !c.ch.has("Commons") {
		return common.Address{}, false
	}
	out, err := c.ch.call("Commons", "citizensOf", idOf(group))
	if err != nil {
		return common.Address{}, false
	}
	h := out[0].(common.Address)
	return h, h != (common.Address{}) && c.ch.has("Citizens") && h == common.HexToAddress(c.ch.dep.Contracts["Citizens"])
}

// citizensFilter: in a citizens-only community, posts by agents and by names
// that are not citizens now are hidden. Returns true if it hid the post.
func (c *Client) citizensFilter(it *Item, group, from string) bool {
	h, ours := c.citizensHouse(group)
	if h == (common.Address{}) {
		return false
	}
	switch {
	case !ours:
		it.Status, it.Trust = "hidden", group+" names a citizens' house this network doesn't know ("+h.Hex()[:10]+"…): its 'humans only' is not verified"
	case fromAgent(from):
		it.Status, it.Trust = "hidden", "agent of "+ownerName(from)+": "+group+" is citizens only (agents read, they don't post)"
	case !c.citizenNow(from):
		it.Status, it.Trust = "hidden", from+" is not a citizen now: "+group+" is citizens only"
	default:
		return false
	}
	return true
}

// makeCitizensOnly: right after founding, before anyone else joins.
func (c *Client) makeCitizensOnly(group string) error {
	if !c.ch.has("Citizens") {
		return fmt.Errorf("this network has no citizens' house")
	}
	_, err := c.ch.send(c.key, nil, "Commons", "citizensOnly", idOf(group), c.id(), common.HexToAddress(c.ch.dep.Contracts["Citizens"]))
	return err
}

// ---- look-alike names (the tables are data: internal/confuse, extended by
// the reader's lookalike policy module; the warning itself is a floor)

// skeleton: a name folded with the built-in tables so that look-alikes compare
// equal ("oscаr" with a Cyrillic а, "0scar", "o5car" and "oscar" share one).
func skeleton(s string) string { return confuse.Default().Skeleton(s) }

// mixedScriptErr: names mixing writing systems are refused (a look-alike
// trick) unless forced.
func mixedScriptErr(name string, force bool) error {
	return confuse.Default().MixedScriptErr(name, force)
}

func sameOwner(a, b string) bool { return ownerName(a) == ownerName(b) }

// imitates: the known name that `name` looks like (same skeleton, or the
// known name inside it with a "support"-like word), or "" (built-in tables).
func imitates(name string, known []string) string {
	return confuse.Default().Imitates(name, known, sameOwner)
}

// knownNames: contacts, names we wrote to, our family, the groups we are in.
func (c *Client) knownNames() []string {
	var out []string
	for n := range c.st.Contacts {
		out = append(out, n)
	}
	for n := range c.st.Wrote {
		out = append(out, n)
	}
	for n := range c.st.Groups {
		out = append(out, n)
	}
	out = append(out, c.f.Name, c.f.Owner)
	if !c.isOwner() {
		out = append(out, c.ownerContacts()...)
	}
	sort.Strings(out)
	return out
}

// lookalikeWarning: a warning when `from` looks like a known name but isn't it.
func (c *Client) lookalikeWarning(from string) string {
	if from == "" || strings.HasPrefix(from, "anon:") || c.st.Contacts[from] || c.st.Contacts[ownerName(from)] {
		return ""
	}
	if k := c.lookTable().Imitates(from, c.knownNames(), sameOwner); k != "" {
		return fmt.Sprintf("LOOK-ALIKE: %s is not your known %s (the names only look alike); treat it as a stranger", from, k)
	}
	if abuseFixesOff {
		return ""
	}
	if k := c.lookalikeOfRegistered(from); k != "" {
		return fmt.Sprintf("LOOK-ALIKE: %s looks like the existing name %s (not the same name); treat it as a stranger", from, k)
	}
	return ""
}

// lookalikeOfRegistered: an owner name registered before `from`'s that
// `from` looks like (the older name is the one imitated; cached per process).
func (c *Client) lookalikeOfRegistered(from string) string {
	t := c.lookTable()
	skeleton := t.Skeleton
	if c.regSkel == nil {
		c.regSkel, c.regAt = map[string][]string{}, map[string]uint64{}
		if evs, err := c.ch.events("Registry", "OwnerRegistered"); err == nil {
			for _, e := range evs {
				n := e.Args["name"].(string)
				c.regSkel[skeleton(n)] = append(c.regSkel[skeleton(n)], n)
				c.regAt[n] = e.Block
			}
		}
	}
	o := ownerName(from)
	at, ok := c.regAt[o]
	if !ok {
		at = ^uint64(0) // registered after we looked: newer than all we know
	}
	// the older name is the one imitated, unless `from` is a citizen and it isn't
	// (a "b0ris" registered first doesn't make the real boris the copy)
	older := func(n string) bool {
		return n != o && c.regAt[n] < at && !(c.citizenNow(o) && !c.citizenNow(n))
	}
	sk := skeleton(o)
	for _, n := range c.regSkel[sk] {
		if older(n) {
			return n
		}
	}
	if m := t.SupportWord(sk); m != "" { // "anna-support": a known name plus a support-like word
		base := strings.Trim(strings.Replace(sk, m, "", 1), "-")
		for _, n := range c.regSkel[base] {
			if older(n) && len(base) >= 3 {
				return n
			}
		}
	}
	// "oscar-support-desk", "official-oscar-team", "oscar-pay", "oscar2": a
	// name plus generic words or digits
	var keep []string
	for _, w := range strings.FieldsFunc(o, func(r rune) bool { return r == '-' || r == '_' }) {
		if w := strings.TrimRight(w, "0123456789"); w != "" && !t.Generic(w) {
			keep = append(keep, w)
		}
	}
	if base := skeleton(strings.Join(keep, "-")); base != sk && len(base) >= 3 {
		for _, n := range c.regSkel[base] {
			if older(n) {
				return n
			}
		}
	}
	return ""
}

// registeredLookalike: an existing name on chain that `name` looks like.
func (c *Client) registeredLookalike(name string) string {
	var known []string
	for _, ev := range []string{"OwnerRegistered", "AgentGranted"} {
		evs, err := c.ch.events("Registry", ev)
		if err != nil {
			continue
		}
		for _, e := range evs {
			known = append(known, e.Args["name"].(string))
		}
	}
	return c.lookTable().Imitates(name, known, sameOwner)
}

// ---- owner accountability

var authorTag = regexp.MustCompile(`\[author ([a-z0-9.\-]+)\]`)

// ownerConduct: the bans, hidden posts and reported posts of an owner's names
// (its agents included), from the chain. A ban counts while it stands, in a
// community of at least conduct_min_members (the reports module: 3) voting members; a reversed action
// never counts.
func (c *Client) ownerConduct(p *ratingPool, name string) map[string]any {
	owner := ownerName(name)
	pol := c.policyOf("reports")
	reportedAtN := int(pol.Param("reported_at")) // the same threshold conductPenalty applies (reports.go), not the hide count
	m := map[string]any{"formula": fmt.Sprintf("penalty = min(100, %g × communities banning one of its names + %g × names hidden + %g × posts hidden + %g × posts reported by ≥%d members); score_with_conduct = clamp(score − penalty)",
		pol.Param("ban_penalty"), pol.Param("name_hidden_penalty"), pol.Param("post_hidden_penalty"), pol.Param("reported_penalty"), reportedAtN)}
	if !c.ch.has("Commons") {
		return m
	}
	bannedIn := map[string]bool{}
	var bans []map[string]any
	agents := map[string]bool{}
	hiddenNames, hiddenPosts := 0, 0
	for _, a := range c.conductActs(owner, false) { // standing, in communities of conduct_min_members, against members
		cn := c.nameOrHex(a.cid)
		if fromAgent(a.name) {
			agents[a.name] = true
		}
		switch {
		case a.kind == 2:
			bannedIn[cn] = true
			bans = append(bans, map[string]any{"community": cn, "name": a.name, "reason": c.outside(cn, a.note)})
		case !a.post:
			hiddenNames++
		default:
			hiddenPosts++
		}
	}
	// Reported posts: members' reports naming one of its names as the author,
	// by at least reported_at owners (the reports module), and not dismissed by a moderator of the
	// community the reports name (audit 5 HM-12); a post we hold must be signed
	// by that author.
	type rep struct {
		by        map[string]bool
		dismissed bool
		author    string
	}
	reps := map[string]*rep{} // post|community
	if p != nil {
		for _, cl := range p.claims {
			if cl.Kind != "report" && cl.Kind != "report-dismissed" {
				continue
			}
			var d struct{ Community, Author string }
			if json.Unmarshal([]byte(cl.Data), &d) != nil {
				continue
			}
			k := cl.Subject.Hex() + "|" + d.Community
			r := reps[k]
			if r == nil {
				r = &rep{by: map[string]bool{}}
				reps[k] = r
			}
			if cl.Kind == "report-dismissed" {
				r.dismissed = r.dismissed || c.commonsRole(idOf(d.Community), idOf(cl.Issuer), uint8(roleMod))
				continue
			}
			if d.Author == "" || ownerName(d.Author) != owner || ownerName(cl.Issuer) == owner || !p.ok(cl) {
				continue
			}
			if it := c.item(cl.Subject.Hex()); it != nil && ownerName(it.From) != owner {
				continue // the post's signer is someone else
			}
			r.author = d.Author
			r.by[ownerName(cl.Issuer)] = true
		}
	}
	reported := 0
	for _, r := range reps {
		if r.author != "" && !r.dismissed && len(r.by) >= reportedAtN {
			reported++
			if fromAgent(r.author) {
				agents[r.author] = true
			}
		}
	}
	pen := math.Min(100, pol.Param("ban_penalty")*float64(len(bannedIn))+pol.Param("name_hidden_penalty")*float64(hiddenNames)+
		pol.Param("post_hidden_penalty")*float64(hiddenPosts)+pol.Param("reported_penalty")*float64(reported))
	var in, ag []string
	for k := range bannedIn {
		in = append(in, k)
	}
	for k := range agents {
		ag = append(ag, k)
	}
	sort.Strings(in)
	sort.Strings(ag)
	if len(bans) > 10 {
		bans = bans[:10]
	}
	m["owner"], m["banned_in"], m["bans"], m["names_hidden"], m["posts_hidden"], m["posts_reported"], m["agents_involved"], m["penalty"] =
		owner, in, bans, hiddenNames, hiddenPosts, reported, ag, pen
	return m
}

// ---- the feed

type feedOpts struct {
	community string
	depth     int    // 0: everyone; N: authors within N steps of your trust graph
	ranking   string // hot | new | trust
	limit     int
}

// feedPost: a post the feed command ranks.
type feedPost struct {
	it        *Item
	community string
	owner     string
	depth     int // -1: outside the trust graph (or no graph asked for)
	replies   int
	ageH      float64
	citizen   bool
	agent     bool
}

// feedRanker: the feed command's ranking module: a built-in one by name
// (new, hot, trust: internal/ui/builtin/ranker-*.json) or an installed
// tern.ranker/1 line (NAME@N), the same modules 'ui feed' runs.
func (c *Client) feedRanker(name string) (*ui.Ranker, error) {
	if !strings.Contains(name, "@") {
		if r := ui.BuiltinRanker(name); r != nil {
			return r, nil
		}
		return nil, fmt.Errorf("ranking %q: %s, or an installed ranking module's line (NAME@1)", name, strings.Join(ui.BuiltinRankers(), ", "))
	}
	return c.uiRanker(name)
}

// trustGraph: owner names within depth steps of us: our family (0), our
// contacts and the owner's signed correspondents (1), then public edges: vouches
// between citizens and paid deals between owners: a Bank lock that paid out at
// least trustDealMin and that its payee accepted with a review citing it (a
// payer alone can't make one: audit 5 HM-6).
func (c *Client) trustGraph(depth int) map[string]int {
	d := map[string]int{ownerName(c.f.Name): 0}
	var frontier []string
	add := func(n string, k int) {
		n = ownerName(n)
		if _, ok := d[n]; !ok && n != "" {
			d[n] = k
			frontier = append(frontier, n)
		}
	}
	for n := range c.st.Contacts {
		add(n, 1)
	}
	if !c.isOwner() {
		for _, n := range c.ownerContacts() {
			add(n, 1)
		}
	}
	if depth <= 1 {
		return d
	}
	// public edges, read once
	edges := map[string][]string{}
	link := func(a, b string) {
		a, b = ownerName(a), ownerName(b)
		if a != b && a != "" && b != "" {
			edges[a] = append(edges[a], b)
			edges[b] = append(edges[b], a)
		}
	}
	if c.ch.has("Citizens") {
		if evs, err := c.ch.events("Citizens", "Vouched"); err == nil {
			for _, e := range evs {
				by, _ := e.Args["by"].([32]byte)
				who, _ := e.Args["who"].([32]byte)
				link(c.nameOr(common.Hash(by)), c.nameOr(common.Hash(who)))
			}
		}
	}
	accepted := map[uint64][]string{} // lock => the owners whose reviews cite it
	if cls, err := c.easClaimsOf(nil, map[string]bool{"review": true}); err == nil {
		for _, cl := range cls {
			var rd ReviewData
			if !cl.Revoked && cl.Issuer != "" && json.Unmarshal([]byte(cl.Data), &rd) == nil && rd.Lock != nil {
				accepted[*rd.Lock] = append(accepted[*rd.Lock], ownerName(cl.Issuer))
			}
		}
	}
	if evs, err := c.ch.events("Bank", "Locked"); err == nil {
		for _, e := range evs {
			id := e.Args["id"].(*big.Int).Uint64()
			if len(accepted[id]) == 0 {
				continue
			}
			if l, err := c.dealLock(id); err == nil && l.Paid.Cmp(trustDealMin) >= 0 {
				for _, o := range accepted[id] {
					if l.payeeSide[o] {
						link(l.Payer, l.Payee)
						break
					}
				}
			}
		}
	}
	for k := 2; k <= depth; k++ {
		cur := frontier
		frontier = nil
		for _, n := range cur {
			for _, m := range edges[n] {
				add(m, k)
			}
		}
	}
	return d
}

// feedRankDepth: how far the feed looks into the trust graph to rank (not
// filter) when --trust-depth isn't given.
const feedRankDepth = 3

// trustDealMin: the least a paid deal moves to be an edge of the trust graph.
var trustDealMin = new(big.Int).Div(coinUnit, big.NewInt(10)) // 0.1 coin

// feed: posts across the communities we joined, filtered and ranked.
func (c *Client) feed(o feedOpts) (map[string]any, error) {
	if o.ranking == "" {
		o.ranking = "trust"
	}
	rk, err := c.feedRanker(o.ranking)
	if err != nil {
		return nil, err
	}
	if _, err := c.scan(); err != nil {
		return nil, err
	}
	defer c.readPass()() // one head for the citizenship and Commons answers below
	var groups []string
	for g := range c.st.Groups {
		if (o.community == "" || g == o.community) && c.isCommunity(g) {
			groups = append(groups, g)
		}
	}
	if o.community != "" && len(groups) == 0 {
		return nil, fmt.Errorf("you are not in community %s ('commons request %s')", o.community, o.community)
	}
	sort.Strings(groups)
	in := map[string]bool{}
	for _, g := range groups {
		c.refreshCommunity(g)
		in["group:"+g] = true
	}
	// --trust-depth N filters to authors within N steps. Without it, a ranking
	// that reads trust distance still gets it (to feedRankDepth steps), so the
	// trust ranking ranks by trust by default instead of seeing every author as
	// "outside" (it filters nothing).
	var graph map[string]int
	filterByTrust := o.depth > 0
	if filterByTrust {
		graph = c.trustGraph(o.depth)
	} else if slices.Contains(rk.M.Inputs, "author.trust_distance") {
		graph = c.trustGraph(feedRankDepth)
	}
	// replies: distinct owners other than the post's author (audit 5 HM-7)
	repliers := map[string]map[string]bool{}
	for _, it := range c.st.Items {
		if in[it.To] && it.Mail != nil && it.Status == "delivered" && it.Mail.InReplyTo != "" {
			if p := c.item(it.Mail.InReplyTo); p != nil && ownerName(p.From) == ownerName(it.From) {
				continue
			}
			if repliers[it.Mail.InReplyTo] == nil {
				repliers[it.Mail.InReplyTo] = map[string]bool{}
			}
			repliers[it.Mail.InReplyTo][ownerName(it.From)] = true
		}
	}
	replies := map[string]int{}
	for id, by := range repliers {
		replies[id] = len(by)
	}
	now := c.chainNow()
	hidden := map[string]int{}
	outside := 0
	var posts []feedPost
	for _, it := range c.st.Items {
		if !in[it.To] || it.Mail == nil {
			continue
		}
		if it.Status != "delivered" {
			reason := it.Trust
			if i := strings.Index(reason, ":"); i > 0 {
				reason = reason[:i]
			}
			hidden[reason]++
			continue
		}
		p := feedPost{it: it, community: strings.TrimPrefix(it.To, "group:"), owner: ownerName(it.From), depth: -1, replies: replies[it.ID],
			ageH: math.Max(0, float64(now-it.Time)/3600), agent: fromAgent(it.From), citizen: c.citizenNow(it.From)}
		if graph != nil {
			if k, ok := graph[p.owner]; ok {
				p.depth = k
			} else if filterByTrust {
				outside++
				continue
			}
		}
		posts = append(posts, p)
	}
	// the ranking module's inputs (docs/FEED.md §4): what the feed command knows of
	// a post; reactions are counted by 'ui feed' (a module that reads them runs there)
	byID := map[string]feedPost{}
	perHour := map[string][]int64{}
	for _, p := range posts {
		perHour[p.community+"|"+p.owner] = append(perHour[p.community+"|"+p.owner], p.it.Time)
	}
	var cands []ui.Candidate
	for _, p := range posts {
		byID[p.it.ID] = p
		dist := 9.0 // outside the trust graph, or none asked for
		if p.depth >= 0 {
			dist = float64(min(p.depth, 8))
		}
		n := 0
		for _, t := range perHour[p.community+"|"+p.owner] {
			if math.Abs(float64(t-p.it.Time)) < 3600 {
				n++
			}
		}
		ageDays := 0.0
		if t := c.registeredAt(p.it.From); t > 0 {
			ageDays = float64(now-t) / 86400
		}
		cands = append(cands, ui.Candidate{ID: p.it.ID, Inputs: ui.Env{
			"item.age_hours": p.ageH, "item.replies": float64(p.replies), "item.hidden": false,
			"item.reports":          ui.Once(func() any { return float64(c.reporters(common.HexToHash(p.it.ID), idOf(p.community))) }),
			"item.has_card":         ui.Once(func() any { pp, ok, _ := ui.ParsePost(p.it.Mail.Data); return ok && len(pp.Card) > 0 }),
			"author.trust_distance": dist, "author.agent": p.agent, "author.citizen": p.citizen, "author.age_days": ageDays,
			"author.posts_hour": float64(n)}})
	}
	ranked, _, err := rk.Rank(cands)
	if err != nil {
		return nil, fmt.Errorf("%w (the feed command doesn't count reactions: run this module with 'ui feed COMMUNITY --name %s')", err, rk.M.Line)
	}
	if o.limit <= 0 {
		o.limit = 30
	}
	items := []map[string]any{}
	for i, x := range ranked {
		if i == o.limit {
			break
		}
		p := byID[x.ID]
		v := map[string]any{"id": p.it.ID[:18], "community": p.community, "from": p.it.From, "author": c.authorLabel(p.it.From),
			"text": c.outside(p.it.From, p.it.Mail.Text), "score": math.Round(x.Score*1000) / 1000, "why": x.Reason}
		if w := c.lookalikeWarning(p.it.From); w != "" {
			v["warning"] = w
		}
		items = append(items, v)
	}
	formula := rk.M.Description
	if formula == "" {
		formula = rk.M.Score
	}
	m := map[string]any{"ranking": o.ranking, "module": fmt.Sprintf("%s %s by %s", rk.M.Line, rk.M.Version, rk.M.Author), "formula": formula,
		"communities": groups, "items": items, "shown": len(items), "candidates": len(posts)}
	if len(hidden) > 0 {
		m["hidden"] = hidden
	}
	if graph != nil {
		m["trust_depth"], m["outside_trust_depth"], m["trust_graph_size"] = o.depth, outside, len(graph)
	}
	c.save()
	return m, nil
}
