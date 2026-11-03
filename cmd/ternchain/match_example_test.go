package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/examples/match"
)

// docToolLines: the lines of a document's ```sh blocks that start with one of
// the tools, in order, as words (the tool's name first).
func docToolLines(t *testing.T, file string, tools ...string) [][]string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var cmds [][]string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "```") {
			in = !in && strings.HasPrefix(l, "```sh")
			continue
		}
		for _, tool := range tools {
			if in && strings.HasPrefix(l, tool+" ") {
				cmds = append(cmds, finalBWords(l))
			}
		}
	}
	return cmds
}

// inProcess: a match.Runner that runs ternchain in this process, as
// launchRun does (an account out of gas gets the faucet once), returning
// errors instead of failing the test (the guard's refusals are answers).
func inProcess(faucet func(common.Address)) match.Runner {
	gas := regexp.MustCompile(`account (0x[0-9a-fA-F]{40}) pays its own gas`)
	return func(args ...string) ([]byte, error) {
		var buf bytes.Buffer
		prev := outW
		defer func() { outW = prev }()
		for try := 0; ; try++ {
			buf.Reset()
			outW = &buf
			err := run(append([]string(nil), args...))
			outW = prev
			if err == nil {
				return buf.Bytes(), nil
			}
			m := gas.FindStringSubmatch(err.Error())
			if m == nil || try > 0 {
				return buf.Bytes(), err
			}
			faucet(common.HexToAddress(m[1]))
		}
	}
}

