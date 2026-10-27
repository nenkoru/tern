package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Founders for the citizens' house, end to end on a devnet over HTTP:
// owner keygen (offline) → devnet --genesis NAME=ADDR,… --shared --operator
// --contact → owner init --key (with only the public net.json) → gov claim.
// The same network checks the shared mode and the notice.
func TestSeason2FoundersSharedNotice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERNCHAIN_RELAY", "")
	t.Setenv("TERNCHAIN_DIR", "")
	names := []string{"oscar", "nina", "victor", "olga"}
	var gen []string
	for _, n := range names {
		addr, err := keygen(filepath.Join(dir, n+".key"))
		if err != nil {
			t.Fatal(err)
		}
		if st, _ := os.Stat(filepath.Join(dir, n+".key")); st.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode %v", st.Mode())
		}
		gen = append(gen, n+"="+addr.Hex())
	}
	if _, err := keygen(filepath.Join(dir, "oscar.key")); err == nil {
		t.Fatal("keygen overwrote a key")
	}
	g, err := parseGenesisStrict(strings.Join(gen, ","))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Join(gen[:3], ","), "oscar=0x12", "Oscar=" + strings.Split(gen[0], "=")[1], gen[0] + "," + gen[0]} {
		if _, err := parseGenesisStrict(bad); err == nil {
			t.Fatalf("--genesis %q accepted", bad)
		}
	}
	cfg := devnetConfig{net: filepath.Join(dir, "op", "net.json"), listen: "127.0.0.1:0", admin: "127.0.0.1:0", dataDir: filepath.Join(dir, "data"),
		faucetCoins: coins(20), shared: true, operator: "Oscar", contact: "t.me/example",
		p: Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12), Gov: GovParams{Genesis: g, Short: true}}}
	d, err := startDevnet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var pub DevnetFile
	readJSON(cfg.net, &pub)
	if pub.Mode != "shared" || !strings.Contains(pub.Notice, "run by Oscar") || !strings.Contains(pub.Notice, "t.me/example") || pub.Faucet != "" {
		t.Fatalf("net.json: %+v", pub)
	}
	if h, _ := contractsHash(); pub.ContractsHash != h {
		t.Fatalf("net.json contracts hash %q, binary %q", pub.ContractsHash, h)
	}
	friend := filepath.Join(dir, "friend", "net.json") // all a founder gets: the public file
	os.MkdirAll(filepath.Dir(friend), 0o755)
	b, _ := os.ReadFile(cfg.net)
	os.WriteFile(friend, b, 0o644)
	// The faucet's reply carries the notice.
	resp, err := http.Post(pub.FaucetURL, "application/json", strings.NewReader(`{"address":"0x000000000000000000000000000000000000dEaD"}`))
	if err != nil {
		t.Fatal(err)
	}
	var fr map[string]string
	json.NewDecoder(resp.Body).Decode(&fr)
	resp.Body.Close()
	if !strings.Contains(fr["notice"], "run by Oscar") {
		t.Fatalf("faucet reply: %v", fr)
	}
	for _, n := range names[:3] { // three founders join and claim (the fourth may come later)
		id := filepath.Join(dir, "friend", "id", n)
		if err := run([]string{"owner", "init", "--net", friend, "--dir", id, "--name", n, "--key", filepath.Join(dir, n+".key"),
			"--guardian-dir", filepath.Join(dir, "friend", "guardian", n)}); err != nil {
			t.Fatalf("owner init --key: %v", err)
		}
		i, _ := loadIdent(id)
		if want := strings.Split(gen[indexOf(names, n)], "=")[1]; i.addr().Hex() != want {
			t.Fatalf("owner init --key used %s, not %s", i.addr().Hex(), want)
		}
		if err := run([]string{"gov", "claim", "--net", friend, "--dir", id}); err != nil {
			t.Fatalf("%s: gov claim: %v", n, err)
		}
		ch, _, err := connect(friend)
		if err != nil {
			t.Fatal(err)
		}
		if v, err := ch.call("Citizens", "isCitizen", idOf(n)); err != nil || !v[0].(bool) {
			t.Fatalf("%s is not a citizen after gov claim: %v %v", n, v, err)
		}
	}
	// A squatter's key can't claim a founding name.
	sq := filepath.Join(dir, "friend", "id", "olga")
	if err := run([]string{"owner", "init", "--net", friend, "--dir", sq, "--name", "olga", "--guardian-dir", filepath.Join(dir, "friend", "guardian", "olga")}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"gov", "claim", "--net", friend, "--dir", sq}); err == nil {
		t.Fatal("a name registered with another key claimed the founder's citizenship")
	}
	// Shared mode: the client refuses what a shared test network doesn't do.
	t.Setenv("TERNCHAIN_MODULES", "fiat") // fiat is an optional module since BRICKS #53: on, so the network's refusal is what answers
	rid := filepath.Join(dir, "friend", "id", "oscar")
	for _, args := range [][]string{
		{"fiat", "paid", "nina", "100", "EUR", "sepa"},
		{"pot", "open", "nina", "1", "a game"},
		{"random", "open", "dice", "--minutes", "5"},
		{"token", "mint", "usd", "oscar", "100"},
	} {
		err := run(append(args, "--net", friend, "--dir", rid))
		if err == nil || !strings.Contains(err.Error(), "shared test network") {
			t.Fatalf("%v on a shared network: %v", args, err)
		}
	}
	if err := sharedRefusal("fiat", []string{"paid", "nina", "1", "TEST", "test"}, false); err != nil {
		t.Fatalf("fiat in TEST refused: %v", err)
	}
	if err := sharedRefusal("private send", []string{"nina", "1"}, false); err != nil { // standard (ERC-5564, Privacy Pools): allowed again
		t.Fatalf("private payments refused on a shared network: %v", err)
	}
	if err := sharedRefusal("owner init", nil, true); err == nil {
		t.Fatal("--passkey allowed on a shared network")
	}
	// A restart without the flags keeps what net.json advertised.
	d.Close()
	cfg2 := cfg
	cfg2.shared, cfg2.operator, cfg2.contact = false, "", ""
	d2, err := startDevnet(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	var again DevnetFile
	readJSON(cfg.net, &again)
	if again.Mode != "shared" || again.Operator != "Oscar" || again.Notice != pub.Notice {
		t.Fatalf("restart dropped the network's settings: %+v", again)
	}
}

