package main

import (
	"strings"
	"testing"
	"time"
)

// receipt: provider p served consumer q n tokens in group g, countersigned.
func receipt(t *testing.T, p, q *Client, g string, n uint64, session string) Usage {
	t.Helper()
	u, err := p.receiptIssue(Usage{Consumer: q.f.Name, Count: n, Unit: "token", Group: g, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if u, err = q.receiptAccept(u, n); err != nil {
		t.Fatal(err)
	}
	return u
}

// A group runs on credits without money: each member computes the same
// ledger from countersigned receipts posted in the group; one family is one
// member, a session counts once at its best count, nothing is transferable,
// and only an admin's policy holds.
func TestGroupCredits(t *testing.T) {
	w := newWorld(t)
	oscar, nina, ada, node := friends(t, w)
	gpu := w.agent(nina, "gpu", coins(1), `{}`) // nina's machine: its work is nina's credit

	a1 := receipt(t, gpu, oscar, "friends", 1000, "")
	a2 := receipt(t, gpu, oscar, "friends", 2000, a1.Session) // the same session, later
	b := receipt(t, ada, oscar, "friends", 1000, "")
	cc := receipt(t, oscar, ada, "friends", 500, "")
	self := receipt(t, gpu, nina, "friends", 9000, "")
	elsewhere := receipt(t, ada, nina, "chess-club", 700, "")

	for _, x := range []struct {
		by *Client
		u  Usage
	}{{nina, a1}, {nina, a2}, {oscar, a1}, {ada, b}, {oscar, cc}, {nina, self}} {
		_, err := x.by.creditsPostUsage("friends", x.u)
		w.must(err)
	}
	if _, err := ada.creditsPostUsage("friends", elsewhere); err == nil || !strings.Contains(err.Error(), "names group") {
		t.Fatalf("a receipt for another group: %v", err)
	}
	_, err := ada.postGroup("friends", "sneaky", jsonOf(creditsPost{Usage: &elsewhere})) // posted by hand: readers ignore it
	w.must(err)
	unsigned := b
	unsigned.ConsumerSig, unsigned.Session, unsigned.Count = "", newSession(), 50000
	_, err = ada.postGroup("friends", "sneaky", jsonOf(creditsPost{Usage: &unsigned}))
	w.must(err)

	if _, err := ada.creditsSetPolicy("friends", CreditsPolicy{Floor: -1, Grants: map[string]float64{"ada": 1e6}}); err == nil || !strings.Contains(err.Error(), "only an admin") {
		t.Fatalf("a member's policy: %v", err)
	}
	_, err = ada.postGroup("friends", "policy", jsonOf(creditsPost{Policy: &CreditsPolicy{Schema: tCreditsPolicy, Grants: map[string]float64{"ada": 1e6}}}))
	w.must(err)
	_, err = oscar.creditsSetPolicy("friends", CreditsPolicy{Rates: map[string]float64{"token": 0.01}, Floor: -15})
	w.must(err)
	w.must(node.flush())

	var ledgers []*Ledger
	for _, c := range []*Client{oscar, nina, ada} {
		scan(t, c)
		l, err := c.ledger("friends")
		w.must(err)
		ledgers = append(ledgers, l)
	}
	l := ledgers[0]
	if l.Balances["nina"] != 20 || l.Balances["ada"] != 5 || l.Balances["oscar"] != -25 || l.Sessions != 3 || l.PolicyBy != "oscar" {
		t.Fatalf("ledger: %+v", l)
	}
	for _, other := range ledgers[1:] {
		if string(jsonOf(other.Balances)) != string(jsonOf(l.Balances)) {
			t.Fatalf("readers disagree: %v vs %v", other.Balances, l.Balances)
		}
	}
	why := strings.Join(l.Ignored, "\n")
	for _, want := range []string{"one family on both sides", "names another group", "signature", "not an admin's"} {
		if !strings.Contains(why, want) {
			t.Errorf("ignored should say %q:\n%s", want, why)
		}
	}
	q := l.priority()
	if q[0].Name != "nina" || q[1].Name != "ada" || q[2].Name != "oscar" || !q[2].Waits || q[0].Waits {
		t.Fatalf("queue: %+v", q)
	}

	// a cap per pair a day: two friends can't pump each other's credits
	_, err = oscar.creditsSetPolicy("friends", CreditsPolicy{Rates: map[string]float64{"token": 0.01}, Floor: -15, PairCapDay: 15, Grants: map[string]float64{"ada": 3}})
	w.must(err)
	w.must(node.flush())
	scan(t, ada)
	l, err = ada.ledger("friends")
	w.must(err)
	if l.Balances["nina"] != 15 || l.Balances["ada"] != 8 || !strings.Contains(strings.Join(l.Ignored, "\n"), "over the pair's cap") {
		t.Fatalf("capped: %v %v", l.Balances, l.Ignored)
	}

	// a month on, the machine's grant has ended: what it signed then still counts
	w.later(31 * 24 * time.Hour)
	if err := ada.receiptVerify(a2, true); err == nil {
		t.Fatal("a receipt signed by an expired grant verifies now")
	}
	if l2, err := ada.ledger("friends"); err != nil || l2.Balances["nina"] != 15 {
		t.Fatalf("credits vanished with the grant: %v %v", l2.Balances, err)
	}
}
