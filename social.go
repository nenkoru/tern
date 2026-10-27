// Social building blocks on top of the primitives: postage (paid reach,
// the honest form of advertising), secret pair markers (mutual likes that
// reveal nothing one-sided), and commit-reveal.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// ---- postage: "strangers may reach my agents for N coins"
//
// The card prices attention, per category if it likes:
//
//	"postage": "0.2"                                                  one price for every stranger
//	"postage": {"default": "0.2", "work": "0.5", "dating": "1", "ads": "5"}
//	"refund_on_reply_days": 7                                         (default 7; 0: never)
//
// Contacts always write for free ("friends" is what contacts pay: 0; a
// stranger can't buy it). Postage goes to the recipient, tied to the envelope
// by its hash; when the recipient replies within refund_on_reply_days, its
// client pays the postage back to the sender (reply --keep-postage keeps it),
// so a wanted letter costs nothing and spam pays for itself.

// postageCard is what a card says about postage.
type postageCard struct {
	Postage    json.RawMessage `json:"postage"`
	RefundDays *int            `json:"refund_on_reply_days"`
}

func (c *Client) postageCardOf(name string) (*postageCard, error) {
	for _, n := range []string{name, ownerName(name)} {
		evs, err := c.ch.events("Registry", "CardSet", []common.Hash{idOf(n)})
		if err != nil {
			return nil, err
		}
		if len(evs) == 0 {
			continue
		}
		var card postageCard
		if json.Unmarshal([]byte(evs[len(evs)-1].Args["card"].(string)), &card) == nil && len(card.Postage) > 0 {
			return &card, nil
		}
	}
	return nil, nil
}

// prices: the card's postage per category ("default" for the rest).
func (p *postageCard) prices() (map[string]*big.Int, error) {
	out := map[string]*big.Int{}
	var one string
	if json.Unmarshal(p.Postage, &one) == nil {
		v, err := parseCoins(one)
		if err != nil {
			return nil, err
		}
		out["default"] = v
		return out, nil
	}
	var per map[string]string
	if err := json.Unmarshal(p.Postage, &per); err != nil {
		return nil, fmt.Errorf("card postage: a price or {category: price}")
	}
	for k, s := range per {
		v, err := parseCoins(s)
		if err != nil {
			return nil, fmt.Errorf("card postage %s: %w", k, err)
		}
		out[strings.ToLower(k)] = v
	}
	return out, nil
}

// postageOf reads the asking price for a category from the recipient's (or its owner's) card.
func (c *Client) postageOf(name string, category ...string) (*big.Int, error) {
	card, err := c.postageCardOf(name)
	if err != nil || card == nil {
		return big.NewInt(0), err
	}
	ps, err := card.prices()
	if err != nil {
		return nil, err
	}
	cat := ""
	if len(category) > 0 {
		cat = strings.ToLower(category[0])
	}
	if v, ok := ps[cat]; ok && cat != "friends" && cat != "default" {
		return v, nil
	}
	if v, ok := ps["default"]; ok {
		return v, nil
	}
	return big.NewInt(0), nil
}

// refundDays: how long after a letter its sender's postage comes back on a reply.
func (c *Client) refundDays(name string) int {
	if card, err := c.postageCardOf(name); err == nil && card != nil && card.RefundDays != nil {
		return *card.RefundDays
	}
	return 7
}

func mailCategory(it *Item) string {
	if it.Mail == nil {
		return ""
	}
	return strings.ToLower(it.Mail.Category)
}

func categoryWords(cat string) string {
	if cat == "" {
		return ""
	}
	return " for " + cat
}

// A sender whose names (an owner and all its agents) paid postage to more
// than the spam module's bulk_postage_at (20) different names in a day is a
// bulk sender: whatever category it claims, its letters are priced as ads
// (the reader's module in use decides).

