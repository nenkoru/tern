// Network privacy by default.
//
//	--privacy standard|strict|off   (TERNCHAIN_PRIVACY, or net.json "privacy"; default standard)
//	--home-rpc URL                  every RPC call to your own node, or one you trust (TERNCHAIN_HOME_RPC)
//	--home URL                      fetch mail from this data node (TERNCHAIN_HOME)
//	--fetch all                     download every envelope, not your buckets (TERNCHAIN_FETCH)
//	--decoys N                      decoy buckets beside yours in each query (default 3; strict 12)
//
// standard: when no --proxy is given, the client looks for Tor on this machine
// (127.0.0.1:9050, then 9150: the Tor Browser) and goes through it; without
// Tor it goes direct and says so on stderr. Net.json's RPCs are tried in a
// random order, so no single provider sees every session.
//
// strict: everything of standard, and
//   - no proxy, no connection: a non-loopback RPC, data node or relay is
//     refused rather than reached from your IP;
//   - envelopes go through a relay (--relay or net.json "relay"), never as a
//     transaction from your own account, which would put "this account mailed
//     at this time" on chain;
//   - log scans (what you read: which events, which groups) go to another of
//     net.json's RPCs than your calls and transactions;
//   - 12 decoy buckets; epoch keys kept 2 epochs past their epoch, read mail 7 days.
//
// off: no Tor detection, no shuffling: the old behaviour.
//
// Tor circuits: with a Tor proxy (port 9050 or 9150, no credentials of its
// own), each destination host gets its own SOCKS credentials, which Tor's
// IsolateSOCKSAuth turns into its own circuit: the RPC, a data node and a
// relay don't see one exit in common.
package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	privacyOff      = "off"
	privacyStandard = "standard"
	privacyStrict   = "strict"

	strictRetain   = 2  // epochs an epoch key outlives its epoch, strict
	strictKeepDays = 7  // days read mail stays, strict
	strictDecoys   = 12 // decoy buckets, strict
)

var (
	privacyLevel = privacyStandard
	proxyInUse   = "" // the proxy every outside call goes through ("" : direct)
	torPorts     = []string{"127.0.0.1:9050", "127.0.0.1:9150"}
	homeRPC      = "" // --home-rpc: replaces net.json's RPCs
	privacyNoted = false
	// privacyChosen: the level came from --privacy or TERNCHAIN_PRIVACY (net.json's "privacy" yields to it).
	privacyChosen    = false
	privacyEndpoints []string // what this command may reach besides the RPCs (relay, home)
)

func strict() bool { return privacyLevel == privacyStrict }

func setPrivacyLevel(s string) error {
	switch s {
	case "":
		return nil
	case privacyOff, privacyStandard, privacyStrict:
		privacyLevel = s
		if s == privacyStrict {
			bucketDecoys = max(bucketDecoys, strictDecoys)
		}
		return nil
	}
	return fmt.Errorf("--privacy %q: standard, strict or off", s)
}

// socksTor: a SOCKS5 server that answers the no-auth greeting (Tor does).
func socksTor(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return false
	}
	b := make([]byte, 2)
	_, err = io.ReadFull(conn, b)
	return err == nil && b[0] == 5 && b[1] == 0
}

// ensurePrivacy runs before connecting: endpoints are what this command will
// reach (RPCs, relay, home). All loopback (a local devnet): nothing to hide.
func ensurePrivacy(endpoints []string) error {
	outside := false
	for _, e := range endpoints {
		if u, err := url.Parse(e); err == nil && u.Hostname() != "" && !loopback(u.Hostname()) {
			outside = true
		}
	}
	if !outside || privacyLevel == privacyOff || proxyInUse != "" {
		return nil
	}
	for _, a := range torPorts {
		if socksTor(a) {
			if err := useProxy("socks5://" + a); err != nil {
				return err
			}
			notePrivacy("network: through Tor at " + a + " (--privacy off goes direct)")
			return nil
		}
	}
	if strict() {
		return fmt.Errorf("--privacy strict: no proxy and no Tor on this machine (%s): refusing to reach RPCs and data nodes from your IP. "+
			"Start Tor, or give --proxy socks5://HOST:PORT", strings.Join(torPorts, ", "))
	}
	notePrivacy("network: direct: RPCs and data nodes see your IP (start Tor or give --proxy; --privacy strict refuses to go direct)")
	return nil
}

func notePrivacy(s string) {
	if !privacyNoted && os.Getenv("TERNCHAIN_QUIET") == "" {
		fmt.Fprintln(os.Stderr, s)
	}
	privacyNoted = true
}

// isolating: a Tor proxy of this machine without credentials of its own.
func isolating(u *url.URL) bool {
	p := u.Port()
	return u.Scheme == "socks5" && u.User == nil && loopback(u.Hostname()) && (p == "9050" || p == "9150")
}

// isolated: the proxy URL for a request to host: Tor gets one credential per
// destination, so one circuit per destination.
func isolated(u *url.URL, host string) *url.URL {
	if !isolating(u) {
		return u
	}
	v := *u
	v.User = url.UserPassword("tern", host)
	return &v
}

// rpcOrder: net.json's RPCs, shuffled unless privacy is off (the first that
// answers is used: over many sessions, each provider sees a share).
func rpcOrder(urls []string) []string {
	if homeRPC != "" {
		return []string{homeRPC}
	}
	if privacyLevel == privacyOff || len(urls) < 2 {
		return urls
	}
	out := append([]string(nil), urls...)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// splitLogs: log queries through another RPC than the rest (strict): the one
// that sees your transactions doesn't see what you read.
type splitLogs struct {
	Backend
	logs logFilterer
}

func (s splitLogs) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	return s.logs.FilterLogs(ctx, q)
}

// relayFirst: strict privacy posts through a relay, not from your own account.
func relayFirst(relay string) error {
	if strict() && relay == "" {
		return fmt.Errorf("--privacy strict posts envelopes through a relay (a data node): give --relay URL or TERNCHAIN_RELAY. " +
			"Posting from your own account puts \"this account mailed now\" on chain")
	}
	return nil
}

// privacyStatus: whoami's "network" section.
func privacyStatus(c *Client) map[string]any {
	m := map[string]any{"privacy": privacyLevel, "proxy": proxyInUse, "decoy_buckets": bucketDecoys, "fetch_all": bucketsOff}
	if proxyInUse == "" {
		m["proxy"] = "none: RPCs and data nodes see your IP"
	}
	if homeRPC != "" {
		m["home_rpc"] = homeRPC
	}
	if c != nil {
		m["home_nodes"] = c.homeNodes()
		m["retain_epochs"], m["keep_days"] = c.retain(), c.keepDays()
	}
	return m
}

// splitVia: the RPC log queries go to (strict, with two RPCs or more).
var splitVia logFilterer

// dialLogs: a second RPC for log queries, if it answers as the same chain.
func dialLogs(f *DevnetFile, u string) logFilterer {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cl, err := ethclient.DialContext(ctx, u)
	if err != nil {
		return nil
	}
	if id, err := cl.ChainID(ctx); err != nil || f.ChainID != 0 && id.Int64() != f.ChainID {
		cl.Close()
		return nil
	}
	if f.LogRange > 0 {
		return newRangedClient(cl, f.LogRange)
	}
	return cl
}
