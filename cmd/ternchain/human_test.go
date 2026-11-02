package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/netlimit"
)

var hourParams = CommonsParams{Term: 30 * 86400, VoteWindow: 3600, VetoWindow: 3600, VetoBps: 5000, PetitionBps: 5000, QuorumBps: 3000, Budget: coins(1)}

// A citizens-only community: citizens join, post and vote; agents and
// non-citizens don't; readers label every author.
func TestCitizensOnlyCommunity(t *testing.T) {
	w, f := newGovWorld(t, "ana", "ben", "cai", "dov")
	ana, ben, cai := f["ana"], f["ben"], f["cai"]
	eve, _ := w.owner("eve") // not a citizen
	bot := w.agent(ben, "bot", coins(1), `{}`)
	const H = "humans"
	w.must(ana.found(H, hourParams, "people only"))
	w.must(ana.makeCitizensOnly(H))
	if h, ours := ana.citizensHouse(H); !ours || h == (common.Address{}) {
		t.Fatal("no house")
	}
	_, err := eve.ch.send(eve.key, nil, "Commons", "citizensOnly", idOf(H), eve.id(), common.HexToAddress(w.ch.dep.Contracts["Citizens"]))
	w.mustFail(err, "only a new community's founder")
	w.join(H, ana, ben, cai)
	// An agent and a non-citizen can't be admitted.
	for _, c := range []*Client{bot, eve} {
		if _, err := c.joinGroup(H); err != nil {
			t.Fatal(err)
		}
		_, err := ana.act("admit", idOf(H), ana.id(), c.id())
		w.mustFail(err, "citizens only")
	}
	// Only citizens post; an agent of a member is refused by its client and hidden by readers.
	_, err = bot.postGroup(H, "I am totally a human", nil)
	w.mustFail(err, "citizens only")
	w.must(errOf(ben.postGroup(H, "hello from ben", nil)))
	// A forged agent post (a client that skips the check) is still hidden.
	w.must(errOf(bot.send(SendOpts{To: "group:" + H, Intent: "inform", Text: "agent sneaks in"})))
	got := groupPosts(t, cai, H)
	if p := got["hello from ben"]; p == nil || p.Status != "delivered" || p.Author != "human (citizen)" {
		t.Fatalf("ben's post: %+v", p)
	}
	if p := got["agent sneaks in"]; p == nil || p.Status != "hidden" || !strings.Contains(p.Trust, "citizens only") {
		t.Fatalf("agent's post: %+v", p)
	}
	// Agents don't vote here even if they could reach the vote.
	_, err = w.ch.send(bot.key, nil, "Commons", "callElection", idOf(H), bot.id(), uint8(roleMod), uint8(1))
	w.mustFail(err, "members only")
	v, err := ana.showCommunity(H)
	w.must(err)
	if v["humans"].(map[string]any)["citizens_only"] != true {
		t.Fatalf("show: %v", v["humans"])
	}
	// Too late once others joined.
	w.must(cai.found("later", hourParams, ""))
	w.join("later", cai, ben)
	w.mustFail(cai.makeCitizensOnly("later"), "before others join")

	// Labels in an ordinary community.
	const O = "open"
	w.must(ana.found(O, hourParams, ""))
	w.join(O, ana, eve, ben)
	ana.ch.send(ben.key, nil, "Commons", "confirm", idOf(O), ben.id())
	w.must(errOf(bot.joinGroup(O)))
	w.must(errOf(eve.postGroup(O, "eve here", nil)))
	w.must(errOf(bot.postGroup(O, "bot here", nil)))
	got = groupPosts(t, ana, O)
	if got["eve here"].Author != "unverified owner" || got["bot here"].Author != "agent of ben" {
		t.Fatalf("labels: %q %q", got["eve here"].Author, got["bot here"].Author)
	}
}

