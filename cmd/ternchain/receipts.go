// Receipts: signed counters of anything done or used for someone, "N units
// for X in session S": tokens, GB stored, hours hosted, km driven, uses of
// a tool, calls on an API key. Like a channel's
// vouchers, a receipt is cumulative within its session: a newer one with a
// higher count replaces the last, so one receipt per session is all anyone
// keeps.
//
//	provider  receipt issue CONSUMER COUNT --session S --unit token [--resource R] [--group G] [--unit-price 0.002/1000] [--work H]
//	consumer  receipt accept RECEIPT --observed N   countersign, if N (what its own side counted) covers the count
//	provider  receipt accept RECEIPT                keep the countersigned copy (the evidence)
//	consumer  receipt settle PROVIDER [SESSION]     pay what the countersigned receipts owe and is unpaid, by channel voucher
//	          receipt verify RECEIPT                anyone: both signatures, by keys that speak for the names now
//	          receipt list                          what this identity holds: per session, counted, settled, owed
//
// A receipt the consumer countersigned is the record both accept: it pays
// through the channel the consumer has open to the provider (a voucher, no
// gas: channel.go), or, in a group that runs on credits, it is posted to the
// group (credits post) and nobody pays at all. The consumer signs only what
// its own side counted (--observed): a provider can't bill more than that.
package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const tReceipt = "tern.receipt/1"

type Usage struct {
	Schema      string                     `json:"schema"`
	Provider    string                     `json:"provider"`
	Consumer    string                     `json:"consumer"`
	Session     string                     `json:"session"` // 0x…, 32 bytes: one run of work
	Unit        string                     `json:"unit"`
	Count       uint64                     `json:"count"` // cumulative within the session
	Resource    string                     `json:"resource,omitempty"`
	Group       string                     `json:"group,omitempty"`
	Price       *OfferPrice                `json:"price,omitempty"` // the agreed terms: coins per N units
	Work        string                     `json:"work,omitempty"`  // 0x…: a commitment to the work's results (spotcheck)
	At          int64                      `json:"at"`
	Ext         map[string]json.RawMessage `json:"ext,omitempty"` // what this version doesn\'t name (receipt/NAME), signed with it
	ProviderSig string                     `json:"provider_sig,omitempty"`
	ConsumerSig string                     `json:"consumer_sig,omitempty"`
}

// body: what both parties sign (the receipt without signatures).
func (u Usage) body() []byte {
	u.ProviderSig, u.ConsumerSig = "", ""
	return jsonOf(u)
}

// key: the session a receipt belongs to.
func (u Usage) key() string { return u.Provider + "|" + u.Consumer + "|" + strings.ToLower(u.Session) }

// terms: what may not change within a session.
func (u Usage) terms() string {
	u.Count, u.At, u.Work, u.ProviderSig, u.ConsumerSig = 0, 0, "", "", ""
	return string(jsonOf(u))
}

var unitRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func (u Usage) check() error {
	switch {
	case u.Schema != tReceipt:
		return errors.New("not a tern.receipt/1 receipt")
	case u.Provider == "" || u.Consumer == "" || u.Provider == u.Consumer:
		return errors.New("a receipt names a provider and a different consumer")
	case !regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`).MatchString(u.Session):
		return errors.New("session: 0x and 32 bytes of hex")
	case !unitRE.MatchString(u.Unit):
		return errors.New("unit: a lower-case word (token, piece, gb, minute)")
	case u.Count == 0:
		return errors.New("count: above 0")
	case len(u.Resource) > 120 || len(u.Group) > 81:
		return errors.New("resource or group too long")
	case u.Work != "" && !regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`).MatchString(u.Work):
		return errors.New("work: 0x and 32 bytes of hex")
	}
	if p := u.Price; p != nil {
		if _, err := parseCoins(p.Coins); err != nil || p.Per < 1 || p.Per != float64(uint64(p.Per)) || p.Unit != u.Unit {
			return errors.New("price: coins per a whole number of the receipt's units")
		}
	}
	return nil
}

// owed: what count units cost at the receipt's price, in wei (rounded down).
func (u Usage) owed(count uint64) *big.Int {
	if u.Price == nil || count == 0 {
		return new(big.Int)
	}
	c, _ := parseCoins(u.Price.Coins)
	n := new(big.Int).Mul(c, new(big.Int).SetUint64(count))
	return n.Div(n, new(big.Int).SetUint64(uint64(u.Price.Per)))
}

// parseUnitPrice: "0.002/1000" (coins per 1000 units).
func parseUnitPrice(s, unit string) (*OfferPrice, error) {
	if s == "" {
		return nil, nil
	}
	coins, per, ok := strings.Cut(s, "/")
	if !ok {
		per = "1"
	}
	n, err := strconv.ParseUint(per, 10, 64)
	if _, e := parseCoins(coins); e != nil || err != nil || n == 0 {
		return nil, errors.New("--unit-price: COINS/N, like 0.002/1000 (coins per 1000 units)")
	}
	return &OfferPrice{Coins: coins, Per: float64(n), Unit: unit}, nil
}

