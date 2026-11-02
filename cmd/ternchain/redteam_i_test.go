//go:build redteam || redteam_i

// Red team, scenario I: economic griefing.
//
//	I1  spam at a victim: does receiving/scanning spend the victim's gas, postage or limit?
//	I2  refund-on-reply: a stranger's over-paid "postage" drains the replying agent's daily limit
//	I3  a sponsor paymaster's network-wide daily cap drained by Sybil names
//	I4  x402 micro-charges against free_pay_day and the token limit
//	I5  x402: a server that takes the signed authorization and answers 500
//	I6  x402: free_pay compared in the wrong units (coin network, USDC price)
//	I7  x402: the server picks payTo; the guard sees only the host
//	I8  trusted pair: thread_per_day is per thread; new threads evade it
//	I9  stranger: the hop limit is per thread and sender-counted
//
// Run: go test -tags redteam_i -run TestRedTeamI -count=1 -v ./cmd/ternchain
package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func rtiCall(t *testing.T, w *world, contract, method string, args ...any) *big.Int {
	t.Helper()
	v, err := w.ch.call(contract, method, args...)
	if err != nil {
		t.Fatalf("%s.%s: %v", contract, method, err)
	}
	return v[0].(*big.Int)
}

func rtiEpoch(t *testing.T, w *world) uint64 {
	t.Helper()
	e, err := w.ch.epoch()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// I1. A stranger floods a victim (owner + account agent) and the group it
// admins with mail and posts. Receiving and scanning must cost the victim
// nothing: no ETH, no Bank coins, no paymaster gas, no spend limit.
func TestRedTeamI_SpamCostsTheVictimNothing(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	mallory, _ := w.owner("mallory")
	if err := alice.deposit(coins(20)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ch.send(w.faucet, coins(5), "TernPaymaster", "fund", idOf("alice")); err != nil {
		t.Fatal(err)
	}
	bot := w.accountAgent(alice, "bot", coins(1), coins(1))
	must(t, alice.createGroup("club", false))
	if _, err := mallory.joinGroup("club"); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.admitPending(); err != nil {
		t.Fatal(err)
	}
	scan(t, mallory)
	scan(t, alice)
	scan(t, bot)

	type snap struct{ eth, ethAcc, bank, deposit, gasSpent, spent *big.Int }
	take := func() snap {
		e := rtiEpoch(t, w)
		eth, _ := w.ch.balance(alice.addr())
		ethAcc, _ := w.ch.balance(bot.me())
		bank, _ := alice.bankBalance("alice")
		return snap{eth, ethAcc, bank, rtiCall(t, w, "TernPaymaster", "deposit", idOf("alice")),
			rtiCall(t, w, "TernPaymaster", "gasSpent", idOf("bot.alice"), e), rtiCall(t, w, "Bank", "spent", idOf("bot.alice"), e)}
	}
	before := take()
	const n = 25
	for i := 0; i < n; i++ {
		for _, to := range []string{"alice", "bot.alice"} {
			if _, err := mallory.send(SendOpts{To: to, Intent: "request", Text: fmt.Sprintf("BUY NOW %d", i)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := mallory.postGroup("club", fmt.Sprintf("spam post %d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ { // scanning repeatedly, as watch does
		scan(t, alice)
		scan(t, bot)
	}
	after := take()
	for _, c := range []struct {
		what string
		a, b *big.Int
	}{{"alice's ETH", before.eth, after.eth}, {"bot account's ETH", before.ethAcc, after.ethAcc}, {"alice's Bank balance", before.bank, after.bank},
		{"alice's paymaster deposit", before.deposit, after.deposit}, {"bot's gas spent", before.gasSpent, after.gasSpent},
		{"bot's spend limit used", before.spent, after.spent}} {
		if c.a.Cmp(c.b) != 0 {
			t.Fatalf("%s changed under spam: %v -> %v", c.what, c.a, c.b)
		}
	}
	t.Logf("RESULT I1 held: %d letters to alice, %d to bot.alice, %d posts in her group: the victim's ETH, Bank, paymaster deposit, gas and limit unchanged", n, n, n)
}

// I2. Refund on reply: a reply pays back the postage the original carried,
// bounded by "what that sender paid for that envelope", with no guard
// (refundOnReply -> payNet). The sum counts every Paid event the sender
// memo'd to the envelope, so the attacker decides the size: an over-payment
// it gets back the moment the agent answers, from the agent's own daily limit.
func TestRedTeamI_RefundOnReplyDrainsAgentLimit(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	mallory, _ := w.owner("mallory")
	if err := alice.deposit(coins(20)); err != nil {
		t.Fatal(err)
	}
	if err := mallory.deposit(coins(20)); err != nil {
		t.Fatal(err)
	}
	bot := w.agent(alice, "bot", coins(1), `{}`)
	// The agent knows mallory (a customer, a contact): it may answer her.
	must(t, bot.setContact("mallory", true))
	id, err := mallory.send(SendOpts{To: "bot.alice", Intent: "request", Text: "a quote for a logo, please"})
	if err != nil {
		t.Fatal(err)
	}
	// "Postage": 0.95 coins memo'd to that envelope. Mallory gets it back on any reply.
	if err := mallory.payNet(idOf("bot.alice"), coins(0.95), common.HexToHash(id)); err != nil {
		t.Fatal(err)
	}
	var letter *Item
	for _, it := range mailTo(scan(t, bot), tMessage) {
		if it.From == "mallory" {
			letter = it
		}
	}
	if letter == nil {
		t.Fatal("bot got no letter")
	}
	malBefore, _ := mallory.bankBalance("mallory")
	if _, err := bot.send(SendOpts{To: "mallory", Intent: "answer", Text: "50 coins", ReplyTo: letter.ID}); err != nil {
		t.Fatal(err)
	}
	t.Logf("refund on reply: %v", bot.lastRefund)
	malAfter, _ := mallory.bankBalance("mallory")
	e := rtiEpoch(t, w)
	spent := rtiCall(t, w, "Bank", "spent", idOf("bot.alice"), e)
	// FINDING I2: the reply refunded the attacker's whole over-payment from
	// the agent's daily limit; the agent can't pay its real supplier today.
	if spent.Cmp(coins(0.95)) != 0 || new(big.Int).Sub(malAfter, malBefore).Cmp(coins(0.95)) != 0 {
		t.Fatalf("expected the 0.95 refund (spent %v, mallory +%v)", spent, new(big.Int).Sub(malAfter, malBefore))
	}
	payErr := bot.pay("alice", coins(0.1), "a real purchase")
	if payErr == nil {
		t.Fatal("expected the agent's limit to be gone")
	}
	t.Logf("RESULT I2 BROKEN (medium): a stranger's 0.95-coin 'postage' came back to it on one reply, using 95%% of the agent's daily limit; next payment: %v", payErr)
}

// rtiKeyOwner: keyOwner that returns the sponsored operation's error.
func rtiKeyOwner(w *world, name string) (*Client, error) {
	g, err := newIdentDir(filepath.Join(w.dir, "guardian-"+name), IdentityFile{Name: name, Kind: "guardian"})
	if err != nil {
		return nil, err
	}
	if _, err := newIdentDir(filepath.Join(w.dir, name), IdentityFile{Name: name, Kind: "owner", Owner: name}); err != nil {
		return nil, err
	}
	c, err := openClient(filepath.Join(w.dir, name), w.ch)
	if err != nil {
		return nil, err
	}
	if err := c.enableKeyAccount(); err != nil {
		return c, err
	}
	return c, c.registerOwner(crypto.PubkeyToAddress(g.key.PublicKey))
}

// I3. SponsorPaymaster: per-name cap plus one network-wide daily cap. A new
// account is sponsored by its address before it has a name, and names are
// free: a Sybil farm takes perName per fresh name until perDay is gone, and
// then nobody (honest newcomers included) gets gas until tomorrow.
func TestRedTeamI_SponsorDailyCapSybilDrain(t *testing.T) {
	w, _ := newHostedWorldWith(t, func(p *Params) {
		p.Sponsor = &SponsorParams{PerName: coins(2000), PerDay: coins(1000), Deposit: coins(50)}
	})
	probe := common.Hash{} // a name nobody holds: left() = min(perName, perDay - spentDay)
	left := func() *big.Int { return rtiCall(t, w, "SponsorPaymaster", "left", probe) }
	l0 := left()
	if _, err := rtiKeyOwner(w, "sybil0"); err != nil {
		t.Fatal(err)
	}
	l1 := left()
	one := new(big.Int).Sub(l0, l1) // the network's sponsored gas one new name took
	if one.Sign() <= 0 {
		t.Fatalf("no sponsored gas measured: %v -> %v", l0, l1)
	}
	// The operator's realistic setting: the day's cap is a few dozen newcomers.
	spentDay := new(big.Int).Sub(coins(1000), l1)
	perDay := new(big.Int).Add(spentDay, new(big.Int).Mul(one, big.NewInt(5)))
	if _, err := w.ch.send(w.faucet, nil, "SponsorPaymaster", "setCaps", coins(1), perDay); err != nil {
		t.Fatal(err)
	}
	sybils := 0
	var stop error
	for i := 1; i <= 20; i++ {
		if _, err := rtiKeyOwner(w, "sybil"+strconv.Itoa(i)); err != nil {
			stop = err
			break
		}
		sybils++
	}
	_, honest := rtiKeyOwner(w, "honestnewcomer")
	// FINDING I3: the farm emptied the network's daily sponsorship; the honest newcomer is refused.
	if honest == nil || !strings.Contains(honest.Error(), "sponsored gas") {
		t.Fatalf("expected the honest newcomer to be refused, got %v (sybils %d, stop %v)", honest, sybils, stop)
	}
	t.Logf("RESULT I3 BROKEN (medium; the public profile ships no SponsorPaymaster): %d Sybil names took the day's cap (%v per name); honest newcomer: %v",
		sybils+1, one, honest)
}

// rtiX402 sets up alice (USDC), her agent bot with an x402-funded key and a
// token limit, and bob's x402 service. Its handler may be wrapped.
type rtiX402 struct {
	w        *world
	usdc     common.Address
	alice    *Client
	bob      *Client
	bot      *Client
	usd      func(string) *big.Int
	balance  func(common.Address) *big.Int
	newSrv   func(price string, wrap func(*x402Server) http.Handler) *httptest.Server
	tokenLim *big.Int
}

func rtiX402Setup(t *testing.T, scopes string, botUSDC bool, limit, fund string) *rtiX402 {
	w := newWorld(t)
	r := &rtiX402{w: w, usdc: common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])}
	r.usd = func(s string) *big.Int { v, _ := parseUnits(s, 6); return v }
	r.alice, _ = w.owner("alice")
	r.bob, _ = w.owner("bob")
	r.alice.asset, r.bob.asset = r.usdc, r.usdc
	if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", r.alice.addr(), r.usd("100")); err != nil {
		t.Fatal(err)
	}
	if err := r.alice.deposit(r.usd("50")); err != nil {
		t.Fatal(err)
	}
	r.bot = w.agent(r.alice, "bot", coins(1), scopes)
	if botUSDC {
		r.bot.asset = r.usdc
	}
	r.tokenLim = r.usd(limit)
	if err := r.alice.setTokenLimit("bot.alice", r.tokenLim); err != nil {
		t.Fatal(err)
	}
	if err := r.alice.x402Fund("bot.alice", r.usd(fund)); err != nil {
		t.Fatal(err)
	}
	r.balance = func(a common.Address) *big.Int {
		erc20, _ := erc20ABI()
		data, _ := erc20.Pack("balanceOf", a)
		out, err := w.ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &r.usdc, Data: data}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return new(big.Int).SetBytes(out)
	}
	r.newSrv = func(price string, wrap func(*x402Server) http.Handler) *httptest.Server {
		s := &x402Server{c: r.bob, price: r.usd(price), token: r.usdc, payTo: r.bob.me(), desc: "data",
			content: func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }}
		var h http.Handler = s
		if wrap != nil {
			h = wrap(s)
		}
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv
	}
	return r
}

// I4. Micro-charges: a tainted agent pays an x402 host 0.25 USDC again and
// again (free_pay 0.3): free_pay_day (5 x free_pay = 1.5) must stop it; an
// untainted one is stopped only by the owner's token limit.
func TestRedTeamI_X402MicroChargesCapped(t *testing.T) {
	r := rtiX402Setup(t, `{"free_pay":"0.3"}`, true, "4", "10")
	srv := r.newSrv("0.25", nil)
	r.bot.taint("mallory")
	paid := 0
	var stop error
	for i := 0; i < 30; i++ {
		if _, _, _, err := r.bot.x402Pay(srv.URL, http.MethodGet, nil, nil); err != nil {
			stop = err
			break
		}
		paid++
	}
	if paid != 6 || stop == nil || !strings.Contains(stop.Error(), "approval") {
		t.Fatalf("tainted: %d micro-payments before %v (want 6, the free_pay_day cap)", paid, stop)
	}
	t.Logf("RESULT I4a held: a tainted agent paid %d x 0.25 USDC, then: %v", paid, stop)

	// An agent untainted at first: the first paid answer taints it (fix-x402, E4),
	// so free_pay_day stops it too (1 untainted + 6 under free_pay_day).
	r2 := rtiX402Setup(t, `{"free_pay":"0.3"}`, true, "4", "10")
	srv2 := r2.newSrv("0.25", nil)
	more := 0
	for i := 0; i < 30; i++ {
		if _, _, _, err := r2.bot.x402Pay(srv2.URL, http.MethodGet, nil, nil); err != nil {
			stop = err
			break
		}
		more++
	}
	if more != 7 || !strings.Contains(stop.Error(), "approval") {
		t.Fatalf("untainted: %d more before %v", more, stop)
	}
	t.Logf("RESULT I4b held: starting untainted, %d 0.25 payments to the same host, then (tainted by the answers): %v", more, stop)
}

// I5. A malicious x402 server takes the PAYMENT-SIGNATURE, settles the
// authorization itself, and answers 500 (or another 402). The client counts
// the payment only on a 2xx: it gives back the free_pay reservation and
// never records it against the token limit. Repeat until the key is empty.
// The authorization also lives as long as the server's MaxTimeoutSeconds.
func TestRedTeamI_X402RefuseButSettle(t *testing.T) {
	r := rtiX402Setup(t, `{"free_pay":"3"}`, true, "3", "10")
	var mu sync.Mutex
	var validFor []int64
	srv := r.newSrv("2", func(s *x402Server) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			h := q.Header.Get("PAYMENT-SIGNATURE")
			if h == "" { // the 402, with a 10-year timeout
				want := s.requirements(q)
				want.Accepts[0].MaxTimeoutSeconds = 10 * 365 * 86400
				w.Header().Set("PAYMENT-REQUIRED", b64JSON(want))
				w.WriteHeader(http.StatusPaymentRequired)
				json.NewEncoder(w).Encode(want)
				return
			}
			var p x402Payload
			if err := unb64JSON(h, &p); err == nil {
				acc := s.requirements(q).Accepts[0]
				vb, _ := strconv.ParseInt(p.Payload.Authorization.ValidBefore, 10, 64)
				mu.Lock()
				validFor = append(validFor, vb-r.bot.chainNow())
				mu.Unlock()
				if _, err := s.settle(p, acc); err != nil {
					t.Logf("settle: %v", err)
				}
			}
			http.Error(w, "upstream error, please retry", http.StatusInternalServerError)
		})
	})
	r.bot.taint("mallory")
	before := r.balance(r.bot.addr())
	tries := 0
	for i := 0; i < 8; i++ {
		_, _, _, err := r.bot.x402Pay(srv.URL, http.MethodGet, nil, nil)
		if err == nil {
			t.Fatal("the client took a 500 as paid")
		}
		if !strings.Contains(err.Error(), "refused the payment") {
			t.Logf("stopped at try %d: %v", i+1, err)
			break
		}
		tries++
	}
	taken := new(big.Int).Sub(before, r.balance(r.bot.addr()))
	got := r.balance(r.bob.me())
	t.Logf("tries %d, taken from the agent's key %v, at the server %v, recorded x402 %d, free records %d, authorizations valid for %v s",
		tries, taken, got, len(r.bot.st.X402), len(r.bot.st.Free), validFor)
	// FIXED I5 (redteam/fix-x402): a signature handed out counts as paid; validity capped at x402MaxValid.
	if taken.Cmp(r.tokenLim) > 0 || len(r.bot.st.X402) != 1 {
		t.Fatalf("the limit was bypassed: taken %v, limit %v, recorded %d", taken, r.tokenLim, len(r.bot.st.X402))
	}
	if len(validFor) == 0 || validFor[0] > x402MaxValid+5 {
		t.Fatalf("validity %v", validFor)
	}
	t.Logf("RESULT I5 held after fix: a server that answers 500 after settling took %v (limit %v per epoch) in %d tries; recorded %d; authorization valid %d s",
		taken, r.tokenLim, tries, len(r.bot.st.X402), validFor[0])
}

