package match

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Usage: matchtool's commands.
const Usage = `matchtool: dating through your agent on tern (examples/match/README.md)

  matchtool check FILE…                                    a profile (tern.match/1) or prefs (tern.match.prefs/1): valid, and why not
  matchtool post --dir D COMMUNITY PROFILE.json             check it, then post it: attached, sealed to the members, local-only
  matchtool candidates --dir D COMMUNITY --me PROFILE.json --prefs PREFS.json [--scorer CMD] [--top N]
                                                           the community's profiles, checked and ranked for you, each with why;
                                                           --scorer: a LOCAL model's command (prompt on stdin, {"score","why"} out)
  matchtool like --dir D --community C [--prefs PREFS.json] NAME REASON…
                                                           a like (a match card), within the daily cap; the guard asks your person
  matchtool matches --dir D                                who liked you, whom you liked, the mutual ones and the next step
  matchtool report --dir D COMMUNITY NAME REASON…          report NAME's profile to the moderators
  matchtool photo IN OUT [--max BYTES]                     shrink a photo under 64 kB, metadata (EXIF, GPS) dropped
  matchtool save --dir OWNER-DIR --from NAME OUT-DIR       keep the photos NAME mailed you, on this device
  matchtool schema                                         print profile.schema.json

It runs ternchain ($TERNCHAIN_BIN, else ternchain on PATH, else ./ternchain) and prints JSON.
`

