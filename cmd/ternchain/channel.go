// Channels between any two names: a Bank lock with VoucherCondition, opened
// once on chain; every payment after that is a voucher signed off chain for
// the new cumulative total, sealed to the payee as a tern.voucher datagram and
// pushed by the data node at once: instant, no gas. The payee claims the
// latest voucher on chain whenever it likes, before the lock's deadline.
//
//	channel open PEER AMOUNT [--hours N]   lock AMOUNT for PEER (guarded as a payment of AMOUNT)
//	channel pay PEER AMOUNT [MEMO]         a voucher for AMOUNT more (guarded as a payment of AMOUNT:
//	                                        free_pay's budget counts vouchers)
//	channel close PEER                     stop paying; after the deadline, take back what is left
//	channel claim [PEER]                   (payee) claim the latest vouchers on chain
//	channel list                           channels both ways
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const tVoucher = "tern.voucher"

// VoucherMsg: what the payee receives (amounts in wei).
type VoucherMsg struct {
	Lock       uint64 `json:"lock"`
	Cumulative string `json:"cumulative"`
	Amount     string `json:"amount,omitempty"` // what this voucher adds
	Sig        string `json:"sig,omitempty"`
	Memo       string `json:"memo,omitempty"`
	Close      bool   `json:"close,omitempty"` // the payer stops: claim before the deadline
}

// PeerChannel: our channel to a payee.
type PeerChannel struct {
	Lock        uint64 `json:"lock"`
	Total       string `json:"total"`      // wei
	Cumulative  string `json:"cumulative"` // wei vouched so far
	RefundAfter int64  `json:"refund_after"`
	Closed      bool   `json:"closed,omitempty"`
}

// InChannel: the best voucher we hold on a channel to us.
type InChannel struct {
	From        string `json:"from"`
	Total       string `json:"total"`
	Cumulative  string `json:"cumulative"`
	Sig         string `json:"sig"`
	Claimed     string `json:"claimed,omitempty"`
	RefundAfter int64  `json:"refund_after"`
	Closing     bool   `json:"closing,omitempty"`
}

// voucherNews: a voucher that paid us more (a channel-paid event).
type voucherNews struct {
	it               *Item
	lock             uint64
	from             string
	amount, cum, tot *big.Int
	closing          bool
	until            int64
}

func weiOf(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return v
}

func (c *Client) channelOpen(peer string, amount *big.Int, hours int) (map[string]any, error) {
	if peer == c.f.Name {
		return nil, errors.New("channel open: that peer is you; open a channel to another name")
	}
	if _, err := c.res.party(peer); err != nil {
		return nil, err
	}
	c.syncGuard()
	if pc := c.st.PeerChans[peer]; pc != nil && !pc.Closed && int64(pc.RefundAfter) > c.chainNow() {
		if rem, err := c.ch.call("Bank", "remaining", u(pc.Lock)); err == nil && rem[0].(*big.Int).Sign() > 0 {
			return nil, fmt.Errorf("a channel to %s is open (lock %d, %s of %s vouched): 'channel pay %s AMOUNT', or 'channel close %s' first",
				peer, pc.Lock, fmtCoins(weiOf(pc.Cumulative)), fmtCoins(weiOf(pc.Total)), peer, peer)
		}
	}
	if hours <= 0 {
		hours = 24
	}
	until := c.chainNow() + int64(hours)*3600
	id, err := c.openChannel(peer, amount, uint64(until)) // guarded: a payment of amount to peer
	if err != nil {
		return nil, err
	}
	err = c.locked(func() {
		if c.st.PeerChans == nil {
			c.st.PeerChans = map[string]*PeerChannel{}
		}
		c.st.PeerChans[peer] = &PeerChannel{Lock: id, Total: amount.String(), Cumulative: "0", RefundAfter: until}
	})
	return map[string]any{"channel": id, "to": peer, "total": fmtCoins(amount), "until": time.Unix(until, 0).UTC().Format(time.RFC3339),
		"next": "'channel pay " + peer + " AMOUNT': a signed voucher, instant, no gas; " + peer + " claims on chain before the deadline"}, err
}

