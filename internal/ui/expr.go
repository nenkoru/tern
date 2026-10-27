package ui

// tern-expr/1: the expression language of ranking modules. A small subset
// of CEL's syntax (numbers, strings, booleans, item.x / author.x / params.x,
// arithmetic, comparisons, && || !, ?:, and pure functions), evaluated with
// a cost limit and no access to anything but the declared inputs: pure,
// deterministic, side-effect free. (CEL itself, via cel-go, is the
// production plan; this subset parses the same expressions.)

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

type tok struct {
	kind string // num str id op eof
	s    string
	n    float64
}

func lex(src string) ([]tok, error) {
	var out []tok
	r := []rune(src)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case unicode.IsDigit(c) || c == '.' && i+1 < len(r) && unicode.IsDigit(r[i+1]):
			j := i
			for j < len(r) && (unicode.IsDigit(r[j]) || r[j] == '.' || r[j] == 'e' || (r[j] == '-' || r[j] == '+') && j > i && r[j-1] == 'e') {
				j++
			}
			f, err := strconv.ParseFloat(string(r[i:j]), 64)
			if err != nil {
				return nil, fmt.Errorf("bad number %q", string(r[i:j]))
			}
			out = append(out, tok{kind: "num", n: f})
			i = j
		case c == '"' || c == '\'':
			j := i + 1
			var b strings.Builder
			for j < len(r) && r[j] != c {
				if r[j] == '\\' && j+1 < len(r) {
					j++
				}
				b.WriteRune(r[j])
				j++
			}
			if j >= len(r) {
				return nil, errors.New("unterminated string")
			}
			out = append(out, tok{kind: "str", s: b.String()})
			i = j + 1
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_' || r[j] == '.') {
				j++
			}
			out = append(out, tok{kind: "id", s: string(r[i:j])})
			i = j
		default:
			two := ""
			if i+1 < len(r) {
				two = string(r[i : i+2])
			}
			switch two {
			case "&&", "||", "==", "!=", "<=", ">=":
				out = append(out, tok{kind: "op", s: two})
				i += 2
				continue
			}
			if strings.ContainsRune("+-*/%<>!?:(),", c) {
				out = append(out, tok{kind: "op", s: string(c)})
				i++
				continue
			}
			return nil, fmt.Errorf("unexpected %q", string(c))
		}
	}
	return append(out, tok{kind: "eof"}), nil
}

// Expr: a parsed expression.
type Expr struct {
	src  string
	root *node
	ids  []string // the variables it reads
}

type node struct {
	op   string // num str bool var call un bin cond
	n    float64
	s    string
	kids []*node
}

type parser struct {
	ts  []tok
	i   int
	ids map[string]bool
}

func (p *parser) peek() tok { return p.ts[p.i] }
func (p *parser) next() tok { t := p.ts[p.i]; p.i++; return t }
func (p *parser) isOp(s string) bool {
	t := p.peek()
	return t.kind == "op" && t.s == s
}

var prec = map[string]int{"||": 1, "&&": 2, "==": 3, "!=": 3, "<": 4, "<=": 4, ">": 4, ">=": 4, "+": 5, "-": 5, "*": 6, "/": 6, "%": 6}

func (p *parser) expr(depth int) (*node, error) {
	if depth > 64 {
		return nil, errors.New("too deeply nested")
	}
	c, err := p.binary(1, depth)
	if err != nil {
		return nil, err
	}
	if p.isOp("?") {
		p.next()
		a, err := p.expr(depth + 1)
		if err != nil {
			return nil, err
		}
		if !p.isOp(":") {
			return nil, errors.New("expected ':'")
		}
		p.next()
		b, err := p.expr(depth + 1)
		if err != nil {
			return nil, err
		}
		return &node{op: "cond", kids: []*node{c, a, b}}, nil
	}
	return c, nil
}

func (p *parser) binary(min, depth int) (*node, error) {
	l, err := p.unary(depth)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		pr, ok := prec[t.s]
		if t.kind != "op" || !ok || pr < min {
			return l, nil
		}
		p.next()
		r, err := p.binary(pr+1, depth+1)
		if err != nil {
			return nil, err
		}
		l = &node{op: "bin", s: t.s, kids: []*node{l, r}}
	}
}

func (p *parser) unary(depth int) (*node, error) {
	if p.isOp("-") || p.isOp("!") {
		op := p.next().s
		x, err := p.unary(depth + 1)
		if err != nil {
			return nil, err
		}
		return &node{op: "un", s: op, kids: []*node{x}}, nil
	}
	return p.primary(depth)
}

var funcs = map[string]int{"log10": 1, "log2": 1, "ln": 1, "sqrt": 1, "abs": 1, "floor": 1, "ceil": 1, "round": 1, "pow": 2, "max": -1, "min": -1, "clamp": 3}

