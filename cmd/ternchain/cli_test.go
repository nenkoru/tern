package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every command the help advertises is dispatched (it may fail later for
// lack of a network, but never as "unknown command"), with flags before or
// after the subcommand.
func TestCLIDispatchesEveryCommand(t *testing.T) {
	w := newWorld(t)
	w.owner("alice")
	testChain = w.ch
	defer func() { testChain = nil }()
	os.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	dir := filepath.Join(w.dir, "alice")
	cmds := [][]string{
		{"pot", "open"}, {"pot", "award"}, {"pot", "claim"},
		{"bounty", "open"}, {"bounty", "award"}, {"bounty", "claim"},
		{"revealbond", "add"}, {"revealbond", "settle"},
		{"lock", "show"}, {"lock", "giveback"}, {"lock", "claim"}, {"lock", "refund"}, {"lock", "release"},
		{"lock", "open"}, {"lock", "open", "escrow"}, {"lock", "decide"}, {"lock", "settle"}, {"lock", "dispute"},
		{"escrow", "open"}, {"escrow", "dispute"}, {"owner", "keygen"}, {"version"}, {"bond", "add"}, {"cond", "open"}, {"sub", "open"}, {"channel", "open"},
		{"artifact", "show"}, {"artifact", "add"}, {"fiat", "list"}, {"reveals", "x"}, {"commit", "x", "y"},
		{"bank", "balance"}, {"group", "read", "g"}, {"gov", "current", "k"},
		{"token", "balance", "usd"}, {"split", "create"}, {"item", "offers"}, {"item", "create"}, {"random", "open"},
		{"clock", "show"}, {"clock", "bond"}, {"panel", "open"}, {"panel", "case"}, {"panel", "evidence"}, {"panel", "argue"}, {"panel", "rule"}, {"panel", "settle"},
		{"price", "show"}, {"price", "commit"}, {"price", "reveal"}, {"price", "finalize"},
		{"commons", "found"}, {"commons", "request"}, {"commons", "admit"}, {"commons", "ban"}, {"commons", "hide"},
		{"commons", "rules"}, {"commons", "fee"}, {"commons", "follow"}, {"commons", "spend"}, {"commons", "payout"},
		{"commons", "veto"}, {"commons", "elect"}, {"commons", "stand"}, {"commons", "vote"}, {"commons", "close"},
		{"commons", "recall"}, {"commons", "sign"}, {"commons", "recall-vote"}, {"commons", "recall-close"}, {"commons", "show"},
		{"commons", "commission"}, {"commons", "fund"},
		{"htlc", "open"}, {"htlc", "claim"}, {"htlc", "show"}, {"htlc", "preimage"},
		{"fund", "create"}, {"fund", "pledge"}, {"fund", "show"}, {"fund", "collect"}, {"fund", "refund"},
		{"bridge", "cite"}, {"bridge", "post"}, {"bridge", "verify"},
		{"blocklist", "follow"}, {"blocklist", "unfollow"}, {"blocklist", "show"},
		{"pot", "--dir", "nowhere", "open"}, {"lock", "--dir", "nowhere", "giveback", "1", "all"},
	}
	for _, c := range cmds {
		err := run(append(c, "--dir", dir))
		if err != nil && strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%v: %v", c, err)
		}
	}
}