// channelPay signs a voucher for amount more and sends it sealed to the peer.
func (c *Client) channelPay(peer string, amount *big.Int, memo string) (map[string]any, error) {
	if amount.Sign() <= 0 {
		return nil, errors.New("the amount must be above zero (coins, like 0.1)")
	}
	c.syncGuard()
	pc := c.st.PeerChans[peer]
	if pc == nil || pc.Closed {
		return nil, fmt.Errorf("no open channel to %s: 'channel open %s AMOUNT' first", peer, peer)
	}
	if left := pc.RefundAfter - c.chainNow(); left < 600 {
		return nil, fmt.Errorf("channel %d to %s ends in %d s, too soon for %s to claim: close it and open a new one", pc.Lock, peer, left, peer)
	}
	done, err := c.guardPay(peer, amount) // free_pay's budget counts vouchers; above it, an owner approval
	if err != nil {
		return nil, err
	}
	var next *big.Int
	var lock uint64
	err = c.locked(func() { // reserved under the state lock: two processes never sign the same total
		pc := c.st.PeerChans[peer]
		total := weiOf(pc.Total)
		n := new(big.Int).Add(weiOf(pc.Cumulative), amount)
		if n.Cmp(total) > 0 {
			return
		}
		next, lock, pc.Cumulative = n, pc.Lock, n.String()
	})
	if err == nil && next == nil {
		err = fmt.Errorf("channel %d to %s has %s left: close it and open a bigger one", pc.Lock, peer,
			fmtCoins(new(big.Int).Sub(weiOf(pc.Total), weiOf(pc.Cumulative))))
	}
	var id string
	if err == nil {
		var sig []byte
		if sig, err = c.voucher(lock, next); err == nil {
			id, err = c.sendDatagram(tVoucher, peer, VoucherMsg{Lock: lock, Cumulative: next.String(), Amount: amount.String(),
				Sig: hex.EncodeToString(sig), Memo: firstLine(memo, 120)})
		}
		if err != nil { // not delivered: give the total back, unless another payment went on meanwhile
			c.locked(func() {
				if pc := c.st.PeerChans[peer]; pc != nil && pc.Lock == lock && pc.Cumulative == next.String() {
					pc.Cumulative = new(big.Int).Sub(next, amount).String()
				}
			})
		}
	}
	done(err == nil)
	if err != nil {
		return nil, err
	}
	pc = c.st.PeerChans[peer]
	return map[string]any{"paid": fmtCoins(amount), "to": peer, "channel": lock, "total_paid": fmtCoins(next),
		"left": fmtCoins(new(big.Int).Sub(weiOf(pc.Total), next)), "voucher": id[:18],
		"gas": "none: a signed voucher, pushed to " + peer + " now; it claims on chain when it likes"}, nil
}

func (c *Client) channelClose(peer string) (map[string]any, error) {
	pc := c.st.PeerChans[peer]
	if pc == nil {
		return nil, fmt.Errorf("no channel to %s", peer)
	}
	rem, err := c.ch.call("Bank", "remaining", u(pc.Lock))
	if err != nil {
		return nil, err
	}
	left := rem[0].(*big.Int)
	now := c.chainNow()
	r := map[string]any{"channel": pc.Lock, "to": peer, "vouched": fmtCoins(weiOf(pc.Cumulative))}
	switch {
	case left.Sign() == 0:
		r["closed"] = "nothing left in it"
	case now > pc.RefundAfter:
		if err := c.refundLock(pc.Lock); err != nil {
			return nil, err
		}
		r["closed"], r["refunded"] = "refunded what the payee didn't claim", fmtCoins(left)
	default:
		if !pc.Closed {
			if _, err := c.sendDatagram(tVoucher, peer, VoucherMsg{Lock: pc.Lock, Cumulative: pc.Cumulative, Close: true}); err != nil {
				return nil, err
			}
		}
		r["closing"] = fmt.Sprintf("no more vouchers; %s claims until %s, then 'channel close %s' again refunds the rest (%s locked)",
			peer, time.Unix(pc.RefundAfter, 0).UTC().Format(time.RFC3339), peer, fmtCoins(left))
	}
	err = c.locked(func() {
		if p := c.st.PeerChans[peer]; p != nil && p.Lock == pc.Lock {
			p.Closed = true
			if left.Sign() == 0 || now > pc.RefundAfter {
				delete(c.st.PeerChans, peer)
			}
		}
	})
	return r, err
}

// channelClaim: the payee turns its latest vouchers into money (one transaction per channel).
func (c *Client) channelClaim(peer string) (map[string]any, error) {
	var done []map[string]any
	var errs []error
	for k, in := range c.st.InChans {
		if peer != "" && in.From != peer {
			continue
		}
		cum, claimed := weiOf(in.Cumulative), weiOf(in.Claimed)
		if cum.Cmp(claimed) <= 0 {
			continue
		}
		sig, _ := hex.DecodeString(in.Sig)
		if err := c.claimVoucher(weiOf(k).Uint64(), cum, sig); err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", k, err))
			continue
		}
		in.Claimed = in.Cumulative
		done = append(done, map[string]any{"channel": k, "from": in.From, "claimed": fmtCoins(new(big.Int).Sub(cum, claimed)), "total_claimed": fmtCoins(cum)})
	}
	if err := c.save(); err != nil {
		return nil, err
	}
	if len(done) == 0 && len(errs) == 0 {
		return map[string]any{"claimed": "nothing new"}, nil
	}
	return map[string]any{"claimed": done}, errors.Join(errs...)
}