func hasEvent(t *testing.T, c *Client, typ, text string) bool {
	t.Helper()
	evs, err := c.pollEvents(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type == typ && strings.Contains(e.Text, text) {
			return true
		}
	}
	t.Logf("events: %+v", evs)
	return false
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

// Pay unless disputed: paid after the window; a dispute hands it to the
// arbiter; an early claim, a late dispute and a dispute by the payee are refused.
func TestOptimisticEscrow(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.owner("carol") // the arbiter: neither side's
	w.must(alice.deposit(coins(10)))
	const hour = 3600

	// No dispute: paid after the window, not before.
	bob.pollEvents(nil, false) // bob's watch runs
	l1, _, err := alice.openOptimistic("bob", coins(1), 2*hour, 2*86400, []string{"carol"}, 1, "logo")
	w.must(err)
	w.mustFail(bob.settleOptimistic(l1, nil), "challenge window is open")
	b0, _ := bob.bankBalance("bob")
	w.later(3 * time.Hour)
	if !hasEvent(t, bob, "claimable", fmt.Sprintf("lock claim %d", l1)) {
		t.Fatal("the payee wasn't told the window is over")
	}
	w.mustFail(alice.disputeOptimistic(l1, "too late"), "window is over")
	w.must(bob.settleOptimistic(l1, nil))
	if b1, _ := bob.bankBalance("bob"); new(big.Int).Sub(b1, b0).Cmp(coins(1)) != 0 {
		t.Fatalf("paid %s after the window", fmtCoins(new(big.Int).Sub(b1, b0)))
	}

	// Disputed: the arbiter decides; the payee can't claim after the window.
	l2, subj, err := alice.openOptimistic("bob", coins(1), 2*hour, 2*86400, []string{"carol"}, 1, "banner")
	w.must(err)
	w.must(bob.acceptLock(l2))
	carol, err := openClient(filepath.Join(w.dir, "carol"), w.ch)
	w.must(err)
	early, err := carol.rule(subj, 10000, "before any dispute") // a ruling before the dispute doesn't count
	if err == nil || !strings.Contains(err.Error(), "not disputed") {
		t.Fatalf("a ruling before the dispute was recorded: %v", err)
	}
	w.mustFail(bob.disputeOptimistic(l2, "I am the payee"), "only the payer")
	carol.pollEvents(nil, false)
	w.must(alice.disputeOptimistic(l2, "the banner is the wrong size"))
	if !hasEvent(t, carol, "disputed", "you decide") {
		t.Fatal("the arbiter wasn't told of the dispute")
	}
	w.mustFail(alice.disputeOptimistic(l2, "again"), "already disputed")
	w.later(3 * time.Hour)
	w.mustFail(bob.settleOptimistic(l2, nil), "")
	w.mustFail(bob.settleOptimistic(l2, []common.Hash{early}), "nothing moved")
	r, err := carol.rule(subj, 3000, "30% delivered")
	w.must(err)
	a0, _ := alice.bankBalance("alice")
	b0, _ = bob.bankBalance("bob")
	w.must(bob.settleOptimistic(l2, []common.Hash{r}))
	a1, _ := alice.bankBalance("alice")
	b1, _ := bob.bankBalance("bob")
	if new(big.Int).Sub(b1, b0).Cmp(exact("0.3")) != 0 || new(big.Int).Sub(a1, a0).Cmp(exact("0.7")) != 0 {
		t.Fatalf("ruling applied: bob +%s, alice +%s", fmtCoins(new(big.Int).Sub(b1, b0)), fmtCoins(new(big.Int).Sub(a1, a0)))
	}

	// The CLI: open, show, dispute through the same commands.
	adir := filepath.Join(w.dir, "alice")
	o, err := cli(t, w, "escrow", "open", "bob", "0.5", "a", "poster", "--optimistic", "24", "--arbiter", "carol", "--dir", adir)
	w.must(err)
	var opened map[string]any
	json.Unmarshal([]byte(o), &opened)
	id := fmt.Sprint(opened["lock"])
	o, err = cli(t, w, "lock", "show", id, "--dir", adir)
	w.must(err)
	if !strings.Contains(o, "pay unless disputed") || !strings.Contains(o, "window_left") {
		t.Fatalf("lock show: %s", o)
	}
	if _, err := cli(t, w, "escrow", "dispute", id, "not", "delivered", "--dir", adir); err != nil {
		t.Fatal(err)
	}
	o, _ = cli(t, w, "lock", "show", id, "--dir", adir)
	if !strings.Contains(o, "disputed_at") {
		t.Fatalf("lock show after a dispute: %s", o)
	}
}

// The payer is refused a lock whose decider is the payee's side, unless --force.
func TestPayerDeciderWarning(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	w.owner("carol")
	w.agent(bob, "ada", coins(1), `{}`)
	w.must(alice.deposit(coins(10)))
	for _, open := range []func() error{
		func() error {
			_, _, err := alice.openEscrow("ada.bob", "bob", coins(0.1), alice.chainNowU()+86400, "x")
			return err
		},
		func() error {
			_, _, err := alice.openEscrow("bob", "ada.bob", coins(0.1), alice.chainNowU()+86400, "x")
			return err
		},
		func() error {
			_, err := alice.openConditional("ada.bob", "bob", "x", "eval", 50, coins(0.1), alice.chainNowU()+86400)
			return err
		},
		func() error {
			_, _, err := alice.bond("bob", "ada.bob", coins(0.1), alice.chainNowU()+86400, "x")
			return err
		},
	} {
		w.mustFail(open(), "payee's side")
	}
	_, _, err := alice.openEscrow("ada.bob", "carol", coins(0.1), alice.chainNowU()+86400, "fine")
	w.must(err)
	alice.force = true
	_, _, err = alice.openEscrow("ada.bob", "bob", coins(0.1), alice.chainNowU()+86400, "forced")
	w.must(err)
}

// The approval round trip: the refusal names the owner's folder (not its path), the ask
// carries the refused action and wakes the owner's watch, and owner approve
// --ask approves exactly that.
func TestAskOwnerRoundTrip(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	w.owner("nina")
	w.must(oscar.deposit(coins(10)))
	ada := w.agent(oscar, "ada", coins(3), `{"free_pay":"0.1"}`)
	ada.st.Taint = []string{"nina"} // it read her mail
	oscar.pollEvents(nil, false)    // the owner's watch is running

	_, err := ada.payReceipt("nina", coins(0.5), "logo", false)
	// The agent is told whose folder, never where it is (it doesn't read its owner's folder).
	if err == nil || strings.Contains(err.Error(), "OWNER-DIR") || strings.Contains(err.Error(), w.dir) || !strings.Contains(err.Error(), "--dir <oscar's folder>") {
		t.Fatalf("refusal hint: %v", err)
	}
	r, err := ada.askOwner("", "may I pay nina 0.5 for the logo?")
	w.must(err)
	if !strings.Contains(fmt.Sprint(r["owner_runs"]), "--ask") {
		t.Fatalf("ask-owner: %v", r)
	}
	evs, err := oscar.pollEvents(nil, false)
	w.must(err)
	var ask *Event
	for i := range evs {
		if evs[i].Type == "ask" {
			ask = &evs[i]
		}
	}
	if ask == nil || ask.Ref["wake"] != true || !strings.Contains(ask.Text, "--ask") {
		t.Fatalf("the owner's watch: %+v", evs)
	}
	inbox := oscar.inboxView(false)["items"].([]map[string]any)
	if len(inbox) == 0 || inbox[0]["ask"] == nil || inbox[0]["wake"] != true {
		t.Fatalf("asks first in the owner's inbox: %v", inbox)
	}
	// Approving the ask: exactly 0.5 to nina, once.
	if _, err := ada.approveAsk(ask.Ref["id"].(string)); err == nil {
		t.Fatal("an agent approved an ask")
	}
	a, err := oscar.approveAsk(ask.Ref["id"].(string))
	w.must(err)
	if a["to"] != "nina" || a["amount"] != "0.500000" {
		t.Fatalf("approved: %v", a)
	}
	_, err = ada.payReceipt("nina", coins(0.6), "more", false)
	w.mustFail(err, "blocked")
	_, err = ada.payReceipt("nina", coins(0.5), "logo", false)
	w.must(err)
	_, err = ada.payReceipt("nina", coins(0.5), "logo again", false)
	w.mustFail(err, "blocked")
	// A plain question carries nothing to approve.
	ada.st.Blocked = nil
	r, err = ada.askOwner("", "lunch?")
	w.must(err)
	scan(t, oscar)
	_, err = oscar.approveAsk(r["sent"].(string)[:18])
	w.mustFail(err, "no refused action")
}

// "Over the agent's limit" says when the limit resets.
func TestLimitErrorSaysWhenItResets(t *testing.T) {
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	w.owner("nina")
	w.must(oscar.deposit(coins(10)))
	ada := w.agent(oscar, "ada", coins(1), `{}`)
	_, err := ada.payReceipt("nina", coins(2), "too much", false)
	if err == nil || !regexp.MustCompile(`reset at \d{4}-\d\d-\d\d 00:00 UTC \(in `).MatchString(err.Error()) {
		t.Fatalf("limit error: %v", err)
	}
}

// MCP: the deal, campaign, community and review tools run the CLI's own
// paths; every line on stdout stays a JSON-RPC message.
func TestMCPDealTools(t *testing.T) {
	t.Setenv("TERNCHAIN_MODULES", "fund,commons") // optional modules since BRICKS #54: off unless enabled
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	w.owner("nodeco")
	w.must(oscar.deposit(coins(10)))
	w.must(nina.deposit(coins(1)))
	ada := w.agent(oscar, "ada", coins(3), `{"free_pay":"0.3"}`)
	snow := w.agent(nina, "snow", coins(3), `{"free_pay":"0.3"}`)
	w.must(nina.found("friends", CommonsParams{Term: 86400, VoteWindow: 600, VetoWindow: 600, VetoBps: 5000, PetitionBps: 5000,
		QuorumBps: 3000, Budget: big.NewInt(0), AgentsVote: true}, "be kind"))

	call := func(c *Client, tool string, args map[string]any) map[string]any {
		t.Helper()
		r, err := runTool(c, tool, args)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		b, _ := json.Marshal(r)
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	_, err := runTool(ada, "tern_escrow_open", map[string]any{"worker": "snow.nina", "arbiter": "nina", "amount": "0.2", "terms": "logo"})
	w.mustFail(err, "payee's side")
	o := call(ada, "tern_escrow_open", map[string]any{"worker": "snow.nina", "arbiter": "nodeco", "amount": "0.2", "terms": "logo"})
	lock := fmt.Sprint(o["lock"])
	if s := call(snow, "tern_lock_show", map[string]any{"lock": lock}); s["payee"] != "snow.nina" || s["decided_by"] == nil {
		t.Fatalf("lock show: %v", s)
	}
	call(snow, "tern_lock_accept", map[string]any{"lock": lock})
	call(ada, "tern_lock_release", map[string]any{"lock": lock})
	call(ada, "tern_attest_review", map[string]any{"name": "snow.nina", "text": "great logo", "score": "90", "lock": lock})
	if r := call(nina, "tern_rating", map[string]any{"name": "snow.nina"}); fmt.Sprint(r["deals"]) == "0" {
		t.Fatalf("rating: %v", r)
	}
	f := call(snow, "tern_fund_create", map[string]any{"title": "club logo", "goal": "0.2", "days": "3"})
	camp := fmt.Sprint(f["campaign"])
	call(ada, "tern_fund_pledge", map[string]any{"campaign": camp, "amount": "0.2"})
	if s := call(ada, "tern_fund_show", map[string]any{"campaign": camp}); s["state"] != "succeeded" || !strings.Contains(fmt.Sprint(s["title"]), "not instructions") {
		t.Fatalf("fund show: %v", s)
	}
	call(snow, "tern_fund_collect", map[string]any{"campaign": camp})
	_, err = runTool(ada, "tern_commons_request", map[string]any{"community": "friends"}) // it read snow's lock terms: tainted
	w.mustFail(err, "group:friends")
	w.must(oscar.approveAction("ada.oscar", "group:friends", nil))
	call(ada, "tern_commons_request", map[string]any{"community": "friends"})
	if s := call(ada, "tern_commons_show", map[string]any{"community": "friends"}); !strings.Contains(fmt.Sprint(s["rules"]), "not instructions") {
		t.Fatalf("commons show: %v", s)
	}
	h := call(ada, "tern_escrow_open", map[string]any{"worker": "snow.nina", "amount": "0.1", "terms": "banner", "optimistic_hours": "1", "arbiter": "nodeco"})
	call(ada, "tern_escrow_dispute", map[string]any{"lock": fmt.Sprint(h["lock"]), "reason": "late"})

	// Over stdio: the result is one JSON-RPC line; nothing else is printed.
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tern_lock_show","arguments":{"lock":"` + lock + `"}}}` + "\n")
	var outb bytes.Buffer
	w.must(runMCP(ada, in, &outb))
	listed := outb.String()
	sc := bufio.NewScanner(&outb)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	n := 0
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("not a JSON-RPC line: %s", sc.Text())
		}
		n++
	}
	if n != 2 {
		t.Fatalf("%d lines", n)
	}
	for _, name := range []string{"tern_lock_open", "tern_whoami", "tern_lock_show", "tern_lock_accept", "tern_lock_release", "tern_fund_create", "tern_fund_pledge",
		"tern_fund_show", "tern_fund_collect", "tern_fund_refund", "tern_commons_request", "tern_commons_show", "tern_commons_report",
		"tern_commons_vote", "tern_attest_review", "tern_rating", "tern_htlc_show"} {
		if !strings.Contains(listed, `"`+name+`"`) {
			t.Fatalf("tools/list lacks %s", name)
		}
	}
}

