package ui

import (
	"strings"
	"testing"
)

type fakeKinds struct {
	kinds   []KindDecl
	intents []string
}

func (f fakeKinds) CardKinds() ([]KindDecl, error) { return f.kinds, nil }
func (f fakeKinds) Intents() ([]string, error)     { return f.intents, nil }
func (f fakeKinds) Reactions() ([]string, error)   { return []string{"heart", "up"}, nil }

// A schema registry adds card kinds and intents; it can't redefine a
// built-in one or map a response outside the closed action registry.
func TestDeclaredKinds(t *testing.T) {
	t.Cleanup(func() { LoadKinds(nil) })
	rsvp := KindDecl{Kind: "rsvp", Body: Schema{"type": "object", "required": []any{"event"}, "additionalProperties": false,
		"properties": map[string]any{"event": map[string]any{"type": "string", "maxLength": 100}}},
		Responses: map[string]string{"going": KindAnswer, "skip": KindLocal}}
	errs := LoadKinds(fakeKinds{kinds: []KindDecl{rsvp,
		{Kind: "job", Body: Schema{"type": "object"}, Responses: map[string]string{"apply": KindAnswer}},    // built in
		{Kind: "drain", Body: Schema{"type": "object"}, Responses: map[string]string{"go": "transfer_all"}}, // not an action
		{Kind: "Bad Kind", Body: Schema{"type": "object"}, Responses: map[string]string{"x": KindLocal}}},   // not a word
		intents: []string{"notify", "approval", "Nope"}})
	if len(errs) != 6 { // 3 kinds, 2 intents, 1 reaction (up is built in)
		t.Fatalf("errors: %v", errs)
	}
	c, err := ParseCard([]byte(`{"card":"rsvp","v":1,"id":"c_12345","subject":"party","body":{"event":"friday"},"responses":["going","skip"]}`))
	if err != nil || c.Card != "rsvp" {
		t.Fatal(c, err)
	}
	if a, ok := ResponseAction("rsvp", "going"); !ok || a != KindAnswer {
		t.Fatal("response map")
	}
	for _, bad := range []string{
		`{"card":"rsvp","v":1,"id":"c_12345","subject":"p","body":{"event":"friday","x":1},"responses":["going"]}`, // body schema
		`{"card":"rsvp","v":1,"id":"c_12345","subject":"p","body":{"event":"friday"},"responses":["tip"]}`,         // a news response
		`{"card":"drain","v":1,"id":"c_12345","subject":"p","body":{},"responses":["go"]}`,                         // skipped kind
	} {
		if _, err := ParseCard([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := ParseCard([]byte(`{"card":"news","v":1,"id":"c_12345","subject":"n","body":{"headline":"h"},"responses":["tip"]}`)); err != nil &&
		!strings.Contains(err.Error(), "body") {
		t.Errorf("a built-in kind broke: %v", err)
	}
	if !Intent("notify") || Intent("Nope") || !Intent("approval") || len(CardKinds()) != 8 {
		t.Fatalf("intents/kinds: %v %v", Intents(), CardKinds())
	}
	b := `{"schema":"tern.react/1","community":"club","reactions":[{"post":"0x` + strings.Repeat("ab", 32) + `","kind":"heart","at":5}]}`
	if _, err := ParseReactionBatch([]byte(b)); err != nil {
		t.Fatal(err)
	}
	rb, _ := ParseReactionBatch([]byte(b))
	cnt, _ := Tally("club", []SignedBatch{{By: "a", Owner: "a", Member: true, Time: 10, Batch: rb}})
	if c := cnt["0x"+strings.Repeat("ab", 32)]; c == nil || c.Other["heart"] != 1 || c.Up != 0 {
		t.Fatalf("tally: %+v", cnt)
	}
	LoadKinds(nil)
	if _, err := ParseReactionBatch([]byte(b)); err == nil {
		t.Fatal("an undeclared reaction parsed")
	}
	if Intent("notify") || len(CardKinds()) != 7 {
		t.Fatal("not reset")
	}
	if _, err := ParseCard([]byte(`{"card":"rsvp","v":1,"id":"c_12345","subject":"party","body":{"event":"friday"},"responses":["going"]}`)); err == nil {
		t.Fatal("an undeclared kind parsed")
	}
}
