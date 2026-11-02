package ui

// A2UI v0.9 surfaces: a flat list of components from a catalog, a data
// model, and (ternchain's addition) the table of actions the surface may
// emit. Everything shown is checked here before it reaches the renderer.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	A2UIVersion = "v0.9"
	CatalogID   = "tern:catalog/basic+tern@1" // the A2UI basic catalog subset + the ternchain catalog, below
)

type Component = map[string]any

// ActionDecl: what a button's event name means: a kind from the closed
// registry (broker.go) and where its parameters come from (paths into the
// data model: "author" relative to the item the button belongs to, "/tip/amount"
// absolute, "=up" a literal). Never from labels, never from the event's context.
type ActionDecl struct {
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
}

// Surface: one screen or card.
type Surface struct {
	ID         string                `json:"surfaceId"`
	Title      string                `json:"title,omitempty"`
	Components []Component           `json:"components"`
	Data       map[string]any        `json:"data"`
	Actions    map[string]ActionDecl `json:"actions"`
	Source     string                `json:"source"` // "reddit-classic 1.0.0 by anya" | "generated" | "base"
	Tainted    bool                  `json:"tainted"`
	Theme      map[string]string     `json:"theme,omitempty"`
}

// Messages: the surface as A2UI v0.9 messages (JSONL on the wire).
func (s *Surface) Messages() []map[string]any {
	return []map[string]any{
		{"version": A2UIVersion, "createSurface": map[string]any{"surfaceId": s.ID, "catalogId": CatalogID}},
		{"version": A2UIVersion, "updateComponents": map[string]any{"surfaceId": s.ID, "components": s.Components}},
		{"version": A2UIVersion, "updateDataModel": map[string]any{"surfaceId": s.ID, "path": "/", "value": s.Data}},
	}
}

// Fence: a string written by someone else, as the data model carries it.
// The renderer shows it only as text, with its origin; it is never a label,
// a link or an image.
func Fence(from, text string) map[string]any {
	return map[string]any{"$fenced": map[string]any{"from": from, "text": text}}
}

// IsFenced: a data-model value from outside.
func IsFenced(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m["$fenced"]
	return ok
}

// ---- the catalog

type prop int

const (
	pText     prop = iota // a literal string or a {path} binding
	pLiteral              // a literal string only
	pEnum                 // a literal from a set
	pNumber               // a literal number or a binding
	pBool                 // a literal bool
	pBinding              // a {path} binding only
	pChild                // a component id
	pChildren             // ids, or a template {componentId, path}
	pAction               // {event: {name, context}}
	pColumns              // Table columns: [{header: literal, value: {path}}]
	pOptions              // ChoicePicker options: [{label: literal, value: literal}]
)

type compSpec struct {
	props map[string]prop
	enums map[string][]string
}

var catalog = map[string]compSpec{
	// A2UI basic catalog (subset)
	"Column":       {props: map[string]prop{"children": pChildren, "justify": pEnum, "align": pEnum, "gap": pEnum}, enums: map[string][]string{"justify": {"start", "center", "end", "spaceBetween"}, "align": {"start", "center", "end", "stretch"}, "gap": {"none", "small", "medium", "large"}}},
	"Row":          {props: map[string]prop{"children": pChildren, "justify": pEnum, "align": pEnum, "gap": pEnum, "wrap": pBool}, enums: map[string][]string{"justify": {"start", "center", "end", "spaceBetween"}, "align": {"start", "center", "end", "stretch"}, "gap": {"none", "small", "medium", "large"}}},
	"Card":         {props: map[string]prop{"child": pChild, "variant": pEnum}, enums: map[string][]string{"variant": {"plain", "outlined", "elevated", "accent"}}},
	"Text":         {props: map[string]prop{"text": pText, "variant": pEnum}, enums: map[string][]string{"variant": {"h1", "h2", "h3", "body", "caption", "mono"}}},
	"Button":       {props: map[string]prop{"child": pChild, "action": pAction, "variant": pEnum}, enums: map[string][]string{"variant": {"primary", "default", "quiet"}}},
	"Divider":      {props: map[string]prop{"axis": pEnum}, enums: map[string][]string{"axis": {"horizontal", "vertical"}}},
	"List":         {props: map[string]prop{"children": pChildren, "direction": pEnum}, enums: map[string][]string{"direction": {"vertical", "horizontal"}}},
	"Image":        {props: map[string]prop{"url": pLiteral, "variant": pEnum}, enums: map[string][]string{"variant": {"icon", "avatar", "smallFeature", "mediumFeature"}}},
	"Icon":         {props: map[string]prop{"name": pEnum}, enums: map[string][]string{"name": {"up", "down", "comment", "coin", "calendar", "work", "vote", "check", "info", "lock"}}},
	"TextField":    {props: map[string]prop{"label": pLiteral, "value": pBinding}},
	"ChoicePicker": {props: map[string]prop{"options": pOptions, "value": pBinding, "variant": pEnum}, enums: map[string][]string{"variant": {"mutuallyExclusive", "multipleSelection"}}},
	// the ternchain catalog
	"Badge": {props: map[string]prop{"text": pText, "tone": pEnum}, enums: map[string][]string{"tone": {"neutral", "good", "warn", "bad", "accent"}}},
	"Stat":  {props: map[string]prop{"label": pLiteral, "value": pText}},
	"Table": {props: map[string]prop{"rows": pBinding, "columns": pColumns, "rowChild": pChild}},
}

