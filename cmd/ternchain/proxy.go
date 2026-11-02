// Transport privacy: --proxy socks5://HOST:PORT (or TERNCHAIN_PROXY) sends
// every outside HTTP call through a proxy: JSON-RPC (and its websocket),
// relays and data nodes, the faucet. With Tor (socks5://127.0.0.1:9050) the
// RPC, the relay and the faucet see a Tor exit, not your IP. Host names are
// resolved by the proxy (Go's SOCKS5 client sends them as names), so .onion
// URLs work. Loopback addresses are never proxied (a local devnet).
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func useProxy(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("--proxy %q: want socks5://HOST:PORT (Tor: socks5://127.0.0.1:9050) or http://HOST:PORT", raw)
	}
	switch u.Scheme {
	case "socks5h":
		u.Scheme = "socks5" // Go's SOCKS5 client already lets the proxy resolve names
	case "socks5", "http", "https":
	default:
		return fmt.Errorf("--proxy %q: socks5, http or https", raw)
	}
	pick := func(req *http.Request) (*url.URL, error) {
		if loopback(req.URL.Hostname()) {
			return nil, nil
		}
		return isolated(u, req.URL.Hostname()), nil // with Tor: a circuit per destination (netprivacy.go)
	}
	proxyInUse = u.Redacted()
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.Proxy = pick
	}
	// Data node fetches: through the proxy too. Their private-address guard
	// can't apply (the proxy dials, not us), so it guards the proxy's exits.
	daHTTP.Transport = &http.Transport{Proxy: pick, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 10 * time.Second}
	daHTTP.Timeout = 15 * time.Second // Tor is slower
	// Websockets (go-ethereum's dialer reads the environment).
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		os.Setenv(k, u.String())
	}
	if os.Getenv("NO_PROXY") == "" {
		os.Setenv("NO_PROXY", "localhost,127.0.0.1,::1")
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// publishASP keeps an association set by a rule (relay serve --asp NAME
// [--rule clean24h]): it republishes whenever the set changed, every 10 minutes.
func (r *Relay) publishASP(name, rule string) {
	if rule == "" {
		rule = "clean24h"
	}
	c := &Client{Ident: &Ident{key: r.key, f: IdentityFile{Name: r.name}}, ch: r.ch, res: newResolver(r.ch), st: &State{}}
	for {
		if ls, err := c.aspRule(rule); err != nil {
			log.Printf("asp %s: %v", name, err)
		} else if len(ls) > 0 {
			if m, err := c.aspPublish(name, ls, "rule:"+rule); err != nil {
				if strings.Contains(err.Error(), "belongs to") { // squatted: clients asking for NAME would get someone else's set (audit 5 PV-9)
					log.Fatalf("asp %s: %v: choose another --asp name", name, err)
				}
				log.Printf("asp %s: %v", name, err)
			} else if m["unchanged"] == nil {
				log.Printf("asp %s: published %v deposits, root %v", name, m["labels"], m["root"])
			}
		}
		time.Sleep(10 * time.Minute)
	}
}
