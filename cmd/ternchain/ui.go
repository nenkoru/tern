// Your own Reddit with a chat client (cmd/ternchain/testdata/reddit/, docs/FEED.md, internal/ui):
// the open feed in a community's channel, reactions batched through data
// nodes and counted by readers, ranking modules and UI packages as signed
// artifacts (a claim on chain names each one's manifest hash), forks that
// pay their parents through a split, and a local chat-first web client
// whose buttons go through this agent's guard.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/ui"
)

type uiFlags struct {
	listen    string
	listenSet bool
	ownerDir  string
	community string
	share     string
	llm       string
	name      string
	out       string
	parentBps int
	trust     string
	title     string
	card      string
	thread    string
	reply     string
}

const claimPackage = "tern.package" // an artifact's manifest hash on chain: subject = the hash, data = what it is and where

func (c *Client) uiStore() ui.Store { return ui.Store{Dir: filepath.Join(c.dir, "ui", "store")} }

// uiPrefs: the owner's choices for this agent's chat client.
type uiPrefs struct {
	Community string `json:"community"`
	Package   string `json:"package"` // the UI package line in use ("" = the device's base templates)
	Ranker    string `json:"ranker"`  // the ranking module line in use ("" = newest first)
	TipSmall  string `json:"tip_small"`
	TipBig    string `json:"tip_big"`
	MaxTip    string `json:"max_tip"`
}

func (c *Client) uiPrefs() uiPrefs {
	p := uiPrefs{TipSmall: "0.05", TipBig: "0.5", MaxTip: "2"}
	readJSON(filepath.Join(c.dir, "ui", "prefs.json"), &p)
	return p
}

func (c *Client) saveUIPrefs(p uiPrefs) error {
	os.MkdirAll(filepath.Join(c.dir, "ui"), 0o700)
	b, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(filepath.Join(c.dir, "ui", "prefs.json"), b, 0o600)
}

