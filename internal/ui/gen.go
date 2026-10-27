package ui

// Agent-generated screens. The model writes OpenUI Lang (one statement per
// line: `id = Component(args…)`; cheaper and streamable) over the same
// catalog; the device compiles it to A2UI, validates it (schema, catalog,
// labels, actions, the task's required elements) and repairs: the errors go
// back to the model, at most twice. What is shown, stored or shared is A2UI.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ---- OpenUI Lang (the subset this catalog needs)

type ouiVal struct {
	kind string // str num ref bind arr
	s    string
	n    float64
	arr  []ouiVal
}

type ouiStmt struct {
	id, comp string
	args     []ouiVal
	line     int
}

func ouiLex(s string) ([]string, error) {
	var out []string
	r := []rune(s)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '"':
			j := i + 1
			for j < len(r) && r[j] != '"' {
				if r[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(r) {
				return nil, errors.New("unterminated string")
			}
			out = append(out, string(r[i:j+1]))
			i = j + 1
		case strings.ContainsRune("=()[],", c):
			out = append(out, string(c))
			i++
		default:
			j := i
			for j < len(r) && !unicode.IsSpace(r[j]) && !strings.ContainsRune("=()[],\"", r[j]) {
				j++
			}
			out = append(out, string(r[i:j]))
			i = j
		}
	}
	return out, nil
}

func ouiParseVal(ts []string, i int) (ouiVal, int, error) {
	if i >= len(ts) {
		return ouiVal{}, i, errors.New("unexpected end")
	}
	t := ts[i]
	switch {
	case strings.HasPrefix(t, "\""):
		s, err := strconv.Unquote(t)
		return ouiVal{kind: "str", s: s}, i + 1, err
	case t == "[":
		var arr []ouiVal
		i++
		for i < len(ts) && ts[i] != "]" {
			v, j, err := ouiParseVal(ts, i)
			if err != nil {
				return v, j, err
			}
			arr = append(arr, v)
			i = j
			if i < len(ts) && ts[i] == "," {
				i++
			}
		}
		if i >= len(ts) {
			return ouiVal{}, i, errors.New("unclosed [")
		}
		return ouiVal{kind: "arr", arr: arr}, i + 1, nil
	case strings.HasPrefix(t, "$"):
		return ouiVal{kind: "bind", s: t[1:]}, i + 1, nil
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return ouiVal{kind: "num", n: f}, i + 1, nil
	}
	if idRe.MatchString(t) {
		return ouiVal{kind: "ref", s: t}, i + 1, nil
	}
	return ouiVal{}, i, fmt.Errorf("unexpected %q", t)
}

// ParseOpenUI: statements, line by line (a line that doesn't parse is an error).
func ParseOpenUI(src string) ([]ouiStmt, []string) {
	var out []ouiStmt
	var errs []string
	for n, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "```") {
			continue
		}
		ts, err := ouiLex(line)
		if err != nil || len(ts) < 4 || ts[1] != "=" || ts[3] != "(" || ts[len(ts)-1] != ")" {
			errs = append(errs, fmt.Sprintf("line %d: expected `id = Component(args…)`", n+1))
			continue
		}
		st := ouiStmt{id: ts[0], comp: ts[2], line: n + 1}
		i := 4
		for i < len(ts)-1 {
			v, j, err := ouiParseVal(ts, i)
			if err != nil {
				errs = append(errs, fmt.Sprintf("line %d: %v", n+1, err))
				break
			}
			st.args = append(st.args, v)
			i = j
			if i < len(ts)-1 && ts[i] == "," {
				i++
			}
		}
		out = append(out, st)
	}
	return out, errs
}

// OpenUISignatures: the components the model may use, with their positional
// arguments (this is the catalog part of the generation prompt).
var OpenUISignatures = []string{
	`Stack(children: [ref], direction: "column"|"row")`,
	`Text(content: "literal" | $path, variant: "h1"|"h2"|"h3"|"body"|"caption"|"mono")`,
	`Card(child: ref)`,
	`Badge(text: "literal" | $path, tone: "neutral"|"good"|"warn"|"bad"|"accent")`,
	`Stat(label: "literal", value: "literal" | $path)`,
	`Button(label: "literal", action: "action-name")   (labels are literal: never data)`,
	`List(items: $/path, item: ref)   (item is repeated per element; paths inside it are relative: $title)`,
	`Table(rows: $/path, headers: ["literal"…], fields: ["relative path"…], rowAction: ref?)`,
	`Divider()`,
}

func strArg(v ouiVal) (any, error) {
	switch v.kind {
	case "str":
		return v.s, nil
	case "bind":
		return map[string]any{"path": v.s}, nil
	}
	return nil, fmt.Errorf("a string or $path, not %s", v.kind)
}