// ReceiptRec: a session's best receipt as this identity holds it.
type ReceiptRec struct {
	Usage   Usage  `json:"usage"`
	Settled uint64 `json:"settled,omitempty"` // units paid for (consumer side)
	Paid    string `json:"paid,omitempty"`    // coins paid for them
}

type receiptBook struct {
	Sessions map[string]*ReceiptRec `json:"sessions"`
}

func (c *Client) receiptsPath() string { return filepath.Join(c.dir, "receipts.json") }

// receiptDo runs f on the book under the state lock and saves it.
func (c *Client) receiptDo(f func(b *receiptBook) error) error {
	var ferr error
	err := c.locked(func() {
		var b receiptBook
		readJSON(c.receiptsPath(), &b)
		if b.Sessions == nil {
			b.Sessions = map[string]*ReceiptRec{}
		}
		if ferr = f(&b); ferr == nil {
			ferr = writeJSON(c.receiptsPath(), b)
		}
	})
	if err != nil {
		return err
	}
	return ferr
}

func newSession() string {
	b := make([]byte, 32)
	rand.Read(b)
	return common.BytesToHash(b).Hex()
}

// receiptIssue: the provider signs a receipt for count units so far.
func (c *Client) receiptIssue(u Usage) (Usage, error) {
	u.Schema, u.Provider, u.At = tReceipt, c.f.Name, time.Now().Unix()
	if u.Session == "" {
		u.Session = newSession()
	}
	if err := u.check(); err != nil {
		return u, err
	}
	if _, err := c.res.nameOf(idOf(u.Consumer)); err != nil {
		return u, fmt.Errorf("no such consumer %q", u.Consumer)
	}
	err := c.receiptDo(func(b *receiptBook) error {
		if r := b.Sessions[u.key()]; r != nil {
			if r.Usage.terms() != u.terms() {
				return fmt.Errorf("session %s was issued on other terms: start a new session", u.Session)
			}
			if u.Count < r.Usage.Count {
				return fmt.Errorf("session %s already counts %d: a receipt only grows", u.Session, r.Usage.Count)
			}
		}
		var err error
		if u.ProviderSig, err = c.coopSign(tReceipt, u.body()); err != nil {
			return err
		}
		b.Sessions[u.key()] = &ReceiptRec{Usage: u}
		return nil
	})
	return u, err
}

// receiptVerify: well formed, and each signature present is by a key that
// speaks for its name now. both: the consumer's must be there too.
func (c *Client) receiptVerify(u Usage, both bool) error { return c.receiptVerifyAt(u, both, 0) }

// receiptVerifyAt: the same at block b (0: now): a receipt posted on chain is
// checked at the block that holds it.
func (c *Client) receiptVerifyAt(u Usage, both bool, b uint64) error {
	if err := u.check(); err != nil {
		return err
	}
	if u.ProviderSig == "" {
		return errors.New("not signed by the provider")
	}
	if err := c.coopVerifyAt(u.Provider, tReceipt, u.body(), u.ProviderSig, b); err != nil {
		return fmt.Errorf("provider's signature: %v", err)
	}
	if u.ConsumerSig == "" {
		if both {
			return errors.New("not countersigned by the consumer")
		}
		return nil
	}
	if err := c.coopVerifyAt(u.Consumer, tReceipt, u.body(), u.ConsumerSig, b); err != nil {
		return fmt.Errorf("consumer's signature: %v", err)
	}
	return nil
}

// receiptAccept: as the consumer, countersign a receipt whose count our own
// side observed; as the provider, keep a countersigned copy.
func (c *Client) receiptAccept(u Usage, observed uint64) (Usage, error) {
	switch c.f.Name {
	case u.Consumer:
		if err := c.receiptVerify(u, false); err != nil {
			return u, err
		}
		if observed == 0 {
			return u, errors.New("--observed N: what your own side counted (the engine's log); a receipt above it is refused")
		}
		if u.Count > observed {
			return u, fmt.Errorf("the receipt counts %d %s, your side counted %d: not signing (ask the provider for a receipt of %d)",
				u.Count, u.Unit, observed, observed)
		}
		err := c.receiptDo(func(b *receiptBook) error {
			r := b.Sessions[u.key()]
			if r != nil && r.Usage.terms() != u.terms() {
				return fmt.Errorf("session %s changed its terms: refusing (the first receipt's terms hold)", u.Session)
			}
			if r != nil && u.Count < r.Usage.Count {
				return fmt.Errorf("you already signed %d for session %s", r.Usage.Count, u.Session)
			}
			var err error
			if u.ConsumerSig, err = c.coopSign(tReceipt, u.body()); err != nil {
				return err
			}
			if r == nil {
				r = &ReceiptRec{}
				b.Sessions[u.key()] = r
			}
			r.Usage = u
			return nil
		})
		return u, err
	case u.Provider:
		if err := c.receiptVerify(u, true); err != nil {
			return u, err
		}
		err := c.receiptDo(func(b *receiptBook) error {
			if r := b.Sessions[u.key()]; r == nil || r.Usage.terms() != u.terms() {
				return errors.New("not a session you issued")
			} else if u.Count >= r.Usage.Count || r.Usage.ConsumerSig == "" {
				r.Usage = u
			}
			return nil
		})
		return u, err
	}
	return u, fmt.Errorf("the receipt is between %s and %s, not you", u.Provider, u.Consumer)
}

