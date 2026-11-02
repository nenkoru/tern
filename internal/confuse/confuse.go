// Package confuse: look-alike names, from data. A name's skeleton folds
// confusable characters (Cyrillic а as Latin a, rn as m, 0 as o, …) so that
// look-alikes compare equal; the tables are data files in Unicode TR39's
// confusables.txt format plus word lists (confusables.txt, words.txt), and a
// client policy module (tern.policy/1, kind lookalike) extends them. An
// extension only adds: the built-in entries are a security floor.
package confuse

import (
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

//go:embed confusables.txt
var builtinConfusables []byte

//go:embed words.txt
var builtinWords []byte

// Table: confusable characters and sequences, and the word lists.
type Table struct {
	chars   map[rune]rune
	seqs    []string // old, new, old, new… (applied after the characters, in order)
	rep     *strings.Replacer
	support []string
	supRE   *regexp.Regexp
	generic map[string]bool
	scripts []string
}

var (
	defOnce sync.Once
	def     *Table
)

// Default: the built-in tables.
func Default() *Table {
	defOnce.Do(func() {
		t, err := (&Table{chars: map[rune]rune{}, generic: map[string]bool{}}).With(builtinConfusables, builtinWords)
		if err != nil {
			panic("confuse: the built-in tables: " + err.Error())
		}
		def = t
	})
	return def
}

func codepoints(s string) (string, error) {
	var b strings.Builder
	for _, f := range strings.Fields(s) {
		v, err := strconv.ParseUint(f, 16, 32)
		if err != nil || v > unicode.MaxRune {
			return "", fmt.Errorf("%q is not a code point in hex", f)
		}
		b.WriteRune(rune(v))
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("no code points")
	}
	return b.String(), nil
}

// With: a copy of t with more confusables (TR39 lines) and words (sections
// [support], [generic], [scripts]). What t maps already stays as it is.
func (t *Table) With(confusables, words []byte) (*Table, error) {
	n := &Table{chars: map[rune]rune{}, seqs: append([]string(nil), t.seqs...), support: append([]string(nil), t.support...),
		generic: map[string]bool{}, scripts: append([]string(nil), t.scripts...)}
	for k, v := range t.chars {
		n.chars[k] = v
	}
	for k := range t.generic {
		n.generic[k] = true
	}
	haveSeq := map[string]bool{}
	for i := 0; i+1 < len(n.seqs); i += 2 {
		haveSeq[n.seqs[i]] = true
	}
	for i, line := range strings.Split(string(confusables), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, ";")
		if len(f) < 2 {
			return nil, fmt.Errorf("confusables line %d: SOURCE ; TARGET ; TYPE", i+1)
		}
		src, err := codepoints(f[0])
		if err != nil {
			return nil, fmt.Errorf("confusables line %d: %v", i+1, err)
		}
		dst, err := codepoints(f[1])
		if err != nil {
			return nil, fmt.Errorf("confusables line %d: %v", i+1, err)
		}
		dst = strings.ToLower(dst)
		if rs := []rune(src); len(rs) == 1 {
			if _, ok := n.chars[rs[0]]; !ok && len([]rune(dst)) == 1 {
				n.chars[rs[0]] = []rune(dst)[0]
			}
			continue
		}
		if !haveSeq[strings.ToLower(src)] {
			haveSeq[strings.ToLower(src)] = true
			n.seqs = append(n.seqs, strings.ToLower(src), dst)
		}
	}
	section := ""
	for i, line := range strings.Split(string(words), "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "["):
			section = strings.Trim(line, "[]")
			if section != "support" && section != "generic" && section != "scripts" {
				return nil, fmt.Errorf("words line %d: sections are [support], [generic], [scripts]", i+1)
			}
		default:
			for _, w := range strings.Fields(line) {
				switch section {
				case "support":
					w = strings.ToLower(w)
					if !contains(n.support, w) {
						n.support = append(n.support, w)
					}
				case "generic":
					n.generic[strings.ToLower(w)] = true
				case "scripts":
					if unicode.Scripts[w] == nil {
						return nil, fmt.Errorf("words line %d: no script %q", i+1, w)
					}
					if !contains(n.scripts, w) {
						n.scripts = append(n.scripts, w)
					}
				default:
					return nil, fmt.Errorf("words line %d: a word before any section", i+1)
				}
			}
		}
	}
	n.rep = strings.NewReplacer(n.seqs...)
	quoted := make([]string, len(n.support))
	for i, w := range n.support {
		quoted[i] = regexp.QuoteMeta(w)
	}
	n.supRE = regexp.MustCompile(strings.Join(quoted, "|"))
	if len(quoted) == 0 {
		n.supRE = regexp.MustCompile(`$^`)
	}
	return n, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Size: confusable characters, sequences, support words, generic words.
func (t *Table) Size() (int, int, int, int) {
	return len(t.chars), len(t.seqs) / 2, len(t.support), len(t.generic)
}

// Skeleton: a name folded so that look-alikes compare equal ("oscаr" with a
// Cyrillic а, "0scar", "o5car" and "oscar" share one).
func (t *Table) Skeleton(s string) string {
	s = strings.ToLower(norm.NFKD.String(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Cf, r) || r == '-' || r == '_' || r == ' ' {
			continue // accents, zero-width and other format characters, separators
		}
		if x, ok := t.chars[r]; ok {
			r = x
		}
		b.WriteRune(r)
	}
	return t.rep.Replace(b.String())
}

// SupportWord: the first support-like word in a skeleton, or "".
func (t *Table) SupportWord(sk string) string { return t.supRE.FindString(sk) }

// Generic: a word that, added to a name, makes a look-alike of it.
func (t *Table) Generic(w string) bool { return t.generic[strings.ToLower(w)] }

// Imitates: the known name that name looks like (same skeleton, or the
// known name inside it with a support-like word), or "". sameFamily says
// whether two names are one owner's (they never imitate each other).
func (t *Table) Imitates(name string, known []string, sameFamily func(a, b string) bool) string {
	sk := t.Skeleton(name)
	for _, k := range known {
		if k == name || k == "" || sameFamily != nil && sameFamily(k, name) {
			continue
		}
		ks := t.Skeleton(k)
		if ks == sk {
			return k
		}
		if len(ks) >= 4 && strings.Contains(sk, ks) && t.SupportWord(sk) != "" {
			return k
		}
	}
	return ""
}

// Scripts: the writing systems (of the table's list) of the letters in s.
func (t *Table) Scripts(s string) []string {
	seen := map[string]bool{}
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		for _, sc := range t.scripts {
			if unicode.Is(unicode.Scripts[sc], r) {
				seen[sc] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MixedScriptErr: a name mixing writing systems is refused (a look-alike
// trick) unless forced.
func (t *Table) MixedScriptErr(name string, force bool) error {
	if sc := t.Scripts(name); len(sc) > 1 && !force {
		return fmt.Errorf("the name %q mixes scripts (%s): a common look-alike trick; refused (add --force if you mean it)", name, strings.Join(sc, " + "))
	}
	return nil
}