// CompileOpenUI: OpenUI Lang → A2UI components (flat, root first).
func CompileOpenUI(src string) ([]Component, []string) {
	sts, errs := ParseOpenUI(src)
	var out []Component
	ids := map[string]bool{}
	for _, st := range sts {
		ids[st.id] = true
	}
	refs := func(v ouiVal) ([]any, error) {
		if v.kind != "arr" {
			return nil, errors.New("a list of [refs]")
		}
		var rs []any
		for _, x := range v.arr {
			if x.kind != "ref" {
				return nil, errors.New("a list of [refs]")
			}
			rs = append(rs, x.s)
		}
		return rs, nil
	}
	arg := func(st ouiStmt, i int) (ouiVal, bool) {
		if i < len(st.args) {
			return st.args[i], true
		}
		return ouiVal{}, false
	}
	for _, st := range sts {
		bad := func(f string, a ...any) {
			errs = append(errs, fmt.Sprintf("line %d (%s): %s", st.line, st.id, fmt.Sprintf(f, a...)))
		}
		c := Component{"id": st.id}
		switch st.comp {
		case "Stack":
			v, _ := arg(st, 0)
			rs, err := refs(v)
			if err != nil {
				bad("Stack: %v", err)
				continue
			}
			c["component"], c["children"] = "Column", rs
			if d, ok := arg(st, 1); ok && d.s == "row" {
				c["component"] = "Row"
			}
		case "Text", "Badge":
			v, _ := arg(st, 0)
			t, err := strArg(v)
			if err != nil {
				bad("%s: %v", st.comp, err)
				continue
			}
			c["component"], c["text"] = st.comp, t
			if d, ok := arg(st, 1); ok && d.kind == "str" {
				if st.comp == "Text" {
					c["variant"] = d.s
				} else {
					c["tone"] = d.s
				}
			}
		case "Stat":
			l, _ := arg(st, 0)
			v, _ := arg(st, 1)
			t, err := strArg(v)
			if l.kind != "str" || err != nil {
				bad("Stat(\"label\", value)")
				continue
			}
			c["component"], c["label"], c["value"] = "Stat", l.s, t
		case "Card":
			v, _ := arg(st, 0)
			if v.kind != "ref" {
				bad("Card(child)")
				continue
			}
			c["component"], c["child"] = "Card", v.s
		case "Button":
			l, _ := arg(st, 0)
			a, _ := arg(st, 1)
			if l.kind != "str" {
				bad("a button's label must be a literal string, not %s (labels never come from data)", l.kind)
				continue
			}
			if a.kind != "str" {
				bad("Button(label, \"action-name\")")
				continue
			}
			lid := st.id + "_label"
			c["component"], c["child"], c["action"] = "Button", lid, map[string]any{"event": map[string]any{"name": a.s}}
			out = append(out, c, Component{"id": lid, "component": "Text", "text": l.s})
			continue
		case "List":
			p, _ := arg(st, 0)
			it, _ := arg(st, 1)
			if p.kind != "bind" || it.kind != "ref" {
				bad("List($/path, itemRef)")
				continue
			}
			c["component"], c["children"] = "List", map[string]any{"componentId": it.s, "path": p.s}
		case "Table":
			p, _ := arg(st, 0)
			h, _ := arg(st, 1)
			f, _ := arg(st, 2)
			if p.kind != "bind" || h.kind != "arr" || f.kind != "arr" || len(h.arr) != len(f.arr) {
				bad("Table($/path, [headers], [fields], rowAction?) with as many headers as fields")
				continue
			}
			var cols []any
			for i := range h.arr {
				cols = append(cols, map[string]any{"header": h.arr[i].s, "value": map[string]any{"path": f.arr[i].s}})
			}
			c["component"], c["rows"], c["columns"] = "Table", map[string]any{"path": p.s}, cols
			if ra, ok := arg(st, 3); ok && ra.kind == "ref" {
				c["rowChild"] = ra.s
			}
		case "Divider":
			c["component"] = "Divider"
		default:
			bad("unknown component %s (use: Stack, Text, Card, Badge, Stat, Button, List, Table, Divider)", st.comp)
			continue
		}
		out = append(out, c)
	}
	// root first, as A2UI renderers like it
	for i, c := range out {
		if c["id"] == "root" && i > 0 {
			out[0], out[i] = out[i], out[0]
		}
	}
	return out, errs
}

// ---- generation with validation and repair

// Generator: writes OpenUI Lang for a prompt (an LLM, or MockGenerator).
type Generator interface {
	Generate(prompt string, attempt int, feedback []string) (string, error)
	Name() string
}

// Task: what the owner asked for, and what the screen must contain.
type Task struct {
	Question string
	Fields   map[string]string // data model fields the screen may bind, and what they are
	Actions  map[string]ActionDecl
	Require  Requirements
}

