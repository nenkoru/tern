// ternchain ui serve: the chat-first client. One stream with the agent;
// items arrive as typed cards and screens (A2UI) drawn from the installed
// UI package or by the agent; buttons go to the broker (internal/ui), the
// broker to this executor, the executor through the agent's guard; the
// approval sheet is drawn by the shell's trusted layer from the structured
// action, with the owner's secret mark.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nenkoru/tern/internal/ui"
)

type uiMsg struct {
	ID      int        `json:"id"`
	From    string     `json:"from"` // agent | owner | system
	Agent   string     `json:"agent,omitempty"`
	Text    string     `json:"text,omitempty"`
	Surface *uiSurface `json:"surface,omitempty"`
}

type uiSurface struct {
	ID      string            `json:"id"`
	A2UI    []map[string]any  `json:"a2ui"`
	Source  string            `json:"source"`
	Tainted bool              `json:"tainted"`
	Theme   map[string]string `json:"theme,omitempty"`
	Note    string            `json:"note,omitempty"`
	Drafts  []ui.Draft        `json:"drafts,omitempty"`
}

type uiPending struct {
	ask    *ui.Ask
	action ui.Action
	to     string // what the owner's approval names
	amount *big.Int
}

type uiServer struct {
	c, owner *Client
	f        uiFlags
	token    string
	gen      ui.Generator
	broker   *ui.Broker

	cmu sync.Mutex // the client is used by one request at a time

	mu     sync.Mutex
	msgs   []uiMsg
	subs   map[chan uiMsg]bool
	asks   map[string]*uiPending
	n      int
	posts  map[string]*Item // the posts on screens now, by id
	cards  map[string]*Item // cards shown, by card id
	reactQ []ui.Reaction
	timer  *time.Timer
}