func runUI(c *Client, cmd string, pos []string, f uiFlags) error {
	need := func(n int, usage string) error {
		if len(pos) < n {
			return fmt.Errorf("usage: ternchain %s", usage)
		}
		return nil
	}
	switch cmd {
	case "ui post":
		if err := need(2, "ui post COMMUNITY TEXT [--title T] [--reply POST] [--card JSON]"); err != nil {
			return err
		}
		id, err := c.uiPost(pos[0], strings.Join(pos[1:], " "), f.title, f.reply, f.card)
		if err != nil {
			return err
		}
		out(map[string]any{"post": id, "community": pos[0], "schema": ui.SchemaFeed})
	case "ui react":
		if err := need(2, "ui react COMMUNITY POST:up|down|none [POST:… …]   (one batch: one envelope)"); err != nil {
			return err
		}
		var rs []ui.Reaction
		for _, p := range pos[1:] {
			post, kind, ok := strings.Cut(p, ":")
			if !ok {
				return fmt.Errorf("%q: POST:up, POST:down or POST:none", p)
			}
			id, err := c.uiResolvePost(pos[0], post)
			if err != nil {
				return err
			}
			rs = append(rs, ui.Reaction{Post: id, Kind: kind})
		}
		env, err := c.uiReact(pos[0], rs)
		if err != nil {
			return err
		}
		out(map[string]any{"envelope": env, "reactions": len(rs), "community": pos[0], "via": firstNonEmpty(c.relay, "chain (in full)"),
			"note": "one datagram for the whole batch; through a data node only the batch's Merkle root goes on chain"})
	case "ui card":
		if err := need(2, "ui card TO CARD-JSON   (a typed card: invitation, offer, match, job, deal, news, vote — docs/FEED.md)"); err != nil {
			return err
		}
		id, err := c.uiSendCard(pos[0], pos[1])
		if err != nil {
			return err
		}
		out(map[string]any{"card": id, "to": pos[0]})
	case "ui feed":
		if err := need(1, "ui feed COMMUNITY [--name RANKER-LINE]"); err != nil {
			return err
		}
		line := f.name
		if line == "" {
			line = c.uiPrefs().Ranker
		}
		rk, err := c.uiRanker(line)
		if err != nil {
			return err
		}
		v, err := c.uiFeed(pos[0], rk)
		if err != nil {
			return err
		}
		out(v.report())
	case "ui publish":
		if err := need(1, "ui publish DIR [--share FOLDER]"); err != nil {
			return err
		}
		r, err := c.uiPublish(pos[0], f.share)
		if err != nil {
			return err
		}
		out(r)
	case "ui list":
		cls, err := c.uiPublished()
		if err != nil {
			return err
		}
		var refs []ui.Ref
		var pub []map[string]any
		for _, cl := range cls {
			refs = append(refs, ui.Ref{Kind: cl.Schema, Name: cl.Name, Version: cl.Version, Line: cl.Line, Hash: cl.Hash, Author: cl.Author, URI: cl.URI})
			pub = append(pub, map[string]any{"kind": cl.Schema, "name": cl.Name, "version": cl.Version, "author": cl.Author, "hash": cl.Hash, "uri": cl.URI})
		}
		out(map[string]any{"published": pub, "installed": c.uiStore().Pins(), "updates": c.uiStore().Updates(refs), "in_use": c.uiPrefs()})
	case "ui install":
		if err := need(1, "ui install NAME[@VERSION]|HASH   (checks the signature and the author's key on chain; pins it; no auto-update)"); err != nil {
			return err
		}
		pin, err := c.uiInstall(pos[0])
		if err != nil {
			return err
		}
		out(map[string]any{"installed": pin, "note": "pinned to this version: newer ones are shown by 'ui list', never applied by themselves"})
	case "ui use":
		if err := need(1, "ui use LINE   (a UI package or ranking module line you installed, e.g. reddit-cards@1; 'base' or 'new' for the built-ins)"); err != nil {
			return err
		}
		r, err := c.uiUse(pos[0], f.community)
		if err != nil {
			return err
		}
		out(r)
	case "ui fork":
		if err := need(1, "ui fork NAME[@VERSION] --name NEW --out DIR [--parent-bps 1000]"); err != nil {
			return err
		}
		if f.name == "" || f.out == "" {
			return errors.New("ui fork needs --name NEW and --out DIR")
		}
		b, err := c.uiFetch(pos[0])
		if err != nil {
			return err
		}
		if err := ui.Fork(b, f.out, f.name, f.parentBps); err != nil {
			return err
		}
		out(map[string]any{"fork": f.name + "@1", "dir": f.out, "parent": b.Header.Name + " " + b.Header.Version, "parent_bps": f.parentBps,
			"next": "edit " + f.out + "/templates/…, then 'ui publish " + f.out + "': the payee becomes a split giving the parent its share"})
	case "ui mark":
		m, err := c.uiMark(firstOr(pos, 0, ""), firstOr(pos, 1, ""))
		if err != nil {
			return err
		}
		out(map[string]any{"mark": m, "note": "the secret mark your approval sheet shows: a sheet without it is fake. Kept in your folder, never shown to the agent or a screen"})
	case "ui serve":
		return c.uiServe(f)
	default:
		return unknownCommand(cmd)
	}
	return nil
}

// ---- the feed: posts, reactions, cards

// uiPost: a group post carrying tern.schema/feed@1 data.
func (c *Client) uiPost(community, text, title, reply, card string) (string, error) {
	p := ui.Post{Schema: ui.SchemaFeed, Kind: "post", Title: title}
	if reply != "" {
		id, err := c.uiResolvePost(community, reply)
		if err != nil {
			return "", err
		}
		p.Kind, p.ReplyTo, p.Thread = "comment", id, id
		if it := c.item(id); it != nil && it.Mail != nil {
			if rp, ok, _ := ui.ParsePost(it.Mail.Data); ok && rp.Thread != "" {
				p.Thread = rp.Thread
			}
		}
	}
	for _, w := range strings.Fields(text) { // mentions: @name
		if n, ok := strings.CutPrefix(strings.TrimRight(w, ".,:;!?"), "@"); ok && n != "" {
			p.Mentions = append(p.Mentions, strings.ToLower(n))
		}
	}
	if card != "" {
		p.Card = json.RawMessage(card)
	}
	data, _ := json.Marshal(p)
	if _, _, err := ui.ParsePost(data); err != nil {
		return "", err
	}
	return c.postGroupReply(community, text, data, p.ReplyTo)
}