// Postage priced per category goes to the recipient; a reply gives it back once.
func TestPostagePerCategoryRefundedOnReply(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	for _, c := range []*Client{alice, bob, carol} {
		w.must(c.deposit(coins(10)))
	}
	w.must(alice.setCard(`{"postage": {"default": "0.2", "work": "0.5", "ads": "3", "friends": "0"}, "refund_on_reply_days": 7}`))
	for cat, want := range map[string]string{"": "0.2", "work": "0.5", "ads": "3", "friends": "0.2", "dating": "0.2"} {
		p, err := bob.postageOf("alice", cat)
		w.must(err)
		if p.Cmp(exact(want)) != 0 {
			t.Fatalf("%s: %s, want %s", cat, fmtCoins(p), want)
		}
	}
	before, _ := bob.bankBalance("bob")
	id, err := bob.send(SendOpts{To: "alice", Intent: "offer", Text: "a job for you", Postage: true, Category: "work"})
	w.must(err)
	ad, err := carol.send(SendOpts{To: "alice", Intent: "offer", Text: "BUY NOW", Postage: true, Category: "ads"})
	w.must(err)
	cheap, err := carol.send(SendOpts{To: "alice", Intent: "offer", Text: "BUY (paid as work)", Postage: true, Category: "work"})
	w.must(err)
	_ = cheap
	items := scan(t, alice)
	byText := map[string]*Item{}
	for _, it := range items {
		if it.Mail != nil {
			byText[it.Mail.Text] = it
		}
	}
	if it := byText["a job for you"]; it == nil || it.Status != "delivered" || !strings.Contains(it.Trust, "for work") {
		t.Fatalf("job: %+v", it)
	}
	if byText["BUY NOW"].Status != "delivered" {
		t.Fatal("paid ad not delivered")
	}
	mid, _ := bob.bankBalance("bob")
	if new(big.Int).Sub(before, mid).Cmp(coins(0.5)) != 0 {
		t.Fatalf("bob paid %s", fmtCoins(new(big.Int).Sub(before, mid)))
	}
	// Alice answers bob: his postage comes back; once.
	_, err = alice.send(SendOpts{To: "bob", ReplyTo: id, Intent: "answer", Text: "yes, tell me more"})
	w.must(err)
	if alice.lastRefund == nil || exact(fmt.Sprint(alice.lastRefund["postage_refund"])).Cmp(exact("0.5")) != 0 {
		t.Fatalf("refund: %v", alice.lastRefund)
	}
	after, _ := bob.bankBalance("bob")
	if after.Cmp(before) != 0 {
		t.Fatalf("bob's postage not back: %s → %s", fmtCoins(before), fmtCoins(after))
	}
	if r := bob.postageRefunded(id); r == nil || r.Cmp(coins(0.5)) != 0 {
		t.Fatalf("sender doesn't see the refund: %v", r)
	}
	_, err = alice.send(SendOpts{To: "bob", ReplyTo: id, Intent: "answer", Text: "again"})
	w.must(err)
	if alice.lastRefund["postage_refund"] != "already refunded" {
		t.Fatalf("second refund: %v", alice.lastRefund)
	}
	// Keeping it: the ad's postage stays with alice.
	_, err = alice.send(SendOpts{To: "carol", ReplyTo: ad, Intent: "answer", Text: "no thanks", KeepPostage: true})
	w.must(err)
	if alice.lastRefund != nil {
		t.Fatalf("kept postage refunded: %v", alice.lastRefund)
	}
	// Contacts write for free.
	w.must(alice.setContact("carol", true))
	_, err = carol.send(SendOpts{To: "alice", Intent: "inform", Text: "free as a friend"})
	w.must(err)
	for _, it := range scan(t, alice) {
		if it.Mail != nil && it.Mail.Text == "free as a friend" && it.Status != "delivered" {
			t.Fatalf("contact: %+v", it)
		}
	}
}