func (p *parser) primary(depth int) (*node, error) {
	t := p.next()
	switch t.kind {
	case "num":
		return &node{op: "num", n: t.n}, nil
	case "str":
		return &node{op: "str", s: t.s}, nil
	case "id":
		switch t.s {
		case "true", "false":
			return &node{op: "bool", n: map[bool]float64{true: 1}[t.s == "true"]}, nil
		}
		if p.isOp("(") {
			arity, ok := funcs[t.s]
			if !ok {
				return nil, fmt.Errorf("unknown function %s", t.s)
			}
			p.next()
			var args []*node
			for !p.isOp(")") {
				a, err := p.expr(depth + 1)
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if p.isOp(",") {
					p.next()
				} else if !p.isOp(")") {
					return nil, fmt.Errorf("expected ',' or ')' in %s(…)", t.s)
				}
			}
			p.next()
			if arity >= 0 && len(args) != arity || arity < 0 && len(args) == 0 {
				return nil, fmt.Errorf("%s takes %d arguments", t.s, arity)
			}
			return &node{op: "call", s: t.s, kids: args}, nil
		}
		if !strings.Contains(t.s, ".") {
			return nil, fmt.Errorf("unknown name %s (inputs are item.x, author.x, params.x)", t.s)
		}
		p.ids[t.s] = true
		return &node{op: "var", s: t.s}, nil
	case "op":
		if t.s == "(" {
			x, err := p.expr(depth + 1)
			if err != nil {
				return nil, err
			}
			if !p.isOp(")") {
				return nil, errors.New("expected ')'")
			}
			p.next()
			return x, nil
		}
	}
	return nil, fmt.Errorf("unexpected %q", t.s)
}

// Parse compiles an expression.
func Parse(src string) (*Expr, error) {
	if len(src) > 4000 {
		return nil, errors.New("expression too long")
	}
	ts, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{ts: ts, ids: map[string]bool{}}
	n, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != "eof" {
		return nil, fmt.Errorf("unexpected %q after the expression", p.peek().s)
	}
	e := &Expr{src: src, root: n}
	for id := range p.ids {
		e.ids = append(e.ids, id)
	}
	return e, nil
}

// Vars: the variables an expression reads.
func (e *Expr) Vars() []string { return append([]string(nil), e.ids...) }

// Env: what an expression may read: declared inputs and params only.
type Env map[string]any // "item.age_hours" => 3.5, "author.agent" => false, "params.gravity" => 12.5

// Lazy: an input computed only when an expression reads it (a policy's
// inputs may cost a chain call; && and || short-circuit, so a module that
// doesn't need one never pays for it). Memoize it with Once.
type Lazy func() any

// Once: a Lazy that computes its value at most once.
func Once(f func() any) Lazy {
	var done bool
	var v any
	return func() any {
		if !done {
			v, done = f(), true
		}
		return v
	}
}

var ErrCost = errors.New("cost limit exceeded")

type evaluator struct {
	env  Env
	cost int
	max  int
}

// Eval runs the expression; max bounds the steps (0: 10000).
func (e *Expr) Eval(env Env, max int) (any, error) {
	if max <= 0 {
		max = 10000
	}
	ev := &evaluator{env: env, max: max}
	return ev.eval(e.root)
}

func truthy(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case float64:
		return x != 0, nil
	}
	return false, fmt.Errorf("not a boolean: %v", v)
}

func toNum(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("not a number: %v", v)
}