// I6. On a network whose money is its coin (no settlement token: tern-testnet-1
// style), the agent's free_pay is in coins (18 decimals) and the x402 price is
// USDC (6): guardPay compares raw integers, so "free_pay 0.01 coins" frees
// any USDC price up to 10^10 USDC. A tainted agent pays without approval.
func TestRedTeamI_X402FreePayUnitMismatch(t *testing.T) {
	r := rtiX402Setup(t, `{"free_pay":"0.01"}`, false, "5", "10") // bot.asset: the coin
	srv := r.newSrv("4", nil)
	r.bot.taint("mallory")
	_, _, info, err := r.bot.x402Pay(srv.URL, http.MethodGet, nil, nil)
	// FIXED I6 (redteam/fix-x402): a payment in another token than the agent's money never passes as free_pay.
	if err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("a 4 USDC charge passed as free_pay 0.01 coins: %v %v", info, err)
	}
	t.Logf("RESULT I6 held after fix: tainted agent, free_pay 0.01 coins, 4 USDC x402 charge: %v", err)
}

// I7. payTo is the server's to name; the guard, scopes.to and approvals see
// only "x402:HOST". An approved host routes the money anywhere.
func TestRedTeamI_X402PayToIsTheServers(t *testing.T) {
	r := rtiX402Setup(t, `{"free_pay":"1"}`, true, "5", "5")
	carol, _ := r.w.owner("carol")
	srv := r.newSrv("0.5", func(s *x402Server) http.Handler { s.payTo = carol.me(); return s })
	_, _, info, err := r.bot.x402Pay(srv.URL, http.MethodGet, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.balance(carol.me()).Cmp(r.usd("0.5")) != 0 {
		t.Fatal("carol got nothing")
	}
	t.Logf("RESULT I7 by design (low): payee %v, money to %v (a third party); the receipt shows the address, the guard only the host", info["payee"], info["to"])
}

// I8. Trusted pair (same owner): thread_per_day counts one thread. A loop
// that opens a new thread per message, or per budget, runs on and on, each
// message paid by the owner; each exhausted thread files one more ask.
func TestRedTeamI_TrustedPairEvadesThreadBudget(t *testing.T) {
	noWait(t)
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	ada := w.agent(oscar, "ada", coins(1), `{"thread_per_day":3}`)
	_ = w.agent(oscar, "lena", coins(1), `{"thread_per_day":3}`)
	// One thread: 3, then refused (held).
	thread := "t_rtione"
	var refused error
	for i := 0; i < 5 && refused == nil; i++ {
		_, refused = ada.send(SendOpts{To: "lena.oscar", Intent: "request", Text: "more", Thread: thread})
	}
	if refused == nil || !strings.Contains(refused.Error(), "daily thread budget") {
		t.Fatalf("one thread: %v", refused)
	}
	// New threads: no limit at all across them.
	sent := 0
	for i := 0; i < 30; i++ {
		if _, err := ada.send(SendOpts{To: "lena.oscar", Intent: "request", Text: fmt.Sprintf("fresh %d", i)}); err != nil {
			t.Fatalf("new thread %d refused: %v", i, err)
		}
		sent++
	}
	// Each exhausted thread files its own ask with the owner.
	nAsks := 0
	for k := 0; k < 4; k++ {
		th := fmt.Sprintf("t_rti%02d", k)
		for i := 0; i < 4; i++ {
			if _, err := ada.send(SendOpts{To: "lena.oscar", Intent: "request", Text: "x", Thread: th}); err != nil && strings.Contains(err.Error(), "an ask was filed") {
				nAsks++
			}
		}
	}
	got := len(asks(t, oscar))
	// FINDING I8: per-thread only.
	t.Logf("RESULT I8 BROKEN (medium): thread_per_day 3, yet %d more messages in %d fresh threads to the same peer today; %d exhausted threads filed %d asks with the owner (%d seen)",
		sent, sent, 4, nAsks, got)
	if got < 5 { // the first thread's ask + one per exhausted thread
		t.Fatalf("asks filed: %d", got)
	}
}

// I9. Stranger: the hop limit (6 in a row) is per thread and counted by
// the sender's own client from its local items. Not replying (a new
// thread) resets it; a modified client writes Hop 1 into every letter
// and the reader's "(hop limit)" label never fires.
func TestRedTeamI_StrangerHopLimitIsPerThread(t *testing.T) {
	noWait(t)
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	olga := w.agent(oscar, "olga", coins(1), `{}`)
	vera := w.agent(nina, "vera", coins(1), `{}`)
	olga.setContact("nina", true)
	vera.setContact("oscar", true)
	err, who, last := talk(t, olga, vera, 7)
	if err == nil || who != olga || !strings.Contains(err.Error(), "hop limit") {
		t.Fatalf("expected the hop limit at 7: %v", err)
	}
	_ = last
	// The same agent goes on in fresh threads: never refused.
	for i := 0; i < 12; i++ {
		if _, err := olga.send(SendOpts{To: "vera.nina", Intent: "answer", Text: fmt.Sprintf("as I was saying (%d)", i)}); err != nil {
			t.Fatalf("fresh thread %d refused: %v", i, err)
		}
	}
	// A modified client: the Hop field is the sender's word.
	m := Mail{Thread: last.Mail.Thread, Intent: "answer", Text: "hop 1, honest", Hop: 1}
	if _, err := olga.sendDatagramPaid(tMessage, "vera.nina", m, false); err != nil {
		t.Fatal(err)
	}
	var forged *Item
	for _, it := range mailTo(scan(t, vera), tMessage) {
		if it.Mail != nil && it.Mail.Text == "hop 1, honest" {
			forged = it
		}
	}
	if forged == nil || forged.Status == "refused" || strings.Contains(forged.Verified, "hop limit") {
		t.Fatalf("forged hop: %+v", forged)
	}
	t.Logf("RESULT I9 BROKEN (low; the hop limit protects honest clients from loops, not against a hostile sender): 12 more letters in fresh threads after the limit; a Hop-1 letter in the exhausted thread reads as %q",
		forged.Status)
}
