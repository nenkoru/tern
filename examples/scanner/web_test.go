package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	s.IDs = map[string]string{"0xabc": "alice"}
	s.Addrs = map[string]string{"0xa11ce": "alice"}
	key := errors.New(`Post "https://base-sepolia.g.alchemy.com/v2/SECRETKEY": context deadline exceeded`)
	srv := httptest.NewServer(handler(func() []byte { return pageOf(s, time.Minute, key) }, "127.0.0.1:8080"))
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
	if csp := r.Header.Get("Content-Security-Policy"); strings.Contains(csp, "img-src") || strings.Contains(csp, "http") {
		t.Fatalf("the policy allows more than it needs: %s", csp)
	}

	r, err = srv.Client().Get(srv.URL + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r.Body)
	if strings.Contains(string(raw), "SECRETKEY") || !strings.Contains(string(raw), "https://base-sepolia.g.alchemy.com") {
		t.Fatalf("the page's error shows the rpc's key: %s", raw)
	}
	if strings.Contains(string(raw), `"ids"`) || strings.Contains(string(raw), `"addrs"`) {
		t.Fatal("state.json serves the ids and addrs tables")
	}
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
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

// Only loopback names and the --listen host are answered (DNS rebinding).
func TestServeHostCheck(t *testing.T) {
	data := func() []byte { return []byte("{}") }
	for _, c := range []struct {
		listen, host string
		ok           bool
	}{
		{"127.0.0.1:8080", "127.0.0.1:8080", true},
		{"127.0.0.1:8080", "localhost:8080", true},
		{"127.0.0.1:8080", "[::1]:8080", true},
		{"127.0.0.1:8080", "evil.example:8080", false},
		{"127.0.0.1:8080", "evil.example", false},
		{"127.0.0.1:8080", "192.168.1.5:8080", false},
		{"scan.example:8080", "scan.example:8080", true},
		{"scan.example:8080", "evil.example:8080", false},
		{"0.0.0.0:8080", "192.168.1.5:8080", true},
		{"0.0.0.0:8080", "evil.example:8080", false},
		{":8080", "[fe80::1]:8080", true},
	} {
		r := httptest.NewRequest("GET", "/state.json", nil)
		r.Host = c.host
		w := httptest.NewRecorder()
		handler(data, c.listen).ServeHTTP(w, r)
		if (w.Code == 200) != c.ok {
			t.Errorf("listen %s, Host %s: %d", c.listen, c.host, w.Code)
		}
	}
}

// An update's error never carries the rpc's key, on the terminal or the page.
func TestUpdateErrorRedacted(t *testing.T) {
	stop := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer func() { close(stop); hang.CloseClientConnections(); hang.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	n := scan.Net{Network: "test", ChainID: 1, Contracts: map[string]string{"Registry": "0x01", "Bank": "0x02"}}
	s := scan.Load(filepath.Join(t.TempDir(), "c.json"), n)
	err := updateFrom(ctx, s, n, hang.URL+"/v2/SECRETKEY?key=SECRET2", filepath.Join(t.TempDir(), "c.json"), 0)
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), hang.URL) {
		t.Fatalf("update: %v", err)
	}
}

// Chain strings reach the terminal without control codes or bidi overrides.
func TestTerminalText(t *testing.T) {
	var b strings.Builder
	claims(&b, []*scan.Claim{{Attester: "alice", Subject: "bob", Kind: "re\x1b]0;pwned\x07view", Data: "fine\u202e\x1b[2J"}})
	if strings.ContainsAny(b.String(), "\x1b\x07\u202e") {
		t.Fatalf("%q", b.String())
	}
	if c := cardLine(`{"offer":"\u001b[31mred"}`); strings.Contains(c, "\x1b") {
		t.Fatalf("card %q", c)
	}
	if fileName("../../etc/x") != "network" || fileName("tern-base-sepolia-1") != "tern-base-sepolia-1" {
		t.Fatal("file name")
	}
}
