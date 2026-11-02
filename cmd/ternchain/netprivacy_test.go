package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
)

// restorePrivacy undoes what a test's privacy setup changed in this process.
func restorePrivacy(t *testing.T) {
	tr := http.DefaultTransport.(*http.Transport)
	saved, savedDA, ports, decoys, off := tr.Proxy, daHTTP.Transport, torPorts, bucketDecoys, bucketsOff
	t.Cleanup(func() {
		tr.Proxy, daHTTP.Transport, torPorts, bucketDecoys, bucketsOff = saved, savedDA, ports, decoys, off
		proxyInUse, privacyLevel, homeRPC, privacyNoted, privacyChosen = "", privacyStandard, "", false, false
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
			os.Unsetenv(k)
		}
	})
}

// fakeTor: a SOCKS5 server that answers the greeting (and records the
// usernames clients authenticate with), then serves one HTTP answer.
func fakeTor(t *testing.T) (string, chan string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	users := make(chan string, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 2)
				if _, err := io.ReadFull(c, b); err != nil {
					return
				}
				methods := make([]byte, b[1])
				io.ReadFull(c, methods)
				if strings.Contains(string(methods), "\x02") { // username/password (RFC 1929)
					c.Write([]byte{5, 2})
					h := make([]byte, 2)
					io.ReadFull(c, h)
					u := make([]byte, h[1])
					io.ReadFull(c, u)
					pl := make([]byte, 1)
					io.ReadFull(c, pl)
					p := make([]byte, pl[0])
					io.ReadFull(c, p)
					users <- string(u) + ":" + string(p)
					c.Write([]byte{1, 0})
				} else {
					c.Write([]byte{5, 0})
				}
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil {
					return
				}
				switch req[3] {
				case 1:
					io.ReadFull(c, make([]byte, 4+2))
				case 3:
					n := make([]byte, 1)
					io.ReadFull(c, n)
					io.ReadFull(c, make([]byte, int(n[0])+2))
				}
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				io.ReadFull(c, make([]byte, 16)) // the request's start
				c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
			}(c)
		}
	}()
	return l.Addr().String(), users
}

func TestPrivacyFindsTorAndIsolatesCircuits(t *testing.T) {
	restorePrivacy(t)
	addr, users := fakeTor(t)
	_, port, _ := net.SplitHostPort(addr)
	torPorts = []string{addr}
	// A local devnet: nothing to hide, no proxy.
	if err := ensurePrivacy([]string{"http://127.0.0.1:8545"}); err != nil || proxyInUse != "" {
		t.Fatalf("loopback: %v %q", err, proxyInUse)
	}
	if err := ensurePrivacy([]string{"https://rpc.example.org"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proxyInUse, addr) {
		t.Fatalf("Tor not used: %q", proxyInUse)
	}
	// Isolation: the fake isn't on 9050/9150, so check the rule directly too.
	u, _ := url.Parse("socks5://127.0.0.1:9050")
	a, b := isolated(u, "rpc.example.org"), isolated(u, "node.example.net")
	if a.User.String() == b.User.String() || a.Host != u.Host {
		t.Fatalf("one circuit for two destinations: %v %v", a, b)
	}
	if v, _ := url.Parse("socks5://user:pw@127.0.0.1:9050"); isolated(v, "x") != v {
		t.Fatal("a proxy with its own credentials got ours")
	}
	if v, _ := url.Parse("socks5://10.0.0.5:1080"); isolated(v, "x") != v {
		t.Fatal("a non-Tor proxy got isolation credentials")
	}
	// Through the fake Tor, on a "Tor" port: the destination is the username.
	torPorts = nil
	proxyInUse = ""
	must(t, useProxy("socks5://"+addr))
	_ = port
	r, err := http.Get("http://rpc.example.org/")
	if err == nil {
		r.Body.Close()
	}
	select {
	case got := <-users:
		t.Fatalf("a proxy on port %s got isolation credentials %q", port, got)
	default:
	}
}

func TestPrivacyStrict(t *testing.T) {
	restorePrivacy(t)
	torPorts = []string{"127.0.0.1:1"} // nothing listens there
	must(t, setPrivacyLevel(privacyStrict))
	if bucketDecoys < strictDecoys {
		t.Fatalf("strict decoys: %d", bucketDecoys)
	}
	err := ensurePrivacy([]string{"https://rpc.example.org"})
	if err == nil || !strings.Contains(err.Error(), "refusing to reach") {
		t.Fatalf("strict without a proxy: %v", err)
	}
	if err := ensurePrivacy([]string{"http://127.0.0.1:8545"}); err != nil {
		t.Fatalf("strict on a local devnet: %v", err)
	}
	if err := relayFirst(""); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("strict posting from our own account: %v", err)
	}
	if relayFirst("http://node.example") != nil {
		t.Fatal("strict refused a relay")
	}
	i := &Ident{f: IdentityFile{Retain: 7, KeepDays: 30}}
	if i.retain() != strictRetain {
		t.Fatalf("strict retention %d", i.retain())
	}
	c := &Client{Ident: i}
	if c.keepDays() != strictKeepDays {
		t.Fatalf("strict keep %d", c.keepDays())
	}
	if setPrivacyLevel("paranoid") == nil {
		t.Fatal("a bad level accepted")
	}
	// standard without Tor: direct, said once.
	must(t, setPrivacyLevel(privacyStandard))
	if err := ensurePrivacy([]string{"https://rpc.example.org"}); err != nil || proxyInUse != "" || !privacyNoted {
		t.Fatalf("standard without Tor: %v %q %v", err, proxyInUse, privacyNoted)
	}
}

func TestRPCRotation(t *testing.T) {
	restorePrivacy(t)
	urls := []string{"https://a", "https://b", "https://c"}
	first := map[string]bool{}
	for i := 0; i < 200; i++ {
		first[rpcOrder(urls)[0]] = true
	}
	if len(first) != 3 {
		t.Fatalf("rotation reaches %d of 3 RPCs", len(first))
	}
	privacyLevel = privacyOff
	if rpcOrder(urls)[0] != "https://a" {
		t.Fatal("privacy off reorders RPCs")
	}
	homeRPC = "http://my.node:8545"
	if o := rpcOrder(urls); len(o) != 1 || o[0] != homeRPC {
		t.Fatalf("home RPC: %v", o)
	}
}

type countLogs struct{ n int }

func (c *countLogs) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	c.n++
	return nil, nil
}
func (c *countLogs) BlockNumber(context.Context) (uint64, error) { return 0, nil }

// strict: log queries go to the second RPC, everything else to the first.
func TestSplitLogs(t *testing.T) {
	w := newWorld(t)
	logs := &countLogs{}
	b := splitLogs{w.ch.b, logs}
	if _, err := b.FilterLogs(context.Background(), ethereum.FilterQuery{}); err != nil || logs.n != 1 {
		t.Fatalf("logs went elsewhere: %d %v", logs.n, err)
	}
	if _, err := b.BlockNumber(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// strict: publish refuses to post from our own account, whatever called it (the MCP server too).
func TestStrictPublishNeedsARelay(t *testing.T) {
	restorePrivacy(t)
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	w.owner("nina")
	w.ch.witness = []Backend{w.ch.b} // satisfy key pinning so this test reaches the relay guard
	must(t, setPrivacyLevel(privacyStrict))
	if _, err := oscar.send(SendOpts{To: "nina", Intent: "inform", Text: "x"}); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("strict sent from its own account: %v", err)
	}
}