// examples/match/README.md: every ternchain and matchtool line, in order, on
// a network shaped like the public one (gas in test USDC). matchtool runs
// in-process (examples/match), its own ternchain calls too.
func TestExampleMatch(t *testing.T) {
	dir := repo("examples/match")
	faucet := launchPeople(t, "pat", "nina", "oscar", "mallory")
	files, _ := filepath.Glob(filepath.Join(dir, "try", "*"))
	for _, f := range files { // the example's files, copied to ~/tern as the README says
		b, _ := os.ReadFile(f)
		os.WriteFile(filepath.Base(f), b, 0o644)
	}
	r := inProcess(faucet)
	cmds := docToolLines(t, filepath.Join(dir, "README.md"), "ternchain", "matchtool")
	if len(cmds) < 30 {
		t.Fatalf("only %d commands in examples/match/README.md", len(cmds))
	}
	// What a command's output must (or, with !, must not) contain, by the start of its line.
	check := map[string][]string{
		"ternchain find --dir agent/ai.nina dating": {"mersin-dating"},
		"ternchain group read --dir agent/ai.oscar": {"withheld: local-model only"},
		"matchtool check": {`"valid": true`},
		"matchtool candidates --dir agent/ai.oscar":            {`"name": "ai.nina"`, `"score": 100`, "not a gender you look for"},
		"matchtool like --dir agent/ai.oscar":                  {`"liked": "ai.nina"`, `"likes_today": 1`},
		"matchtool matches --dir agent/ai.nina":                {`"name": "ai.oscar"`, "hike and play chess"},
		"matchtool candidates --dir agent/ai.nina":             {`"name": "ai.oscar"`, "romance scam"},
		"matchtool like --dir agent/ai.nina":                   {`"mutual": true`, "ternchain send --dir id/nina --to oscar"},
		"matchtool report":                                     {`"reported": "ai.mallory"`},
		"ternchain commons ban":                                {`"banned": true`},
		"ternchain inbox --dir agent/ai.nina":                  {"ai.oscar", "!ai.mallory"},
		"matchtool matches --dir agent/ai.oscar":               {`"mutual": [` + "\n    {\n      \"name\": \"ai.nina\""},
		"matchtool photo":                                      {"metadata", "EXIF"},
		"matchtool save --dir id/oscar --from nina":            {"photos/nina-"},
		"matchtool save --dir id/nina --from oscar":            {"photos/oscar-"},
		"ternchain send --dir id/nina --to oscar --local-only": {`"sent"`},
	}
	used := map[string]bool{}
	for _, c := range cmds {
		line := strings.Join(c, " ")
		var out []byte
		var err error
		if c[0] == "ternchain" {
			out, err = r(c[1:]...)
		} else {
			var buf bytes.Buffer
			err = match.Main(c[1:], r, &buf)
			out = buf.Bytes()
		}
		if err != nil {
			t.Fatalf("%s: %v\n%s", line, err, out)
		}
		if c[0] == "matchtool" && c[1] == "candidates" { // one proposal, the right one; the rest not proposed
			var v struct {
				Candidates  []match.Candidate `json:"candidates"`
				NotProposed []map[string]any  `json:"not_proposed"`
			}
			json.Unmarshal(out, &v)
			if len(v.Candidates) != 1 || len(v.NotProposed) != 1 || v.NotProposed[0]["name"] != "ai.mallory" {
				t.Fatalf("%s: want one candidate and ai.mallory not proposed:\n%s", line, out)
			}
		}
		for k, wants := range check {
			if !strings.HasPrefix(line, k) {
				continue
			}
			used[k] = true
			for _, w := range wants {
				if no, ok := strings.CutPrefix(w, "!"); ok && strings.Contains(string(out), no) || !ok && !strings.Contains(string(out), w) {
					t.Fatalf("%s: want %q in\n%s", line, w, out)
				}
			}
		}
	}
	for k := range check {
		if !used[k] {
			t.Errorf("no line of the README starts with %q", k)
		}
	}
	// The photos arrived whole, as the JPEGs that were sent, under 64 kB, without the originals' EXIF.
	for _, p := range []string{"nina", "oscar"} {
		got, _ := filepath.Glob(filepath.Join("photos", p+"-*.jpg"))
		sent, _ := os.ReadFile(p + ".jpg")
		if len(got) != 1 || len(sent) == 0 || len(sent) > match.AttachMax {
			t.Fatalf("%s's photo: %v (sent %d bytes)", p, got, len(sent))
		}
		if b, _ := os.ReadFile(got[0]); !bytes.Equal(b, sent) || bytes.Contains(b, []byte("Exif")) {
			t.Fatalf("%s's photo changed on the way, or kept its EXIF", p)
		}
	}
	// The photo letters went person to person: neither agent holds them.
	for _, a := range []string{"agent/ai.oscar", "agent/ai.nina"} {
		if out, err := r("inbox", "--dir", a, "--all"); err != nil || strings.Contains(string(out), "for you only") {
			t.Fatalf("%s's inbox has a photo letter: %v\n%s", a, err, out)
		}
	}
	// Nina's agent's matches: Oscar's like, not Mallory's (banned, blocked).
	var buf bytes.Buffer
	if err := match.Main([]string{"matches", "--dir", "agent/ai.nina"}, r, &buf); err != nil || strings.Contains(buf.String(), "ai.mallory") {
		t.Fatalf("nina's matches after the ban: %v\n%s", err, buf.String())
	}
	var mv match.MatchView
	json.Unmarshal(buf.Bytes(), &mv)
	if len(mv.Mutual) != 1 || mv.Mutual[0].Name != "ai.oscar" {
		t.Fatalf("nina's mutual likes: %+v", mv.Mutual)
	}
}