// bulkSender: the owner's names paid postage-like amounts to more than
// bulk_postage_at names in the last day (public Bank payments; cached per process).
func (c *Client) bulkSender(from string) bool {
	if from == "" || abuseFixesOff {
		return false
	}
	o, bulkAt := ownerName(from), int(c.policyOf("spam").Param("bulk_postage_at"))
	if c.bulk == nil {
		c.bulk = map[string]bool{}
	}
	key := fmt.Sprintf("%s|%d", o, c.chainNow()/600) // fresh every ten minutes
	if v, ok := c.bulk[key]; ok {
		return v
	}
	ids := []common.Hash{idOf(o)}
	if evs, err := c.ch.events("Registry", "AgentGranted", nil, []common.Hash{idOf(o)}); err == nil {
		for _, e := range evs {
			ids = append(ids, common.Hash(e.Args["id"].([32]byte)))
		}
	}
	to := map[common.Hash]bool{}
	now := c.chainNow()
	if evs, err := c.ch.events("Bank", "Paid", ids); err == nil {
		for _, e := range evs {
			if bt, err := c.ch.blockTime(e.Block); err == nil && now-bt < 86400 {
				to[common.Hash(e.Args["to"].([32]byte))] = true
			}
		}
	}
	// postage paid privately (StealthPay) from the family's keys: each payment
	// goes to a fresh one-time address, so each counts as one more name (audit 5
	// HM-9; a payment from a private wallet key shows no payer at all)
	if len(to) <= bulkAt && c.ch.has("StealthPay") {
		if evs, err := c.ch.events("ERC5564Announcer", "Announcement"); err == nil {
			for i := len(evs) - 1; i >= 0 && len(to) <= bulkAt; i-- {
				e := evs[i]
				bt, err := c.ch.blockTime(e.Block)
				if err != nil || now-bt >= 86400 {
					break // oldest first: the rest are older
				}
				if from, ok := c.txSender(e.Tx); ok && ownerName(c.res.nameAt(from, e.Block)) == o {
					to[common.BytesToHash(e.Args["stealthAddress"].(common.Address).Bytes())] = true
				}
			}
		}
	}
	v := len(to) > bulkAt
	c.bulk[key] = v
	return v
}

// txSender: who signed this transaction (a backend that serves transactions).
func (c *Client) txSender(h common.Hash) (common.Address, bool) {
	b, ok := c.ch.b.(interface {
		TransactionByHash(context.Context, common.Hash) (*types.Transaction, bool, error)
	})
	if !ok {
		return common.Address{}, false
	}
	tx, _, err := b.TransactionByHash(context.Background(), h)
	if err != nil {
		return common.Address{}, false
	}
	from, err := types.Sender(types.LatestSignerForChainID(c.ch.chainID), tx)
	return from, err == nil
}

// paidPostage: was at least our asking price (for the letter's category) paid for this envelope?
// A bulk sender pays at least the ads price, whatever it says the letter is.
func (c *Client) paidPostage(envelope, to string, category ...string) bool {
	price, err := c.postageOf(to, category...)
	if err != nil || price.Sign() == 0 {
		return false
	}
	if len(category) > 1 && c.bulkSender(category[1]) {
		if ads, err := c.postageOf(to, "ads"); err == nil && ads.Cmp(price) > 0 {
			price = ads
		}
	}
	var sender []string
	if len(category) > 1 {
		sender = category[1:2]
	}
	if p, _ := c.postagePaid(envelope, to, sender...); p != nil && p.amount.Cmp(price) >= 0 {
		return true
	}
	return c.paidStealthPostage(envelope, price)
}

type postagePayment struct {
	from   common.Hash
	amount *big.Int
	block  uint64
}

// postagePaid: the public Bank payments to `to` with this envelope as their
// memo, by the sender's family (summed; from: its first payer), or, with no
// sender named, the first such payment. Anyone can pay with any memo: a
// stranger's dust payment first doesn't stand for the sender's (audit 5 HM-14).
func (c *Client) postagePaid(envelope, to string, sender ...string) (*postagePayment, error) {
	evs, err := c.ch.events("Bank", "Paid", nil, []common.Hash{idOf(to)})
	if err != nil {
		return nil, err
	}
	var out *postagePayment
	for _, e := range evs {
		if common.Hash(e.Args["memo"].([32]byte)) != common.HexToHash(envelope) {
			continue
		}
		from := common.Hash(e.Args["from"].([32]byte))
		if len(sender) > 0 && sender[0] != "" && ownerName(c.nameOr(from)) != ownerName(sender[0]) {
			continue
		}
		if out == nil {
			out = &postagePayment{from, new(big.Int), e.Block}
		}
		out.amount.Add(out.amount, e.Args["amount"].(*big.Int))
		if len(sender) == 0 || sender[0] == "" {
			break
		}
	}
	return out, nil
}