// reserved: names a surface may never use: the trusted layer's own
// (approval sheets, system banners), overlays, and the A2UI reserved names.
var reserved = regexp.MustCompile(`^(?i:approve|approval|pay|payment|confirm|systembanner|system|surface|modal|dialog|overlay|trusted.*|owner.*|@.*)$`)

var idRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Requirements: what a screen for a task must contain (the lesson of the
// calculator without "." and "=": check before showing).
type Requirements struct {
	Actions    []string // event names some button must emit
	Components []string // component types it must use
}

func binding(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", false
	}
	p, ok := m["path"].(string)
	return p, ok
}

var pathRe = regexp.MustCompile(`^/?[A-Za-z0-9_]+(/[A-Za-z0-9_]+)*$|^/$`)

// ValidateSurface checks a surface against the catalog and the rules of
// research/generative-ui.md 3.2: closed catalog, no reserved names, labels
// literal (foreign data can't become a button), every action declared,
// images only content-addressed, no links, sizes bounded; then the task's
// requirements. All problems are returned (a generator gets them to repair).
func ValidateSurface(s *Surface, req Requirements) []string {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if len(s.Components) == 0 {
		return []string{"no components"}
	}
	if len(s.Components) > 400 {
		bad("%d components (at most 400)", len(s.Components))
	}
	byID := map[string]Component{}
	for i, c := range s.Components {
		id, _ := c["id"].(string)
		if !idRe.MatchString(id) {
			bad("component %d: bad id %q", i, id)
			continue
		}
		if _, dup := byID[id]; dup {
			bad("%s: duplicate id", id)
		}
		byID[id] = c
	}
	if _, ok := byID["root"]; !ok {
		bad("no root component")
	}
	usedActions := map[string]bool{}
	usedComps := map[string]bool{}
	ids := keys(byID)
	for _, id := range ids {
		c := byID[id]
		name, _ := c["component"].(string)
		if reserved.MatchString(name) {
			bad("%s: %q is reserved for the owner's trusted layer", id, name)
			continue
		}
		spec, ok := catalog[name]
		if !ok {
			bad("%s: unknown component %q (catalog: %s)", id, name, strings.Join(keys(catalog), ", "))
			continue
		}
		usedComps[name] = true
		for k, v := range c {
			if k == "id" || k == "component" {
				continue
			}
			pk, ok := spec.props[k]
			if !ok {
				bad("%s: %s has no property %q", id, name, k)
				continue
			}
			switch pk {
			case pText:
				if _, isStr := v.(string); !isStr {
					if p, ok := binding(v); !ok || !pathRe.MatchString(p) {
						bad("%s.%s: a string or {\"path\": …}", id, k)
					}
				}
			case pLiteral:
				str, isStr := v.(string)
				if !isStr {
					bad("%s.%s: must be literal text, not data", id, k)
				} else if name == "Image" && !strings.HasPrefix(str, "tern:artifact:") {
					bad("%s.url: images come only from content-addressed artifacts (tern:artifact:ID), never a URL", id)
				}
			case pEnum:
				str, _ := v.(string)
				if !contains(spec.enums[k], str) {
					bad("%s.%s: %v not one of %v", id, k, v, spec.enums[k])
				}
			case pNumber:
				if _, isNum := v.(float64); !isNum {
					if _, ok := binding(v); !ok {
						bad("%s.%s: a number or a binding", id, k)
					}
				}
			case pBool:
				if _, ok := v.(bool); !ok {
					bad("%s.%s: true or false", id, k)
				}
			case pBinding:
				if p, ok := binding(v); !ok || !pathRe.MatchString(p) {
					bad("%s.%s: must be {\"path\": …}", id, k)
				}
			case pChild:
				ref, _ := v.(string)
				if _, ok := byID[ref]; !ok {
					bad("%s.%s: no component %q", id, k, ref)
				}
			case pChildren:
				switch x := v.(type) {
				case []any:
					for _, r := range x {
						ref, _ := r.(string)
						if _, ok := byID[ref]; !ok {
							bad("%s.children: no component %q", id, ref)
						}
					}
				case map[string]any:
					ref, _ := x["componentId"].(string)
					p, _ := x["path"].(string)
					if _, ok := byID[ref]; !ok || !pathRe.MatchString(p) || len(x) != 2 {
						bad("%s.children: a template is {\"componentId\": ID, \"path\": …}", id)
					}
				default:
					bad("%s.children: a list of ids or a template", id)
				}
			case pAction:
				ev, _ := v.(map[string]any)["event"].(map[string]any)
				nm, _ := ev["name"].(string)
				if nm == "" {
					bad("%s.action: {\"event\": {\"name\": …}} (functionCall is not allowed on surfaces)", id)
					continue
				}
				if _, ok := s.Actions[nm]; !ok {
					bad("%s: action %q is not declared in the surface's action table", id, nm)
				}
				usedActions[nm] = true
				if ctx, ok := ev["context"].(map[string]any); ok {
					for ck, cv := range ctx {
						if _, isStr := cv.(string); !isStr {
							if _, ok := binding(cv); !ok {
								bad("%s.action.context.%s: a literal or a binding", id, ck)
							}
						}
					}
				}
			case pColumns:
				cols, ok := v.([]any)
				if !ok || len(cols) == 0 || len(cols) > 12 {
					bad("%s.columns: 1 to 12 columns", id)
					continue
				}
				for i, col := range cols {
					m, _ := col.(map[string]any)
					if _, ok := m["header"].(string); !ok {
						bad("%s.columns[%d].header: literal text", id, i)
					}
					if p, ok := binding(m["value"]); !ok || strings.HasPrefix(p, "/") {
						bad("%s.columns[%d].value: a path relative to the row", id, i)
					}
				}
			case pOptions:
				opts, ok := v.([]any)
				if !ok || len(opts) == 0 || len(opts) > 12 {
					bad("%s.options: 1 to 12 options", id)
					continue
				}
				for i, o := range opts {
					m, _ := o.(map[string]any)
					_, l := m["label"].(string)
					_, val := m["value"].(string)
					if !l || !val {
						bad("%s.options[%d]: a literal label and value (foreign text is never an option label)", id, i)
					}
				}
			}
		}
		if name == "Button" {
			child, _ := c["child"].(string)
			lc := byID[child]
			if lc == nil || lc["component"] != "Text" {
				bad("%s: a button's child is a Text with its label", id)
			} else if _, lit := lc["text"].(string); !lit {
				bad("%s: a button's label must be literal text from the template, never data (%s.text is bound to data)", id, child)
			}
			if c["action"] == nil {
				bad("%s: a button without an action", id)
			}
		}
	}
	// a tree from root (templates count as edges), no cycles, bounded depth
	seen := map[string]int{}
	var walk func(id string, depth int, stack map[string]bool)
	walk = func(id string, depth int, stack map[string]bool) {
		if stack[id] {
			bad("%s: a cycle", id)
			return
		}
		if depth > 24 {
			bad("%s: nested deeper than 24", id)
			return
		}
		seen[id]++
		c := byID[id]
		if c == nil {
			return
		}
		stack[id] = true
		for _, ref := range childRefs(c) {
			walk(ref, depth+1, stack)
		}
		delete(stack, id)
	}
	if _, ok := byID["root"]; ok {
		walk("root", 0, map[string]bool{})
	}
	for _, a := range req.Actions {
		if !usedActions[a] {
			bad("missing required action %q (a button that emits it)", a)
		}
	}
	for _, cm := range req.Components {
		if !usedComps[cm] {
			bad("missing required component %s", cm)
		}
	}
	sort.Strings(errs)
	return errs
}

