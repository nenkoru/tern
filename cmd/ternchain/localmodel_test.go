package main

import (
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