func (c *Client) uiServe(f uiFlags) error {
	listen := f.listen
	if !f.listenSet {
		listen = "127.0.0.1:8790"
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("ui serve listens on this machine only (127.0.0.1 or ::1), not %s: the shell holds your approvals", host)
	}
	s := &uiServer{c: c, f: f, token: randHex(16), subs: map[chan uiMsg]bool{}, asks: map[string]*uiPending{}, posts: map[string]*Item{}, cards: map[string]*Item{}}
	if f.ownerDir != "" {
		if s.owner, err = openClient(f.ownerDir, c.ch); err != nil {
			return fmt.Errorf("--owner-dir: %w", err)
		}
		if !s.owner.isOwner() || ownerName(c.f.Name) != s.owner.f.Name {
			return fmt.Errorf("--owner-dir %s is not %s's owner", f.ownerDir, c.f.Name)
		}
	}
	s.gen = ui.MockGenerator{}
	if f.llm != "" {
		s.gen = cmdGenerator{cmd: f.llm}
	}
	s.broker = ui.NewBroker(s)
	p := c.uiPrefs()
	if f.community != "" {
		p.Community = f.community
		c.saveUIPrefs(p)
	}
	if p.Community == "" {
		for g := range c.st.Groups {
			if c.isCommunity(g) {
				p.Community = g
				break
			}
		}
		c.saveUIPrefs(p)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/static/", s.static)
	mux.HandleFunc("/api/events", s.events)
	mux.HandleFunc("/api/say", s.post(s.apiSay))
	mux.HandleFunc("/api/action", s.post(s.apiAction))
	mux.HandleFunc("/trusted/mark", s.trustedMark)
	mux.HandleFunc("/trusted/approve", s.post(s.trustedApprove))
	mux.HandleFunc("/trusted/decline", s.post(s.trustedDecline))
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		s.cmu.Lock()
		defer s.cmu.Unlock()
		who := c.f.Name
		s.say("agent", fmt.Sprintf("Hi. I'm %s. This is your chat: your communities, invitations, work and votes arrive here as cards. "+
			"Every button goes through my guard; anything with money or new people comes to you to approve.", who), nil)
		s.pushFeed("")
		s.pushCards(false)
	}()
	go func() {
		for range time.Tick(15 * time.Second) {
			s.cmu.Lock()
			s.pushCards(true)
			s.cmu.Unlock()
		}
	}()
	log.Printf("ui: http://%s (agent %s, community %s)", listen, c.f.Name, p.Community)
	out(map[string]any{"serving": "http://" + listen, "agent": c.f.Name, "community": p.Community, "owner_sheet": s.owner != nil})
	srv := &http.Server{Handler: s.hostGuard(listen, mux), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// hostGuard: only requests addressed to this machine (no DNS rebinding).
func (s *uiServer) hostGuard(listen string, h http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(listen)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hh, hp, err := net.SplitHostPort(r.Host)
		if err != nil || hp != port || (hh != "127.0.0.1" && hh != "localhost" && hh != "::1" && hh != "[::1]") {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func (s *uiServer) shellOK(r *http.Request) bool {
	t := r.Header.Get("X-Tern-Shell")
	if t == "" {
		t = r.URL.Query().Get("t")
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

func (s *uiServer) post(h func(w http.ResponseWriter, r *http.Request, body []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !s.shellOK(r) {
			http.Error(w, "the shell only", http.StatusForbidden)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h(w, r, b)
	}
}

func uiJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *uiServer) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := ui.Web.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(strings.Replace(string(b), "{{TOKEN}}", s.token, 1)))
}

func (s *uiServer) static(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	b, err := fs.ReadFile(ui.Web, "web/"+name)
	if err != nil || name == "index.html" {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	w.Write(b)
}

// ---- the stream

func (s *uiServer) state() map[string]any {
	p := s.c.uiPrefs()
	pkg, rk := p.Package, p.Ranker
	if pkg == "" {
		pkg = "base"
	}
	if rk == "" {
		rk = "newest first"
	}
	return map[string]any{"agent": s.c.f.Name, "owner": s.c.f.Owner, "community": p.Community, "ui": pkg, "ranker": rk}
}

func (s *uiServer) events(w http.ResponseWriter, r *http.Request) {
	if !s.shellOK(r) {
		http.Error(w, "the shell only", http.StatusForbidden)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch := make(chan uiMsg, 64)
	s.mu.Lock()
	history := append([]uiMsg(nil), s.msgs...)
	s.subs[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	st, _ := json.Marshal(s.state())
	fmt.Fprintf(w, "event: state\ndata: %s\n\n", st)
	for _, m := range history {
		b, _ := json.Marshal(m)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
	}
	fl.Flush()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			b, _ := json.Marshal(m)
			if m.From == "state" {
				fmt.Fprintf(w, "event: state\ndata: %s\n\n", m.Text)
			} else {
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			}
			fl.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		}
	}
}

func (s *uiServer) emit(m uiMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.From != "state" {
		s.n++
		m.ID = s.n
		s.msgs = append(s.msgs, m)
		if len(s.msgs) > 300 {
			s.msgs = s.msgs[len(s.msgs)-300:]
		}
	}
	for ch := range s.subs {
		select {
		case ch <- m:
		default:
		}
	}
}

func (s *uiServer) pushState() {
	b, _ := json.Marshal(s.state())
	s.emit(uiMsg{From: "state", Text: string(b)})
}

func (s *uiServer) say(from, text string, surf *uiSurface) {
	s.emit(uiMsg{From: from, Agent: s.c.f.Name, Text: text, Surface: surf})
}

func (s *uiServer) show(text string, sf *ui.Surface, note string, drafts []ui.Draft) {
	s.broker.Register(sf)
	s.say("agent", text, &uiSurface{ID: sf.ID, A2UI: sf.Messages(), Source: sf.Source, Tainted: sf.Tainted, Theme: sf.Theme, Note: note, Drafts: drafts})
}

func (s *uiServer) nextID(kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("%s-%d", kind, s.n+1)
}

// ---- screens

// template: the template from the package in use, or the base one.
func (s *uiServer) template(id string) (*ui.Package, string, error) {
	p := s.c.uiPrefs()
	pkg, pin, err := s.c.uiPackage(p.Package)
	if err != nil {
		return nil, "", err
	}
	if pkg != nil {
		if _, ok := pkg.Templates[id]; ok {
			note := ""
			if pin != nil {
				note = pin.Note
			}
			return pkg, note, nil
		}
	}
	b, err := ui.Base()
	return b, "", err
}

func (s *uiServer) feed() (*feedView, error) {
	p := s.c.uiPrefs()
	rk, err := s.c.uiRanker(p.Ranker)
	if err != nil {
		return nil, err
	}
	return s.c.uiFeed(p.Community, rk)
}

// feedData: the feed's data model: what the agent writes and the broker
// resolves parameters from. Others' text is fenced.
func (s *uiServer) feedData(v *feedView) (map[string]any, bool) {
	c, p := s.c, s.c.uiPrefs()
	tainted := false
	var items []any
	for _, it := range v.Posts {
		own := c.family(it.From)
		fence := func(x string) any {
			if own {
				return x
			}
			tainted = true
			return ui.Fence(it.From, x)
		}
		post, _, _ := ui.ParsePost(it.Mail.Data)
		title := post.Title
		if title == "" {
			title = clipText(it.Mail.Text, 90)
		}
		cnt := v.Counts[it.ID]
		if cnt == nil {
			cnt = &ui.Count{}
		}
		age := float64(v.Now-it.Time) / 3600
		origin := "outside:" + ownerName(it.From)
		if own {
			origin = "own"
		} else if d, ok := v.Graph[ownerName(it.From)]; ok && d <= 1 {
			origin = "contact:" + ownerName(it.From)
		}
		label := it.Author
		if label == "" {
			label = c.authorLabel(it.From)
		}
		byline := fmt.Sprintf("%s · %s · %s · %d comments", it.From, label, agoWords(age), len(v.Comments[it.ID]))
		why := v.Ranked[it.ID].Reason
		if it.Status == "hidden" {
			why += " · hidden: " + it.Trust
		}
		if bad := v.BadData[it.ID]; bad != "" {
			why += " · " + bad
		}
		if it.Warning != "" {
			why += " · ⚠ " + it.Warning
		}
		s.mu.Lock()
		s.posts[it.ID] = it
		s.mu.Unlock()
		items = append(items, map[string]any{"id": it.ID, "title": fence(title), "text": fence(it.Mail.Text), "author": it.From, "origin": origin,
			"byline": byline, "why": why, "net": float64(cnt.Up - cnt.Down), "up": float64(cnt.Up), "down": float64(cnt.Down),
			"age": agoWords(age), "score": v.Ranked[it.ID].Score})
	}
	if items == nil {
		items = []any{}
	}
	return ui.Normalize(map[string]any{"community": v.Community, "summary": v.summary(), "ranker": v.rankerName(),
		"tip": map[string]any{"amount": p.TipSmall, "big": p.TipBig}, "items": items}), tainted
}

func agoWords(h float64) string {
	switch {
	case h < 1:
		return fmt.Sprintf("%.0f min ago", h*60)
	case h < 48:
		return fmt.Sprintf("%.0f h ago", h)
	}
	return fmt.Sprintf("%.0f days ago", h/24)
}

func (s *uiServer) pushFeed(intro string) {
	v, err := s.feed()
	if err != nil {
		s.say("agent", "I can't read the feed: "+err.Error(), nil)
		return
	}
	data, tainted := s.feedData(v)
	pkg, note, err := s.template("community.feed")
	if err != nil {
		s.say("agent", "The UI package in use doesn't load: "+err.Error()+" ('use base' switches to the built-in templates)", nil)
		return
	}
	sf, err := pkg.Surface("community.feed", s.nextID("feed"), data, tainted)
	if err != nil {
		s.say("agent", err.Error(), nil)
		return
	}
	if errs := ui.ValidateSurface(sf, ui.TemplateNeeds["community.feed"]); len(errs) > 0 {
		s.say("agent", "I won't show this screen: "+strings.Join(errs, "; "), nil)
		return
	}
	if pkg.Bundle == nil {
		sf.Source = "the device's base templates"
	}
	if intro == "" {
		intro = fmt.Sprintf("%s: %d posts, ranked by %s. Each says why it is where it is.", v.Community, len(v.Posts), v.rankerName())
		if v.Dropped > 0 {
			intro += fmt.Sprintf(" %d filtered out by the ranking module.", v.Dropped)
		}
	}
	s.show(intro, sf, note, nil)
}

func (s *uiServer) pushThread(post string) {
	v, err := s.feed()
	if err != nil {
		s.say("agent", err.Error(), nil)
		return
	}
	it := v.ByID[post]
	if it == nil {
		s.say("agent", "That post isn't in the feed any more.", nil)
		return
	}
	one := &feedView{Community: v.Community, Ranker: v.Ranker, Posts: []*Item{it}, Ranked: v.Ranked, ByID: v.ByID, Comments: v.Comments, Counts: v.Counts,
		Stats: v.Stats, BadData: v.BadData, Graph: v.Graph, Now: v.Now}
	data, tainted := s.feedData(one)
	var cs []any
	for _, cm := range v.Comments[post] {
		if cm.Status != "delivered" {
			continue
		}
		var text any = cm.Mail.Text
		if !s.c.family(cm.From) {
			text, tainted = ui.Fence(cm.From, cm.Mail.Text), true
		}
		cs = append(cs, map[string]any{"text": text, "byline": fmt.Sprintf("%s · %s", cm.From, agoWords(float64(v.Now-cm.Time)/3600))})
	}
	if cs == nil {
		cs = []any{}
	}
	data["comments"] = ui.Normalize(map[string]any{"x": cs})["x"]
	data["comments_summary"] = fmt.Sprintf("%d comments", len(cs))
	pkg, note, err := s.template("community.thread")
	if err != nil {
		s.say("agent", err.Error(), nil)
		return
	}
	sf, err := pkg.Surface("community.thread", s.nextID("thread"), data, tainted)
	if err != nil {
		s.say("agent", err.Error(), nil)
		return
	}
	if pkg.Bundle == nil {
		sf.Source = "the device's base templates"
	}
	s.show("The thread:", sf, note, nil)
}

// cardData: a typed card's data model (the sender's words fenced; the
// sender's name is the envelope's verified signer, not the card's claim).
func (s *uiServer) cardData(it *Item, card ui.Card) map[string]any {
	from := it.From
	f := func(x string) any {
		if s.c.family(from) {
			return x
		}
		return ui.Fence(from, x)
	}
	var body map[string]any
	json.Unmarshal(card.Body, &body)
	str := func(k string) string { x, _ := body[k].(string); return x }
	d := map[string]any{"id": card.ID, "kind": card.Card, "from": from, "subject": f(card.Subject),
		"from_line": fmt.Sprintf("from %s · %s · %s", from, s.c.authorLabel(from), trustWords(s.c, it))}
	if cm, ok := card.Refs["commons"].(string); ok {
		d["commons"] = cm
	}
	switch card.Card {
	case "invitation":
		d["what"] = f(str("what"))
		if w, ok := body["when"].(map[string]any); ok {
			st, _ := w["start"].(float64)
			en, _ := w["end"].(float64)
			line := time.Unix(int64(st), 0).UTC().Format("Mon 2 Jan 15:04")
			if en > st {
				line += "–" + time.Unix(int64(en), 0).UTC().Format("15:04") + " UTC"
			}
			if where := str("where"); where != "" {
				line += " · " + where
			}
			d["when"] = f(line)
		}
	case "job":
		d["role"] = f(str("role"))
		pay := str("pay") + " coins " + str("basis")
		if e, _ := body["escrow"].(bool); e {
			pay += ", in escrow"
		}
		if a := str("arbiter"); a != "" {
			pay += ", arbiter " + a
		}
		d["pay_line"] = f(pay)
	case "vote":
		d["question"] = f(str("question"))
		cl, _ := body["closes"].(float64)
		d["closes_line"] = fmt.Sprintf("closes %s · one vote per member, secret (ring signature)", time.Unix(int64(cl), 0).UTC().Format("Mon 2 Jan 15:04 UTC"))
		if cm, ok := body["commons"].(string); ok {
			d["commons"] = cm
		}
	}
	out := map[string]any{"card": d}
	if card.Card == "vote" {
		var opts []any
		if os, ok := body["options"].([]any); ok {
			for _, o := range os {
				v, _ := o.(string)
				opts = append(opts, map[string]any{"label": f(v), "value": v})
			}
		}
		out["options"] = opts
	}
	return ui.Normalize(out)
}

// pushCards: typed cards that passed triage (contacts, paid postage); the rest are only counted.
func (s *uiServer) pushCards(onlyNew bool) {
	if _, err := s.c.scan(); err != nil {
		return
	}
	held := map[string]int{}
	shown := 0
	for _, it := range s.c.st.Items {
		if it.Type != ui.TypeCard {
			continue
		}
		s.c.loadKinds()
		card, err := ui.ParseCard(it.Body)
		if err != nil {
			continue
		}
		// triage: delivered (contacts, paid postage) or a correspondent the owner signed for this agent
		if it.Status != "delivered" && !(it.Status == "quarantined" && (s.c.approvedSender(it.From) || s.c.approvedSender(ownerName(it.From)))) {
			held[it.From]++
			continue
		}
		s.c.taint(it.From) // its words reach the screen (fenced) and this agent's next actions are guarded as after outside text
		key := it.From + "/" + card.ID
		s.mu.Lock()
		_, seen := s.cards[key]
		if !seen {
			s.cards[key] = it
		}
		s.mu.Unlock()
		if seen && onlyNew {
			continue
		}
		tpl := "card." + card.Card
		pkg, note, err := s.template(tpl)
		if err != nil {
			continue
		}
		if _, ok := pkg.Templates[tpl]; !ok {
			tpl = "card.generic"
			if pkg, note, err = s.template(tpl); err != nil {
				continue
			}
		}
		sf, err := pkg.Surface(tpl, "card-"+card.ID, s.cardData(it, card), !s.c.family(it.From))
		if err != nil {
			continue
		}
		if pkg.Bundle == nil {
			sf.Source = "the device's base templates"
		}
		intro := map[string]string{"invitation": "An invitation:", "job": "A job offer that fits your skills:", "vote": "A vote in your community:"}[card.Card]
		if intro == "" {
			intro = "A " + card.Card + ":"
		}
		s.show(intro, sf, note, nil)
		shown++
	}
	if !onlyNew && len(held) > 0 {
		var ws []string
		for k, n := range held {
			ws = append(ws, fmt.Sprintf("%d from %s", n, k))
		}
		sort.Strings(ws)
		s.say("agent", "Held back by triage (strangers who didn't pay your postage; not drawn): "+strings.Join(ws, ", ")+".", nil)
	}
}

// ---- the chat: the owner's words (a deterministic agent: no model needed)

func (s *uiServer) apiSay(w http.ResponseWriter, r *http.Request, body []byte) {
	var in struct {
		Text string `json:"text"`
	}
	json.Unmarshal(body, &in)
	text := strings.TrimSpace(in.Text)
	if text == "" || len(text) > 500 {
		uiJSON(w, map[string]any{"ok": false})
		return
	}
	s.say("owner", text, nil)
	go func() {
		s.cmu.Lock()
		defer s.cmu.Unlock()
		s.handle(text)
	}()
	uiJSON(w, map[string]any{"ok": true})
}

func (s *uiServer) handle(text string) {
	t := strings.ToLower(text)
	c := s.c
	switch {
	case strings.Contains(t, "table") || strings.HasPrefix(t, "show me") || strings.Contains(t, "compact") || strings.HasPrefix(t, "make "):
		s.generate(text)
	case strings.HasPrefix(t, "rank ") || strings.Contains(t, " rank ") || t == "hot" || strings.Contains(t, "circle") || strings.Contains(t, "without bots"):
		s.rankBy(t)
	case strings.HasPrefix(t, "use "):
		s.useLine(strings.TrimSpace(t[4:]))
	case strings.HasPrefix(t, "install "):
		s.installFromChat(strings.TrimSpace(t[8:]))
	case t == "cards" || t == "inbox":
		s.pushCards(false)
	case t == "updates":
		cls, err := c.uiPublished()
		if err != nil {
			s.say("agent", err.Error(), nil)
			return
		}
		var refs []ui.Ref
		for _, cl := range cls {
			refs = append(refs, ui.Ref{Name: cl.Name, Version: cl.Version, Line: cl.Line, Hash: cl.Hash, Author: cl.Author})
		}
		ups := c.uiStore().Updates(refs)
		if len(ups) == 0 {
			s.say("agent", "Everything you installed is at its latest version.", nil)
			return
		}
		var ls []string
		for _, u := range ups {
			ls = append(ls, fmt.Sprintf("%s: %s is out (you have %s, by %s). Not installed: nothing updates itself; say 'install %s@%s' to move.", u.Line, u.Available, u.Installed, u.Author, strings.Split(u.Line, "@")[0], u.Available))
		}
		s.say("agent", strings.Join(ls, "\n"), nil)
	case t == "feed" || strings.Contains(t, "new") || strings.Contains(t, "feed"):
		s.pushFeed("")
	default:
		var pkgs, rks []string
		for _, p := range c.uiStore().Pins() {
			name := strings.TrimSuffix(p.Line, "@1")
			switch p.Kind {
			case ui.KindPackage:
				pkgs = append(pkgs, "'use "+name+"'")
			case ui.KindRanker:
				rks = append(rks, "'rank "+name+"'")
			}
		}
		for _, n := range ui.BuiltinRankers() {
			rks = append(rks, "'rank "+n+"'")
		}
		pkgs = append(pkgs, "'use base'")
		s.say("agent", "I can: 'feed', "+strings.Join(rks, ", ")+" (the ranking modules you have), "+strings.Join(pkgs, ", ")+" (your UI packages), "+
			"'install NAME', 'updates', 'cards', or draw a screen for a question: 'show me this week's hot posts as a table'.", nil)
	}
}

// rankBy: the ranking module the owner's words name, among those installed
// and the built-in ones ("rank by my circle" → circle@1, "rank new without
// bots" → new-without-bots@1): the line whose name shares most words with
// what was said; an installed one wins a tie. Nothing is hard-wired.
func (s *uiServer) rankBy(t string) {
	said := map[string]bool{}
	for _, w := range strings.FieldsFunc(t, func(r rune) bool { return r == ' ' || r == '-' || r == ',' || r == '.' }) {
		switch w {
		case "rank", "by", "my", "the", "a", "use", "feed":
		default:
			said[w] = true
		}
	}
	best, bestN, bestInstalled := "", 0, false
	try := func(line string, installed bool) {
		n := 0
		for _, part := range strings.Split(strings.TrimSuffix(line, "@"+strings.SplitN(line, "@", 2)[1]), "-") {
			if said[part] {
				n++
			}
		}
		if n > bestN || n == bestN && n > 0 && installed && !bestInstalled {
			best, bestN, bestInstalled = line, n, installed
		}
	}
	var have []string
	for _, p := range s.c.uiStore().Pins() {
		if p.Kind == ui.KindRanker {
			try(p.Line, true)
			have = append(have, p.Line)
		}
	}
	for _, n := range ui.BuiltinRankers() {
		try(n+"@1", false)
		have = append(have, n+"@1 (built in)")
	}
	if best == "" {
		s.say("agent", "No ranking module you have matches that. You have: "+strings.Join(have, ", ")+"; 'install NAME' adds one.", nil)
		return
	}
	s.useLine(best)
}

func (s *uiServer) useLine(line string) {
	r, err := s.c.uiUse(line, "")
	if err != nil {
		s.say("agent", err.Error(), nil)
		return
	}
	_ = r
	s.pushState()
	what := strings.TrimSuffix(line, "@1")
	pin, ok := s.c.uiStore().Pin(func() string {
		if strings.Contains(line, "@") {
			return line
		}
		return line + "@1"
	}())
	intro := "Switched to " + what + "."
	if ok {
		intro = fmt.Sprintf("Switched to %s %s by %s (installed, pinned to hash %s; it won't change by itself).", what, pin.Version, pin.Author, pin.Hash[:12])
		if pin.Note != "" && pin.Note != "original" {
			intro += " " + pin.Note + "."
		}
	}
	s.pushFeed(intro)
}

func (s *uiServer) installFromChat(ref string) {
	b, err := s.c.uiFetch(ref)
	if err != nil {
		s.say("agent", err.Error(), nil)
		return
	}
	a := ui.Action{Kind: ui.KindInstall, Params: map[string]string{"artifact": b.Hash}, Name: "install", Surface: "chat", Source: "your words in the chat"}
	a.Hash = ui.ActionHash(a.Kind, a.Params)
	r := s.Execute(a)
	switch r.Status {
	case "needs_approval":
		s.say("agent", "Installing "+b.Header.Name+" needs your approval: I've asked (the sheet).", nil)
		s.emit(uiMsg{From: "system", Text: "approval requested: " + r.Ask.What})
	default:
		s.say("agent", r.Message, nil)
	}
}

// generate: an agent-generated screen for an ad-hoc question.
func (s *uiServer) generate(q string) {
	v, err := s.feed()
	if err != nil {
		s.say("agent", err.Error(), nil)
		return
	}
	data, tainted := s.feedData(v)
	req := ui.Requirements{Actions: []string{"open", "up"}}
	if strings.Contains(strings.ToLower(q), "table") {
		req = ui.Requirements{Actions: []string{"open"}, Components: []string{"Table"}}
	}
	task := ui.Task{Question: q, Actions: ui.BaseActions(), Require: req, Fields: map[string]string{
		"/summary":        "one line: how many posts, reactions, batches, which ranking",
		"/items":          "the posts, ranked (a list)",
		"/items/N/title":  "the post's title (someone else's text: shown fenced)",
		"/items/N/author": "the author's name",
		"/items/N/net":    "up minus down reactions",
		"/items/N/age":    "how long ago",
		"/items/N/why":    "the ranking module's reason",
		"/items/N/byline": "author · kind · age · comments",
	}}
	sf, drafts, err := ui.GenerateSurface(s.gen, task, s.nextID("gen"), data)
	if err != nil {
		s.say("agent", "I couldn't draw a valid screen for that: "+err.Error(), nil)
		return
	}
	sf.Tainted = tainted
	note := fmt.Sprintf("generated for: %q; validated (catalog, literal labels, declared actions, required: %s)", q, strings.Join(append(req.Actions, req.Components...), ", "))
	s.show(fmt.Sprintf("Here it is, drawn for your question (%d draft(s); the data and the buttons are the same as in any other UI).", len(drafts)), sf, note, drafts)
}

// cmdGenerator: an external model command: prompt on stdin, OpenUI Lang on stdout.
type cmdGenerator struct{ cmd string }

func (g cmdGenerator) Name() string { return "llm-cmd (" + g.cmd + ")" }

func (g cmdGenerator) Generate(prompt string, attempt int, feedback []string) (string, error) {
	if len(feedback) > 0 {
		prompt += "\nYour previous draft was rejected:\n- " + strings.Join(feedback, "\n- ") + "\nWrite the whole screen again, fixed.\n"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", g.cmd)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), err
}

// ---- actions

func (s *uiServer) apiAction(w http.ResponseWriter, r *http.Request, body []byte) {
	var ev ui.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ev.Context = nil // the broker never reads it; neither do we
	s.cmu.Lock()
	res := s.broker.Dispatch(ev)
	if res.Status == "local" && res.Action != nil {
		switch res.Action.Name {
		case "open":
			s.pushThread(res.Action.Params["post"])
		case "more":
			s.say("agent", "That card's details are all on it; ask me anything about it.", nil)
		}
	}
	if res.Status == "done" {
		s.say("agent", "Done: "+res.Message, nil)
	}
	if res.Status == "dropped" {
		s.emit(uiMsg{From: "system", Text: "dropped: " + res.Message})
	}
	s.cmu.Unlock()
	uiJSON(w, res)
}

func (s *uiServer) refused(err error) ui.Result {
	return ui.Result{Status: "refused", Message: err.Error()}
}

// Execute runs a typed action through the agent's guard. Approvals it needs
// are bound to the action's hash (Approval.Action in guard.go). Called with cmu held.
func (s *uiServer) Execute(a ui.Action) ui.Result {
	c := s.c
	c.wantAction = a.Hash
	defer func() { c.wantAction = "" }()
	p := c.uiPrefs()
	taintWords := "you have not read outside text"
	if len(c.st.Taint) > 0 {
		taintWords = "your agent has read others' text (" + strings.Join(clipList(c.st.Taint, 4), ", ") + ")"
	}
	ask := func(err error, to string, amount *big.Int, approveTo, what string, why []string) ui.Result {
		if !strings.HasPrefix(err.Error(), "blocked:") {
			return s.refused(err)
		}
		k := &ui.Ask{ID: randHex(6), Action: a, What: what, Counterparty: to, Why: why, Created: c.chainNow()}
		amt := ""
		if amount != nil {
			k.Amount = fmtCoins(amount)
			amt = " " + k.Amount
		}
		if !strings.HasPrefix(to, "ui:") && to != "" {
			k.Rating = c.ratingLine(ownerName(to))
		}
		k.Command = fmt.Sprintf("ternchain owner approve --dir %s %s %s%s --action %s", c.ownerDirWords(), c.f.Name, approveTo, amt, a.Hash)
		s.mu.Lock()
		s.asks[k.ID] = &uiPending{ask: k, action: a, to: approveTo, amount: amount}
		s.mu.Unlock()
		return ui.Result{Status: "needs_approval", Message: what + ": waiting for your approval", Ask: k}
	}
	switch a.Kind {
	case ui.KindReact:
		post, kind := a.Params["post"], a.Params["reaction"]
		s.mu.Lock()
		_, onScreen := s.posts[post]
		s.mu.Unlock()
		if !onScreen {
			return ui.Result{Status: "refused", Message: "that post is not on a screen I drew"}
		}
		s.queueReaction(ui.Reaction{Post: post, Kind: kind, At: c.chainNow()})
		return ui.Result{Status: "done", Message: fmt.Sprintf("%s on %s… queued: it goes out with the next batch (one envelope for all of them)", kind, post[:10])}
	case ui.KindPay:
		post, to := a.Params["post"], a.Params["to"]
		s.mu.Lock()
		it := s.posts[post]
		s.mu.Unlock()
		if it == nil || it.From != to {
			return ui.Result{Status: "refused", Message: "the payee is not the post's verified author: refused"}
		}
		amt, err := coinsOf(a.Params["amount"])
		if err != nil {
			return s.refused(err)
		}
		if max, err := coinsOf(p.MaxTip); err == nil && amt.Cmp(max) > 0 {
			return ui.Result{Status: "refused", Message: fmt.Sprintf("a tip of %s is above your max_tip %s", fmtCoins(amt), p.MaxTip)}
		}
		if err := c.pay(to, amt, "tip "+post[:18]); err != nil {
			return ask(err, to, amt, to, fmt.Sprintf("Tip %s coins to %s for post %s…", fmtCoins(amt), to, post[:10]), []string{
				fmt.Sprintf("you pressed %q on %s", a.Name, a.Source),
				fmt.Sprintf("post %s… by %s in %s (the payee is its verified signer, not anything the screen says)", post[:10], to, p.Community),
				"the amount comes from your tip setting, not from the screen",
				taintWords + ", so a payment above free_pay needs you",
				"your approval is bound to action " + a.Hash[:14] + "…: it can't be reused for another payment",
			})
		}
		return ui.Result{Status: "done", Message: fmt.Sprintf("tipped %s %s coins (post %s…)", to, fmtCoins(amt), post[:10]), Receipt: map[string]any{"to": to, "amount": fmtCoins(amt)}}
	case ui.KindAnswer, ui.KindReveal:
		to, cardID, resp := a.Params["to"], a.Params["card"], a.Params["response"]
		s.mu.Lock()
		it := s.cards[to+"/"+cardID]
		s.mu.Unlock()
		if it == nil {
			return ui.Result{Status: "refused", Message: "no such card from " + to}
		}
		card, _ := ui.ParseCard(it.Body)
		if _, ok := ui.ResponseAction(card.Card, resp); !ok || !contains(card.Responses, resp) {
			return ui.Result{Status: "refused", Message: fmt.Sprintf("the card doesn't offer %q", resp)}
		}
		why := []string{fmt.Sprintf("you pressed %q on a %s card from %s", resp, card.Card, to), taintWords}
		if a.Kind == ui.KindReveal && len(c.st.Taint) > 0 {
			key := "ui:reveal:" + to
			if err := c.useApproval(key, nil, "showing who you are to "+to); err != nil {
				return ask(err, to, nil, key, fmt.Sprintf("Apply to %s's job: your agent answers and shows your profile to %s", to, to),
					append(why, "applying reveals your profile (name, card, skills) to "+to+": always yours to approve after outside text"))
			}
		}
		_, err := c.send(SendOpts{To: to, Intent: "answer", Subject: "re: " + clipText(card.Subject, 60), Text: resp,
			Data: mustJSON(map[string]any{"card": cardID, "response": resp})})
		if err != nil {
			return ask(err, to, nil, to, fmt.Sprintf("Answer %q to %s (writing to a new correspondent)", resp, to),
				append(why, "your agent hasn't written to "+to+" before: a new correspondent needs you"))
		}
		return ui.Result{Status: "done", Message: fmt.Sprintf("answered %q to %s's %s", resp, to, card.Card)}
	case ui.KindVote:
		cardID, community, opt := a.Params["card"], a.Params["community"], a.Params["option"]
		var it *Item
		s.mu.Lock()
		for k, x := range s.cards {
			if strings.HasSuffix(k, "/"+cardID) {
				it = x
			}
		}
		s.mu.Unlock()
		if it == nil {
			return ui.Result{Status: "refused", Message: "no such vote card"}
		}
		card, _ := ui.ParseCard(it.Body)
		var body struct {
			Options []string `json:"options"`
			Commons string   `json:"commons"`
		}
		json.Unmarshal(card.Body, &body)
		if !contains(body.Options, opt) || body.Commons != community || c.st.Groups[community] == nil {
			return ui.Result{Status: "refused", Message: "not an option of that vote, or not your community"}
		}
		if len(c.st.Taint) > 0 {
			key := "ui:vote:" + cardID
			if err := c.useApproval(key, nil, "voting "+opt+" in "+community); err != nil {
				return ask(err, community, nil, key, fmt.Sprintf("Vote %q in %s: %s (irreversible, secret)", opt, community, clipText(card.Subject, 80)),
					[]string{fmt.Sprintf("you pressed \"Vote for this\" next to option %q (the option is the card's id for it; its text is the sender's)", opt),
						taintWords, "a vote can't be taken back: always yours to approve after outside text"})
			}
		}
		if _, err := c.postAnon(community, "vote:"+cardID, "", mustJSON(map[string]string{"vote": opt})); err != nil {
			return s.refused(err)
		}
		return ui.Result{Status: "done", Message: fmt.Sprintf("voted %q in %s (one secret vote per member)", opt, community)}
	case ui.KindInstall:
		ref := a.Params["artifact"]
		b, err := c.uiFetch(ref)
		if err != nil {
			return s.refused(err)
		}
		pin, err := c.uiInstall(ref)
		if err != nil {
			to, _ := installAction(b)
			return ask(err, b.Header.Author, nil, to, fmt.Sprintf("Install %s %s by %s", b.Header.Name, b.Header.Version, b.Header.Author),
				[]string{"you asked in the chat", "it can ask for: " + strings.Join(kindsOf(b), ", "), taintWords + ": installing is consequential"})
		}
		return ui.Result{Status: "done", Message: fmt.Sprintf("installed %s %s (pinned %s). %s", pin.Line, pin.Version, pin.Hash[:12], pin.Note)}
	}
	return ui.Result{Status: "refused", Message: a.Kind + " is not in this prototype yet"}
}

func kindsOf(b *ui.Bundle) []string {
	var m ui.PackageManifest
	json.Unmarshal(b.Raw, &m)
	if len(m.Actions) == 0 {
		return []string{"nothing (a ranking module: it reads only its declared inputs)"}
	}
	return ui.DeclaredKinds(m.Actions)
}

func clipList(xs []string, n int) []string {
	if len(xs) > n {
		return append(append([]string(nil), xs[:n]...), fmt.Sprintf("+%d", len(xs)-n))
	}
	return xs
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// queueReaction: reactions go out in batches (one datagram per flush).
func (s *uiServer) queueReaction(r ui.Reaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reactQ = append(s.reactQ, r)
	if s.timer == nil {
		s.timer = time.AfterFunc(1500*time.Millisecond, s.flushReactions)
	}
}

func (s *uiServer) flushReactions() {
	s.mu.Lock()
	rs := s.reactQ
	s.reactQ, s.timer = nil, nil
	s.mu.Unlock()
	if len(rs) == 0 {
		return
	}
	s.cmu.Lock()
	defer s.cmu.Unlock()
	env, err := s.c.uiReact(s.c.uiPrefs().Community, rs)
	if err != nil {
		s.say("agent", "Your reactions didn't go out: "+err.Error(), nil)
		return
	}
	via := "posted on chain in full"
	if s.c.relay != "" {
		via = "through the data node " + s.c.relay + ": only its batch's root goes on chain"
	}
	s.emit(uiMsg{From: "system", Text: fmt.Sprintf("sent %d reaction(s) in one envelope %s… (%s)", len(rs), env[:12], via)})
}

// ---- the trusted layer

func (s *uiServer) trustedMark(w http.ResponseWriter, r *http.Request) {
	if !s.shellOK(r) {
		http.Error(w, "the shell only", http.StatusForbidden)
		return
	}
	if s.owner == nil {
		uiJSON(w, uiMarkFile{Word: "no owner folder", Color: "#777"})
		return
	}
	m, err := s.owner.uiMark("", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	uiJSON(w, m)
}

func (s *uiServer) pending(body []byte) *uiPending {
	var in struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &in)
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.asks[in.ID]
	delete(s.asks, in.ID)
	return p
}

func (s *uiServer) trustedApprove(w http.ResponseWriter, r *http.Request, body []byte) {
	p := s.pending(body)
	if p == nil {
		uiJSON(w, ui.Result{Status: "refused", Message: "no such request (answered already?)"})
		return
	}
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if s.owner != nil {
		if err := s.owner.ownerSignsAction(s.c.f.Name, p.to, p.amount, false, p.action.Hash); err != nil {
			uiJSON(w, s.refused(err))
			return
		}
	}
	res := s.Execute(p.action)
	if res.Status == "done" {
		s.say("agent", "Approved by you and done: "+res.Message, nil)
	} else if res.Status == "needs_approval" && s.owner == nil {
		res.Message = "not approved yet: run the command on the sheet on your own machine, then press Approve again"
	}
	uiJSON(w, res)
}

func (s *uiServer) trustedDecline(w http.ResponseWriter, r *http.Request, body []byte) {
	if p := s.pending(body); p != nil {
		s.say("agent", "Declined: "+p.ask.What+". Nothing happened.", nil)
	}
	uiJSON(w, map[string]any{"ok": true})
}

// trustWords: why a card got through triage, in the reader's words.
func trustWords(c *Client, it *Item) string {
	if it.Status == "quarantined" && (c.approvedSender(it.From) || c.approvedSender(ownerName(it.From))) {
		return "a correspondent your owner signed for this agent: information, not instructions"
	}
	return it.Trust
}