func refundMemo(envelope string) common.Hash {
	return crypto.Keccak256Hash([]byte("postage-refund:" + envelope))
}

// refundOnReply: replying to a letter that paid us postage pays it back to
// its sender, within our refund window, once. The refund is bounded by what
// that sender paid for that envelope, so the guard lets it through (the
// chain's spend limit still applies to an agent). Returns what happened, or nil.
func (c *Client) refundOnReply(envelope string) map[string]any {
	it := c.item(envelope)
	if it == nil {
		return nil
	}
	if c.st.Refunds[it.ID] != "" {
		return map[string]any{"postage_refund": "already refunded", "amount": c.st.Refunds[it.ID]}
	}
	p, err := c.postagePaid(it.ID, it.To, it.From)
	if err != nil || p == nil || p.amount.Sign() == 0 {
		return nil // no public postage (contacts, or private postage whose payer we can't see)
	}
	days := c.refundDays(it.To)
	if days <= 0 {
		return map[string]any{"postage_refund": "kept: your card's refund_on_reply_days is 0"}
	}
	if bt, err := c.ch.blockTime(p.block); err == nil && c.chainNow()-bt > int64(days)*86400 {
		return map[string]any{"postage_refund": fmt.Sprintf("kept: the letter is older than %d days", days)}
	}
	if evs, err := c.ch.events("Bank", "Paid", nil, []common.Hash{p.from}); err == nil {
		for _, e := range evs {
			if common.Hash(e.Args["memo"].([32]byte)) == refundMemo(it.ID) {
				return map[string]any{"postage_refund": "already refunded (by another device)"}
			}
		}
	}
	if _, err := c.ch.send(c.key, nil, "Bank", "pay", c.id(), p.from, p.amount, refundMemo(it.ID)); err != nil {
		return map[string]any{"postage_refund": "failed: " + c.limitErr(err).Error()}
	}
	if c.st.Refunds == nil {
		c.st.Refunds = map[string]string{}
	}
	c.st.Refunds[it.ID] = fmtCoins(p.amount)
	c.save()
	return map[string]any{"postage_refund": fmtCoins(p.amount), "to": c.nameOr(p.from), "note": "you replied: the sender's postage went back (reply --keep-postage keeps it)"}
}

// postageRefunded: did the recipient give back our postage for this envelope (it replied)?
func (c *Client) postageRefunded(envelope string) *big.Int {
	evs, err := c.ch.events("Bank", "Paid", nil, []common.Hash{c.id()})
	if err != nil {
		return nil
	}
	for _, e := range evs {
		if common.Hash(e.Args["memo"].([32]byte)) == refundMemo(envelope) {
			return e.Args["amount"].(*big.Int)
		}
	}
	return nil
}

// ---- secret pair markers: mutual interest, nothing one-sided leaks

// pairMarker is a topic only `other` and we can compute: both sides derive
// it from their view keys (X25519), without talking. Observers can't tell
// whose marker it is.
// pairMarker is the marker that `by` publishes when it likes the other side of
// the pair (c, other) for topic. Both sides can compute both markers (they
// share an ECDH secret); outsiders can't link either marker to anyone.
func (c *Client) pairMarker(other, topic, by string) (common.Hash, error) {
	if c.view == nil {
		return common.Hash{}, errNotJoined
	}
	e, err := c.ch.epoch()
	if err != nil {
		return common.Hash{}, err
	}
	their, _, err := c.res.keysAt(other, e)
	if err != nil {
		return common.Hash{}, err
	}
	shared, err := c.view.ECDH(their)
	if err != nil {
		return common.Hash{}, err
	}
	h := sha256.New()
	h.Write([]byte("tern-pair\n"))
	h.Write(shared)
	h.Write([]byte("\n" + topic + "\n" + by))
	return common.BytesToHash(h.Sum(nil)), nil
}