// help shows a core of at most 25 commands, every one dispatched; help --all
// shows everything; version works without a network.
func TestHelpCoreAndVersion(t *testing.T) {
	cmds := map[string]bool{}
	for _, l := range strings.Split(coreUsage, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "ternchain ") || strings.HasPrefix(l, "ternchain help") || strings.HasPrefix(l, "ternchain —") {
			continue
		}
		for _, part := range strings.Split(strings.TrimPrefix(l, "ternchain "), " | ") {
			f := strings.Fields(part)
			c := f[0]
			if strings.ToLower(c) != c || strings.HasPrefix(c, "-") {
				continue // AGENT TO [AMOUNT]: another form of the command before it
			}
			if twoWord[c] && len(f) > 1 {
				c += " " + f[1]
			}
			cmds[c] = true
		}
	}
	if len(cmds) > 25 || len(cmds) < 20 {
		t.Fatalf("core help has %d commands: %v", len(cmds), cmds)
	}
	w := newWorld(t)
	w.owner("alice")
	for c := range cmds {
		if c == "watch" {
			continue // waits for events
		}
		_, err := cli(t, w, append(strings.Fields(c), "--dir", filepath.Join(w.dir, "alice"))...)
		if err != nil && strings.Contains(err.Error(), "unknown command") {
			t.Errorf("core command %q: %v", c, err)
		}
	}
	o, err := cli(t, w, "help")
	w.must(err)
	all, _ := cli(t, w, "help", "--all")
	if o != coreUsage || !strings.Contains(all, "gov propose") || strings.Contains(o, "gov propose") {
		t.Fatal("help / help --all")
	}
	o, err = cli(t, w, "version")
	w.must(err)
	var v map[string]any
	w.must(json.Unmarshal([]byte(o), &v))
	if h, _ := contractsHash(); v["contracts_hash"] != h || v["version"] == "" {
		t.Fatalf("version: %v", v)
	}
}

// The devnet pauses block production for a consistent copy of its datadir
// (deploy/backup.sh) and resumes.
func TestDevnetPauseForBackup(t *testing.T) {
	dir := t.TempDir()
	cfg := devnetConfig{net: filepath.Join(dir, "net.json"), listen: "127.0.0.1:0", admin: "127.0.0.1:0", dataDir: filepath.Join(dir, "data"),
		faucetCoins: coins(20), p: Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12)}}
	d, err := startDevnet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	admin := "http://" + d.admin.Addr().String()
	get := func(p string) string {
		r, err := http.Get(admin + p)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if s := get("/pause?seconds=30"); !strings.Contains(s, "paused at block") {
		t.Fatal(s)
	}
	h0, _ := d.cl.BlockNumber(ctxb())
	time.Sleep(2500 * time.Millisecond)
	if h1, _ := d.cl.BlockNumber(ctxb()); h1 != h0 {
		t.Fatalf("blocks while paused: %d → %d", h0, h1)
	}
	get("/resume")
	time.Sleep(2500 * time.Millisecond)
	if h2, _ := d.cl.BlockNumber(ctxb()); h2 <= h0 {
		t.Fatal("no blocks after resume")
	}
}