// Main: matchtool with these arguments, ternchain through r, JSON to w.
func Main(args []string, r Runner, w io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(w, Usage)
		return nil
	}
	cmd := args[0]
	flags := map[string]string{}
	var pos []string
	valued := map[string]bool{"--dir": true, "--me": true, "--prefs": true, "--scorer": true, "--top": true, "--community": true, "--max": true, "--from": true}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if valued[a] {
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", a)
			}
			flags[a] = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(a, "--") && strings.Contains(a, "=") {
			k, v, _ := strings.Cut(a, "=")
			if valued[k] {
				flags[k] = v
				continue
			}
		}
		if strings.HasPrefix(a, "--") {
			return fmt.Errorf("unknown flag %s (matchtool help)", a)
		}
		pos = append(pos, a)
	}
	dir := firstNonEmpty(flags["--dir"], os.Getenv("TERNCHAIN_DIR"))
	need := func(n int, usage string) error {
		if len(pos) < n {
			return errors.New("usage: matchtool " + usage)
		}
		if cmd != "check" && cmd != "photo" && cmd != "schema" && dir == "" {
			return errors.New("matchtool " + cmd + ": --dir D (your agent's folder)")
		}
		return nil
	}
	emit := func(v any) error {
		b, _ := json.MarshalIndent(v, "", "  ")
		_, err := fmt.Fprintln(w, string(b))
		return err
	}
	loadPrefs := func(required bool) (*Prefs, error) {
		f := flags["--prefs"]
		if f == "" {
			if required {
				return nil, errors.New("--prefs PREFS.json: what your person wants (tern.match.prefs/1)")
			}
			return nil, nil
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		return ParsePrefs(b)
	}
	switch cmd {
	case "schema":
		_, err := w.Write(SchemaJSON())
		return err
	case "check":
		if err := need(1, "check FILE…"); err != nil {
			return err
		}
		var res []map[string]any
		bad := 0
		for _, f := range pos {
			b, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			var head struct {
				Schema string `json:"schema"`
			}
			json.Unmarshal(b, &head)
			m := map[string]any{"file": f, "schema": head.Schema}
			switch head.Schema {
			case PrefsID:
				_, err = ParsePrefs(b)
			default:
				var p *Profile
				if p, err = ParseProfile(b); err == nil {
					if wn := Warnings(p); len(wn) > 0 {
						m["warnings"] = wn
					}
				}
			}
			m["valid"] = err == nil
			if err != nil {
				m["why"] = err.Error()
				bad++
			}
			res = append(res, m)
		}
		if err := emit(res); err != nil {
			return err
		}
		if bad > 0 {
			return fmt.Errorf("%d of %d files are not valid", bad, len(pos))
		}
		return nil
	case "post":
		if err := need(2, "post --dir D COMMUNITY PROFILE.json"); err != nil {
			return err
		}
		v, err := Post(r, dir, pos[0], pos[1])
		if err != nil {
			return err
		}
		return emit(v)
	case "candidates":
		if err := need(1, "candidates --dir D COMMUNITY --me PROFILE.json --prefs PREFS.json [--scorer CMD] [--top N]"); err != nil {
			return err
		}
		prefs, err := loadPrefs(true)
		if err != nil {
			return err
		}
		var me *Profile
		if f := flags["--me"]; f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			if me, err = ParseProfile(b); err != nil {
				return fmt.Errorf("--me: %w", err)
			}
		}
		cs, sk, err := Candidates(r, dir, pos[0], me, prefs, flags["--scorer"])
		if err != nil {
			return err
		}
		top := 5
		fmt.Sscan(flags["--top"], &top)
		var shown, below []Candidate
		for _, c := range cs {
			if c.Excluded == "" && c.Score >= prefs.MinScore && len(shown) < top {
				shown = append(shown, c)
			} else {
				below = append(below, c)
			}
		}
		out := map[string]any{"community": pos[0], "candidates": nonNil(shown), "not_proposed": brief(below), "skipped": nonNilS(sk),
			"likes_left_today": max(0, Cap(prefs)-loadLedger(dir).today(Now())),
			"next":             "show the candidates to your person, with the reasons and any warning; like only the ones they choose (matchtool like)"}
		return emit(out)
	case "like":
		if err := need(2, "like --dir D --community C [--prefs PREFS.json] NAME REASON…"); err != nil {
			return err
		}
		if flags["--community"] == "" {
			return errors.New("like: --community C (where you found them)")
		}
		prefs, err := loadPrefs(false)
		if err != nil {
			return err
		}
		v, err := Like(r, dir, flags["--community"], pos[0], strings.Join(pos[1:], " "), prefs)
		if err != nil {
			return err
		}
		return emit(v)
	case "matches":
		if err := need(0, "matches --dir D"); err != nil {
			return err
		}
		v, err := Matches(r, dir)
		if err != nil {
			return err
		}
		return emit(v)
	case "report":
		if err := need(3, "report --dir D COMMUNITY NAME REASON…"); err != nil {
			return err
		}
		v, err := Report(r, dir, pos[0], pos[1], strings.Join(pos[2:], " "))
		if err != nil {
			return err
		}
		return emit(v)
	case "photo":
		if err := need(2, "photo IN OUT [--max BYTES]"); err != nil {
			return err
		}
		m := 0
		fmt.Sscan(flags["--max"], &m)
		v, err := Photo(pos[0], pos[1], m)
		if err != nil {
			return err
		}
		return emit(v)
	case "save":
		if err := need(1, "save --dir OWNER-DIR --from NAME OUT-DIR"); err != nil {
			return err
		}
		if flags["--from"] == "" {
			return errors.New("save: --from NAME (whose photos)")
		}
		v, err := SaveFrom(r, dir, flags["--from"], pos[0])
		if err != nil {
			return err
		}
		return emit(v)
	}
	return fmt.Errorf("matchtool %s: no such command (matchtool help)", cmd)
}

func nonNil(c []Candidate) []Candidate {
	if c == nil {
		return []Candidate{}
	}
	return c
}

func nonNilS(s []Skipped) []Skipped {
	if s == nil {
		return []Skipped{}
	}
	return s
}

// brief: the candidates not proposed, without their profiles (the person
// sees only whom and why not).
func brief(cs []Candidate) []map[string]any {
	out := []map[string]any{}
	for _, c := range cs {
		why := c.Excluded
		switch {
		case why == "" && len(c.Warnings) > 0:
			why = fmt.Sprintf("score %d: %s", c.Score, c.Warnings[0])
		case why == "":
			why = fmt.Sprintf("score %d, below min_score or the top", c.Score)
		}
		m := map[string]any{"name": c.Name, "why": why}
		if len(c.Warnings) > 0 {
			m["warnings"] = c.Warnings
		}
		out = append(out, m)
	}
	return out
}