func childRefs(c Component) []string {
	var out []string
	for _, k := range []string{"child", "rowChild"} {
		if s, ok := c[k].(string); ok {
			out = append(out, s)
		}
	}
	switch x := c["children"].(type) {
	case []any:
		for _, r := range x {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
	case map[string]any:
		if s, ok := x["componentId"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Template: a package's screen or card template (templates/*.json).
type Template struct {
	Title      string      `json:"title,omitempty"`
	Components []Component `json:"components"`
}

func ParseScreen(b []byte) (Template, error) {
	var t Template
	err := json.Unmarshal(b, &t)
	return t, err
}

// Lookup: a JSON Pointer-ish path ("/items/3/author", or "author" relative
// to scope) in a data model.
func Lookup(data any, scope, path string) (any, bool) {
	full := path
	if !strings.HasPrefix(path, "/") {
		full = strings.TrimSuffix(scope, "/") + "/" + path
	}
	cur := data
	for _, part := range strings.Split(strings.Trim(full, "/"), "/") {
		if part == "" {
			continue
		}
		switch x := cur.(type) {
		case map[string]any:
			v, ok := x[part]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i := -1
			fmt.Sscanf(part, "%d", &i)
			if i < 0 || i >= len(x) {
				return nil, false
			}
			cur = x[i]
		case []map[string]any:
			i := -1
			fmt.Sscanf(part, "%d", &i)
			if i < 0 || i >= len(x) {
				return nil, false
			}
			cur = x[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// Normalize: a data model through JSON (so lookups see maps and []any only).
func Normalize(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}
