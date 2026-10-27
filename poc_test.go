package main

import (
	"strings"
	"testing"
	"time"
)

// TestPoCRandomRoundGriefedByOneEntry proves the Random griefing vector:
// Commitments.commit does not know about rounds, so anyone can add an entry
// under a round's topic and never reveal it. Random.draw then refuses to draw
// ("waiting for reveals") until the full reveal window elapses, because one
// unrevealed entry sets all=false. A single griefer thus blocks (delays) any
// round -- and bloats the per-draw loop. The exploit "works" when the draw is
// blocked even though the only honest entry was revealed.
func TestPoCRandomRoundGriefedByOneEntry(t *testing.T) {
	w := newWorld(t)
	host, _ := w.owner("host")
	player, _ := w.owner("player")
	griefer, _ := w.owner("griefer")

	topic := "lottery-1"
	now := uint64(host.chainNow())
	commitBy := now + 100
	revealBy := now + 100000
	if err := host.openRound(topic, commitBy, revealBy); err != nil {
		t.Fatal(err)
	}

	// The honest player commits and reveals its entry.
	id, pre, err := player.commitValue(topic, []byte("7"), revealBy)
	if err != nil {
		t.Fatal(err)
	}
	if err := player.revealValue(id, pre); err != nil {
		t.Fatal(err)
	}

	// The griefer adds an entry under the same topic and never reveals it.
	if _, _, err := griefer.commitValue(topic, []byte("junk"), revealBy); err != nil {
		t.Fatal(err)
	}

	// Time passes the commit deadline but not the reveal deadline.
	w.later(300 * time.Second)

	// The draw is blocked by the one unrevealed griefer entry.
	if _, _, err := host.drawRound(topic); err == nil {
		t.Fatal("draw succeeded despite an unrevealed griefer entry; expected it to be blocked")
	} else if !strings.Contains(err.Error(), "waiting for reveals") {
		t.Fatalf("draw failed for the wrong reason: %v", err)
	}

	// Confirm it is only a delay (griefing, not permanent): after the reveal
	// window the round can finally be drawn, skipping the griefer.
	w.later(200000 * time.Second)
	if _, n, err := host.drawRound(topic); err != nil {
		t.Fatalf("draw after the reveal window failed: %v", err)
	} else if n != 1 {
		t.Fatalf("expected 1 counted entry after the griefer was skipped, got %d", n)
	}
	// Exploit succeeded: a single unrevealed entry forced every participant to
	// wait the entire reveal window before the round could resolve.
}
