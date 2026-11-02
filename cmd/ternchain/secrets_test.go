package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rule 12 is a floor: the client refuses to ship its own keys, whatever asks.
func TestNoSecretsLeaveTheClient(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	ada := w.agent(oscar, "ada", coins(1), `{}`)
	must(t, nina.createGroup("club", false))
	for _, p := range []string{filepath.Join(ada.dir, "eth.key"), filepath.Join(ada.dir, "view.key"), filepath.Join(ada.dir, "identity.json"), ada.dir} {
		if _, err := attachFile(p); err == nil || !strings.Contains(err.Error(), "rule 12") {
			t.Fatalf("attach %s: %v", filepath.Base(p), err)
		}
		if _, err := ada.addArtifact(p, "https://example.org/x", "", nil); err == nil || !strings.Contains(err.Error(), "rule 12") {
			t.Fatalf("artifact add %s: %v", filepath.Base(p), err)
		}
	}
	key, _ := os.ReadFile(filepath.Join(ada.dir, "eth.key"))
	k := strings.TrimSpace(string(key))
	for name, text := range map[string]string{
		"verbatim": "here: " + k,
		"upper":    strings.ToUpper(k),
		"base64":   base64.StdEncoding.EncodeToString(key),
	} {
		if _, err := ada.send(SendOpts{To: oscar.f.Name, Intent: "inform", Text: text}); err == nil || !strings.Contains(err.Error(), "rule 12") {
			t.Fatalf("%s key in text: %v", name, err)
		}
	}
	// An epoch key, and a private group's key, are secrets too.
	eps, _ := filepath.Glob(filepath.Join(nina.dir, "epochs", "*"))
	ek, _ := os.ReadFile(eps[0])
	if _, err := nina.send(SendOpts{To: oscar.f.Name, Intent: "inform", Text: string(ek)}); err == nil {
		t.Fatal("an epoch key went out")
	}
	if _, err := nina.postGroup("club", "key: "+nina.st.Groups["club"].ViewKey, nil); err == nil {
		t.Fatal("a private group's view key went out")
	}
	// Ordinary mail, and an artifact's own key file, still go.
	if _, err := ada.send(SendOpts{To: oscar.f.Name, Intent: "inform", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "film.enc"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "film.key"), []byte("0123456789abcdef0123456789abcdef"), 0o600)
	if _, err := attachFile(filepath.Join(dir, "film.key")); err != nil {
		t.Fatal(err)
	}
}