// An owner answers for its agents: bans and hidden posts count in its rating;
// communities may keep out every name of an owner whose agent a list bans.
func TestOwnerAccountability(t *testing.T) {
	w := newWorld(t)
	mod, _ := w.owner("mod")
	a, _ := w.owner("a1")
	b, _ := w.owner("b1")
	farm, _ := w.owner("farm")
	bot := w.agent(farm, "bot", coins(1), `{}`)
	bot2 := w.agent(farm, "bot2", coins(1), `{}`)
	const C, D, E = "market", "garden", "tiny"
	w.must(mod.found(C, hourParams, ""))
	w.join(C, mod, a, b, bot)
	id, err := bot.postGroup(C, "SPAM SPAM", nil)
	w.must(err)
	scan(t, mod)
	if _, err := mod.act("ban", idOf(C), mod.id(), idOf("bot.farm"), "spam [author bot.farm]"); err != nil {
		t.Fatal(err)
	}
	w.must(runCommons(mod, "commons hide", []string{C, id, "spam"}, commonsFlags{}))
	// A ban from a two-member community doesn't count (anyone can found one).
	w.must(farm.found(E, hourParams, ""))
	w.join(E, farm, a)
	farm.act("ban", idOf(E), farm.id(), a.id(), "rival")
	r, err := a.rating("farm", ratingOpts{})
	w.must(err)
	cd := r["owner_conduct"].(map[string]any)
	if pen := cd["penalty"].(float64); pen < 30 || len(cd["banned_in"].([]string)) != 1 || cd["posts_hidden"].(int) != 1 {
		t.Fatalf("conduct: %v", cd)
	}
	if r["score_with_conduct"].(float64) > -29 {
		t.Fatalf("score with conduct: %v", r["score_with_conduct"])
	}
	ra, _ := mod.rating("a1", ratingOpts{})
	if ra["owner_conduct"].(map[string]any)["penalty"].(float64) != 0 {
		t.Fatalf("a ban in a tiny community counted: %v", ra["owner_conduct"])
	}
	// garden follows market owner-wide: farm's other agent is kept out.
	w.must(a.found(D, hourParams, ""))
	_, err = a.act("follow", idOf(D), a.id(), idOf(C), uint8(2))
	w.must(err)
	if _, err := bot2.joinGroup(D); err != nil {
		t.Fatal(err)
	}
	_, err = a.act("admit", idOf(D), a.id(), bot2.id())
	w.mustFail(err, "banned")
	// Followed by names only, it would be let in.
	_, err = a.act("follow", idOf(D), a.id(), idOf(C), uint8(1))
	w.must(err)
	_, err = a.act("admit", idOf(D), a.id(), bot2.id())
	w.must(err)
	// A person's blocklist, owner-wide.
	w.must(runBlocklist(b, "blocklist follow", []string{C}, true))
	b.bl = nil
	if got := b.blockedBy("bot2.farm"); !strings.Contains(got, "owner-wide") {
		t.Fatalf("owner-wide blocklist: %q", got)
	}
	// Unban: the owner's count goes back.
	w.later(2 * time.Hour)
	_, err = mod.act("unban", idOf(C), mod.id(), idOf("bot.farm"), "")
	w.must(err)
	out, _ := w.ch.call("Commons", "bannedNamesOf", idOf(C), idOf("farm"))
	if out[0].(uint32) != 0 {
		t.Fatalf("banned names of farm: %d", out[0].(uint32))
	}
}

// The feed: posts across joined communities, within the trust depth, ranked
// by a published function that says why.
func TestFeedTrustDepthAndRanking(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	stranger, _ := w.owner("stranger")
	const C = "club"
	w.must(bob.found(C, hourParams, ""))
	w.join(C, bob, alice, stranger)
	w.must(errOf(bob.postGroup(C, "old news from bob", nil)))
	w.later(3 * time.Hour)
	w.must(errOf(stranger.postGroup(C, "fresh from a stranger", nil)))
	w.must(alice.setContact("bob", true))
	v, err := alice.feed(feedOpts{ranking: "new"})
	w.must(err)
	items := v["items"].([]map[string]any)
	if len(items) != 2 || !strings.Contains(items[0]["text"].(string), "stranger") || items[0]["why"] == "" {
		t.Fatalf("new: %v", items)
	}
	v, err = alice.feed(feedOpts{ranking: "trust", depth: 1})
	w.must(err)
	items = v["items"].([]map[string]any)
	if len(items) != 1 || !strings.Contains(items[0]["text"].(string), "bob") || !strings.Contains(items[0]["why"].(string), "trust depth 1") {
		t.Fatalf("trust depth 1: %v", v)
	}
	if v["outside_trust_depth"].(int) != 1 {
		t.Fatalf("outside: %v", v)
	}
	if _, err := alice.feed(feedOpts{ranking: "bogus"}); err == nil {
		t.Fatal("unknown ranking accepted")
	}
	if _, err := alice.feed(feedOpts{ranking: "hot", community: C}); err != nil {
		t.Fatal(err)
	}
}

