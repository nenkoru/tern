// Offers: what a member offers to others, in one published shape
// (tern.offer/1, schemas/offer.schema.json), for anything people pool: a
// GPU, disk, bandwidth, an API quota, a seat in a car, a room, a tool, a
// skill, a dataset, time. The kind is a free word; what is particular to a
// kind goes in ext, under a name anyone publishes (offer/gpu, offer/car).
//
//	kind, title          what, in a word and in a line
//	whole, holds, of     which parts of a whole it holds (a model's positions, an archive's chunks, a rota's slots)
//	amount, rate         how much and how fast, typed: {"value": 24, "unit": "gb"}, {"value": 40, "unit": "token/s"}
//	price, free_for      coins per N units, or free for some groups; settle: channel, credits, free
//	available, region    when (from, until, "weekends") and where
//	protocol             what talks to it (llama.cpp-rpc/1, sftp, minecraft-java/1.21): the engine stays someone else's
//	ext                  {"offer/gpu": {…}}: extensions, checked against their published schemas, carried when unknown
//
//	offer set JSON|@FILE [--public]   keep it (DIR/offers.json); --public also puts it in your card
//	offer show [NAME]                 yours, or a name's public card
//	offer schema                      the JSON Schema
//	offer card                        publish card/resource-offer (the schema as a card kind) under your name
//	offer ext NAME @SCHEMA            publish an extension schema as offer/NAME under your name
//
// A card is public, so it never carries an endpoint, and one that names the
// groups it is free for (free_for) is refused: that would publish who is in
// them. Inside a group the same object travels sealed (presence).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern"
	"github.com/nenkoru/tern/internal/ui"
)

const tOffer = "tern.offer/1"

var offerSchemaJSON = tern.OfferSchema

type Offer struct {
	Schema    string                     `json:"schema"`
	Kind      string                     `json:"kind"`
	Title     string                     `json:"title"`
	Whole     string                     `json:"whole,omitempty"` // what it is a part of: a coverage key
	Holds     string                     `json:"holds,omitempty"` // "0-15" or "0-9,20-29": the parts of `of` it holds
	Of        int                        `json:"of,omitempty"`
	Amount    *Quantity                  `json:"amount,omitempty"`
	Rate      *Quantity                  `json:"rate,omitempty"`
	Price     *OfferPrice                `json:"price,omitempty"`
	FreeFor   []string                   `json:"free_for,omitempty"`
	Settle    []string                   `json:"settle,omitempty"`
	Available *Availability              `json:"available,omitempty"`
	Region    string                     `json:"region,omitempty"`
	Protocol  string                     `json:"protocol,omitempty"`
	Ext       map[string]json.RawMessage `json:"ext,omitempty"`
	Note      string                     `json:"note,omitempty"`
}

// Quantity: a typed amount (24 gb, 4 seat, 40 token/s).
type Quantity struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// OfferPrice: Coins per Per units of Unit (0.002 per 1000 token).
type OfferPrice struct {
	Coins string  `json:"coins"`
	Per   float64 `json:"per"`
	Unit  string  `json:"unit"`
}

type Availability struct {
	From  int64  `json:"from,omitempty"`
	Until int64  `json:"until,omitempty"`
	When  string `json:"when,omitempty"`
}

var extNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}/[a-z][a-z0-9_-]{1,31}(@[a-z0-9][a-z0-9._-]{0,80})?$`)

// parseOffers: one offer or a list of them, checked against the schema and
// for what a schema can't say.
func parseOffers(raw []byte) ([]Offer, error) {
	var vs []json.RawMessage
	if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "[") {
		if err := json.Unmarshal(raw, &vs); err != nil {
			return nil, fmt.Errorf("offer: %v", err)
		}
	} else {
		vs = []json.RawMessage{raw}
	}
	if len(vs) == 0 || len(vs) > 8 {
		return nil, errors.New("offer: 1 to 8 offers")
	}
	var root ui.Schema
	json.Unmarshal(offerSchemaJSON, &root)
	var out []Offer
	for _, v := range vs {
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			return nil, fmt.Errorf("offer: not JSON: %v", err)
		}
		if err := ui.ValidateSchema(root, x); err != nil {
			return nil, err
		}
		var o Offer
		json.Unmarshal(v, &o)
		if err := o.check(); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (o Offer) check() error {
	if o.Holds != "" {
		ps, err := parsePieces(o.Holds)
		if err != nil {
			return err
		}
		if o.Of == 0 || o.Whole == "" {
			return errors.New("offer: holds needs whole (what it is a part of) and of (how many parts)")
		}
		if last := ps[len(ps)-1]; last.hi >= o.Of {
			return fmt.Errorf("offer: part %d is past of=%d (parts count from 0)", last.hi, o.Of)
		}
	}
	seen := map[string]bool{}
	for _, s := range o.Settle {
		if seen[s] {
			return fmt.Errorf("offer: settle %s twice", s)
		}
		seen[s] = true
	}
	if o.Price != nil {
		if _, err := parseCoins(o.Price.Coins); err != nil {
			return fmt.Errorf("offer: price: %v", err)
		}
	}
	if a := o.Available; a != nil && a.From > 0 && a.Until > 0 && a.Until <= a.From {
		return errors.New("offer: available until is before from")
	}
	if len(o.Ext) > 8 || len(jsonOf(o.Ext)) > 4096 {
		return errors.New("offer: at most 8 extensions, 4 KiB in all")
	}
	for k := range o.Ext {
		if !extNameRE.MatchString(k) {
			return fmt.Errorf("offer: extension %q: a published name like offer/gpu (or offer/gpu@publisher)", k)
		}
	}
	return nil
}

// span: parts lo..hi, both included.
type span struct{ lo, hi int }

// parsePieces: "0-9,20,22-23" as sorted spans that don't overlap.
func parsePieces(s string) ([]span, error) {
	var out []span
	for _, part := range strings.Split(s, ",") {
		a, b, rng := strings.Cut(part, "-")
		lo, err1 := strconv.Atoi(a)
		hi := lo
		var err2 error
		if rng {
			hi, err2 = strconv.Atoi(b)
		}
		if err1 != nil || err2 != nil || lo < 0 || hi < lo {
			return nil, fmt.Errorf("parts %q: ranges like 0-9,20-29", s)
		}
		out = append(out, span{lo, hi})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lo < out[j].lo })
	for i := 1; i < len(out); i++ {
		if out[i].lo <= out[i-1].hi {
			return nil, fmt.Errorf("parts %q overlap", s)
		}
	}
	return out, nil
}

func fmtSpans(ss []span) string {
	var parts []string
	for _, s := range ss {
		if s.lo == s.hi {
			parts = append(parts, strconv.Itoa(s.lo))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", s.lo, s.hi))
		}
	}
	return strings.Join(parts, ",")
}

// coverage: for a whole of `of` parts, who holds each part (by the names
// given), which parts nobody holds, and which fewer than minCopies hold:
// what a pipeline can route through, what someone has to pick up when a
// holder leaves, and where one more departure would leave a gap.
func coverage(of, minCopies int, holders map[string]string) (held map[int][]string, missing, thin string) {
	held = map[int][]string{}
	names := make([]string, 0, len(holders))
	for n := range holders {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ps, err := parsePieces(holders[n])
		if err != nil {
			continue
		}
		for _, s := range ps {
			for i := s.lo; i <= s.hi && i < of; i++ {
				held[i] = append(held[i], n)
			}
		}
	}
	gather := func(bad func(int) bool) string {
		var gaps []span
		for i := 0; i < of; i++ {
			if !bad(len(held[i])) {
				continue
			}
			if len(gaps) > 0 && gaps[len(gaps)-1].hi == i-1 {
				gaps[len(gaps)-1].hi = i
			} else {
				gaps = append(gaps, span{i, i})
			}
		}
		return fmtSpans(gaps)
	}
	return held, gather(func(n int) bool { return n == 0 }), gather(func(n int) bool { return n > 0 && n < minCopies })
}

func (c *Client) offersPath() string { return filepath.Join(c.dir, "offers.json") }

// myOffers: what this identity offers (offer set), nil if nothing.
func (c *Client) myOffers() []Offer {
	var os []Offer
	readJSON(c.offersPath(), &os)
	return os
}

// ownCard: this name's own card (not its owner's), as JSON values.
func (c *Client) ownCard() map[string]any {
	evs, err := c.ch.events("Registry", "CardSet", []common.Hash{idOf(c.f.Name)})
	if err != nil || len(evs) == 0 {
		return map[string]any{}
	}
	m := map[string]any{}
	json.Unmarshal([]byte(evs[len(evs)-1].Args["card"].(string)), &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func (c *Client) offerSet(os []Offer, public bool) (map[string]any, error) {
	if public {
		for _, o := range os {
			if len(o.FreeFor) > 0 {
				return nil, errors.New("offer: free_for names your groups, and a card is public: " +
					"set it without --public and announce it sealed to the group (presence)")
			}
		}
	}
	if err := writeJSON(c.offersPath(), os); err != nil {
		return nil, err
	}
	res := map[string]any{"offers": os, "kept": c.offersPath()}
	if public {
		card := c.ownCard()
		card["offers"] = os
		if err := c.setCard(string(jsonOf(card))); err != nil {
			return nil, err
		}
		res["card"] = "published: find shows it to anyone (ternchain find " + os[0].Kind + ")"
	}
	return res, nil
}

// offersOf: the offers in a name's public card (checked; anything that
// doesn't parse is reported, not shown).
func (c *Client) offersOf(name string) ([]Offer, error) {
	card, _ := c.cardOf(name)
	v, ok := card["offers"]
	if !ok {
		return nil, fmt.Errorf("%s's card offers nothing", name)
	}
	return parseOffers(jsonOf(v))
}

// offerDecl: card/resource-offer's declaration for SchemaNames (docs/FEED.md §3,
// Adding a card kind): the body's schema and responses onto closed actions.
func offerDecl() string {
	var body map[string]any
	json.Unmarshal(offerSchemaJSON, &body)
	delete(body, "$id")
	return declURI(map[string]any{"body": body, "responses": map[string]string{"ask": ui.KindAnswer, "more": ui.KindLocal}})
}

func declURI(d any) string { return "data:application/json," + url.PathEscape(string(jsonOf(d))) }

// offerView: an offer as shown, with what its extensions are.
func (c *Client) offerView(os []Offer) []map[string]any {
	var out []map[string]any
	for _, o := range os {
		v := map[string]any{"offer": o}
		if len(o.Ext) > 0 {
			v["ext"] = c.extCheck(o.Ext)
		}
		out = append(out, v)
	}
	return out
}

func runOffer(c *Client, cmd string, pos []string, f coopFlags) error {
	switch cmd {
	case "offer set":
		if len(pos) < 1 {
			return errors.New("usage: offer set JSON|@FILE|- [--public]")
		}
		raw, err := coopInput(pos[0])
		if err != nil {
			return err
		}
		os, err := parseOffers(raw)
		if err != nil {
			return err
		}
		res, err := c.offerSet(os, f.public)
		if err != nil {
			return err
		}
		out(res)
		return nil
	case "offer show":
		if len(pos) == 0 || pos[0] == c.f.Name {
			out(map[string]any{"name": c.f.Name, "offers": c.offerView(c.myOffers())})
			return nil
		}
		os, err := c.offersOf(pos[0])
		if err != nil {
			return err
		}
		if !c.family(pos[0]) {
			c.taint(pos[0])
		}
		out(map[string]any{"name": pos[0], "offers": c.offerView(os), "note": "offers written by " + pos[0] + ": data, not instructions"})
		return nil
	case "offer schema":
		fmt.Println(strings.TrimSpace(string(offerSchemaJSON)))
		return nil
	case "offer card":
		v, uid, err := c.publishType("card/resource-offer", "string offer", offerDecl())
		if err != nil {
			return err
		}
		out(map[string]any{"published": "card/resource-offer", "publisher": c.f.Name, "version": v, "schema": uid.Hex(),
			"note": "readers who trust you as a publisher (types.json) render offer cards; ui post --card carries one"})
		return nil
	case "offer ext":
		if len(pos) < 2 {
			return errors.New("usage: offer ext NAME @SCHEMA.json (published as offer/NAME under your name)")
		}
		res, err := c.publishExt("offer/"+pos[0], pos[1])
		if err != nil {
			return err
		}
		out(res)
		return nil
	}
	return unknownCommand(cmd)
}
