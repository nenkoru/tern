// Package ui: the open feed, typed cards, ranking modules, UI packages
// (A2UI templates: data, not code), the action broker in front of the
// agent's guard, and the chat-first web shell (docs/FEED.md, cmd/ternchain/testdata/reddit/).
//
// Everything here is pure: the chain, the guard and the keys come in through
// small interfaces, so the rules are tested without a network.
package ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed schemas/*.json
var schemaFS embed.FS

// Schema: a JSON Schema document (the subset the network's schemas use:
// type, const, enum, required, properties, additionalProperties, items,
// min/maxItems, min/maxLength, minimum/maximum, pattern, $ref to #/$defs/…).
type Schema map[string]any

var (
	schemasOnce sync.Once
	schemas     map[string]Schema // by $id
)

// Schemas: the embedded schemas by $id (tern.schema/feed@1, tern.react/1, tern.card/1,
// tern.ui-package/1, tern.ranker/1).
func Schemas() map[string]Schema {
	schemasOnce.Do(func() {
		schemas = map[string]Schema{}
		ents, _ := schemaFS.ReadDir("schemas")
		for _, e := range ents {
			b, _ := schemaFS.ReadFile("schemas/" + e.Name())
			var s Schema
			if err := json.Unmarshal(b, &s); err != nil {
				panic("schema " + e.Name() + ": " + err.Error())
			}
			schemas[s["$id"].(string)] = s
		}
	})
	return schemas
}

// SchemaFile: the raw bytes of one embedded schema, by $id.
func SchemaFile(id string) []byte {
	ents, _ := schemaFS.ReadDir("schemas")
	for _, e := range ents {
		b, _ := schemaFS.ReadFile("schemas/" + e.Name())
		var s Schema
		if json.Unmarshal(b, &s) == nil && s["$id"] == id {
			return b
		}
	}
	return nil
}

// Validate checks a JSON value (as decoded by encoding/json into any) against
// the schema with that $id. def, when set, picks #/$defs/<def> as the root.
func Validate(id, def string, v any) error {
	root, ok := Schemas()[id]
	if !ok {
		return fmt.Errorf("unknown schema %s", id)
	}
	s := root
	if def != "" {
		d, ok := defs(root)[def]
		if !ok {
			return fmt.Errorf("schema %s has no $defs.%s", id, def)
		}
		s = d
	}
	var errs []string
	check(root, s, v, "", &errs)
	if len(errs) > 0 {
		if len(errs) > 6 {
			errs = append(errs[:6], fmt.Sprintf("… %d more", len(errs)-6))
		}
		return fmt.Errorf("%s: %s", id, strings.Join(errs, "; "))
	}
	return nil
}

// ValidateSchema checks a JSON value against a schema the caller holds (one
// not embedded here: the client's own types, like tern.offer/1).
func ValidateSchema(root Schema, v any) error {
	var errs []string
	check(root, root, v, "", &errs)
	if len(errs) > 0 {
		if len(errs) > 6 {
			errs = append(errs[:6], fmt.Sprintf("… %d more", len(errs)-6))
		}
		id, _ := root["$id"].(string)
		return fmt.Errorf("%s: %s", id, strings.Join(errs, "; "))
	}
	return nil
}

// ValidateJSON: Validate on raw bytes.
func ValidateJSON(id, def string, b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("%s: not JSON: %v", id, err)
	}
	return Validate(id, def, v)
}

func defs(root Schema) map[string]Schema {
	out := map[string]Schema{}
	if d, ok := root["$defs"].(map[string]any); ok {
		for k, v := range d {
			if m, ok := v.(map[string]any); ok {
				out[k] = m
			}
		}
	}
	return out
}

var reCache sync.Map

func re(p string) *regexp.Regexp {
	if r, ok := reCache.Load(p); ok {
		return r.(*regexp.Regexp)
	}
	r := regexp.MustCompile(p)
	reCache.Store(p, r)
	return r
}

func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if x == math.Trunc(x) {
			return "integer"
		}
		return "number"
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return "number"
		}
		return "integer"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// maxSchemaDepth: how deep check follows a schema ($ref hops and nesting
// together). A declared kind is outside data: a $ref loop must fail, not
// overflow the stack (audit 6 CM-1).
const maxSchemaDepth = 64

func check(root, s Schema, v any, at string, errs *[]string) { checkAt(root, s, v, at, errs, 0) }

func checkAt(root, s Schema, v any, at string, errs *[]string, depth int) {
	where := at
	if where == "" {
		where = "/"
	}
	fail := func(f string, a ...any) { *errs = append(*errs, where+": "+fmt.Sprintf(f, a...)) }
	if depth > maxSchemaDepth {
		fail("the schema nests deeper than %d levels (a $ref loop?)", maxSchemaDepth)
		return
	}
	if ref, ok := s["$ref"].(string); ok {
		name, ok := strings.CutPrefix(ref, "#/$defs/")
		d, found := defs(root)[name]
		if !ok || !found {
			fail("bad $ref %s", ref)
			return
		}
		checkAt(root, d, v, at, errs, depth+1)
		return
	}
	if c, ok := s["const"]; ok && !equalJSON(c, v) {
		fail("must be %v", c)
		return
	}
	if e, ok := s["enum"].([]any); ok {
		hit := false
		for _, x := range e {
			hit = hit || equalJSON(x, v)
		}
		if !hit {
			fail("must be one of %v", e)
			return
		}
	}
	if t, ok := s["type"]; ok {
		got := typeOf(v)
		var want []string
		switch x := t.(type) {
		case string:
			want = []string{x}
		case []any:
			for _, y := range x {
				want = append(want, fmt.Sprint(y))
			}
		}
		okType := false
		for _, w := range want {
			okType = okType || w == got || w == "number" && got == "integer"
		}
		if !okType {
			fail("must be %s, not %s", strings.Join(want, "|"), got)
			return
		}
	}
	switch x := v.(type) {
	case string:
		n := utf8.RuneCountInString(x)
		if m, ok := num(s["maxLength"]); ok && float64(n) > m {
			fail("longer than %v characters", m)
		}
		if m, ok := num(s["minLength"]); ok && float64(n) < m {
			fail("shorter than %v characters", m)
		}
		if p, ok := s["pattern"].(string); ok && !re(p).MatchString(x) {
			fail("%q doesn't match %s", clip(x, 40), p)
		}
	case float64, json.Number:
		f, _ := num(x)
		if m, ok := num(s["minimum"]); ok && f < m {
			fail("below %v", m)
		}
		if m, ok := num(s["maximum"]); ok && f > m {
			fail("above %v", m)
		}
	case []any:
		if m, ok := num(s["maxItems"]); ok && float64(len(x)) > m {
			fail("more than %v items", m)
		}
		if m, ok := num(s["minItems"]); ok && float64(len(x)) < m {
			fail("fewer than %v items", m)
		}
		if it, ok := s["items"].(map[string]any); ok {
			for i, y := range x {
				checkAt(root, it, y, fmt.Sprintf("%s/%d", at, i), errs, depth+1)
			}
		}
	case map[string]any:
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, ok := x[r.(string)]; !ok {
					fail("missing %q", r)
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p, ok := props[k].(map[string]any); ok {
				checkAt(root, p, x[k], at+"/"+k, errs, depth+1)
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					fail("unknown field %q", k)
				}
			case map[string]any:
				checkAt(root, ap, x[k], at+"/"+k, errs, depth+1)
			}
		}
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