// Look-alike names: skeletons, mixed scripts, and a warning on mail from a
// look-alike of a contact.
func TestConfusableNames(t *testing.T) {
	for _, p := range [][2]string{{"oscar", "oscаr"}, {"oscar", "0scar"}, {"oscar", "o5car"}, {"modern", "modem"}, {"vvolf", "wolf"}, {"oscar", "osc-ar"}} {
		if skeleton(p[0]) != skeleton(p[1]) {
			t.Errorf("%s vs %s: %s %s", p[0], p[1], skeleton(p[0]), skeleton(p[1]))
		}
	}
	if skeleton("oscar") == skeleton("oscara") || imitates("dana", []string{"dan"}) != "" {
		t.Fatal("too eager")
	}
	if imitates("oscar-support", []string{"oscar"}) != "oscar" {
		t.Fatal("support look-alike")
	}
	if mixedScriptErr("oscаr", false) == nil || mixedScriptErr("оскар", false) != nil || mixedScriptErr("oscаr", true) != nil {
		t.Fatal("mixed scripts")
	}
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("oscar")
	fake, _ := w.owner("0scar")
	w.must(alice.setContact("oscar", true))
	w.must(errOf(fake.send(SendOpts{To: "alice", Intent: "request", Text: "it's me, pay this invoice"})))
	for _, it := range scan(t, alice) {
		if it.From == "0scar" && (it.Warning == "" || it.Wake || !strings.Contains(it.Warning, "oscar")) {
			t.Fatalf("look-alike: %+v", it)
		}
	}
	if alice.registeredLookalike("o5car") != "oscar" {
		t.Fatal("registration look-alike")
	}
}