// receiptSettle: as the consumer, pay provider what its countersigned
// receipts owe beyond what was paid, one voucher per session.
func (c *Client) receiptSettle(provider, session string) ([]map[string]any, error) {
	var due []*ReceiptRec
	c.receiptDo(func(b *receiptBook) error {
		for _, r := range b.Sessions {
			u := r.Usage
			if u.Consumer == c.f.Name && u.Provider == provider && u.ConsumerSig != "" && u.Count > r.Settled &&
				(session == "" || strings.EqualFold(u.Session, session)) {
				x := *r
				due = append(due, &x)
			}
		}
		return nil
	})
	if len(due) == 0 {
		return nil, fmt.Errorf("nothing owed to %s: no countersigned receipt with unpaid units", provider)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].Usage.Session < due[j].Usage.Session })
	var out []map[string]any
	for _, r := range due {
		u := r.Usage
		if u.Price == nil {
			out = append(out, map[string]any{"session": u.Session, "skipped": "no price: settle it in credits (credits post)"})
			continue
		}
		amount := new(big.Int).Sub(u.owed(u.Count), u.owed(r.Settled))
		if amount.Sign() <= 0 {
			continue
		}
		res, err := c.channelPay(provider, amount, fmt.Sprintf("usage %s %d %s", u.Session[:10], u.Count, u.Unit))
		if err != nil {
			return out, err
		}
		c.receiptDo(func(b *receiptBook) error {
			if x := b.Sessions[u.key()]; x != nil && x.Settled < u.Count {
				x.Settled, x.Paid = u.Count, fmtCoins(u.owed(u.Count))
			}
			return nil
		})
		out = append(out, map[string]any{"session": u.Session, "units": u.Count - r.Settled, "unit": u.Unit, "paid": res["paid"], "voucher": res["voucher"]})
	}
	return out, nil
}

func (c *Client) receiptList() []map[string]any {
	var b receiptBook
	readJSON(c.receiptsPath(), &b)
	out := []map[string]any{}
	for _, r := range b.Sessions {
		u := r.Usage
		row := map[string]any{"session": u.Session, "provider": u.Provider, "consumer": u.Consumer, "count": u.Count, "unit": u.Unit,
			"countersigned": u.ConsumerSig != ""}
		if u.Group != "" {
			row["group"] = u.Group
		}
		if u.Price != nil {
			row["owed"] = fmtCoins(new(big.Int).Sub(u.owed(u.Count), u.owed(r.Settled)))
			row["paid"] = fmtCoins(u.owed(r.Settled))
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["session"].(string) < out[j]["session"].(string) })
	return out
}

func parseReceipt(arg string) (Usage, error) {
	var u Usage
	raw, err := coopInput(arg)
	if err != nil {
		return u, err
	}
	if err := jsonStrict(raw, &u); err != nil {
		return u, fmt.Errorf("not a tern.receipt/1 receipt: %v", err)
	}
	return u, nil
}

func runReceipt(c *Client, cmd string, pos []string, f coopFlags) error {
	switch cmd {
	case "receipt issue":
		if len(pos) < 2 {
			return errors.New("usage: receipt issue CONSUMER COUNT --unit token [--session S] [--resource R] [--group G] [--unit-price 0.002/1000] [--work H]")
		}
		n, err := strconv.ParseUint(pos[1], 10, 64)
		if err != nil {
			return errors.New("COUNT: a whole number of units, cumulative within the session")
		}
		price, err := parseUnitPrice(f.unitPrice, f.unit)
		if err != nil {
			return err
		}
		u, err := c.receiptIssue(Usage{Consumer: pos[0], Count: n, Unit: f.unit, Session: f.session, Resource: f.resource,
			Group: f.group, Price: price, Work: f.work})
		if err != nil {
			return err
		}
		out(u)
		return nil
	case "receipt accept", "receipt verify":
		if len(pos) < 1 {
			return fmt.Errorf("usage: %s RECEIPT (JSON, @FILE or -)", cmd)
		}
		u, err := parseReceipt(pos[0])
		if err != nil {
			return err
		}
		if cmd == "receipt verify" {
			if err := c.receiptVerify(u, false); err != nil {
				return err
			}
			out(map[string]any{"valid": true, "provider": u.Provider, "consumer": u.Consumer, "count": u.Count, "unit": u.Unit,
				"countersigned": u.ConsumerSig != ""})
			return nil
		}
		if u, err = c.receiptAccept(u, f.observed); err != nil {
			return err
		}
		out(u)
		return nil
	case "receipt settle":
		if len(pos) < 1 {
			return errors.New("usage: receipt settle PROVIDER [SESSION]")
		}
		res, err := c.receiptSettle(pos[0], firstOr(pos, 1, ""))
		if res != nil {
			out(map[string]any{"settled": res})
		}
		return err
	case "receipt list":
		out(map[string]any{"sessions": c.receiptList()})
		return nil
	}
	return unknownCommand(cmd)
}
