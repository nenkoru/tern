package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalModelOnly(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	bot := w.agent(nina, "bot", coins(1), `{}`)
	kid := w.agent(nina, "kid", coins(1), `{}`) // joins a group before reading anything outside
	must(t, nina.createGroup("family", false))
	if _, err := kid.joinGroup("family"); err != nil {
		t.Fatal(err)
	}
	nina.admitPending()
	scan(t, kid)
	bot.setContact("oscar", true)
	nina.setContact("oscar", true)

	// An undeclared agent counts as cloud; an owner on its own device doesn't.
	if !bot.cloudModel() || nina.cloudModel() {
		t.Fatalf("defaults: agent cloud %v, owner cloud %v", bot.cloudModel(), nina.cloudModel())
	}
	if _, err := oscar.send(SendOpts{To: "bot.nina", Intent: "inform", Text: "the address is 12 Elm St", LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	oscar.send(SendOpts{To: "bot.nina", Intent: "inform", Text: "public hello"})
	var secret, plain *Item
	for _, it := range scan(t, bot) {
		if it.Mail == nil {
			continue
		}
		if it.Mail.LocalOnly {
			secret = it
		} else {
			plain = it
		}
	}
	if secret == nil || plain == nil {
		t.Fatal("mail not received")
	}
	v := bot.readViewOf(secret)
	if strings.Contains(v["content"].(string), "Elm") || v["local_only"] == nil {
		t.Fatalf("a cloud agent read local-only mail: %v", v)
	}
	if v := bot.readViewOf(plain); !strings.Contains(v["content"].(string), "public hello") {
		t.Fatal("ordinary mail withheld")
	}
	if iv := bot.inboxView(false); iv["local_only"] == nil {
		t.Fatal("the inbox doesn't say what it withheld")
	}
	// Its owner reads it on its own device.
	scan(t, nina)
	for _, it := range nina.st.Items {
		if it.ID == secret.ID {
			if v := nina.readViewOf(it); !strings.Contains(v["content"].(string), "Elm") {
				t.Fatal("the owner can't read it")
			}
		}
	}
	// Declared local: shown. Warn mode on a cloud model: shown under a warning.
	bot.f.Model = modelLocal
	if v := bot.readViewOf(secret); !strings.Contains(v["content"].(string), "Elm") {
		t.Fatal("a local model can't read local-only mail")
	}
	bot.f.Model, bot.f.LocalOnlyMode = "cloud:example", localOnlyWarn
	if v := bot.readViewOf(secret); !strings.Contains(v["content"].(string), "Elm") || v["warning"] == nil {
		t.Fatalf("warn mode: %v", v)
	}
	bot.f.LocalOnlyMode = ""
	// The owner marks a correspondent: all its mail is local-only.
	must(t, bot.markLocalOnly(true, "oscar"))
	if v := bot.readViewOf(plain); strings.Contains(v["content"].(string), "public hello") {
		t.Fatal("a marked correspondent's mail reached a cloud model")
	}
	must(t, bot.markLocalOnly(false, "oscar"))
	if err := validModel("gpt"); err == nil {
		t.Fatal("a model that is neither local nor cloud:P accepted")
	}

	// A group's admin marks the group: members get the mark with the keys.
	if _, err := nina.groupLocalOnly("family", true); err != nil {
		t.Fatal(err)
	}
	scan(t, kid)
	if !kid.st.Groups["family"].LocalOnly {
		t.Fatal("the group's mark didn't reach its members")
	}
	nina.postGroup("family", "medical results attached", nil)
	scan(t, kid)
	for _, it := range kid.groupItems("family") {
		if it.Mail != nil && it.Mail.Text == "medical results attached" {
			if v, _ := kid.blockedView(it, false); strings.Contains(v["subject"].(string), "medical") {
				t.Fatalf("a local-only group's post reached a cloud model: %v", v)
			}
		}
	}
}

// group post --local-only: the author marks one post; a cloud reader gets it
// withheld, and its attachment isn't saved either (read --save); a declared
// local model reads and saves it.
func TestLocalOnlyGroupPost(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	bot := w.agent(nina, "bot", coins(1), `{}`)
	must(t, nina.createGroup("club", true))
	if _, err := bot.joinGroup("club"); err != nil {
		t.Fatal(err)
	}
	scan(t, bot)
	f := filepath.Join(t.TempDir(), "profile.json")
	must(t, os.WriteFile(f, []byte(`{"age":31,"area":"Mersin"}`), 0o600))
	data, err := attachFile(f)
	must(t, err)
	id, err := nina.postGroupWith("club", "my profile", data, "", true)
	must(t, err)
	nina.postGroup("club", "an ordinary post", nil)
	scan(t, bot)
	var it *Item
	for _, x := range bot.groupItems("club") {
		if x.ID == id {
			it = x
		}
	}
	if it == nil || it.Mail == nil || !it.Mail.LocalOnly {
		t.Fatalf("the post's mark didn't travel: %+v", it)
	}
	if v, _ := bot.blockedView(it, false); strings.Contains(fmt.Sprint(v["subject"]), "profile") || v["local_only"] == nil {
		t.Fatalf("a cloud agent read a local-only post: %v", v)
	}
	testChain = w.ch
	defer func() { testChain = nil }()
	t.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	out := filepath.Join(t.TempDir(), "saved.json")
	save := []string{"read", "--dir", bot.dir, id, "--save", out}
	if _, err := cliResult(func() error { return run(save) }); err == nil || !strings.Contains(err.Error(), "local-model only") {
		t.Fatalf("a cloud agent saved a local-only attachment: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("the file was written anyway")
	}
	must(t, bot.setModel(modelLocal, ""))
	if _, err := cliResult(func() error { return run(save) }); err != nil {
		t.Fatalf("a local model can't save it: %v", err)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "Mersin") {
		t.Fatalf("saved: %q", b)
	}
}