// uiResolvePost: a post id or a prefix of one, among the community's posts.
func (c *Client) uiResolvePost(community, s string) (string, error) {
	if len(s) == 66 && strings.HasPrefix(s, "0x") {
		return strings.ToLower(s), nil
	}
	c.scan()
	var hits []string
	for _, it := range c.groupItems(community) {
		if it.Mail != nil && strings.HasPrefix(it.ID, s) {
			hits = append(hits, it.ID)
		}
	}
	if len(hits) != 1 {
		return "", fmt.Errorf("post %q: %d matches in %s (give more of its id)", s, len(hits), community)
	}
	return hits[0], nil
}

// uiReact: one batch of reactions, one datagram to the community.
func (c *Client) uiReact(community string, rs []ui.Reaction) (string, error) {
	if c.st.Groups[community] == nil {
		return "", fmt.Errorf("you are not in %s", community)
	}
	now := c.chainNow()
	for i := range rs {
		if rs[i].At == 0 {
			rs[i].At = now
		}
	}
	c.loadKinds() // reactions the schema registry declares (kinds.go)
	b := ui.ReactionBatch{Schema: ui.SchemaReact, Community: community, Reactions: rs}
	raw, _ := json.Marshal(b)
	if _, err := ui.ParseReactionBatch(raw); err != nil {
		return "", err
	}
	if err := c.guardSend("group:"+community, "inform"); err != nil {
		return "", err
	}
	return c.sendDatagram(ui.TypeReact, "group:"+community, b)
}

// uiSendCard: a typed card to a name.
func (c *Client) uiSendCard(to, raw string) (string, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", fmt.Errorf("card: %w", err)
	}
	m["v"], m["from"], m["to"], m["created"] = 1, c.f.Name, to, c.chainNow()
	if _, ok := m["id"]; !ok {
		m["id"] = "c_" + randHex(8)
	}
	b, _ := json.Marshal(m)
	c.loadKinds() // kinds the network's schema registry declares (kinds.go)
	card, err := ui.ParseCard(b)
	if err != nil {
		return "", err
	}
	if err := c.guardSend(to, "inform"); err != nil {
		return "", err
	}
	if _, err := c.sendDatagram(ui.TypeCard, to, card); err != nil {
		return "", err
	}
	return card.ID, nil
}

// feedView: one community, read and ranked on this device.
type feedView struct {
	Community string
	Ranker    *ui.Ranker
	Posts     []*Item // top-level posts, in rank order
	Ranked    map[string]ui.Ranked
	ByID      map[string]*Item // every post and comment
	Comments  map[string][]*Item
	Counts    map[string]*ui.Count
	Stats     ui.TallyStats
	Inputs    map[string]map[string]any
	Dropped   int
	Hidden    int
	BadData   map[string]string // post => why its data was ignored
	Graph     map[string]int
	Now       int64
}

func (v *feedView) rankerName() string {
	if v.Ranker == nil {
		return "newest first (built in)"
	}
	return fmt.Sprintf("%s %s by %s", v.Ranker.M.Name, v.Ranker.M.Version, v.Ranker.M.Author)
}

func (v *feedView) summary() string {
	return fmt.Sprintf("%d posts · %d reactions by %d people from %d batches (%d data-node roots on chain, %d in full) · ranked on this device by %s",
		len(v.Posts), v.Stats.Counted, v.Stats.Reactors, v.Stats.Batches, len(v.Stats.Roots), v.Stats.OnChain, v.rankerName())
}

// newestFirst: the built-in order when no ranking module is chosen
// (internal/ui/builtin/ranker-new.json; hot@1 and trust@1 are built in too).
var newestFirst = ui.BuiltinRanker("new")

