package main

import (
	"strings"
	"sync"
	"testing"

	"github.com/nenkoru/tern/internal/ui"
)

const gpuOffer = `{"schema":"tern.offer/1","kind":"gpu","title":"llama 8B, positions 0-15","whole":"llama-3.1-8b-q8",
 "holds":"0-15","of":32,"amount":{"value":12,"unit":"gb-vram"},"rate":{"value":40,"unit":"token/s"},
 "price":{"coins":"0.002","per":1000,"unit":"token"},"settle":["channel","credits"],"protocol":"llama.cpp-rpc/1",
 "region":"eu-central","ext":{"offer/gpu":{"vram_gb":12,"device":"rtx 3060"}}}`

const seatOffer = `{"schema":"tern.offer/1","kind":"seat","title":"2 seats, Mersin to Adana, Saturday 9:00","amount":{"value":2,"unit":"seat"},
 "free_for":["neighbours"],"settle":["credits"],"available":{"from":1793001600,"until":1793008800,"when":"Saturday morning"},"region":"tr-mersin"}`

// An offer of anything is checked against the published schema and for
// what a schema can't say (parts inside the whole, extension names, a price).
func TestOfferParse(t *testing.T) {
	os, err := parseOffers([]byte(gpuOffer))
	if err != nil || len(os) != 1 || os[0].Holds != "0-15" || os[0].Amount.Value != 12 || os[0].Price.Per != 1000 || len(os[0].Ext) != 1 {
		t.Fatalf("a GPU offer: %+v %v", os, err)
	}
	os, err = parseOffers([]byte(`[` + seatOffer + `,{"schema":"tern.offer/1","kind":"storage","title":"2 TB for the family's backups","amount":{"value":2000,"unit":"gb"}}]`))
	if err != nil || len(os) != 2 || os[0].Kind != "seat" || os[0].Available.When != "Saturday morning" {
		t.Fatalf("a seat and a disk: %+v %v", os, err)
	}
	for bad, want := range map[string]string{
		`{"schema":"tern.offer/1","kind":"gpu","title":"x","whole":"m","holds":"0-40","of":32}`:          "past of",
		`{"schema":"tern.offer/1","kind":"gpu","title":"x","whole":"m","holds":"0-9,5-12","of":32}`:      "overlap",
		`{"schema":"tern.offer/1","kind":"gpu","title":"x","holds":"0-9","of":32}`:                       "needs whole",
		`{"schema":"tern.offer/1","kind":"Car","title":"x"}`:                                             "doesn't match",
		`{"schema":"tern.offer/1","kind":"car","title":"x","price":{"coins":"1e3","per":1,"unit":"km"}}`: "doesn't match",
		`{"schema":"tern.offer/1","kind":"gpu","title":"x","endpoint":"10.0.0.2:50052"}`:                 "unknown field",
		`{"schema":"tern.offer/1","kind":"gpu","title":"x","settle":["credits","credits"]}`:              "twice",
		`{"schema":"tern.offer/1","kind":"car","title":"x","ext":{"car":{"seats":4}}}`:                   "a published name",
		`{"schema":"tern.offer/1","kind":"car","title":"x","available":{"from":10,"until":5}}`:           "before from",
		`{"schema":"tern.offer/2","kind":"gpu","title":"x"}`:                                             "schema",
	} {
		if _, err := parseOffers([]byte(bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", bad, err, want)
		}
	}
}

// Who holds which parts of a whole, what nobody holds (what someone must
// pick up) and what only one holds (one departure from a gap).
func TestOfferCoverage(t *testing.T) {
	held, missing, thin := coverage(32, 2, map[string]string{"gpu.oscar": "0-9", "box.nina": "10-19", "pc.ada": "15-25"})
	if missing != "26-31" || thin != "0-14,20-25" || len(held[15]) != 2 || held[15][0] != "box.nina" || len(held[3]) != 1 {
		t.Fatalf("coverage: %v missing %q thin %q", held, missing, thin)
	}
	if _, missing, thin := coverage(4, 1, map[string]string{"a": "0", "b": "2"}); missing != "1,3" || thin != "" {
		t.Fatalf("gaps: %q %q", missing, thin)
	}
}

// A public offer goes into the card (find shows it, anyone reads it); one
// naming the groups it is free for stays off the card. An extension is
// checked against the schema its publisher published, and carried when
// nobody published one.
func TestOfferCard(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	w.must(oscar.setCard(`{"about":"oscar's machines"}`))
	os, err := parseOffers([]byte(gpuOffer))
	w.must(err)
	_, err = oscar.offerSet(os, true)
	w.must(err)
	if card := oscar.ownCard(); card["about"] != "oscar's machines" || card["offers"] == nil {
		t.Fatalf("the card lost its fields or got no offers: %v", card)
	}
	got, err := nina.offersOf("oscar")
	if err != nil || len(got) != 1 || got[0].Whole != "llama-3.1-8b-q8" {
		t.Fatalf("nina reads oscar's offer: %+v %v", got, err)
	}
	fs, err := nina.find("gpu llama")
	w.must(err)
	if len(fs) != 1 || fs[0].Name != "oscar" {
		t.Fatalf("find: %+v", fs)
	}
	if st := nina.extCheck(got[0].Ext); !strings.HasPrefix(st["offer/gpu"], "unchecked") {
		t.Fatalf("an extension nobody published: %v", st)
	}
	extOnce = sync.Once{}
	_, err = oscar.publishExt("offer/gpu", "@"+repo("schemas/ext/offer-gpu.schema.json"))
	w.must(err)
	if st := nina.extCheck(got[0].Ext); st["offer/gpu"] != "checked against oscar's schema" {
		t.Fatalf("a published extension: %v", st)
	}
	extOnce = sync.Once{}
	bad, _ := parseOffers([]byte(strings.Replace(gpuOffer, `"vram_gb":12`, `"vram_gb":"twelve"`, 1)))
	if st := nina.extCheck(bad[0].Ext); !strings.HasPrefix(st["offer/gpu"], "invalid") {
		t.Fatalf("an extension against its schema: %v", st)
	}
	if st := nina.extCheck(bad[0].Ext); !strings.Contains(st["offer/gpu"], "oscar") {
		t.Fatal(st)
	}

	seats, err := parseOffers([]byte(seatOffer))
	w.must(err)
	if _, err := oscar.offerSet(seats, true); err == nil || !strings.Contains(err.Error(), "free_for") {
		t.Fatalf("a public card naming groups: %v", err)
	}
	if _, err := oscar.offerSet(seats, false); err != nil || len(oscar.myOffers()) != 1 || oscar.myOffers()[0].FreeFor[0] != "neighbours" {
		t.Fatalf("kept locally: %v", err)
	}

	// card/resource-offer: a kind any client can load (docs/FEED.md §3), published under a name
	d, err := declOf(offerDecl())
	w.must(err)
	d.Kind = "resource-offer"
	if errs := ui.LoadKinds(fakeKinds{d}); len(errs) > 0 {
		t.Fatalf("card/resource-offer's declaration: %v", errs)
	}
	defer ui.LoadKinds(nil)
	if a, ok := ui.ResponseAction("resource-offer", "ask"); !ok || a != ui.KindAnswer {
		t.Fatal("responses")
	}
	if _, _, err := oscar.publishType("card/resource-offer", "string offer", offerDecl()); err != nil {
		t.Fatal(err)
	}
}

type fakeKinds []ui.KindDecl

func (f fakeKinds) CardKinds() ([]ui.KindDecl, error) { return f, nil }
func (f fakeKinds) Intents() ([]string, error)        { return nil, nil }
func (f fakeKinds) Reactions() ([]string, error)      { return nil, nil }

// The module is off until enabled; its commands are dispatched once on.
func TestCoopModule(t *testing.T) {
	t.Setenv("TERNCHAIN_MODULES", "")
	dir := t.TempDir()
	if err := checkModule("offer set", dir); err == nil || !strings.Contains(err.Error(), "optional module") {
		t.Fatalf("coop off: %v", err)
	}
	c := &Client{Ident: &Ident{dir: dir}}
	for _, tl := range toolsFor(c) {
		if strings.HasPrefix(tl.Name, "tern_coop_") {
			t.Fatalf("%s listed while off", tl.Name)
		}
	}
	t.Setenv("TERNCHAIN_MODULES", "coop")
	if checkModule("offer set", dir) != nil || !knownCommands()["offer show"] || !knownCommands()["offer ext"] {
		t.Fatal("coop on")
	}
}
