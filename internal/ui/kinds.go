package ui

// Card kinds, mail intents and reactions from a schema registry (BRICKS D2). Today's
// seven card kinds and seven intents are built in and can't be redefined; a
// registry (the network's schema registry, or anything that implements
// KindSource) adds kinds and intents. A declared card kind brings its body's
// JSON Schema and a map from its responses onto the CLOSED action registry
// (broker.go): a schema can add a kind, never an action. A kind without a
// template in the UI package in use is drawn as card.generic.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// KindDecl: a card kind a registry declares.
type KindDecl struct {
	Kind      string            `json:"kind"`
	Body      Schema            `json:"body"`      // the body's JSON Schema (the subset schema.go checks)
	Responses map[string]string `json:"responses"` // response => action kind (KindAnswer, KindLocal, …)
	Source    string            `json:"source,omitempty"`
}

// KindSource: where declared card kinds and intents come from. The
// registry's client implements it; nil or empty: the built-in kinds only.
type KindSource interface {
	CardKinds() ([]KindDecl, error)
	Intents() ([]string, error)
	Reactions() ([]string, error) // reaction kinds beyond up, down and none (counted per owner, latest wins)
}

// BuiltinReactions: the reactions every client counts ("none" takes one back).
var BuiltinReactions = []string{"up", "down", "none"}

// BuiltinIntents: the mail intents every client knows. "approval" is
// special (only an owner's counts) and can't be declared by a registry.
var BuiltinIntents = []string{"request", "question", "inform", "answer", "offer", "result", "approval"}

var (
	kindsMu      sync.RWMutex
	extraKinds   = map[string]KindDecl{}
	extraIntents = map[string]bool{}
	extraReacts  = map[string]bool{}
	cardRoot     Schema // tern.card/1 widened to the declared kinds (nil: none declared)
	reactRoot    Schema // tern.react/1 widened to the declared reactions (nil: none declared)
)

var declName = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

// LoadKinds replaces the declared kinds and intents with what src declares.
// A declaration that redefines a built-in kind or intent, maps a response
// to an action outside the closed registry, or carries a bad schema is
// skipped and reported; the rest are used.
func LoadKinds(src KindSource) []error {
	var errs []error
	kinds := map[string]KindDecl{}
	intents := map[string]bool{}
	reacts := map[string]bool{}
	if src != nil {
		ds, err := src.CardKinds()
		if err != nil {
			errs = append(errs, err)
		}
		for _, d := range ds {
			if err := checkDecl(d); err != nil {
				errs = append(errs, err)
				continue
			}
			kinds[d.Kind] = d
		}
		is, err := src.Intents()
		if err != nil {
			errs = append(errs, err)
		}
		for _, i := range is {
			if !declName.MatchString(i) || builtinIntent(i) {
				errs = append(errs, fmt.Errorf("intent %q: built in, or not a lower-case word", i))
				continue
			}
			intents[i] = true
		}
		rs, err := src.Reactions()
		if err != nil {
			errs = append(errs, err)
		}
		for _, r := range rs {
			if !declName.MatchString(r) || builtinReaction(r) {
				errs = append(errs, fmt.Errorf("reaction %q: built in, or not a lower-case word", r))
				continue
			}
			reacts[r] = true
		}
	}
	kindsMu.Lock()
	defer kindsMu.Unlock()
	extraKinds, extraIntents, extraReacts, cardRoot, reactRoot = kinds, intents, reacts, nil, nil
	if len(kinds) > 0 {
		cardRoot = widenCard(kinds)
	}
	if len(reacts) > 0 {
		var root Schema
		b, _ := json.Marshal(Schemas()[SchemaReact])
		json.Unmarshal(b, &root)
		kind := root["properties"].(map[string]any)["reactions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)
		en, _ := kind["enum"].([]any)
		for _, r := range keys(reacts) {
			en = append(en, r)
		}
		kind["enum"] = en
		reactRoot = root
	}
	return errs
}

func builtinReaction(r string) bool {
	for _, b := range BuiltinReactions {
		if b == r {
			return true
		}
	}
	return false
}

// ReactionKind: a reaction this client counts (built in or declared).
func ReactionKind(r string) bool {
	if builtinReaction(r) {
		return true
	}
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	return extraReacts[r]
}

// validateReact: a reaction batch against tern.react/1, widened to the declared reactions.
func validateReact(raw []byte) error {
	kindsMu.RLock()
	root := reactRoot
	kindsMu.RUnlock()
	if root == nil {
		return ValidateJSON(SchemaReact, "", raw)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("%s: not JSON: %v", SchemaReact, err)
	}
	var errs []string
	check(root, root, v, "", &errs)
	if len(errs) > 0 {
		return fmt.Errorf("%s: %v", SchemaReact, errs)
	}
	return nil
}

func builtinIntent(i string) bool {
	for _, b := range BuiltinIntents {
		if b == i {
			return true
		}
	}
	return false
}

func checkDecl(d KindDecl) error {
	if !declName.MatchString(d.Kind) {
		return fmt.Errorf("card kind %q: not a lower-case word", d.Kind)
	}
	if _, ok := CardResponses[d.Kind]; ok || d.Kind == "generic" {
		return fmt.Errorf("card kind %q is built in: a registry can't redefine it", d.Kind)
	}
	if len(d.Responses) == 0 || len(d.Responses) > 6 {
		return fmt.Errorf("card kind %s: 1 to 6 responses", d.Kind)
	}
	for r, a := range d.Responses {
		if !declName.MatchString(r) {
			return fmt.Errorf("card kind %s: response %q is not a lower-case word", d.Kind, r)
		}
		if !Known(a) {
			return fmt.Errorf("card kind %s: response %s maps to %q, not an action of the closed registry", d.Kind, r, a)
		}
	}
	if d.Body == nil {
		return fmt.Errorf("card kind %s: no body schema", d.Kind)
	}
	b, err := json.Marshal(d.Body)
	if err != nil {
		return fmt.Errorf("card kind %s: body schema: %v", d.Kind, err)
	}
	if len(b) > 32<<10 {
		return fmt.Errorf("card kind %s: body schema over 32 KiB", d.Kind)
	}
	// A body that refers to itself (directly) is a loop, not a schema; any
	// deeper loop through other kinds stops at check's depth cap (audit 6 CM-1).
	var bad error
	walkRefs(map[string]any(d.Body), func(ref string) {
		if name, ok := strings.CutPrefix(ref, "#/$defs/"); !ok || name == d.Kind {
			bad = fmt.Errorf("card kind %s: body schema $ref %q: only #/$defs/OTHER-KIND, never itself", d.Kind, ref)
		}
	})
	return bad
}

// walkRefs: every $ref string in a JSON value.
func walkRefs(v any, f func(string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			if r, ok := y.(string); ok && k == "$ref" {
				f(r)
				continue
			}
			walkRefs(y, f)
		}
	case Schema:
		walkRefs(map[string]any(x), f)
	case []any:
		for _, y := range x {
			walkRefs(y, f)
		}
	}
}