// uiRanker: the installed ranking module of that line ("" or "new": newest
// first); a built-in module (new@1, hot@1, trust@1) unless one of that line is installed.
func (c *Client) uiRanker(line string) (*ui.Ranker, error) {
	if line == "" || line == "new" || line == "new@1" {
		return newestFirst, nil
	}
	if !strings.Contains(line, "@") {
		line += "@1"
	}
	b, err := c.uiStore().Load(line)
	if err != nil && strings.HasSuffix(line, "@1") {
		if r := ui.BuiltinRanker(strings.TrimSuffix(line, "@1")); r != nil {
			return r, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ranking module %s: %w ('ui install %s')", line, err, strings.TrimSuffix(line, "@1"))
	}
	r, err := ui.CompileRanker(b.Raw)
	if err != nil {
		return nil, err
	}
	r.Hash = b.Hash
	return r, nil
}

// uiFeed reads the community's channel: posts with their feed data, the
// reaction batches (counted here, one per owner), comments, the moderators'
// and members' hides; then runs the ranking module over it.
func (c *Client) uiFeed(community string, rk *ui.Ranker) (*feedView, error) {
	if _, err := c.scan(); err != nil {
		return nil, err
	}
	if c.st.Groups[community] == nil {
		return nil, fmt.Errorf("you are not in %s ('commons request %s', then a moderator admits you)", community, community)
	}
	defer c.readPass()()
	c.refreshCommunity(community)
	c.loadKinds()
	if rk == nil {
		rk = newestFirst
	}
	v := &feedView{Community: community, Ranker: rk, ByID: map[string]*Item{}, Comments: map[string][]*Item{}, Ranked: map[string]ui.Ranked{},
		Inputs: map[string]map[string]any{}, BadData: map[string]string{}, Now: c.chainNow()}
	v.Graph = c.trustGraph(2)
	var batches []ui.SignedBatch
	var tops []*Item
	parsed := map[string]ui.Post{}
	for _, it := range c.groupItems(community) {
		switch {
		case it.Type == ui.TypeReact:
			b, err := ui.ParseReactionBatch(it.Body)
			if err != nil {
				continue
			}
			batches = append(batches, ui.SignedBatch{By: it.From, Owner: ownerName(it.From), Envelope: it.ID, Root: it.Batch, Time: it.Time,
				Member: it.Status == "delivered", Batch: b})
		case it.Type == tMessage && it.Mail != nil && it.Status != "refused":
			v.ByID[it.ID] = it
			p, ok, err := ui.ParsePost(it.Mail.Data)
			if err != nil {
				v.BadData[it.ID] = "its data is not a valid feed post (ignored): " + err.Error()
				ok = false
			}
			if ok {
				parsed[it.ID] = p
			}
			reply := it.Mail.InReplyTo
			if ok && p.ReplyTo != "" {
				reply = p.ReplyTo
			}
			if reply != "" {
				root := reply
				if ok && p.Thread != "" {
					root = p.Thread
				}
				v.Comments[root] = append(v.Comments[root], it)
				continue
			}
			tops = append(tops, it)
		}
	}
	v.Counts, v.Stats = ui.Tally(community, batches)
	cid := idOf(community)
	var cands []ui.Candidate
	for _, it := range tops {
		cnt := v.Counts[it.ID]
		if cnt == nil {
			cnt = &ui.Count{}
		}
		circleUp, circleDown := 0, 0
		for _, o := range cnt.UpBy {
			if d, ok := v.Graph[o]; ok && d <= 1 {
				circleUp++
			}
		}
		for _, o := range cnt.DownBy {
			if d, ok := v.Graph[o]; ok && d <= 1 {
				circleDown++
			}
		}
		repliers := map[string]bool{}
		for _, cm := range v.Comments[it.ID] {
			if cm.Status == "delivered" && ownerName(cm.From) != ownerName(it.From) {
				repliers[ownerName(cm.From)] = true
			}
		}
		dist := 9
		if d, ok := v.Graph[ownerName(it.From)]; ok {
			dist = d
		}
		perHour := 0
		for _, o := range tops {
			if ownerName(o.From) == ownerName(it.From) && math.Abs(float64(o.Time-it.Time)) < 3600 {
				perHour++
			}
		}
		ageDays := 0.0
		if t := c.registeredAt(it.From); t > 0 {
			ageDays = float64(v.Now-t) / 86400
		}
		if it.Status == "hidden" {
			v.Hidden++
		}
		// the ranking module's inputs (docs/FEED.md §4): it gets only those it declares
		in := map[string]any{}
		in["item.age_hours"] = math.Max(0, float64(v.Now-it.Time)/3600)
		in["item.votes_up"] = float64(cnt.Up)
		in["item.votes_down"] = float64(cnt.Down)
		in["item.circle_up"] = float64(circleUp)
		in["item.circle_down"] = float64(circleDown)
		in["item.replies"] = float64(len(repliers))
		in["item.hidden"] = it.Status == "hidden"
		in["item.reports"] = float64(c.reporters(common.HexToHash(it.ID), cid))
		in["item.has_card"] = len(parsed[it.ID].Card) > 0
		in["author.trust_distance"] = float64(dist)
		in["author.agent"] = fromAgent(it.From)
		in["author.citizen"] = c.citizenNow(it.From)
		in["author.age_days"] = ageDays
		in["author.posts_hour"] = float64(perHour)
		v.Inputs[it.ID] = in
		cands = append(cands, ui.Candidate{ID: it.ID, Inputs: in})
	}
	ranked, dropped, err := rk.Rank(cands)
	if err != nil {
		return nil, err
	}
	v.Dropped = dropped
	for _, r := range ranked {
		v.Ranked[r.ID] = r
		v.Posts = append(v.Posts, v.ByID[r.ID])
	}
	for _, cs := range v.Comments {
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].Time < cs[j].Time })
	}
	return v, nil
}