// like publishes our marker for (other, topic), anonymously.
func (c *Client) like(other, topic string) error {
	k, err := c.pairMarker(other, topic, c.f.Name)
	if err != nil {
		return err
	}
	if c.relay != "" {
		return relayPost(c.relay, "/marker", k.Bytes())
	}
	_, err = c.ch.send(c.key, nil, "Commitments", "commit", common.Hash{}, k, common.Hash{}, uint64(0))
	return err
}

// mutual: did both sides publish the marker?
func (c *Client) mutual(other, topic string) (bool, error) {
	// Each side has its own marker, so liking twice from one side is not a match.
	for _, by := range []string{c.f.Name, other} {
		k, err := c.pairMarker(other, topic, by)
		if err != nil {
			return false, err
		}
		evs, err := c.ch.events("Commitments", "Committed", nil, []common.Hash{k})
		if err != nil {
			return false, err
		}
		if len(evs) == 0 {
			return false, nil
		}
	}
	return true, nil
}

// ---- commit-reveal: sealed bids, fair games, secret ballots

func (c *Client) commitValue(topic string, value []byte, revealBy uint64) (uint64, []byte, error) {
	preimage := append(randBytes(16), value...)
	r, err := c.ch.send(c.key, nil, "Commitments", "commit", c.id(), crypto.Keccak256Hash([]byte(topic)), crypto.Keccak256Hash(preimage), revealBy)
	if err != nil {
		return 0, nil, err
	}
	id, err := c.firstIndexed(r, "Commitments", "Committed")
	return id, preimage, err
}

// commitValueKept commits and keeps the preimage with the identity (what the CLI does).
func (c *Client) commitValueKept(topic, value string, revealBy uint64) (uint64, []byte, error) {
	id, pre, err := c.commitValue(topic, []byte(value), revealBy)
	if err != nil {
		return id, pre, err
	}
	if c.st.Secrets == nil {
		c.st.Secrets = map[string]string{}
	}
	c.st.Secrets[fmt.Sprint(id)] = hex.EncodeToString(pre)
	return id, pre, c.save()
}

func (c *Client) revealValue(id uint64, preimage []byte) error {
	_, err := c.ch.send(c.key, nil, "Commitments", "reveal", u(id), preimage)
	return err
}

// commitments lists every commitment under a topic: who committed, by when
// they must reveal, and what they revealed. Anyone can check a game or a
// sealed bid from this alone.
func (c *Client) commitments(topic string) ([]map[string]any, error) {
	th := crypto.Keccak256Hash([]byte(topic))
	cs, err := c.ch.events("Commitments", "Committed", nil, []common.Hash{th})
	if err != nil {
		return nil, err
	}
	rs, err := c.ch.events("Commitments", "Revealed", nil, []common.Hash{th})
	if err != nil {
		return nil, err
	}
	shown := map[string]string{}
	for _, r := range rs {
		p := r.Args["preimage"].([]byte)
		if len(p) >= 16 {
			shown[r.Args["id"].(*big.Int).String()] = string(p[16:])
		}
	}
	var out []map[string]any
	for _, e := range cs {
		id := e.Args["id"].(*big.Int).String()
		by, _ := c.res.nameOf(common.Hash(e.Args["by"].([32]byte)))
		m := map[string]any{"id": id, "by": by, "block": e.Block, "reveal_by": e.Args["revealBy"]}
		if v, ok := shown[id]; ok {
			m["revealed"] = v
		} else {
			m["revealed"] = nil
		}
		out = append(out, m)
	}
	return out, nil
}

// revealed lists the values revealed under a topic (the 16-byte nonce stripped).
func (c *Client) revealed(topic string) ([][]byte, error) {
	evs, err := c.ch.events("Commitments", "Revealed", nil, []common.Hash{crypto.Keccak256Hash([]byte(topic))})
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, e := range evs {
		p := e.Args["preimage"].([]byte)
		if len(p) < 16 {
			return nil, errors.New("short preimage")
		}
		out = append(out, p[16:])
	}
	return out, nil
}