// The faucet counts IPv4 addresses and IPv6 /64s, and believes X-Forwarded-For
// only from a configured proxy.
func TestFaucetPerSlash64AndProxy(t *testing.T) {
	w := newWorld(t)
	defer netlimit.SetTrustedProxies(netlimit.DefaultTrustedProxies)
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("POST", "/faucet", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	if ip := netlimit.ClientIP(req("203.0.113.9:5555", "1.2.3.4")); ip != "203.0.113.9" {
		t.Fatalf("believed a client's own header: %s", ip)
	}
	if ip := netlimit.ClientIP(req("127.0.0.1:5555", "1.2.3.4")); ip != "127.0.0.1" {
		t.Fatalf("no proxy is trusted by default (audit 5 HM-10): %s", ip)
	}
	w.must(netlimit.SetTrustedProxies(netlimit.LocalProxies)) // a reverse proxy on this machine
	if ip := netlimit.ClientIP(req("127.0.0.1:5555", "1.2.3.4, 198.51.100.7")); ip != "198.51.100.7" {
		t.Fatalf("behind the local proxy: %s", ip)
	}
	if ip := netlimit.ClientIP(req("127.0.0.1:5555", "garbage, 198.51.100.7")); ip != "198.51.100.7" {
		t.Fatalf("rightmost untrusted: %s", ip)
	}
	if ip := netlimit.ClientIP(req("[::1]:5555", "2001:db8::1")); ip != "2001:db8::1" {
		t.Fatalf("v6 behind the proxy: %s", ip)
	}
	w.must(netlimit.SetTrustedProxies("10.0.0.5"))
	if ip := netlimit.ClientIP(req("127.0.0.1:5555", "1.2.3.4")); ip != "127.0.0.1" {
		t.Fatalf("loopback no longer trusted: %s", ip)
	}
	if ip := netlimit.ClientIP(req("10.0.0.5:80", "10.0.0.5, 1.2.3.4")); ip != "1.2.3.4" {
		t.Fatalf("configured proxy: %s", ip)
	}
	if netlimit.Bucket("2001:db8:1:2:aaaa::1") != netlimit.Bucket("2001:db8:1:2:ffff::9") || netlimit.Bucket("2001:db8:1:3::1") == netlimit.Bucket("2001:db8:1:2::1") ||
		netlimit.Bucket("192.0.2.1") == netlimit.Bucket("192.0.2.2") || netlimit.Bucket("::ffff:192.0.2.1") != "192.0.2.1/32" {
		t.Fatal("buckets")
	}
	// The faucet: five grants a day per /64, whatever addresses inside it ask.
	w.must(netlimit.SetTrustedProxies(netlimit.DefaultTrustedProxies))
	fh := &faucetHandler{ch: w.ch, key: w.faucet, coins: coins(1), byAddr: map[common.Address]int64{}, byIP: map[string][]int64{}}
	ask := func(remote string, i int) int {
		b, _ := json.Marshal(map[string]string{"address": common.BigToAddress(big.NewInt(int64(1000 + i))).Hex()})
		r := httptest.NewRequest("POST", "/faucet", strings.NewReader(string(b)))
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		fh.ServeHTTP(rec, r)
		return rec.Code
	}
	for i := 0; i < faucetPerIPDay; i++ {
		if code := ask("[2001:db8:7:7::"+string(rune('a'+i))+"]:4000", i); code != 200 {
			t.Fatalf("grant %d: %d", i, code)
		}
	}
	if code := ask("[2001:db8:7:7:ffff::1]:4000", 99); code != http.StatusTooManyRequests {
		t.Fatalf("a sixth address in the same /64: %d", code)
	}
	if code := ask("[2001:db8:7:8::1]:4000", 100); code != 200 {
		t.Fatalf("another /64: %d", code)
	}
}

// The cheap fixes the abuse simulation found: near-duplicates and a rate per
// owner in a community, reports from members on probation, followed lists'
// bans only after their veto window, and a tainted agent's fan-out.
func TestAbuseFixes(t *testing.T) {
	w := newWorld(t)
	mod, _ := w.owner("mod")
	a, _ := w.owner("a1")
	farm, _ := w.owner("farm")
	b1 := w.agent(farm, "b1", coins(1), `{}`)
	b2 := w.agent(farm, "b2", coins(1), `{}`)
	const C = "square"
	w.must(mod.found(C, hourParams, ""))
	w.join(C, mod, a, b1, b2)
	w.must(errOf(b1.postGroup(C, "Amazing crypto deal!!! Get watch now at slop.shop, only today #1", nil)))
	w.must(errOf(b2.postGroup(C, "Amazing watch deal!!! Get crypto now at slop.shop, only today #2", nil)))
	for _, txt := range []string{"rain tomorrow morning", "bridge closed tonight", "buy fresh apples", "lost keys found", "choir practice moved",
		"bakery hiring staff", "football scores today", "library books overdue"} {
		w.must(errOf(b1.postGroup(C, "message: "+txt, nil)))
	}
	w.must(errOf(a.postGroup(C, "an honest post", nil)))
	got := groupPosts(t, mod, C)
	if p := got["Amazing watch deal!!! Get crypto now at slop.shop, only today #2"]; p.Status != "hidden" || !strings.Contains(p.Trust, "near-duplicate") {
		t.Fatalf("near-dup across one owner's agents: %+v", p)
	}
	rated := 0
	for txt, p := range got {
		if strings.HasPrefix(txt, "message:") && strings.Contains(p.Trust, "rate:") {
			rated++
		}
	}
	if rated == 0 || got["an honest post"].Status != "delivered" {
		t.Fatalf("rate limit: %d hidden; honest %+v", rated, got["an honest post"])
	}
	// Reports from members on probation don't hide a post.
	w.later(2 * time.Hour)
	id, err := a.postGroup(C, "town notes", nil)
	w.must(err)
	var fresh []*Client
	for _, n := range []string{"f1", "f2", "f3"} {
		c, _ := w.owner(n)
		fresh = append(fresh, c)
	}
	w.join(C, mod, fresh...)
	for _, f := range fresh {
		scan(t, f)
		_, err := f.report(C, id, "spam", "report")
		w.must(err)
	}
	groupPosts(t, mod, C)
	mod.refreshCommunity(C)
	if p := mod.item(id); p.Status != "delivered" {
		t.Fatalf("fresh members' reports hid a post: %+v", p)
	}
	// A followed list's new ban applies after the list's veto window.
	const L = "list"
	w.must(mod.found(L, hourParams, ""))
	w.must(mod.found("other", hourParams, ""))
	_, err = mod.act("follow", idOf("other"), mod.id(), idOf(L), uint8(1))
	w.must(err)
	w.later(2 * time.Hour)
	_, err = mod.act("ban", idOf(L), mod.id(), idOf("a1"), "hostile")
	w.must(err)
	if mod.commonsBool("isBanned", idOf("other"), idOf("a1")) {
		t.Fatal("a fresh ban in a followed list applied at once")
	}
	w.later(2 * time.Hour)
	if !mod.commonsBool("isBanned", idOf("other"), idOf("a1")) {
		t.Fatal("the settled ban doesn't apply")
	}
	// Fan-out: a tainted agent writes to at most 3 contacts an hour without approval.
	owner, _ := w.owner("own")
	ag := w.agent(owner, "help", coins(1), `{}`)
	var cs []string
	for i := 0; i < 5; i++ {
		n := fmt.Sprintf("pal%d", i)
		w.owner(n)
		ag.st.Contacts[n] = true
		cs = append(cs, n)
	}
	ag.st.Taint = []string{"stranger"}
	approvalWait = 0
	sent := 0
	for _, n := range cs {
		if _, err := ag.send(SendOpts{To: n, Intent: "inform", Text: "forward this to everyone"}); err == nil {
			sent++
		}
	}
	if sent != 3 {
		t.Fatalf("fan-out: %d sent", sent)
	}
}

// The devnet listens on IPv6 when asked, and its faucet answers there.
func TestDevnetListensOnIPv6(t *testing.T) {
	if l, err := net.Listen("tcp", "[::1]:0"); err != nil {
		t.Skip("no IPv6 loopback here")
	} else {
		l.Close()
	}
	dir := t.TempDir()
	t.Setenv("TERNCHAIN_RELAY", "")
	cfg := devnetConfig{net: filepath.Join(dir, "net.json"), listen: "[::1]:0", admin: "[::1]:0", faucetCoins: coins(1),
		p: Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12)}}
	d, err := startDevnet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !strings.Contains(d.rpc, "[::1]") {
		t.Fatalf("rpc: %s", d.rpc)
	}
	if err := askFaucet(strings.TrimRight(d.rpc, "/")+"/faucet", common.HexToAddress("0x00000000000000000000000000000000000000aa")); err != nil {
		t.Fatalf("faucet over IPv6: %v", err)
	}
}