// report: the feed as JSON for the CLI.
func (v *feedView) report() map[string]any {
	var items []map[string]any
	for _, it := range v.Posts {
		cnt := v.Counts[it.ID]
		if cnt == nil {
			cnt = &ui.Count{}
		}
		post, _, _ := ui.ParsePost(it.Mail.Data)
		item := map[string]any{"id": it.ID, "from": it.From, "title": post.Title, "text": clipText(it.Mail.Text, 120), "up": cnt.Up, "down": cnt.Down,
			"score": v.Ranked[it.ID].Score, "why": v.Ranked[it.ID].Reason, "status": it.Status, "batch": it.Batch}
		if len(cnt.Other) > 0 { // reactions the schema registry declared (kinds.go)
			item["reactions"] = cnt.Other
		}
		items = append(items, item)
	}
	return map[string]any{"community": v.Community, "ranker": v.rankerName(), "items": items, "filtered_out": v.Dropped,
		"reactions": map[string]any{"batches": v.Stats.Batches, "roots_on_chain": v.Stats.Roots, "in_full_on_chain": v.Stats.OnChain,
			"counted": v.Stats.Counted, "ignored": v.Stats.Ignored, "reactors": v.Stats.Reactors}, "summary": v.summary()}
}

func clipText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return string(r)
}

// ---- signed artifacts on chain: publish, list, fetch, install, use

type uiClaim struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Line    string `json:"line"`
	Author  string `json:"author"`
	URI     string `json:"uri"`
	Hash    string `json:"hash"`
	Parent  string `json:"parent,omitempty"`
	Block   uint64 `json:"block"`
}