// Draft: one attempt, kept for the "how this screen was made" footer.
type Draft struct {
	Attempt int      `json:"attempt"`
	Source  string   `json:"openui"`
	Errors  []string `json:"errors,omitempty"`
	Tokens  int      `json:"tokens"` // rough: characters / 4
}

// Prompt: the system prompt + task the generator gets (catalog, syntax, data, actions).
func Prompt(t Task) string {
	var b strings.Builder
	b.WriteString("You answer with a screen in openui-lang, a declarative UI language: one statement per line, `id = Component(args…)`; `root` is the entry point.\n")
	b.WriteString("Components (positional arguments):\n")
	for _, s := range OpenUISignatures {
		b.WriteString("  " + s + "\n")
	}
	b.WriteString("Rules: labels are literal strings; never put data in a button label; no links, no images; bind data with $path.\n")
	b.WriteString("Data model fields:\n")
	for _, k := range keys(t.Fields) {
		b.WriteString("  " + k + ": " + t.Fields[k] + "\n")
	}
	b.WriteString("Actions you may attach to buttons (by name):\n")
	for _, k := range keys(t.Actions) {
		b.WriteString("  " + k + " (" + t.Actions[k].Kind + ")\n")
	}
	if len(t.Require.Actions) > 0 {
		b.WriteString("The screen must have buttons for: " + strings.Join(t.Require.Actions, ", ") + "\n")
	}
	b.WriteString("Request: " + t.Question + "\n")
	return b.String()
}

// GenerateSurface: generate, compile, validate, repair (at most 2 repairs).
func GenerateSurface(g Generator, t Task, id string, data map[string]any) (*Surface, []Draft, error) {
	var drafts []Draft
	var feedback []string
	prompt := Prompt(t)
	for attempt := 1; attempt <= 3; attempt++ {
		src, err := g.Generate(prompt, attempt, feedback)
		if err != nil {
			return nil, drafts, err
		}
		comps, errs := CompileOpenUI(src)
		s := &Surface{ID: id, Components: comps, Data: data, Actions: t.Actions, Source: "generated by " + g.Name()}
		if len(errs) == 0 {
			errs = ValidateSurface(s, t.Require)
		}
		drafts = append(drafts, Draft{Attempt: attempt, Source: src, Errors: errs, Tokens: len(src) / 4})
		if len(errs) == 0 {
			return s, drafts, nil
		}
		feedback = errs
	}
	return nil, drafts, fmt.Errorf("no valid screen after 3 drafts: %s", strings.Join(feedback, "; "))
}

// MockGenerator: a deterministic stand-in for the model (no API key needed).
// Its first draft of a table forgets the row button, as fast models do;
// the validator catches it and the repair adds it.
type MockGenerator struct{}

func (MockGenerator) Name() string { return "mock-model (deterministic)" }

func (MockGenerator) Generate(prompt string, attempt int, feedback []string) (string, error) {
	q := strings.ToLower(prompt[strings.LastIndex(prompt, "Request: ")+9:])
	missing := func(a string) bool {
		for _, f := range feedback {
			if strings.Contains(f, `missing required action "`+a+`"`) {
				return true
			}
		}
		return false
	}
	title := "What's new"
	if strings.Contains(q, "hot") {
		title = "Hot this week"
	}
	if strings.Contains(q, "table") {
		lines := []string{
			`root = Stack([title, meta, table], "column")`,
			`title = Text("` + title + `", "h2")`,
			`meta = Text($/summary, "caption")`,
			`table = Table($/items, ["Post", "Author", "Net", "Age", "Why"], ["title", "author", "net", "age", "why"])`,
		}
		if attempt > 1 && missing("open") {
			lines[3] = `table = Table($/items, ["Post", "Author", "Net", "Age", "Why"], ["title", "author", "net", "age", "why"], open)`
			lines = append(lines, `open = Button("Open", "open")`)
		}
		return strings.Join(lines, "\n") + "\n", nil
	}
	compact := strings.Contains(q, "compact") || strings.Contains(q, "no images")
	lines := []string{
		`root = Stack([title, meta, list], "column")`,
		`title = Text("` + title + `", "h2")`,
		`meta = Text($/summary, "caption")`,
		`list = List($/items, row)`,
	}
	if compact {
		lines = append(lines,
			`row = Stack([net, text, up, open], "row")`,
			`net = Badge($net, "accent")`,
			`text = Text($title, "body")`,
		)
	} else {
		lines = append(lines,
			`row = Card(body)`,
			`body = Stack([head, text, why, bar], "column")`,
			`head = Text($author, "caption")`,
			`text = Text($title, "h3")`,
			`why = Text($why, "caption")`,
			`bar = Stack([net, up, open], "row")`,
			`net = Badge($net, "accent")`,
		)
	}
	lines = append(lines, `up = Button("▲", "up")`, `open = Button("Open", "open")`)
	return strings.Join(lines, "\n") + "\n", nil
}