func (ev *evaluator) eval(n *node) (any, error) {
	if ev.cost++; ev.cost > ev.max {
		return nil, ErrCost
	}
	switch n.op {
	case "num":
		return n.n, nil
	case "str":
		return n.s, nil
	case "bool":
		return n.n == 1, nil
	case "var":
		v, ok := ev.env[n.s]
		if !ok {
			return nil, fmt.Errorf("%s is not an input of this module", n.s)
		}
		if l, ok := v.(Lazy); ok {
			v = l()
		}
		switch x := v.(type) {
		case int:
			return float64(x), nil
		case int64:
			return float64(x), nil
		}
		return v, nil
	case "un":
		x, err := ev.eval(n.kids[0])
		if err != nil {
			return nil, err
		}
		if n.s == "!" {
			b, err := truthy(x)
			return !b, err
		}
		f, err := toNum(x)
		return -f, err
	case "cond":
		c, err := ev.eval(n.kids[0])
		if err != nil {
			return nil, err
		}
		b, err := truthy(c)
		if err != nil {
			return nil, err
		}
		if b {
			return ev.eval(n.kids[1])
		}
		return ev.eval(n.kids[2])
	case "bin":
		if n.s == "&&" || n.s == "||" {
			l, err := ev.eval(n.kids[0])
			if err != nil {
				return nil, err
			}
			lb, err := truthy(l)
			if err != nil {
				return nil, err
			}
			if n.s == "&&" && !lb || n.s == "||" && lb {
				return lb, nil
			}
			r, err := ev.eval(n.kids[1])
			if err != nil {
				return nil, err
			}
			return truthy(r)
		}
		l, err := ev.eval(n.kids[0])
		if err != nil {
			return nil, err
		}
		r, err := ev.eval(n.kids[1])
		if err != nil {
			return nil, err
		}
		if n.s == "==" || n.s == "!=" {
			eq := fmt.Sprint(l) == fmt.Sprint(r)
			if lf, e1 := toNum(l); e1 == nil {
				if rf, e2 := toNum(r); e2 == nil {
					eq = lf == rf
				}
			}
			return eq == (n.s == "=="), nil
		}
		if ls, ok := l.(string); ok && n.s == "+" {
			return ls + fmt.Sprint(r), nil
		}
		a, err := toNum(l)
		if err != nil {
			return nil, err
		}
		b, err := toNum(r)
		if err != nil {
			return nil, err
		}
		switch n.s {
		case "+":
			return a + b, nil
		case "-":
			return a - b, nil
		case "*":
			return a * b, nil
		case "/":
			if b == 0 {
				return 0.0, nil // total: a ranking never fails on a zero
			}
			return a / b, nil
		case "%":
			if b == 0 {
				return 0.0, nil
			}
			return math.Mod(a, b), nil
		case "<":
			return a < b, nil
		case "<=":
			return a <= b, nil
		case ">":
			return a > b, nil
		case ">=":
			return a >= b, nil
		}
	case "call":
		var xs []float64
		for _, k := range n.kids {
			v, err := ev.eval(k)
			if err != nil {
				return nil, err
			}
			f, err := toNum(v)
			if err != nil {
				return nil, err
			}
			xs = append(xs, f)
		}
		switch n.s {
		case "log10":
			return math.Log10(math.Max(xs[0], 1e-12)), nil
		case "log2":
			return math.Log2(math.Max(xs[0], 1e-12)), nil
		case "ln":
			return math.Log(math.Max(xs[0], 1e-12)), nil
		case "sqrt":
			return math.Sqrt(math.Max(xs[0], 0)), nil
		case "abs":
			return math.Abs(xs[0]), nil
		case "floor":
			return math.Floor(xs[0]), nil
		case "ceil":
			return math.Ceil(xs[0]), nil
		case "round":
			return math.Round(xs[0]), nil
		case "pow":
			return math.Pow(xs[0], xs[1]), nil
		case "clamp":
			return math.Max(xs[1], math.Min(xs[2], xs[0])), nil
		case "max", "min":
			m := xs[0]
			for _, x := range xs[1:] {
				if n.s == "max" {
					m = math.Max(m, x)
				} else {
					m = math.Min(m, x)
				}
			}
			return m, nil
		}
	}
	return nil, fmt.Errorf("bad node %s", n.op)
}

// Template: "text {expr} text {expr:1}" (":N" = N decimals; default: up to 1).
type ReasonTemplate struct {
	parts []string
	exprs []*Expr
	decs  []int
}

func ParseReason(s string) (*ReasonTemplate, error) {
	t := &ReasonTemplate{}
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			t.parts = append(t.parts, s)
			return t, nil
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return nil, errors.New("unclosed { in the reason")
		}
		inner := s[i+1 : i+j]
		dec := -1
		if k := strings.LastIndexByte(inner, ':'); k > 0 && !strings.Contains(inner, "?") {
			if d, err := strconv.Atoi(strings.TrimSpace(inner[k+1:])); err == nil {
				dec, inner = d, inner[:k]
			}
		}
		e, err := Parse(inner)
		if err != nil {
			return nil, fmt.Errorf("reason {%s}: %w", inner, err)
		}
		t.parts = append(t.parts, s[:i])
		t.exprs = append(t.exprs, e)
		t.decs = append(t.decs, dec)
		s = s[i+j+1:]
	}
}

func (t *ReasonTemplate) Vars() []string {
	var out []string
	for _, e := range t.exprs {
		out = append(out, e.Vars()...)
	}
	return out
}

func (t *ReasonTemplate) Render(env Env, max int) (string, error) {
	var b strings.Builder
	for i, p := range t.parts {
		b.WriteString(p)
		if i < len(t.exprs) {
			v, err := t.exprs[i].Eval(env, max)
			if err != nil {
				return "", err
			}
			switch x := v.(type) {
			case float64:
				if t.decs[i] >= 0 {
					b.WriteString(strconv.FormatFloat(x, 'f', t.decs[i], 64))
				} else {
					b.WriteString(strconv.FormatFloat(math.Round(x*10)/10, 'f', -1, 64))
				}
			case bool:
				b.WriteString(map[bool]string{true: "yes", false: "no"}[x])
			default:
				b.WriteString(fmt.Sprint(x))
			}
		}
	}
	return b.String(), nil
}
