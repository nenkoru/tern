package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirectMailMissingOwnerKeyFailsClosed(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ag := w.agent(nina, "a", coins(1), `{}`)
	w.later(8 * 24 * time.Hour)
	_, err := ag.rotateEpochs()
	must(t, err)
	env := receiveTestEnvelope(t, w, nina, ag, "owner-offline", nina.chainNow(), false)
	must(t, nina.post([]Envelope{env}))
	got := mailTo(scan(t, ag), tMessage)
	if len(got) != 1 || got[0].Wake || got[0].Status != "refused" || !strings.Contains(got[0].Verified, "cannot verify recipient owner oversight") || len(ag.st.MailReceipts) != 0 {
		t.Fatalf("missing owner publication was accepted or claimed: %+v", got)
	}
}

func receiveTestEnvelope(t *testing.T, w *world, sender, recipient *Client, id string, created int64, valid bool) Envelope {
	t.Helper()
	e, err := w.ch.epoch()
	must(t, err)
	view, rec, err := sender.res.keysAt(recipient.f.Name, e)
	must(t, err)
	readers := []string{rec}
	if valid {
		_, owner, err := sender.res.keysAt(recipient.f.Owner, e)
		must(t, err)
		readers = append(readers, owner)
	}
	body, _ := json.Marshal(Mail{Text: "once only", Intent: "inform"})
	d := Datagram{Type: tMessage, From: sender.f.Name, To: recipient.f.Name, Created: created, DeliveryID: id, Body: body}
	for _, r := range readers {
		d.Readers = append(d.Readers, fingerprint(r))
	}
	signed, err := signDatagram(d, ethSigner{sender.key})
	must(t, err)
	env, err := seal(signed, e, view, nil, readers, w.ch.bucketBits())
	must(t, err)
	return env
}

func TestDirectMailReceiptRefreshOrders(t *testing.T) {
	for _, order := range []string{"original-first", "refresh-first", "refused-first", "legacy"} {
		t.Run(order, func(t *testing.T) {
			w := newWorld(t)
			nina, _ := w.owner("nina")
			ag := w.agent(nina, "a", coins(1), `{}`)
			id := "stable-delivery"
			if order == "legacy" {
				id = ""
			}
			created := nina.chainNow()
			first := receiveTestEnvelope(t, w, nina, ag, id, created, order != "refused-first")
			second := receiveTestEnvelope(t, w, nina, ag, id, created, true)
			if first.hash() == second.hash() {
				t.Fatal("expected independently sealed envelopes")
			}
			if order == "refresh-first" {
				first, second = second, first
			}
			must(t, nina.post([]Envelope{first}))
			got := mailTo(scan(t, ag), tMessage)
			if order == "refused-first" {
				if len(got) != 1 || got[0].Status != "refused" || len(ag.st.MailReceipts) != 0 {
					t.Fatalf("bad original claimed receipt: %+v", got)
				}
			} else if len(got) != 1 || !got[0].Wake {
				t.Fatalf("first delivery: %+v", got)
			}
			must(t, nina.post([]Envelope{second}))
			got = mailTo(scan(t, ag), tMessage)
			if order == "refused-first" {
				if len(got) != 1 || !got[0].Wake {
					t.Fatalf("valid refresh suppressed: %+v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("second envelope delivered twice: %+v", got)
			}
		})
	}
}

func TestDirectMailReceiptPurgeConcurrentAndExpiry(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ag := w.agent(nina, "a", coins(1), `{}`)
	ag.f.Retain = 30
	must(t, ag.saveIdentityFile())
	dir := filepath.Join(w.dir, ag.f.Name)
	stale, err := openClient(dir, w.ch)
	must(t, err)
	created := nina.chainNow()
	env := receiveTestEnvelope(t, w, nina, ag, "purge-once", created, true)
	refresh := receiveTestEnvelope(t, w, nina, ag, "purge-once", created, true)
	must(t, nina.post([]Envelope{env}))
	got := mailTo(scan(t, ag), tMessage)
	if len(got) != 1 || !got[0].Wake {
		t.Fatalf("first delivery: %+v", got)
	}
	must(t, stale.save())
	if stale.item(env.hash().Hex()) == nil {
		t.Fatal("stale save lost another process's received archive")
	}
	got[0].Done = true
	if ag.pruneItems(true) != 1 {
		t.Fatal("expected one purged message")
	}
	must(t, ag.save())
	must(t, stale.save())
	if stale.item(env.hash().Hex()) != nil {
		t.Fatal("stale save resurrected purged plaintext")
	}
	ag, err = openClient(dir, w.ch)
	must(t, err)
	must(t, nina.post([]Envelope{refresh, env}))
	if got := mailTo(scan(t, ag), tMessage); len(got) != 0 {
		t.Fatalf("purged mail replayed: %+v", got)
	}
	// Keep a process with an unexpired receipt, then expire the durable copy.
	stale, err = openClient(dir, w.ch)
	must(t, err)
	w.later(8 * 24 * time.Hour)
	scan(t, ag)
	if len(ag.st.MailReceipts) != 0 {
		t.Fatal("expired receipt retained")
	}
	must(t, stale.save())
	if len(stale.st.MailReceipts) != 0 {
		t.Fatal("stale writer restored expired receipt")
	}
	ag, err = openClient(dir, w.ch)
	must(t, err)
	ag.st.Cursor = w.ch.dep.Block
	for _, it := range mailTo(scan(t, ag), tMessage) {
		if it.Status != "refused" || it.Wake {
			t.Fatalf("historical rescan woke expired instruction: %+v", it)
		}
	}
}

func TestDirectMailArchiveOutlivesReceipt(t *testing.T) {
	w := newWorld(t)
	nina, _ := w.owner("nina")
	ag := w.agent(nina, "a", coins(1), `{}`)
	env := receiveTestEnvelope(t, w, nina, ag, "archive", nina.chainNow(), true)
	must(t, nina.post([]Envelope{env}))
	scan(t, ag)
	stale, err := openClient(filepath.Join(w.dir, ag.f.Name), w.ch)
	must(t, err)
	w.later(8 * 24 * time.Hour)
	scan(t, ag)
	if len(ag.st.MailReceipts) != 0 {
		t.Fatal("receipt did not expire")
	}
	it := ag.item(env.hash().Hex())
	if it == nil || it.Mail == nil || it.Mail.Text != "once only" {
		t.Fatal("receipt expiry removed readable archive")
	}
	it.Done = true
	if ag.pruneItems(true) != 1 {
		t.Fatal("old archive cannot be purged")
	}
	must(t, ag.save())
	must(t, stale.save())
	if stale.item(env.hash().Hex()) != nil || len(stale.st.MailReceipts) != 0 {
		t.Fatal("stale writer resurrected plaintext or receipt after the floor advanced")
	}
}