func (c *Client) channelList() map[string]any {
	out, in := []map[string]any{}, []map[string]any{}
	for p, pc := range c.st.PeerChans {
		out = append(out, map[string]any{"to": p, "channel": pc.Lock, "total": fmtCoins(weiOf(pc.Total)), "vouched": fmtCoins(weiOf(pc.Cumulative)),
			"until": time.Unix(pc.RefundAfter, 0).UTC().Format(time.RFC3339), "closed": pc.Closed})
	}
	for k, x := range c.st.InChans {
		in = append(in, map[string]any{"from": x.From, "channel": k, "total": fmtCoins(weiOf(x.Total)), "vouched": fmtCoins(weiOf(x.Cumulative)),
			"claimed": fmtCoins(weiOf(x.Claimed)), "claim_before": time.Unix(x.RefundAfter, 0).UTC().Format(time.RFC3339), "closing": x.Closing})
	}
	return map[string]any{"paying": out, "paid_by": in}
}

// voucherLockFor: lock id is a channel (VoucherCondition) paying us.
func (c *Client) voucherLockFor(id uint64) bool {
	l, err := c.ch.call("Bank", "locks", u(id))
	return err == nil && common.Hash(l[2].([32]byte)) == idOf(c.f.Name) && l[7].(common.Address) == c.ch.addr("VoucherCondition")
}

// takeVoucher checks a voucher against its lock on chain (paying us, signed
// by the lock's signer, within its total) and keeps it if it pays more.
func (c *Client) takeVoucher(it *Item) (voucherNews, bool) {
	var v VoucherMsg
	if json.Unmarshal(it.Body, &v) != nil {
		return voucherNews{}, false
	}
	l, err := c.ch.call("Bank", "locks", u(v.Lock))
	if err != nil || common.Hash(l[2].([32]byte)) != idOf(c.f.Name) || l[7].(common.Address) != c.ch.addr("VoucherCondition") {
		return voucherNews{}, false
	}
	if l[9].(common.Address) != (common.Address{}) { // IP-6: amounts are coins; a channel in a (free-mint) token pays nothing here
		return voucherNews{}, false
	}
	key := fmt.Sprint(v.Lock)
	if c.st.InChans == nil {
		c.st.InChans = map[string]*InChannel{}
	}
	in := c.st.InChans[key]
	total, until := l[3].(*big.Int), int64(l[6].(uint64))
	from := c.nameOr(common.Hash(l[1].([32]byte)))
	if v.Close {
		if in != nil {
			in.Closing = true
		}
		return voucherNews{it: it, lock: v.Lock, from: from, closing: true, until: until, tot: total}, true
	}
	signer, err := abiArgsAddress().Unpack(l[8].([]byte))
	if err != nil {
		return voucherNews{}, false
	}
	cum := weiOf(v.Cumulative)
	sig, err := hex.DecodeString(v.Sig)
	if err != nil || len(sig) != 65 || cum.Sign() <= 0 {
		return voucherNews{}, false
	}
	d, err := c.ch.call("VoucherCondition", "voucherDigest", c.ch.addr("Bank"), u(v.Lock), cum)
	if err != nil {
		return voucherNews{}, false
	}
	digest := d[0].([32]byte)
	pub, err := crypto.SigToPub(digest[:], sig)
	if err != nil || crypto.PubkeyToAddress(*pub) != signer[0].(common.Address) {
		return voucherNews{}, false
	}
	if cum.Cmp(total) > 0 {
		cum = new(big.Int).Set(total) // the condition pays at most the lock
	}
	prev := new(big.Int)
	if in != nil {
		prev = weiOf(in.Cumulative)
	}
	if cum.Cmp(prev) <= 0 {
		return voucherNews{}, false // an older voucher: we hold a better one
	}
	if in == nil {
		in = &InChannel{Claimed: "0"}
		c.st.InChans[key] = in
	}
	in.From, in.Total, in.Cumulative, in.Sig, in.RefundAfter = from, total.String(), cum.String(), v.Sig, until
	return voucherNews{it: it, lock: v.Lock, from: from, amount: new(big.Int).Sub(cum, prev), cum: cum, tot: total, until: until}, true
}

func (n voucherNews) event() Event {
	ref := map[string]any{"lock": fmt.Sprint(n.lock), "from": n.from, "id": n.it.ID[:18], "wake": true}
	if n.it.Onchain != "" {
		ref["onchain"] = n.it.Onchain
	}
	before := time.Unix(n.until, 0).UTC().Format(time.RFC3339)
	if n.closing {
		return Event{"channel-closing", n.it.Block, fmt.Sprintf("%s closes channel %d to you: claim before %s ('channel claim %s')", n.from, n.lock, before, n.from), ref}
	}
	ref["amount"], ref["cumulative"] = fmtCoins(n.amount), fmtCoins(n.cum)
	return Event{"channel-paid", n.it.Block, fmt.Sprintf("%s paid you %s over channel %d (%s of %s in total; 'channel claim %s' collects it on chain, before %s)",
		n.from, fmtCoins(n.amount), n.lock, fmtCoins(n.cum), fmtCoins(n.tot), n.from, before), ref}
}