// uiPublish: seal (sign) a bundle, copy it where readers fetch it, and put
// its manifest hash on chain as a claim by the author's name. A fork's payee
// becomes a split that gives the parent's payee its share.
func (c *Client) uiPublish(dir, share string) (map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	res := map[string]any{}
	if par, ok := m["parent"].(map[string]any); ok {
		bps := 0
		if r, ok := m["royalty"].(map[string]any); ok {
			f, _ := r["parent_bps"].(float64)
			bps = int(f)
		}
		ppayee, _ := par["payee"].(string)
		if bps > 0 && bps < 10000 && ppayee != "" {
			split, err := c.createSplitOf([]string{c.f.Name, ppayee}, []uint16{uint16(10000 - bps), uint16(bps)})
			if err != nil {
				return nil, fmt.Errorf("the fork's royalty split: %w", err)
			}
			m["payee"] = split.Hex()
			res["split"] = map[string]any{"id": split.Hex(), c.f.Name: 10000 - bps, ppayee: bps}
			b, _ := json.MarshalIndent(m, "", "  ")
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
				return nil, err
			}
		}
	}
	b, err := ui.Seal(dir, c.f.Name, c.key)
	if err != nil {
		return nil, err
	}
	if b.Header.Schema == ui.KindPackage {
		if _, err := ui.LoadPackage(b); err != nil {
			return nil, err
		}
	}
	if share == "" {
		share = filepath.Join(filepath.Dir(c.dir), "..", "share")
	}
	dst := filepath.Join(share, fmt.Sprintf("%s-%s-%s", b.Header.Name, b.Header.Version, b.Hash[2:10]))
	if err := copyTree(dir, dst); err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(dst)
	// The claim is public: on a network that isn't on this machine, never put the absolute path in it (it names the
	// OS user); a path under the home folder is written as ~/… and expanded by the reader's client.
	if home, err := os.UserHomeDir(); err == nil && !localURL(c.ch.dep.RPC) {
		if rel, err := filepath.Rel(home, abs); err == nil && !strings.HasPrefix(rel, "..") {
			abs = "~/" + filepath.ToSlash(rel)
		}
	}
	cl := uiClaim{Schema: b.Header.Schema, Name: b.Header.Name, Version: b.Header.Version, Line: b.Header.Line, Author: c.f.Name, URI: "file://" + abs, Hash: b.Hash}
	if b.Header.Parent != nil {
		cl.Parent = b.Header.Parent.Hash
	}
	data, _ := json.Marshal(cl)
	if err := c.attest(b.Hash, claimPackage, string(data), 0); err != nil {
		return nil, err
	}
	res["published"], res["version"], res["hash"], res["signer"], res["uri"], res["kind"] = b.Header.Name, b.Header.Version, b.Hash, b.Sig.Signer, cl.URI, b.Header.Schema
	res["payee"] = b.Header.Payee
	return res, nil
}

// createSplitOf: a split whose payees may be names or split ids.
func (c *Client) createSplitOf(payees []string, bps []uint16) (common.Hash, error) {
	ids := make([][32]byte, len(payees))
	for i, p := range payees {
		ids[i] = payeeID(p)
	}
	r, err := c.ch.send(c.key, nil, "Bank", "createSplit", ids, bps)
	if err != nil {
		return common.Hash{}, err
	}
	ev := c.ch.abis["Bank"].Events["SplitCreated"]
	for _, l := range r.Logs {
		if len(l.Topics) > 1 && l.Topics[0] == ev.ID {
			return l.Topics[1], nil
		}
	}
	return common.Hash{}, errors.New("no split id in the receipt (it may exist already: same payees and shares)")
}

func copyTree(src, dst string) error {
	os.RemoveAll(dst)
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(t, b, 0o644)
	})
}

// uiPublished: the artifacts claimed on chain, each claimed by its own author.
func (c *Client) uiPublished() ([]uiClaim, error) {
	cls, err := c.easClaimsOf(nil, map[string]bool{claimPackage: true})
	if err != nil {
		return nil, err
	}
	var out []uiClaim
	for _, cl := range cls {
		var d uiClaim
		if cl.Revoked || cl.Issuer == "" || json.Unmarshal([]byte(cl.Data), &d) != nil {
			continue
		}
		if d.Author != cl.Issuer || !strings.EqualFold(d.Hash, cl.Subject.Hex()) { // only an author's own claim names its artifact
			continue
		}
		d.Block = cl.Block
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Block < out[j].Block })
	return out, nil
}

// uiFind: a published artifact by NAME (its latest version), NAME@VERSION, or hash prefix.
func (c *Client) uiFind(ref string) (uiClaim, error) {
	cls, err := c.uiPublished()
	if err != nil {
		return uiClaim{}, err
	}
	name, ver, _ := strings.Cut(ref, "@")
	var best *uiClaim
	for i := range cls {
		cl := &cls[i]
		hit := strings.HasPrefix(ref, "0x") && len(ref) >= 10 && strings.HasPrefix(cl.Hash, strings.ToLower(ref)) ||
			cl.Name == name && (ver == "" || cl.Version == ver || cl.Line == ref)
		if hit && (best == nil || newerVersion(cl.Version, best.Version)) {
			best = cl
		}
	}
	if best == nil {
		return uiClaim{}, fmt.Errorf("no published artifact %s ('ui list' shows what is)", ref)
	}
	return *best, nil
}

