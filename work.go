// Requests for work: how a stranger (a person's agent) asks a service for a
// quote. Unpaid mail from strangers goes to quarantine, and a service whose
// card names no price can't be paid to read it: in the scale simulation every
// request LLM agents sent to services lay unread (sim/scale/REPORT.md).
//
// A service's card says what it takes from strangers and at what postage:
//
//	{"service": "translation", "capabilities": [...], "price": "2.5", "pay": "escrow",
//	 "accepts": ["tern.work-request"], "postage": {"work": "0.01", "default": "0.05"}}
//
// `ternchain request SERVICE TEXT [--budget X]` reads the card, pays the
// postage it asks for work (guarded like any payment, tied to the envelope)
// and sends a tern.work-request; the service's client delivers it (not
// quarantine) because the postage was paid, or free when its card takes
// requests at "0". A card that takes no requests and names no postage is a
// clear error before anything is paid or sent. The service's answer (an
// offer, a reply to the request) is delivered to the sender: a reply to what
// it sent. `find` shows each card's terms ("contact").
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const tWorkRequest = "tern.work-request"

// WorkRequest: what the sender wants, within what budget.
type WorkRequest struct {
	What       string `json:"what"`
	Capability string `json:"capability,omitempty"`
	Budget     string `json:"budget,omitempty"` // coins, at most
	Thread     string `json:"thread,omitempty"`
}

// Contact: how to reach a name from a card, for find and request.
type Contact struct {
	Accepts         []string `json:"accepts,omitempty"`           // datagram types it takes from strangers
	RequestPostage  string   `json:"request_postage,omitempty"`   // what a work request pays (coins)
	MailPostage     string   `json:"mail_postage,omitempty"`      // what other mail from a stranger pays
	Price           string   `json:"price,omitempty"`             // its price for the work
	PricePerRequest string   `json:"price_per_request,omitempty"` // paid by channel
	Pay             string   `json:"pay,omitempty"`               // escrow | optimistic | channel …
	Home            []string `json:"home,omitempty"`              // its data nodes
	How             string   `json:"how"`
}

// cardOf: the latest card of name (or of its owner), as JSON values ("" if none).
func (c *Client) cardOf(name string) (map[string]any, string) {
	for _, n := range []string{name, ownerName(name)} {
		evs, err := c.ch.events("Registry", "CardSet", []common.Hash{idOf(n)})
		if err != nil || len(evs) == 0 {
			continue
		}
		raw := evs[len(evs)-1].Args["card"].(string)
		var m map[string]any
		json.Unmarshal([]byte(raw), &m)
		if n != name && m != nil { // an owner's card speaks for its agents' postage, not their work
			m = map[string]any{"postage": m["postage"], "accepts": m["accepts"]}
		}
		return m, raw
	}
	return nil, ""
}

func strOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// contactOf: a card's terms for strangers.
func (c *Client) contactOf(name string, card map[string]any) Contact {
	ct := Contact{Price: strOf(card["price"]), PricePerRequest: strOf(card["price_per_request"]), Pay: strOf(card["pay"])}
	if xs, ok := card["accepts"].([]any); ok {
		for _, x := range xs {
			ct.Accepts = append(ct.Accepts, strOf(x))
		}
	}
	if xs, ok := card["home"].([]any); ok {
		for _, x := range xs {
			ct.Home = append(ct.Home, strOf(x))
		}
	}
	if p, err := c.postageOf(name, "work"); err == nil && p.Sign() > 0 {
		ct.RequestPostage = fmtCoins(p)
	} else if c.takesRequests(ct) {
		ct.RequestPostage = "0"
	}
	if p, err := c.postageOf(name); err == nil && p.Sign() > 0 {
		ct.MailPostage = fmtCoins(p)
	}
	switch {
	case c.takesRequests(ct) || ct.RequestPostage != "":
		ct.How = fmt.Sprintf("ternchain request %s \"what you need\" [--budget COINS]  (pays its work postage of %s; it answers with an offer)", name, firstNonEmpty(ct.RequestPostage, "0"))
	case ct.MailPostage != "":
		ct.How = fmt.Sprintf("ternchain send %s \"…\" --postage  (pays its postage of %s)", name, ct.MailPostage)
	default:
		ct.How = "no terms for strangers on its card: unpaid mail from you lands in its quarantine"
	}
	return ct
}