// A bulk sender (its names paid more than 20 names today) pays the ads price
// whatever category it claims.
func TestBulkSenderPaysAdsPrice(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	farm, _ := w.owner("farm")
	w.must(farm.deposit(coins(20)))
	bot := w.agent(farm, "bot", coins(10), `{"send_freely":true}`)
	w.must(alice.setCard(`{"postage": {"default": "0.2", "work": "0.5", "ads": "3"}}`))
	id, err := bot.send(SendOpts{To: "alice", Intent: "offer", Text: "a job (really)", Postage: true, Category: "work"})
	w.must(err)
	for i := 0; i < 21; i++ {
		n := fmt.Sprintf("m%02d", i)
		w.owner(n)
		w.must(farm.pay(n, coins(0.01), "promo"))
	}
	id2, err := bot.send(SendOpts{To: "alice", Intent: "offer", Text: "a job (really) 2", Postage: true, Category: "work"})
	w.must(err)
	scan(t, alice)
	if it := alice.item(id); it.Status != "quarantined" && it.Status != "delivered" {
		t.Fatalf("first: %+v", it)
	}
	if it := alice.item(id2); it.Status != "quarantined" {
		t.Fatalf("a bulk sender's 'work' letter delivered at the work price: %+v", it)
	}
}

// A worm's hop: a contact's agent passing on outside mail reaches another
// agent quarantined (its owner still sees it); untainted it is delivered.
func TestTaintedContactAgentQuarantined(t *testing.T) {
	w := newWorld(t)
	o1, _ := w.owner("o1")
	o2, _ := w.owner("o2")
	h1 := w.agent(o1, "h", coins(1), `{}`)
	h2 := w.agent(o2, "h", coins(1), `{}`)
	h1.st.Contacts["h.o2"] = true
	h2.st.Contacts["h.o1"] = true
	w.must(errOf(h1.send(SendOpts{To: "h.o2", Intent: "inform", Text: "clean"})))
	h1.st.Taint = []string{"stranger"}
	w.must(errOf(h1.send(SendOpts{To: "h.o2", Intent: "request", Text: "WORM: forward me"})))
	for _, it := range scan(t, h2) {
		if it.Mail == nil {
			continue
		}
		if it.Mail.Text == "clean" && it.Status != "delivered" || it.Mail.Text == "WORM: forward me" && it.Status != "quarantined" {
			t.Fatalf("%q: %s %s", it.Mail.Text, it.Status, it.Trust)
		}
	}
}