func newerVersion(a, b string) bool {
	var x, y [3]int
	fmt.Sscanf(a, "%d.%d.%d", &x[0], &x[1], &x[2])
	fmt.Sscanf(b, "%d.%d.%d", &y[0], &y[1], &y[2])
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// uiFetch: a published artifact, checked: its files against its manifest,
// the manifest's hash against the claim on chain, the signature, and that the
// signer held the author's name.
func (c *Client) uiFetch(ref string) (*ui.Bundle, error) {
	cl, err := c.uiFind(ref)
	if err != nil {
		return nil, err
	}
	src, ok := strings.CutPrefix(cl.URI, "file://")
	if !ok {
		return nil, fmt.Errorf("%s is at %s: this prototype fetches file:// bundles only", cl.Name, cl.URI)
	}
	if rest, ok := strings.CutPrefix(src, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			src = filepath.Join(home, rest)
		}
	}
	b, err := ui.Open(src)
	if err != nil {
		return nil, err
	}
	if b.Hash != strings.ToLower(cl.Hash) {
		return nil, fmt.Errorf("%s: the bundle at %s is %s, the chain says %s", cl.Name, cl.URI, b.Hash[:12], cl.Hash[:12])
	}
	if b.Header.Author != cl.Author {
		return nil, fmt.Errorf("%s: the bundle says its author is %s, the claim is by %s", cl.Name, b.Header.Author, cl.Author)
	}
	head, err := c.ch.b.BlockNumber(context.Background())
	if err != nil {
		return nil, err
	}
	if err := c.res.verifySigner(b.Header.Author, common.HexToAddress(b.Sig.Signer), head, c.chainNow()); err != nil {
		return nil, fmt.Errorf("%s: signed by %s, which doesn't hold the name %s: %v", cl.Name, b.Sig.Signer, b.Header.Author, err)
	}
	return b, nil
}

// uiForkNote: what the client says about a bundle's lineage, checked on chain.
func (c *Client) uiForkNote(h ui.Header) (bool, string) {
	var shares map[string]int
	if strings.HasPrefix(h.Payee, "0x") && len(h.Payee) == 66 {
		if r, err := c.ch.call("Bank", "splitOf", common.HexToHash(h.Payee)); err == nil {
			ids, _ := r[0].([][32]byte)
			bps, _ := r[1].([]uint16)
			if len(ids) > 0 {
				shares = map[string]int{}
				for i := range ids {
					shares[common.Hash(ids[i]).Hex()] += int(bps[i])
				}
			}
		}
	}
	return ui.ForkCheck(h, shares, func(n string) string { return payeeID(n).Hex() })
}

// uiInstall: fetch, check, and pin. Installing is a consequential action:
// a tainted agent needs its owner's approval for exactly this artifact.
func (c *Client) uiInstall(ref string) (ui.Pin, error) {
	b, err := c.uiFetch(ref)
	if err != nil {
		return ui.Pin{}, err
	}
	switch b.Header.Schema {
	case ui.KindPackage:
		if _, err := ui.LoadPackage(b); err != nil {
			return ui.Pin{}, err
		}
	case ui.KindRanker:
		if _, err := ui.CompileRanker(b.Raw); err != nil {
			return ui.Pin{}, err
		}
	case ui.KindPolicy:
		if _, err := ui.LoadPolicy(b); err != nil {
			return ui.Pin{}, err
		}
	}
	honest, note := c.uiForkNote(b.Header)
	if !honest {
		note = "warning: " + note
	}
	if err := c.guardInstall(b); err != nil {
		return ui.Pin{}, err
	}
	return c.uiStore().Install(b, note)
}

// installAction: the typed action an install is, and its hash.
func installAction(b *ui.Bundle) (string, string) {
	return "ui:install:" + b.Hash, ui.ActionHash(ui.KindInstall, map[string]string{"artifact": b.Hash})
}