// The guard and the cap, beyond the README: a like to someone new after
// reading profiles is refused until the owner approves that name (matchtool
// says which line), and a like over the day's cap never goes out.
func TestMatchLikeGuardAndCap(t *testing.T) {
	faucet := launchPeople(t, "pat", "nina", "oscar", "lev")
	r := inProcess(faucet)
	for _, f := range []string{"nina.profile.json", "oscar.profile.json"} {
		b, _ := os.ReadFile(repo("examples/match/try/" + f))
		os.WriteFile(f, b, 0o644)
	}
	steps := [][]string{
		{"commons", "found", "--dir", "id/pat", "mersin-dating", "--rules", "18+ only"},
		{"commons", "request", "--dir", "agent/ai.nina", "mersin-dating"},
		{"commons", "request", "--dir", "agent/ai.oscar", "mersin-dating"},
		{"commons", "request", "--dir", "agent/ai.lev", "mersin-dating"},
		{"commons", "admit", "--dir", "id/pat", "mersin-dating", "ai.nina"},
		{"commons", "admit", "--dir", "id/pat", "mersin-dating", "ai.oscar"},
		{"commons", "admit", "--dir", "id/pat", "mersin-dating", "ai.lev"},
		{"model", "set", "local", "--dir", "agent/ai.oscar", "--owner-dir", "id/oscar"},
	}
	for _, s := range steps {
		if out, err := r(s...); err != nil {
			t.Fatalf("%v: %v %s", s, err, out)
		}
	}
	if _, err := match.Post(r, "agent/ai.nina", "mersin-dating", "nina.profile.json"); err != nil {
		t.Fatal(err)
	}
	me, _ := match.ParseProfile(must1(os.ReadFile("oscar.profile.json")))
	prefs := &match.Prefs{Schema: match.PrefsID, AgeMin: 18, AgeMax: 99, DailyLikes: 1}
	cs, _, err := match.Candidates(r, "agent/ai.oscar", "mersin-dating", me, prefs, "")
	if err != nil || len(cs) != 1 || cs[0].Name != "ai.nina" {
		t.Fatalf("candidates: %+v %v", cs, err)
	}
	// Oscar's agent read a stranger's profile: a like needs Oscar's approval of that name.
	_, err = match.Like(r, "agent/ai.oscar", "mersin-dating", "ai.nina", "We both hike.", prefs)
	var need *match.ErrNeedsOwner
	if !errors.As(err, &need) || need.Line != "ternchain owner approve --dir id/oscar ai.oscar ai.nina" {
		t.Fatalf("an unapproved like: %v", err)
	}
	if out, err := r("owner", "approve", "--dir", "id/oscar", "ai.oscar", "ai.nina"); err != nil {
		t.Fatalf("approve: %v %s", err, out)
	}
	if _, err := match.Like(r, "agent/ai.oscar", "mersin-dating", "ai.nina", "We both hike.", prefs); err != nil {
		t.Fatal(err)
	}
	if _, err := match.Like(r, "agent/ai.oscar", "mersin-dating", "ai.nina", "Again!", prefs); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("a second like to one name: %v", err)
	}
	r("owner", "approve", "--dir", "id/oscar", "ai.oscar", "ai.lev")
	if _, err := match.Like(r, "agent/ai.oscar", "mersin-dating", "ai.lev", "Chess?", prefs); err == nil || !strings.Contains(err.Error(), "daily cap") {
		t.Fatalf("a like over the daily cap of 1: %v", err)
	}
	// No contacts in a like.
	if _, err := match.Like(r, "agent/ai.oscar", "mersin-dating", "ai.lev", "Text me on telegram", &match.Prefs{DailyLikes: 5}); err == nil || !strings.Contains(err.Error(), "no contacts") {
		t.Fatalf("a like with a contact: %v", err)
	}
	// Lev's agent never saw a profile marked local-only withheld? It counts as cloud: candidates refuses.
	if _, _, err := match.Candidates(r, "agent/ai.lev", "mersin-dating", nil, prefs, ""); err == nil || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("a cloud agent scored local-only profiles: %v", err)
	}
	// A cloud agent can't save the attachment either (read --save is gated like the text).
	posts, _ := r("group", "read", "--dir", "agent/ai.lev", "mersin-dating")
	id := regexp.MustCompile(`"id": "(0x[0-9a-f]{16})"`).FindStringSubmatch(string(posts))
	if id == nil {
		t.Fatalf("no post for lev: %s", posts)
	}
	if _, err := r("read", "--dir", "agent/ai.lev", id[1], "--save", "leak.json"); err == nil {
		t.Fatal("a cloud agent saved a local-only profile")
	}
}

func must1[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// examples/match/demo.sh on a fresh local devnet, as a person runs it (bash,
// the real binaries, the stub scorer). Skipped with -short or without bash.
func TestMatchDemo(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: the demo builds three binaries and runs a devnet")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	addr := freeAddr(t)
	_, port, _ := strings.Cut(addr, ":")
	cmd := exec.Command(bash, repo("examples/match/demo.sh"))
	cmd.Env = append(os.Environ(), "W="+t.TempDir(), "PORT="+port, "TERNCHAIN_RELAY=", "TERNCHAIN_DIR=", "TERNCHAIN_NET=", "SCORER=")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "demo finished: every step ran") {
		t.Fatalf("demo.sh: %v\n%s", err, out)
	}
}
