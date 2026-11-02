package main

import (
	"strings"
	"testing"
)

// send --to group:G (and MCP tern_send) into an anonymous group would seal a
// datagram signed by name: refused, and nothing reaches the members (redteam A-9).
func TestAnonGroupRefusesNamedSend(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ada, _ := w.owner("ada")
	anonCell(t, w, "cell", false, 0, nina, ada)
	_, err := ada.send(SendOpts{To: "group:cell", Intent: "inform", Text: "named"})
	if err == nil || !strings.Contains(err.Error(), "anonymous group") {
		t.Fatalf("send to an anonymous group: %v", err)
	}
	if _, err := runTool(ada, "tern_send", map[string]any{"to": "group:cell", "text": "named too"}); err == nil {
		t.Fatal("tern_send to an anonymous group went out")
	}
	for _, it := range anonPosts(t, nina, "cell") {
		if it.Mail != nil && strings.HasPrefix(it.Mail.Text, "named") {
			t.Fatalf("a member received a name-signed post: %+v", it)
		}
	}
	// The anonymous post path still works.
	if _, err := ada.postGroup("cell", "anonymous hello", nil); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range anonPosts(t, nina, "cell") {
		found = found || (it.Mail != nil && it.Mail.Text == "anonymous hello" && it.From != "ada")
	}
	if !found {
		t.Fatal("the anonymous post didn't arrive without a name")
	}
}