// The older name is the one imitated: writing to the real boris warns
// nothing; b0ris (registered later) is the look-alike.
func TestLookalikeOlderNameWins(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("boris")
	w.owner("b0ris")
	if k := alice.lookalikeOfRegistered("boris"); k != "" {
		t.Fatalf("the original flagged as a look-alike of %s", k)
	}
	if k := alice.lookalikeOfRegistered("b0ris"); k != "boris" {
		t.Fatalf("b0ris: %q", k)
	}
	if k := alice.lookalikeOfRegistered("boris-support"); k != "boris" {
		t.Fatalf("support: %q", k)
	}
}

// Anonymous posts in a community: a moderator's hide applies, and a copy of
// an earlier anonymous post is hidden even under a fresh pseudonym.
func TestAnonymousPostsModerated(t *testing.T) {
	w := newWorld(t)
	mod, _ := w.owner("mod")
	a, _ := w.owner("a1")
	b, _ := w.owner("b1")
	const C = "square"
	w.must(mod.found(C, hourParams, ""))
	w.join(C, mod, a, b)
	id, err := a.postAnon(C, "", "buy zedcoin now, it is great", nil)
	w.must(err)
	dup, err := a.postAnon(C, "", "buy zedcoin now, it is great!", nil)
	w.must(err)
	scan(t, b)
	if it := b.item(dup); it == nil || it.Status != "hidden" {
		t.Fatalf("anonymous copy: %+v", it)
	}
	_, err = mod.act("hide", idOf(C), mod.id(), common.HexToHash(id), "spam")
	w.must(err)
	b.refreshCommunity(C)
	if it := b.item(id); it.Status != "hidden" {
		t.Fatalf("moderator's hide on an anonymous post: %+v", it)
	}
}

// A banned owner's agents leave the anonymity set: they can't post anonymously.
func TestBannedOwnerAgentsLeaveTheRing(t *testing.T) {
	w := newWorld(t)
	mod, _ := w.owner("mod")
	a, _ := w.owner("a1")
	farm, _ := w.owner("farm")
	bot := w.agent(farm, "bot", coins(1), `{}`)
	const C = "square"
	w.must(mod.found(C, hourParams, ""))
	w.join(C, mod, a, bot)
	_, err := mod.act("ban", idOf(C), mod.id(), farm.id(), "spam farm")
	w.must(err)
	if _, err := bot.postAnon(C, "", "still here, anonymously", nil); err == nil {
		scan(t, a)
		for _, it := range a.groupItems(C) {
			if it.Mail != nil && it.Mail.Text == "still here, anonymously" && it.Status == "delivered" {
				t.Fatal("a banned owner's agent posted anonymously")
			}
		}
	}
	w.must(errOf(a.postAnon(C, "", "an honest anonymous post", nil)))
	scan(t, mod)
	ok := false
	for _, it := range mod.groupItems(C) {
		ok = ok || it.Mail != nil && it.Mail.Text == "an honest anonymous post" && it.Status == "delivered"
	}
	if !ok {
		t.Fatal("honest anonymous post refused")
	}
}