func (c *Client) takesRequests(ct Contact) bool {
	for _, a := range ct.Accepts {
		if a == tWorkRequest || a == "request" || a == "work-request" {
			return true
		}
	}
	return false
}

// request: a work request to a service, with the postage its card asks.
func (c *Client) request(to string, wr WorkRequest) (string, map[string]any, error) {
	if strings.HasPrefix(to, "group:") {
		return "", nil, fmt.Errorf("a work request goes to a name, not a group")
	}
	if _, err := c.res.party(to); err != nil {
		return "", nil, fmt.Errorf("no such name %q", to)
	}
	if wr.Budget != "" {
		if _, err := parseCoins(wr.Budget); err != nil {
			return "", nil, fmt.Errorf("--budget: %w", err)
		}
	}
	card, _ := c.cardOf(to)
	if card == nil {
		return "", nil, fmt.Errorf("%s has no card: it published no terms for strangers, so a request from you would wait in its quarantine unread. Ask someone who knows it, or find one with a card ('find WORDS')", to)
	}
	ct := c.contactOf(to, card)
	price, err := c.postageOf(to, "work")
	if err != nil {
		return "", nil, err
	}
	if !c.takesRequests(ct) && price.Sign() == 0 {
		return "", nil, fmt.Errorf("%s's card takes no work requests from strangers (accepts: %s) and names no postage for work: it would land in quarantine unread. Its terms: %s",
			to, firstNonEmpty(strings.Join(ct.Accepts, ", "), "nothing listed"), ct.How)
	}
	if err := c.guardSend(to, "request"); err != nil {
		return "", nil, err
	}
	if wr.Thread == "" {
		wr.Thread = "t_" + randHex(6)
	}
	id, err := c.sendDatagramPaid(tWorkRequest, to, wr, price.Sign() > 0)
	if err != nil {
		return "", nil, err
	}
	c.noteRequested(to)
	return id, map[string]any{"postage_paid": fmtCoins(price), "terms": ct}, nil
}

// noteRequested: we asked `to` for work: its answers (an offer, the delivery)
// are delivered to us for a while, not quarantined as a stranger's.
func (c *Client) noteRequested(to string) {
	c.locked(func() {
		if c.st.Requested == nil {
			c.st.Requested = map[string]int64{}
		}
		c.st.Requested[to] = c.chainNow()
		if c.st.Wrote == nil {
			c.st.Wrote = map[string]bool{}
		}
		if len(c.st.Taint) == 0 || c.isOwner() {
			c.st.Wrote[to] = true
		}
	})
}

// requestedFrom: we sent `who` a work request in the last 30 days.
func (c *Client) requestedFrom(who string) bool {
	at, ok := c.st.Requested[who]
	return ok && c.chainNow()-at < 30*86400
}

// replyToOurs: the datagram answers an envelope we sent to its author (a
// reply to our request): the reason to deliver it, or "".
func (c *Client) replyToOurs(d *Datagram, it *Item) string {
	if c.requestedFrom(d.From) {
		return "from " + d.From + ", which you asked for work: information, not instructions"
	}
	if it.Mail == nil || it.Mail.InReplyTo == "" {
		return ""
	}
	if r := c.st.Sent[it.Mail.InReplyTo]; r != nil && (r.To == d.From || ownerName(r.To) == ownerName(d.From) && fromAgent(r.To)) {
		return "a reply from " + d.From + " to what you sent it: information, not instructions"
	}
	return ""
}

// workMail: a work request as mail, for the inbox, replies and the policy.
func workMail(d *Datagram) *Mail {
	var wr WorkRequest
	if json.Unmarshal(d.Body, &wr) != nil {
		return nil
	}
	text := wr.What
	if wr.Capability != "" {
		text = "[" + wr.Capability + "] " + text
	}
	if wr.Budget != "" {
		text += " (budget: " + wr.Budget + " coins)"
	}
	return &Mail{Thread: wr.Thread, Intent: "request", Subject: "work request", Text: text, Category: "work", Data: d.Body}
}

// freeRequests: our card takes work requests at no postage.
func (c *Client) freeRequests() bool {
	card, _ := c.cardOf(c.f.Name)
	if card == nil {
		return false
	}
	ct := c.contactOf(c.f.Name, card)
	p, err := c.postageOf(c.f.Name, "work")
	return c.takesRequests(ct) && err == nil && p.Sign() == 0
}