func (c *Client) guardInstall(b *ui.Bundle) error {
	if c.isOwner() {
		return nil
	}
	c.syncGuard()
	if len(c.st.Taint) == 0 {
		return nil
	}
	to, hash := installAction(b)
	c.wantAction = hash
	defer func() { c.wantAction = "" }()
	what := fmt.Sprintf("installing %s %s by %s (%s)", b.Header.Name, b.Header.Version, b.Header.Author, b.Header.Schema)
	if err := c.useApproval(to, nil, what); err != nil {
		return fmt.Errorf("%w\nFor exactly this artifact: ternchain owner approve --dir %s %s %s --action %s", err, c.ownerDirWords(), c.f.Name, to, hash)
	}
	return nil
}

// uiUse: switch the line in use (the owner's explicit move; never automatic).
func (c *Client) uiUse(line, community string) (map[string]any, error) {
	p := c.uiPrefs()
	if community != "" {
		p.Community = community
	}
	if !strings.Contains(line, "@") {
		line += "@1"
	}
	switch line {
	case "base@1":
		p.Package = ""
	case "new@1":
		p.Ranker = ""
	default:
		pin, ok := c.uiStore().Pin(line)
		if !ok && strings.HasSuffix(line, "@1") && ui.BuiltinRanker(strings.TrimSuffix(line, "@1")) != nil {
			p.Ranker = line // a built-in ranking module (hot@1, trust@1)
			return map[string]any{"in_use": p}, c.saveUIPrefs(p)
		}
		if !ok {
			return nil, fmt.Errorf("%s is not installed ('ui install %s')", line, strings.TrimSuffix(line, "@1"))
		}
		if pin.Kind == ui.KindPolicy { // a client policy module: policy.go
			return c.policyUse(line)
		}
		if pin.Kind == ui.KindRanker {
			p.Ranker = line
		} else {
			p.Package = line
		}
	}
	return map[string]any{"in_use": p}, c.saveUIPrefs(p)
}

// uiPackage: the UI package in use (nil: the base templates).
func (c *Client) uiPackage(line string) (*ui.Package, *ui.Pin, error) {
	if line == "" {
		return nil, nil, nil
	}
	b, err := c.uiStore().Load(line)
	if err != nil {
		return nil, nil, err
	}
	pin, _ := c.uiStore().Pin(line)
	p, err := ui.LoadPackage(b)
	return p, &pin, err
}

// ---- the owner's secret mark (approval sheets show it; fakes can't)

type uiMarkFile struct {
	Word  string `json:"word"`
	Color string `json:"color"`
}

var markWords = []string{"heron", "juniper", "copper", "lantern", "saffron", "harbor", "quartz", "willow", "ember", "tundra", "falcon", "marigold"}
var markColors = []string{"#e4572e", "#29335c", "#f3a712", "#669bbc", "#a8c686", "#8e44ad", "#16a085", "#d35400"}

// uiMark: the owner's mark, kept in the owner's folder (set it, or one is chosen at random).
func (c *Client) uiMark(word, color string) (uiMarkFile, error) {
	path := filepath.Join(c.dir, "ui-mark.json")
	var m uiMarkFile
	readJSON(path, &m)
	if word != "" {
		m.Word = word
	}
	if color != "" {
		m.Color = color
	}
	if m.Word == "" {
		var b [2]byte
		randRead(b[:])
		m.Word, m.Color = markWords[int(b[0])%len(markWords)], markColors[int(b[1])%len(markColors)]
	}
	if m.Color == "" {
		m.Color = markColors[0]
	}
	bs, _ := json.MarshalIndent(m, "", "  ")
	return m, os.WriteFile(path, bs, 0o600)
}

// ---- helpers the executor uses

func (c *Client) ratingLine(name string) string {
	r, err := c.ratingTimed(name, ratingOpts{}, 3*time.Second, false)
	if err != nil {
		return ""
	}
	switch s := r["stars"].(type) {
	case float64:
		return fmt.Sprintf("rating %.1f★ from %v reviewed deals", s, r["reviews_counted"])
	case string:
		return s
	}
	return ""
}

func coinsOf(s string) (*big.Int, error) { return parseCoins(s) }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
