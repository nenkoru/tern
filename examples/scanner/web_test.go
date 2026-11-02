package main

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nenkoru/tern/internal/scan"
)

// The page and its data: the scan as JSON with the names' order and the swap
// rows, and a policy that loads nothing from elsewhere.
func TestServePage(t *testing.T) {
	s := &scan.State{Network: "test", ToBlock: 7,
		Names: map[string]*scan.Name{
			"alice":    {Name: "alice", Kind: "owner", Block: 1, Agents: []string{"ai.alice"}, Card: `<script>alert(1)</script>`},
			"ai.alice": {Name: "ai.alice", Kind: "agent", Owner: "alice", Block: 2},
			"bob":      {Name: "bob", Kind: "owner", Block: 3},
		},
		Locks:  []*scan.Lock{{ID: 0, From: "alice", Item: "4", Token: "0xabc item", Total: "1"}, {ID: 1, From: "bob", Token: scan.Coin, Total: "2"}},
		Swaps:  []*scan.Swap{{Offer: 0, Answer: 1}},
		Tokens: map[string]*scan.Token{scan.Coin: {Symbol: "coin", Decimals: 18}, "0xabc": {Symbol: "SWORD", Decimals: 0}},
	}
	srv := httptest.NewServer(handler(func() []byte { return pageOf(s, time.Minute, nil) }))
	defer srv.Close()

	r, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(body), "tern scanner") ||
		!strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("page: %d %q", r.StatusCode, r.Header.Get("Content-Security-Policy"))
	}
	if strings.Contains(string(body), "innerHTML") {
		t.Fatal("the page puts chain data in as markup")
	}

	r, err = srv.Client().Get(srv.URL + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Network string         `json:"network"`
		Order   []string       `json:"order"`
		Swaps   []swapRow      `json:"swap_rows"`
		Every   int64          `json:"every_seconds"`
		Names   map[string]any `json:"names"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Network != "test" || strings.Join(p.Order, ",") != "alice,ai.alice,bob" || p.Every != 60 || len(p.Names) != 3 {
		t.Fatalf("state: %+v", p)
	}
	if len(p.Swaps) != 1 || p.Swaps[0].Price != "2 coin per item 4 of SWORD" {
		t.Fatalf("swap rows: %+v", p.Swaps)
	}

	if r, _ := srv.Client().Get(srv.URL + "/other"); r.StatusCode != 404 {
		t.Fatalf("an unknown path: %d", r.StatusCode)
	}
}
