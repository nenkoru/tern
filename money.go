// Money, attestations, cards and search: thin wrappers over the contracts.
package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/text/unicode/norm"
)

func (c *Client) deposit(amount *big.Int) error {
	_, err := c.ch.send(c.key, amount, "Bank", "deposit", idOf(c.f.Owner))
	return err
}

func (c *Client) bankBalance(owner string) (*big.Int, error) {
	out, err := c.ch.call("Bank", "balance", idOf(owner))
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// PayRec: a payment we made recently.
type PayRec struct {
	To     string `json:"to"`
	Amount string `json:"amount"`
	Memo   string `json:"memo"`
	Time   int64  `json:"time"`
	Tx     string `json:"tx"`
}

// repeatWindow: the same payment again within this many seconds is likely a mistake.
const repeatWindow = 600

// repeatedPay finds the same payment (to, amount, memo) made in the last ten minutes.
func (c *Client) repeatedPay(to string, amount *big.Int, memo string) *PayRec {
	now := c.chainNow()
	for i := len(c.st.Pays) - 1; i >= 0; i-- {
		p := c.st.Pays[i]
		if p.To == to && p.Amount == fmtCoins(amount) && p.Memo == memo && now-p.Time < repeatWindow {
			return &p
		}
	}
	return nil
}

// payReceipt pays and describes the payment: the receipt the payer keeps.
func (c *Client) payReceipt(to string, amount *big.Int, memo string, again bool) (map[string]any, error) {
	if p := c.repeatedPay(to, amount, memo); p != nil && !again {
		return nil, fmt.Errorf("you already paid %s %s coins (%q) %d seconds ago, tx %s; to pay again on purpose add --again",
			to, p.Amount, memo, c.chainNow()-p.Time, p.Tx)
	}
	if err := c.pay(to, amount, memo); err != nil {
		return nil, err
	}
	r := map[string]any{"paid": fmtCoins(amount), "to": to, "memo": memo, "from": c.f.Name}
	if c.lastReceipt != nil {
		r["tx"], r["block"] = c.lastReceipt.TxHash.Hex(), c.lastReceipt.BlockNumber.Uint64()
	}
	now := c.chainNow()
	rec := PayRec{To: to, Amount: fmtCoins(amount), Memo: memo, Time: now, Tx: fmt.Sprint(r["tx"])}
	var keep []PayRec
	for _, p := range c.st.Pays {
		if now-p.Time < repeatWindow {
			keep = append(keep, p)
		}
	}
	c.st.Pays = append(keep, rec)
	c.save()
	if b, err := c.bankBalance(c.f.Owner); err == nil {
		r["balance_after"] = fmtCoins(b) + " (" + c.f.Owner + "'s bank)"
	}
	return r, nil
}

func (c *Client) pay(to string, amount *big.Int, memo string) error {
	done, err := c.guardPay(to, amount) // checks the limit before asking the owner (spendCheck, freeCapNote)
	if err != nil {
		return err
	}
	err = c.payNow(to, amount, memo)
	done(err == nil)
	return err
}

func (c *Client) payNow(to string, amount *big.Int, memo string) (err error) {
	if c.asset != (common.Address{}) {
		c.lastReceipt, err = c.ch.send(c.key, nil, "Bank", "payToken", c.id(), payeeID(to), c.asset, amount, crypto.Keccak256Hash([]byte(memo)))
		return c.limitErr(err)
	}
	c.lastReceipt, err = c.ch.send(c.key, nil, "Bank", "pay", c.id(), payeeID(to), amount, crypto.Keccak256Hash([]byte(memo)))
	return c.limitErr(err)
}

// agentEpochMoney: what this agent spent (of its limit) and what was paid to
// its own name in epoch e. Money paid to an agent lands in the owner's bank.
func (c *Client) agentEpochMoney(e uint64) map[string]any {
	m := map[string]any{}
	if lim, err := c.ch.call("Registry", "spendLimit", c.id()); err == nil {
		m["limit"] = fmtCoins(lim[0].(*big.Int))
	}
	if sp, err := c.ch.call("Bank", "spent", c.id(), e); err == nil {
		m["spent_by_you"] = fmtCoins(sp[0].(*big.Int))
	}
	n, _ := c.ch.epochLength()
	got, count := new(big.Int), 0
	if evs, err := c.ch.events("Bank", "Paid", nil, []common.Hash{c.id()}); err == nil && n > 0 {
		for _, ev := range evs {
			if bt, err := c.ch.blockTime(ev.Block); err == nil && uint64(bt)/n == e {
				got.Add(got, ev.Args["amount"].(*big.Int))
				count++
			}
		}
	}
	m["received_by_your_name"], m["payments_received"] = fmtCoins(got), count
	m["resets"] = c.epochWords()
	return m
}

// limitErr adds the numbers to "over the agent's limit": the limit per
// epoch and what is spent already.
func (c *Client) limitErr(err error) error {
	if err == nil || !strings.Contains(err.Error(), "over the agent's limit") || c.isOwner() {
		return err
	}
	lim, err1 := c.ch.call("Registry", "spendLimit", c.id())
	e, err2 := c.ch.epoch()
	if err1 != nil || err2 != nil {
		return err
	}
	sp, err3 := c.ch.call("Bank", "spent", c.id(), e)
	if err3 != nil {
		return err
	}
	return fmt.Errorf("%w: your limit is %s coins per epoch and %s are spent this epoch (%s; the owner raises it with a new grant, or pays/locks it itself)",
		err, fmtCoins(lim[0].(*big.Int)), fmtCoins(sp[0].(*big.Int)), c.epochWords())
}

// openEscrow: we (client) lock amount for worker; arbiter decides disputes.

// ---- attestations: labels, reviews, receipts, paid placement, skills

func (c *Client) attest(subject, kind, data string, expires uint64) error {
	_, err := c.easAttest(subjectID(subject), kind, []byte(data), 0, expires, common.Hash{})
	return err
}

// subjectID: a name, "group:<name>", or a 0x hash (an envelope, a deal).
func subjectID(subject string) common.Hash {
	if strings.HasPrefix(subject, "0x") && len(subject) == 66 {
		return common.HexToHash(subject)
	}
	if g, ok := strings.CutPrefix(subject, "group:"); ok {
		return setID(g)
	}
	return idOf(subject)
}

type Attest struct {
	UID     string `json:"uid"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Kind    string `json:"kind"`
	Data    string `json:"data"`
	Score   int64  `json:"score"`
}

// attestationsAbout lists attestations about subject (optionally only from trusted issuers).
func (c *Client) attestationsAbout(subject string, trusted map[string]bool) ([]Attest, error) {
	// An agent's reputation is its owner's too: show claims about both.
	subjects := []string{subject}
	if o := ownerName(subject); o != subject && !strings.HasPrefix(subject, "0x") && !strings.Contains(subject, ":") {
		subjects = append(subjects, o)
	}
	out := []Attest{}
	for _, sub := range subjects {
		cls, err := c.trustedClaims(subjectID(sub), trusted)
		if err != nil {
			return nil, err
		}
		for _, cl := range cls {
			out = append(out, Attest{UID: cl.UID.Hex(), Issuer: cl.Issuer, Subject: sub, Kind: cl.Kind, Data: c.outside(cl.Issuer, cl.Data), Score: cl.Score})
		}
	}
	return out, nil
}

// ---- cards and search (an indexer does the same at scale, for a fee)

func (c *Client) setCard(card string) error {
	_, err := c.ch.send(c.key, nil, "Registry", "setCard", c.id(), card)
	return err
}

type Found struct {
	Kind    string   `json:"kind"` // card | group
	Name    string   `json:"name"`
	Text    string   `json:"text"`
	Contact *Contact `json:"contact,omitempty"` // a card's terms for strangers: how to reach it (work.go)
}

// find: cards, groups and communities whose text has every word of q, in any
// language: case and Unicode forms are folded (NFKC, ё = е), a card's JSON
// is searched by its values, a community also by its rules.
func (c *Client) find(q string) ([]Found, error) {
	terms := strings.Fields(foldText(q))
	match := func(texts ...string) bool {
		hay := foldText(strings.Join(texts, " "))
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				return false
			}
		}
		return true
	}
	out := []Found{}
	latest := map[string]string{}
	cards, err := c.ch.events("Registry", "CardSet")
	if err != nil {
		return nil, err
	}
	c.res.loadNames() // names of all at once, not a search per card
	for _, e := range cards {
		n, err := c.res.nameOf(common.Hash(e.Args["id"].([32]byte)))
		if err == nil {
			latest[n] = e.Args["card"].(string)
		}
	}
	for n, card := range latest {
		if match(n, cardText(card)) {
			f := Found{Kind: "card", Name: n, Text: c.outside(n, card)} // a card is its owner's words: data
			var m map[string]any
			if json.Unmarshal([]byte(card), &m) == nil {
				ct := c.contactOf(n, m)
				f.Contact = &ct
			}
			out = append(out, f)
		}
	}
	gs, err := c.ch.events("Sets", "Created")
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		n := g.Args["name"].(string)
		rules := ""
		if c.ch.has("Commons") && c.isCommunity(n) {
			if cm, err := c.community(n); err == nil {
				rules = cm.Rules
			}
		}
		if match(n, rules) {
			kind := []string{"open", "by approval", "closed"}[g.Args["rule"].(uint8)]
			if rules != "" {
				kind = "community: " + c.outside(n, rules)
			}
			// A group's name is its creator's words (any 1-64 bytes): data, and
			// it taints an agent that reads it (audit 4 AS-3).
			by, err := c.res.nameOf(common.Hash(g.Args["owner"].([32]byte)))
			if err != nil || by == "" {
				by = "a stranger"
			}
			if !c.isOwner() && !c.family(by) {
				c.taint(by)
				if by == "a stranger" {
					c.taint("set:" + n)
				}
				kind = "named by " + by + ": " + fence(by, n) + "; " + kind
			}
			out = append(out, Found{Kind: "set", Name: n, Text: kind})
		}
	}
	return out, nil
}

// ---- recovery

func (c *Client) startRecovery(owner string, next common.Address) error {
	_, err := c.ch.send(c.key, nil, "Registry", "startRecovery", idOf(owner), next)
	return err
}

func (c *Client) cancelRecovery() error {
	_, err := c.ch.send(c.key, nil, "Registry", "cancelRecovery", c.id())
	return err
}

func (c *Client) finishRecovery(owner string) error {
	_, err := c.ch.send(c.key, nil, "Registry", "finishRecovery", idOf(owner))
	return err
}

func (c *Client) revokeAgent(agent string, compromised bool) error {
	_, err := c.ch.send(c.key, nil, "Registry", "revoke", idOf(agent), compromised)
	return err
}

// foldText: lower case, NFKC, and ё as е, for matching across scripts and input methods.
func foldText(s string) string {
	return strings.ReplaceAll(strings.ToLower(norm.NFKC.String(s)), "ё", "е")
}

// cardText: a card's searchable text: its JSON values (so \u-escaped text
// matches too), or the card as written when it isn't JSON.
func cardText(card string) string {
	var v any
	if json.Unmarshal([]byte(card), &v) != nil {
		return card
	}
	var parts []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			parts = append(parts, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				parts = append(parts, k)
				walk(e)
			}
		default:
			parts = append(parts, fmt.Sprint(t))
		}
	}
	walk(v)
	return strings.Join(parts, " ")
}
