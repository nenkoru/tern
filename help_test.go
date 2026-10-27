package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// dispatched: every command the dispatchers handle ("case" strings of the
// run* functions that name a command word).
func dispatched(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	cmdRe := regexp.MustCompile(`^[a-z][a-z-]*( [a-z][a-z-]*)?$`)
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "run") || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				var lits []ast.Expr
				switch x := n.(type) {
				case *ast.CaseClause:
					lits = x.List
				case *ast.BinaryExpr: // cmd == "relay serve"
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "cmd" && x.Op == token.EQL {
						lits = []ast.Expr{x.Y}
					}
				default:
					return true
				}
				for _, e := range lits {
					bl, ok := e.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						continue
					}
					s, _ := strconv.Unquote(bl.Value)
					w, _, _ := strings.Cut(s, " ")
					if cmdRe.MatchString(s) && (hasSubs(w) && strings.Contains(s, " ") || topLevel[s]) && !seen[s] {
						seen[s] = true
						out = append(out, s)
					}
				}
				return true
			})
		}
	}
	return out
}

// Every command has --help of its own, and help --all names it.
func TestHelpCoversEveryCommand(t *testing.T) {
	cmds := dispatched(t)
	if len(cmds) < 150 {
		t.Fatalf("found only %d commands", len(cmds))
	}
	known := knownCommands()
	for _, c := range cmds {
		if !known[c] {
			t.Errorf("help --all doesn't name %q", c)
			continue
		}
		h := helpFor(c)
		if h == usage || !strings.Contains(h, "ternchain ") {
			t.Errorf("%s --help: the whole usage", c)
		}
	}
	// The core is 25 commands, each one that exists.
	n := 0
	for _, l := range strings.Split(coreUsage, "\n") {
		if t2 := strings.TrimSpace(l); strings.HasPrefix(t2, "ternchain ") && !strings.HasPrefix(t2, "ternchain —") {
			n++
			f := strings.Fields(t2)
			c := f[1]
			if hasSubs(c) {
				c += " " + f[2]
			}
			if !known[c] {
				t.Errorf("the core names %q, which help --all doesn't", c)
			}
		}
	}
	if n != 25 {
		t.Errorf("the core has %d commands", n)
	}
	if h := helpFor("lock settle"); !strings.Contains(h, "lock settle LOCK") || strings.Contains(h, "lock open KIND") {
		t.Errorf("lock settle --help: %s", h)
	}
	if err := unknownCommand("lock opne"); !strings.Contains(err.Error(), "open") || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown: %v", err)
	}
	if err := unknownCommand("inbx"); !strings.Contains(err.Error(), "inbox") {
		t.Errorf("unknown: %v", err)
	}
}

// What the CLI prints is JSON with snake_case keys (outside protocol and
// file formats it mirrors: MCP, genesis files).
func TestOutputKeysSnakeCase(t *testing.T) {
	snake := regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "mcp.go" || strings.HasPrefix(f, "validator") {
			continue
		}
		af, _ := parser.ParseFile(fset, f, nil, 0)
		ast.Inspect(af, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			mt, ok := cl.Type.(*ast.MapType)
			if !ok {
				return true
			}
			if v, ok := mt.Value.(*ast.Ident); !ok || v.Name != "any" {
				return true
			}
			for _, e := range cl.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						if s, _ := strconv.Unquote(bl.Value); !snake.MatchString(s) && !strings.HasPrefix(s, "_") {
							t.Errorf("%s: key %q", fset.Position(bl.Pos()), s)
						}
					}
				}
			}
			return true
		})
	}
}
