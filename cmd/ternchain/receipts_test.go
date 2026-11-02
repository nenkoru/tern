package main

import (
	"strings"
	"testing"
)

// A provider counts work, the consumer countersigns what its own side
// counted, and pays the difference by voucher on its channel; a receipt
// only grows, keeps its terms, and binds only its two names.
func TestReceipts(t *testing.T) {
	noApprovalWait(t)
	w := newWorld(t)
	oscar, _ := w.owner("oscar")
	nina, _ := w.owner("nina")
	mallory, _ := w.owner("mallory")
	w.must(oscar.deposit(coins(5)))
	_, err := oscar.channelOpen("nina", coins(1), 2)
	w.must(err)

	price, err := parseUnitPrice("0.002/1000", "token")
	w.must(err)
	u, err := nina.receiptIssue(Usage{Consumer: "oscar", Count: 1500, Unit: "token", Resource: "tiny-32 layers 12-23", Price: price})
	w.must(err)
	if u.ProviderSig == "" || u.ConsumerSig != "" || len(u.Session) != 66 {
		t.Fatalf("issued: %+v", u)
	}
	if _, err := oscar.receiptAccept(u, 1400); err == nil || !strings.Contains(err.Error(), "your side counted 1400") {
		t.Fatalf("a receipt above what the consumer counted: %v", err)
	}
	if _, err := oscar.receiptAccept(u, 0); err == nil || !strings.Contains(err.Error(), "--observed") {
		t.Fatalf("no count of its own: %v", err)
	}
	forged := u
	forged.Count = 15000
	if err := oscar.receiptVerify(forged, false); err == nil || !strings.Contains(err.Error(), "provider's signature") {
		t.Fatalf("a count changed after signing: %v", err)
	}
	if _, err := mallory.receiptAccept(u, 1500); err == nil || !strings.Contains(err.Error(), "not you") {
		t.Fatalf("a third name: %v", err)
	}
	signed, err := oscar.receiptAccept(u, 1500)
	w.must(err)
	w.must(oscar.receiptVerify(signed, true))
	if _, err := nina.receiptAccept(signed, 0); err != nil {
		t.Fatalf("the provider keeps the countersigned copy: %v", err)
	}

	res, err := oscar.receiptSettle("nina", "")
	w.must(err)
	if len(res) != 1 || res[0]["paid"] != "0.003000" || res[0]["units"] != uint64(1500) {
		t.Fatalf("settle: %v", res)
	}
	if _, err := oscar.receiptSettle("nina", ""); err == nil || !strings.Contains(err.Error(), "nothing owed") {
		t.Fatalf("settled twice: %v", err)
	}

	// the session goes on: a bigger receipt, only the difference is paid
	u.Count, u.ProviderSig = 4000, ""
	u2, err := nina.receiptIssue(u)
	w.must(err)
	_, err = oscar.receiptAccept(u2, 4000)
	w.must(err)
	res, err = oscar.receiptSettle("nina", u.Session)
	w.must(err)
	if res[0]["paid"] != "0.005000" {
		t.Fatalf("second settle: %v", res)
	}
	scan(t, nina)
	if in := nina.st.InChans; len(in) != 1 {
		t.Fatalf("nina holds the vouchers: %+v", in)
	}
	for _, ic := range nina.st.InChans {
		if fmtCoins(weiOf(ic.Cumulative)) != "0.008000" {
			t.Fatalf("voucher total %s", ic.Cumulative)
		}
	}

	// a receipt only grows and keeps its terms
	u.Count, u.ProviderSig = 100, ""
	if _, err := nina.receiptIssue(u); err == nil || !strings.Contains(err.Error(), "only grows") {
		t.Fatalf("a smaller count: %v", err)
	}
	u.Count, u.Price = 5000, &OfferPrice{Coins: "0.02", Per: 1000, Unit: "token"}
	if _, err := nina.receiptIssue(u); err == nil || !strings.Contains(err.Error(), "other terms") {
		t.Fatalf("a new price mid-session: %v", err)
	}
	if rows := oscar.receiptList(); len(rows) != 1 || rows[0]["owed"] != "0.000000" || rows[0]["paid"] != "0.008000" {
		t.Fatalf("list: %v", rows)
	}

	// strict parsing: a field nobody signed is refused, not shown
	if _, err := parseReceipt(`{"schema":"tern.receipt/1","provider":"nina","consumer":"oscar","bonus":"1"}`); err == nil {
		t.Fatal("an unknown field passed")
	}
}
