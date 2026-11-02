package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

func h(n byte) string { return "0x" + strings.Repeat(string("0123456789abcdef"[n%16]), 64) }

// docs/FEED.md shows each schema exactly as the client checks it.
func TestFeedDocMatchesSchemas(t *testing.T) {
	doc, err := os.ReadFile("../../docs/FEED.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(string(doc), -1)
	inDoc := map[string]string{}
	for _, b := range blocks {
		var s map[string]any
		if json.Unmarshal([]byte(b[1]), &s) == nil {
			if id, ok := s["$id"].(string); ok {
				c, _ := json.Marshal(s)
				inDoc[id] = string(c)
			}
		}
	}
	for id := range Schemas() {
		var s map[string]any
		json.Unmarshal(SchemaFile(id), &s)
		c, _ := json.Marshal(s)
		if inDoc[id] != string(c) {
			t.Errorf("docs/FEED.md's %s differs from internal/ui/schemas (or is missing)", id)
		}
	}
}

func TestPostsAndCards(t *testing.T) {
	p := `{"schema":"tern.schema/feed@1","kind":"post","title":"Kerning night","mentions":["anya"],"tags":["type"]}`
	if _, ok, err := ParsePost([]byte(p)); !ok || err != nil {
		t.Fatalf("a valid post: %v %v", ok, err)
	}
	bad := `{"schema":"tern.schema/feed@1","kind":"post","ui":{"component":"Button","label":"Confirm"}}`
	if _, ok, err := ParsePost([]byte(bad)); !ok || err == nil || !strings.Contains(err.Error(), `unknown field "ui"`) {
		t.Fatalf("a post smuggling UI must be refused: %v %v", ok, err)
	}
	if _, ok, _ := ParsePost([]byte(`{"other":"app"}`)); ok {
		t.Fatal("other apps' data is not a feed post")
	}
	inv := `{"card":"invitation","v":1,"id":"c_quest1","subject":"Finish the quest","body":{"what":"the haunted lighthouse","when":{"start":1793001600}},"responses":["accept","decline"]}`
	if _, err := ParseCard([]byte(inv)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]string{
		"response not offered by its kind": `{"card":"invitation","v":1,"id":"c_x1","subject":"s","body":{"what":"w","when":{"start":1}},"responses":["release"]}`,
		"vote without options":             `{"card":"vote","v":1,"id":"c_x2","subject":"s","body":{"question":"q","closes":1},"responses":["cast"]}`,
		"job with bad money":               `{"card":"job","v":1,"id":"c_x3","subject":"s","body":{"role":"r","pay":"lots","basis":"fixed"},"responses":["apply"]}`,
		"unknown card":                     `{"card":"lottery","v":1,"id":"c_x4","subject":"s","body":{},"responses":["more"]}`,
	} {
		if _, err := ParseCard([]byte(c)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Reactions are counted by the reader from batches: one per owner and post
// (its agents count as it), the latest wins, "none" takes one back,
// non-members and other communities don't count.
func TestTallyFromBatches(t *testing.T) {
	p1, p2 := h(1), h(2)
	b := func(by, owner, root string, member bool, at int64, rs ...Reaction) SignedBatch {
		return SignedBatch{By: by, Owner: owner, Root: root, Member: member, Time: at, Envelope: by + root,
			Batch: ReactionBatch{Schema: SchemaReact, Community: "club", Reactions: rs}}
	}
	bs := []SignedBatch{
		b("anya", "anya", "0xr1", true, 100, Reaction{p1, "up", 90}, Reaction{p2, "up", 90}),
		b("bot.anya", "anya", "0xr1", true, 100, Reaction{p1, "up", 95}), // anya's agent: still one vote for anya
		b("cem", "cem", "0xr2", true, 200, Reaction{p1, "down", 150}),    // later changes his mind:
		b("cem", "cem", "0xr3", true, 300, Reaction{p1, "up", 9999}),     // "at" from the future: clamped to 300
		b("dila", "dila", "0xr2", true, 200, Reaction{p2, "up", 150}),    // then takes it back:
		b("dila", "dila", "0xr3", true, 300, Reaction{p2, "none", 250}),
		b("mallory", "mallory", "0xr3", false, 300, Reaction{p1, "down", 250}), // not a member
		b("emre", "emre", "", true, 300, Reaction{p1, "up", 250}),              // posted on chain in full
	}
	other := b("fatma", "fatma", "0xr3", true, 300, Reaction{p1, "down", 250})
	other.Batch.Community = "elsewhere"
	bs = append(bs, other)
	counts, st := Tally("club", bs)
	if c := counts[p1]; c == nil || c.Up != 3 || c.Down != 0 || strings.Join(c.UpBy, ",") != "anya,cem,emre" {
		t.Fatalf("post 1: %+v", c)
	}
	if c := counts[p2]; c == nil || c.Up != 1 || strings.Join(c.UpBy, ",") != "anya" {
		t.Fatalf("post 2: %+v", c)
	}
	if st.Batches != 9 || len(st.Roots) != 3 || st.OnChain != 1 || st.Reactors != 3 || st.Counted != 4 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestRankerModules(t *testing.T) {
	hot := `{"schema":"tern.ranker/1","name":"hot","version":"1.0.0","line":"hot@1","applies_to":"tern.schema/feed@1","engine":"tern-expr/1",
	 "inputs":["item.votes_up","item.votes_down","item.age_hours","item.hidden"],"params":{"gravity":12.5},"filter":"!item.hidden",
	 "score":"log10(max(1, item.votes_up - item.votes_down)) - item.age_hours / params.gravity",
	 "reason":"{item.votes_up - item.votes_down} net, {item.age_hours:0} h old","author":"anya","payee":"anya","files":{}}`
	r, err := CompileRanker([]byte(hot))
	if err != nil {
		t.Fatal(err)
	}
	in := func(up, down, age float64, hidden bool) map[string]any {
		return map[string]any{"item.votes_up": up, "item.votes_down": down, "item.age_hours": age, "item.hidden": hidden, "author.agent": true}
	}
	out, dropped, err := r.Rank([]Candidate{{"a", in(1, 0, 1, false)}, {"b", in(50, 2, 3, false)}, {"c", in(999, 0, 1, true)}, {"d", in(1, 0, 1, false)}})
	if err != nil || dropped != 1 || len(out) != 3 || out[0].ID != "b" || out[1].ID != "a" || out[2].ID != "d" {
		t.Fatalf("%+v %d %v", out, dropped, err)
	}
	if out[0].Reason != "48 net, 3 h old" {
		t.Fatalf("reason %q", out[0].Reason)
	}
	// a module reads only what it declares
	sneaky := strings.Replace(hot, `"score":"log10`, `"score":"author.agent ? 9 : log10`, 1)
	if _, err := CompileRanker([]byte(sneaky)); err == nil || !strings.Contains(err.Error(), "doesn't declare") {
		t.Fatalf("undeclared input: %v", err)
	}
	if _, err := CompileRanker([]byte(strings.Replace(hot, "params.gravity", "params.speed", 1))); err == nil {
		t.Fatal("an unset param was accepted")
	}
	if _, err := CompileRanker([]byte(strings.Replace(hot, `"engine":"tern-expr/1"`, `"engine":"wasm"`, 1))); err == nil {
		t.Fatal("an unknown engine was accepted")
	}
	// bounded cost
	e, _ := Parse(strings.Repeat("1+", 300) + "1")
	if _, err := e.Eval(Env{}, 100); err != ErrCost {
		t.Fatalf("cost: %v", err)
	}
}

func writePkg(t *testing.T, dir, name, version string, patch func(m map[string]any)) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
	raw, _ := baseFS.ReadFile("base/manifest.json")
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["name"], m["version"], m["line"] = name, version, name+"@"+strings.Split(version, ".")[0]
	delete(m, "author")
	delete(m, "payee")
	if patch != nil {
		patch(m)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644)
	ents, _ := baseFS.ReadDir("base/templates")
	for _, e := range ents {
		tb, _ := baseFS.ReadFile("base/templates/" + e.Name())
		os.WriteFile(filepath.Join(dir, "templates", e.Name()), tb, 0o644)
	}
}

// A package is a signed artifact: changing any file or the manifest breaks
// it; installing pins it; a newer version is shown, never applied.
func TestPackageSignInstallNoAutoUpdate(t *testing.T) {
	key, _ := crypto.GenerateKey()
	d := t.TempDir()
	v1 := filepath.Join(d, "v1")
	writePkg(t, v1, "reddit-classic", "1.0.0", nil)
	b, err := Seal(v1, "anya", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPackage(b); err != nil {
		t.Fatal(err)
	}
	if b.Sig.Signer != crypto.PubkeyToAddress(key.PublicKey).Hex() || b.Header.Author != "anya" {
		t.Fatalf("signature %+v", b.Sig)
	}
	// tampering
	tam := filepath.Join(d, "tampered")
	copyDir(v1, tam)
	os.WriteFile(filepath.Join(tam, "templates", "feed.json"), []byte(`{"components":[]}`), 0o644)
	if _, err := Open(tam); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("a changed template: %v", err)
	}
	tam2 := filepath.Join(d, "tampered2")
	copyDir(v1, tam2)
	mb, _ := os.ReadFile(filepath.Join(tam2, "manifest.json"))
	os.WriteFile(filepath.Join(tam2, "manifest.json"), []byte(strings.Replace(string(mb), `"kind": "pay"`, `"kind": "local"`, 1)), 0o644)
	if _, err := Open(tam2); err == nil || !strings.Contains(err.Error(), "signature is for manifest") {
		t.Fatalf("a changed manifest: %v", err)
	}
	other, _ := crypto.GenerateKey()
	tam3 := filepath.Join(d, "tampered3")
	copyDir(v1, tam3)
	var sig Signature
	sb, _ := os.ReadFile(filepath.Join(tam3, sigFile))
	json.Unmarshal(sb, &sig)
	sig.Signer = crypto.PubkeyToAddress(other.PublicKey).Hex() // claims someone else signed
	sb, _ = json.Marshal(sig)
	os.WriteFile(filepath.Join(tam3, sigFile), sb, 0o644)
	if _, err := Open(tam3); err == nil || !strings.Contains(err.Error(), "not by") {
		t.Fatalf("a wrong signer: %v", err)
	}
	extra := filepath.Join(d, "extra")
	copyDir(v1, extra)
	os.WriteFile(filepath.Join(extra, "templates", "evil.js"), []byte("fetch('https://x')"), 0o644)
	if _, err := Open(extra); err == nil || !strings.Contains(err.Error(), "not in the manifest") {
		t.Fatalf("an unlisted file: %v", err)
	}

	st := Store{Dir: filepath.Join(d, "store")}
	pin, err := st.Install(b, "original")
	if err != nil || pin.Version != "1.0.0" || pin.Hash != b.Hash {
		t.Fatalf("install: %+v %v", pin, err)
	}
	v2 := filepath.Join(d, "v2")
	writePkg(t, v2, "reddit-classic", "1.1.0", nil)
	b2, err := Seal(v2, "anya", key)
	if err != nil {
		t.Fatal(err)
	}
	stranger := Ref{Name: "reddit-classic", Version: "9.0.0", Line: "reddit-classic@1", Hash: h(9), Author: "mallory"}
	ups := st.Updates([]Ref{{Name: "reddit-classic", Version: "1.1.0", Line: "reddit-classic@1", Hash: b2.Hash, Author: "anya"}, stranger})
	if len(ups) != 1 || ups[0].Available != "1.1.0" || ups[0].Installed != "1.0.0" {
		t.Fatalf("updates: %+v", ups)
	}
	if p, _ := st.Pin("reddit-classic@1"); p.Version != "1.0.0" || p.Hash != b.Hash {
		t.Fatalf("the pin moved by itself: %+v", p)
	}
	loaded, err := st.Load("reddit-classic@1")
	if err != nil || loaded.Header.Version != "1.0.0" {
		t.Fatalf("load: %v", err)
	}
	// the installed copy is checked again on every load
	os.WriteFile(filepath.Join(st.Dir, pin.Dir, "templates", "feed.json"), []byte("{}"), 0o644)
	if _, err := st.Load("reddit-classic@1"); err == nil {
		t.Fatal("a changed installed copy loaded")
	}
	// a ranker is the same kind of artifact
	rk := filepath.Join(d, "rk")
	os.MkdirAll(rk, 0o755)
	os.WriteFile(filepath.Join(rk, "manifest.json"), []byte(`{"schema":"tern.ranker/1","name":"hot","version":"1.0.0","line":"hot@1","applies_to":"tern.schema/feed@1",
		"engine":"tern-expr/1","inputs":["item.age_hours"],"score":"-item.age_hours","reason":"new","files":{}}`), 0o644)
	rb, err := Seal(rk, "anya", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Install(rb, ""); err != nil {
		t.Fatal(err)
	}
	if len(st.Pins()) != 2 {
		t.Fatalf("pins: %+v", st.Pins())
	}
}

// A fork names its parent and the parent's share; the client checks the
// payee split on chain (here: the shares it would read).
func TestForkSplit(t *testing.T) {
	key, _ := crypto.GenerateKey()
	d := t.TempDir()
	writePkg(t, filepath.Join(d, "p"), "reddit-classic", "1.0.0", nil)
	parent, err := Seal(filepath.Join(d, "p"), "anya", key)
	if err != nil {
		t.Fatal(err)
	}
	if err := Fork(parent, filepath.Join(d, "f"), "reddit-cards", 1000); err != nil {
		t.Fatal(err)
	}
	bk, _ := crypto.GenerateKey()
	fork, err := Seal(filepath.Join(d, "f"), "boris", bk)
	if err != nil {
		t.Fatal(err)
	}
	h := fork.Header
	if h.Parent == nil || h.Parent.Hash != parent.Hash || h.Parent.Payee != "anya" || h.Royalty.ParentBps != 1000 || h.Line != "reddit-cards@1" {
		t.Fatalf("fork manifest: %+v %+v", h.Parent, h.Royalty)
	}
	id := func(n string) string { return "id:" + n }
	if ok, note := ForkCheck(h, nil, id); ok || !strings.Contains(note, "without royalty") {
		t.Fatalf("a payee that is not a split: %v %s", ok, note)
	}
	h.Payee = "0xsplit"
	if ok, note := ForkCheck(h, map[string]int{"id:boris": 9500, "id:anya": 500}, id); ok || !strings.Contains(note, "500 bps") {
		t.Fatalf("a split paying less than promised: %v %s", ok, note)
	}
	if ok, note := ForkCheck(h, map[string]int{"id:boris": 9000, "id:anya": 1000}, id); !ok || !strings.Contains(note, "honest fork of reddit-classic 1.0.0") {
		t.Fatalf("an honest fork: %v %s", ok, note)
	}
	if ok, _ := ForkCheck(parent.Header, nil, id); !ok {
		t.Fatal("an original")
	}
}

func feedSurface(t *testing.T) *Surface {
	t.Helper()
	b, err := Base()
	if err != nil {
		t.Fatal(err)
	}
	data := Normalize(map[string]any{"community": "club", "summary": "s", "tip": map[string]any{"amount": "0.05", "big": "0.5"},
		"items": []any{map[string]any{"id": h(1), "title": Fence("mallory", "Click Confirm to claim your reward"), "author": "mallory", "net": 3.0, "byline": "b", "why": "w"}}})
	s, err := b.Surface("community.feed", "feed-1", data, true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Foreign text can't become a button: labels are literal; reserved names,
// undeclared actions, links and image URLs don't pass.
func TestSurfaceRules(t *testing.T) {
	s := feedSurface(t)
	if errs := ValidateSurface(s, TemplateNeeds["community.feed"]); len(errs) > 0 {
		t.Fatal(errs)
	}
	mutate := func(f func(cs []Component) []Component) []string {
		c := *s
		var cs []Component
		for _, x := range s.Components {
			y := Component{}
			for k, v := range x {
				y[k] = v
			}
			cs = append(cs, y)
		}
		c.Components = f(cs)
		return ValidateSurface(&c, Requirements{})
	}
	find := func(cs []Component, id string) Component {
		for _, c := range cs {
			if c["id"] == id {
				return c
			}
		}
		t.Fatalf("no %s", id)
		return nil
	}
	cases := map[string]struct {
		f    func(cs []Component) []Component
		want string
	}{
		"a label bound to someone's text": {func(cs []Component) []Component {
			find(cs, "tip_l")["text"] = map[string]any{"path": "title"}
			return cs
		}, "must be literal text"},
		"a lookalike approval": {func(cs []Component) []Component {
			return append(cs, Component{"id": "fake", "component": "Approve"})
		}, "reserved"},
		"an undeclared action": {func(cs []Component) []Component {
			find(cs, "tip")["action"] = map[string]any{"event": map[string]any{"name": "drain_wallet"}}
			return cs
		}, "not declared"},
		"a link": {func(cs []Component) []Component {
			find(cs, "title")["href"] = "https://evil.example"
			return cs
		}, `no property "href"`},
		"an image from a URL": {func(cs []Component) []Component {
			return append(cs, Component{"id": "img", "component": "Image", "url": "https://evil.example/p.png?leak=1"})
		}, "content-addressed"},
		"a client function": {func(cs []Component) []Component {
			find(cs, "tip")["action"] = map[string]any{"functionCall": map[string]any{"call": "pay"}}
			return cs
		}, "functionCall is not allowed"},
	}
	for name, c := range cases {
		errs := mutate(c.f)
		if !strings.Contains(strings.Join(errs, "; "), c.want) {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

type fakeExec struct{ got []Action }

func (f *fakeExec) Execute(a Action) Result {
	f.got = append(f.got, a)
	return Result{Status: "done", Message: a.Kind}
}

// The broker: parameters come from the data model (the item the button is
// in), never from the event; undeclared names are dropped; a fenced value is
// never a parameter; consequential actions need a real gesture; a surface
// that clicks too fast is frozen.
func TestBroker(t *testing.T) {
	s := feedSurface(t)
	ex := &fakeExec{}
	b := NewBroker(ex)
	b.Register(s)
	r := b.Dispatch(Event{SurfaceID: "feed-1", Component: "tip", Name: "tip", Scope: "/items/0", Gesture: true,
		Context: map[string]any{"to": "mallory", "amount": "500"}})
	if r.Status != "done" || len(ex.got) != 1 {
		t.Fatalf("%+v", r)
	}
	a := ex.got[0]
	if a.Kind != KindPay || a.Params["to"] != "mallory" || a.Params["amount"] != "0.05" || a.Params["post"] != h(1) || a.Hash != ActionHash(KindPay, a.Params) {
		t.Fatalf("params: %+v", a)
	}
	if r := b.Dispatch(Event{SurfaceID: "feed-1", Component: "tip", Name: "drain", Scope: "/items/0", Gesture: true}); r.Status != "dropped" {
		t.Fatalf("undeclared: %+v", r)
	}
	if r := b.Dispatch(Event{SurfaceID: "feed-1", Component: "up", Name: "tip", Scope: "/items/0", Gesture: true}); r.Status != "dropped" {
		t.Fatalf("a button that doesn't emit it: %+v", r)
	}
	if r := b.Dispatch(Event{SurfaceID: "feed-1", Component: "tip", Name: "tip", Scope: "/items/0", Gesture: false}); r.Status != "refused" {
		t.Fatalf("no gesture: %+v", r)
	}
	// a template whose payee path points at someone's text
	s2 := *s
	s2.ID = "feed-2"
	s2.Actions = map[string]ActionDecl{"tip": {Kind: KindPay, Params: map[string]string{"to": "title", "amount": "/tip/amount"}}}
	b.Register(&s2)
	if r := b.Dispatch(Event{SurfaceID: "feed-2", Component: "tip", Name: "tip", Scope: "/items/0", Gesture: true}); r.Status != "refused" || !strings.Contains(r.Message, "someone else's text") {
		t.Fatalf("fenced param: %+v", r)
	}
	// rate limit
	now := time.Unix(1000, 0)
	b.Now = func() time.Time { return now }
	var last Result
	for i := 0; i < 8; i++ {
		last = b.Dispatch(Event{SurfaceID: "feed-1", Component: "tip", Name: "tip", Scope: "/items/0", Gesture: true})
	}
	if last.Status != "refused" || !strings.Contains(last.Message, "frozen") {
		t.Fatalf("rate: %+v", last)
	}
	if r := b.Dispatch(Event{SurfaceID: "feed-1", Component: "open", Name: "open", Scope: "/items/0", Gesture: true}); r.Status != "refused" {
		t.Fatalf("a frozen surface: %+v", r)
	}
}

// A generated screen is checked before it is shown; a draft missing what
// the task needs goes back to the generator with the errors.
func TestGenerateValidateRepair(t *testing.T) {
	data := Normalize(map[string]any{"summary": "s", "items": []any{map[string]any{"id": h(1), "title": Fence("anya", "t"), "author": "anya", "net": 2.0, "age": "1 h", "why": "w"}}})
	task := Task{Question: "show me this week's hot posts as a table", Actions: BaseActions(), Require: Requirements{Actions: []string{"open"}, Components: []string{"Table"}}}
	s, drafts, err := GenerateSurface(MockGenerator{}, task, "gen-1", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 2 || len(drafts[0].Errors) == 0 || !strings.Contains(drafts[0].Errors[0], `missing required action "open"`) || len(drafts[1].Errors) != 0 {
		t.Fatalf("drafts: %+v", drafts)
	}
	if errs := ValidateSurface(s, task.Require); len(errs) > 0 {
		t.Fatal(errs)
	}
	// a model that puts data in a label is refused at compile
	if _, errs := CompileOpenUI(`root = Stack([b], "column")` + "\n" + `b = Button($title, "tip")`); len(errs) == 0 || !strings.Contains(errs[0], "literal") {
		t.Fatalf("data label: %v", errs)
	}
	if _, errs := CompileOpenUI(`root = Iframe("https://x")`); len(errs) == 0 {
		t.Fatal("unknown component compiled")
	}
}

// The demo's sources (cmd/ternchain/testdata/reddit/src) are valid modules and packages.
func TestDemoSourcesCompile(t *testing.T) {
	ms, _ := filepath.Glob("../../cmd/ternchain/testdata/reddit/src/rankers/*/manifest.json")
	if len(ms) != 3 {
		t.Fatalf("rankers: %v", ms)
	}
	for _, f := range ms {
		var m map[string]any
		b, _ := os.ReadFile(f)
		json.Unmarshal(b, &m)
		m["author"], m["payee"] = "x", "x"
		b, _ = json.Marshal(m)
		r, err := CompileRanker(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		in := map[string]any{}
		for k := range FeedInputs {
			in[k] = 1.0
		}
		in["item.hidden"], in["author.agent"], in["item.has_card"], in["author.citizen"] = false, false, false, false
		in["item.reports"], in["author.posts_hour"] = 0.0, 1.0
		out, _, err := r.Rank([]Candidate{{ID: "a", Inputs: in}})
		if err != nil || len(out) != 1 || out[0].Reason == "" {
			t.Fatalf("%s: %v %+v", f, err, out)
		}
		t.Logf("%s: %s", r.M.Name, out[0].Reason)
	}
	key, _ := crypto.GenerateKey()
	d := filepath.Join(t.TempDir(), "rc")
	copyDir("../../cmd/ternchain/testdata/reddit/src/reddit-classic", d)
	b, err := Seal(d, "anya", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPackage(b); err != nil {
		t.Fatal(err)
	}
	cards, _ := os.ReadFile("../../cmd/ternchain/testdata/reddit/src/reddit-cards/feed.json")
	tpl, err := ParseScreen(cards)
	if err != nil {
		t.Fatal(err)
	}
	if errs := ValidateSurface(&Surface{Components: tpl.Components, Actions: BaseActions(), Data: map[string]any{}}, TemplateNeeds["community.feed"]); len(errs) > 0 {
		t.Fatal(errs)
	}
}