// widenCard: tern.card/1 with the declared kinds and responses allowed and
// their body schemas as $defs.
func widenCard(kinds map[string]KindDecl) Schema {
	var root Schema
	b, _ := json.Marshal(Schemas()[SchemaCard])
	json.Unmarshal(b, &root)
	props := root["properties"].(map[string]any)
	card := props["card"].(map[string]any)
	resp := props["responses"].(map[string]any)["items"].(map[string]any)
	ks, _ := card["enum"].([]any)
	rs, _ := resp["enum"].([]any)
	have := map[string]bool{}
	for _, r := range rs {
		have[fmt.Sprint(r)] = true
	}
	ds := root["$defs"].(map[string]any)
	for _, k := range keys(kinds) {
		d := kinds[k]
		ks = append(ks, k)
		bb, _ := json.Marshal(d.Body)
		var body map[string]any
		json.Unmarshal(bb, &body)
		ds[k] = body
		for _, r := range keys(d.Responses) {
			if !have[r] {
				have[r] = true
				rs = append(rs, r)
			}
		}
	}
	card["enum"], resp["enum"] = ks, rs
	return root
}

// validateCard: a card envelope (def "") or a kind's body against tern.card/1,
// widened to the declared kinds.
func validateCard(def string, raw []byte) error {
	kindsMu.RLock()
	root := cardRoot
	kindsMu.RUnlock()
	if root == nil {
		return ValidateJSON(SchemaCard, def, raw)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("%s: not JSON: %v", SchemaCard, err)
	}
	s := root
	if def != "" {
		d, ok := defs(root)[def]
		if !ok {
			return fmt.Errorf("schema %s has no $defs.%s", SchemaCard, def)
		}
		s = d
	}
	var errs []string
	check(root, s, v, "", &errs)
	if len(errs) > 0 {
		return fmt.Errorf("%s: %v", SchemaCard, errs)
	}
	return nil
}

// ResponseAction: the action kind a card kind's response maps to.
func ResponseAction(kind, response string) (string, bool) {
	if m, ok := CardResponses[kind]; ok {
		a, ok := m[response]
		return a, ok
	}
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	d, ok := extraKinds[kind]
	if !ok {
		return "", false
	}
	a, ok := d.Responses[response]
	return a, ok
}

func responsesOf(kind string) map[string]string {
	if m, ok := CardResponses[kind]; ok {
		return m
	}
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	return extraKinds[kind].Responses
}

// CardKinds: every card kind this client knows: built in, then declared.
func CardKinds() []string {
	out := keys(CardResponses)
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	ex := keys(extraKinds)
	sort.Strings(ex)
	return append(out, ex...)
}

// DeclaredKind: a kind a registry declared (not built in), and where from.
func DeclaredKind(kind string) (KindDecl, bool) {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	d, ok := extraKinds[kind]
	return d, ok
}

// Intent: a mail intent this client knows (built in or declared).
func Intent(i string) bool {
	if builtinIntent(i) {
		return true
	}
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	return extraIntents[i]
}

// Intents: every intent this client knows.
func Intents() []string {
	out := append([]string(nil), BuiltinIntents...)
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	return append(out, keys(extraIntents)...)
}
