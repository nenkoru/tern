// A data node's business: it keeps batches on disk, serves them, mirrors
// other nodes' batches, and is paid by the senders whose envelopes it posts.
//
// Pay per envelope through a payment channel (a Bank lock with
// VoucherCondition): the sender opens it once, every envelope carries a
// signed voucher for the new cumulative total, and the node claims what it
// has earned in one transaction now and then. No transaction per message.
//
// Mirrors copy other nodes' batches. A reader that can't reach a batch's
// poster fetches it from any node. A mirror that answers a challenge for
// someone else's batch earns the challenger's bond: holding data pays.
package main

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ---- storage: memory in front of a directory of <root>.batch files

func (r *Relay) put(root common.Hash, batch []byte) error {
	r.mu.Lock()
	if r.store == nil {
		r.store = map[common.Hash][]byte{}
	}
	r.store[root] = batch
	r.mu.Unlock()
	if r.dir == "" {
		return nil
	}
	p := filepath.Join(r.dir, root.Hex()+".batch")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, batch, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p) // atomic: a crash leaves the old file or the new one
}

func (r *Relay) get(root common.Hash) ([]byte, bool) {
	r.mu.Lock()
	b, ok := r.store[root]
	r.mu.Unlock()
	if ok || r.dir == "" {
		return b, ok
	}
	b, err := os.ReadFile(filepath.Join(r.dir, root.Hex()+".batch"))
	if err != nil || crypto.Keccak256Hash(b) != root {
		return nil, false
	}
	return b, true
}

// ---- what a node charges, and who it is

type NodeInfo struct {
	Name  string `json:"name"`  // the Tern name paid (empty: free node)
	Price string `json:"price"` // per envelope, in the smallest unit of Token (wei of the coin when empty)
	DA    bool   `json:"da"`
	Token string `json:"token,omitempty"` // the token it charges in (the network's settlement token); empty: the coin
}

func (r *Relay) info() NodeInfo {
	p := "0"
	if r.price != nil {
		p = r.price.String()
	}
	ni := NodeInfo{Name: r.name, Price: p, DA: r.da}
	if r.token != (common.Address{}) {
		ni.Token = r.token.Hex()
	}
	return ni
}

// chanInfo: what a channel lock says, as read lately.
type chanInfo struct {
	signer      common.Address
	total       *big.Int
	refunded    *big.Int
	refundAfter uint64
	paid        *big.Int // claimed on chain so far: no voucher at or below it pays anything (IP-7)
	at          time.Time
}

type voucherState struct {
	Cumulative  string `json:"cumulative"`
	Sig         string `json:"sig"`
	Claimed     string `json:"claimed"`
	RefundAfter uint64 `json:"refund_after,omitempty"` // when the payer can take the rest back (claim before)
}

// acceptVoucher checks that a voucher pays at least one more envelope.
func (r *Relay) acceptVoucher(lockS, cumS, sigS string) error {
	if r.price == nil || r.price.Sign() == 0 {
		return nil
	}
	if lockS == "" {
		return errPaymentRequired
	}
	lock, ok1 := new(big.Int).SetString(lockS, 10)
	cum, ok2 := new(big.Int).SetString(cumS, 10)
	sig, err := hex.DecodeString(strings.TrimPrefix(sigS, "0x"))
	if !ok1 || !ok2 || err != nil || len(sig) != 65 {
		return errors.New("bad voucher")
	}
	var ci *chanInfo
	if v, ok := r.lockInfo.Load(lock.String()); perfFixes && ok && time.Since(v.(*chanInfo).at) < time.Minute {
		ci = v.(*chanInfo) // a channel's payee, signer, total and end don't change; a refund is read again each minute
	} else {
		l, err := r.ch.call("Bank", "locks", lock)
		if err != nil {
			return err
		}
		if common.Hash(l[2].([32]byte)) != idOf(r.name) || l[7].(common.Address) != r.ch.addr("VoucherCondition") {
			return errors.New("that lock doesn't pay this node through a channel")
		}
		if l[9].(common.Address) != r.token { // IP-6: only what its price is in (the coin, or the network's settlement token): a free-mint token pays nothing
			return fmt.Errorf("that channel pays in %s: this node takes %s", r.ch.assetSymbol(l[9].(common.Address)), r.ch.assetSymbol(r.token))
		}
		signer, err := (abiArgsAddress()).Unpack(l[8].([]byte))
		if err != nil {
			return err
		}
		ci = &chanInfo{signer: signer[0].(common.Address), total: l[3].(*big.Int), paid: l[4].(*big.Int), refunded: l[5].(*big.Int), refundAfter: l[6].(uint64), at: time.Now()}
		r.lockInfo.Store(lock.String(), ci)
	}
	var digest [32]byte
	if perfFixes { // VoucherCondition.voucherDigest, computed here (chain id and condition are fixed): one call less per envelope
		inner, _ := argsOf("string", "uint256", "address", "address", "uint256", "uint256").Pack("tern-voucher", r.ch.chainID,
			r.ch.addr("VoucherCondition"), r.ch.addr("Bank"), lock, cum)
		digest = crypto.Keccak256Hash([]byte("\x19Ethereum Signed Message:\n32"), crypto.Keccak256(inner))
	} else {
		d, err := r.ch.call("VoucherCondition", "voucherDigest", r.ch.addr("Bank"), lock, cum)
		if err != nil {
			return err
		}
		digest = d[0].([32]byte)
	}
	pub, err := crypto.SigToPub(digest[:], sig)
	if err != nil || crypto.PubkeyToAddress(*pub) != ci.signer {
		return errors.New("voucher not signed by the channel's signer")
	}
	total, refunded, refundAfter := ci.total, ci.refunded, ci.refundAfter
	if cum.Cmp(total) > 0 {
		return errors.New("voucher exceeds the channel: open a new one")
	}
	// A channel the payer took back, or can take back soon, pays nothing: open a new one.
	if refunded.Sign() > 0 || int64(refundAfter) < chainTime(r.ch)+3600 {
		return errors.New("that channel is refunded or ends within the hour: open a new one")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.vouchers == nil {
		r.vouchers = map[string]voucherState{}
	}
	key := lock.String()
	last := new(big.Int)
	if v, ok := r.vouchers[key]; ok {
		last.SetString(v.Cumulative, 10)
	}
	if ci.paid != nil && ci.paid.Cmp(last) > 0 { // IP-7: what was claimed on chain, whatever the local file says (a restart, a lost file)
		last.Set(ci.paid)
	}
	if new(big.Int).Sub(cum, last).Cmp(r.price) < 0 {
		return fmt.Errorf("voucher must add at least %s (the price of one envelope, in the smallest unit of %s)", r.price, r.ch.assetSymbol(r.token))
	}
	claimed := "0"
	if v, ok := r.vouchers[key]; ok {
		claimed = v.Claimed
	}
	r.vouchers[key] = voucherState{Cumulative: cum.String(), Sig: hex.EncodeToString(sig), Claimed: claimed, RefundAfter: refundAfter}
	if perfFixes {
		r.vdirty = true // written once a second by Serve's ticker, not once per envelope (the file holds every channel)
	} else {
		r.saveVouchers()
	}
	return nil
}

func chainTime(ch *Chain) int64 {
	if h, err := ch.b.HeaderByNumber(ctxb(), nil); err == nil {
		return int64(h.Time)
	}
	return time.Now().Unix()
}

var errPaymentRequired = errors.New("payment required: send a voucher on a channel to this node")

func abiArgsAddress() abi.Arguments { return argsOf("address") }

func (r *Relay) saveVouchers() {
	if r.dir == "" {
		return
	}
	b, _ := json.Marshal(r.vouchers)
	os.WriteFile(filepath.Join(r.dir, "vouchers.json"), b, 0o600)
}

func (r *Relay) loadVouchers() {
	r.vouchers = map[string]voucherState{}
	if r.dir != "" {
		if b, err := os.ReadFile(filepath.Join(r.dir, "vouchers.json")); err == nil {
			json.Unmarshal(b, &r.vouchers)
		}
	}
}

// claimEarnings turns the latest voucher of every channel into money (anyone
// may claim for the payee; the node does it for itself).
func (r *Relay) claimEarnings() (*big.Int, error) { return r.claimEarningsIf(false) }

// claimEarningsIf: with due, only channels worth a transaction now (see claimDue).
func (r *Relay) claimEarningsIf(due bool) (*big.Int, error) {
	earned := new(big.Int)
	r.mu.Lock()
	todo := map[string]voucherState{}
	now := uint64(chainTime(r.ch))
	for k, v := range r.vouchers {
		if v.Cumulative == v.Claimed {
			continue
		}
		if due && r.price != nil && v.RefundAfter != 0 {
			// A claim costs a transaction: make one when it's worth claimDue
			// envelopes, or when the channel is about to end. (Before, every
			// channel with anything new was claimed each minute: thousands of
			// transactions a day, each worth one envelope.)
			cum, _ := new(big.Int).SetString(v.Cumulative, 10)
			cl, _ := new(big.Int).SetString(v.Claimed, 10)
			due := new(big.Int).Mul(r.price, big.NewInt(claimDue))
			if new(big.Int).Sub(cum, cl).Cmp(due) < 0 && v.RefundAfter > now+86400 {
				continue
			}
		}
		todo[k] = v
	}
	r.mu.Unlock()
	var errs []error
	for k, v := range todo { // one dead channel doesn't stop the others
		lock, _ := new(big.Int).SetString(k, 10)
		cum, _ := new(big.Int).SetString(v.Cumulative, 10)
		claimed, _ := new(big.Int).SetString(v.Claimed, 10)
		sig, _ := hex.DecodeString(v.Sig)
		f, err := argsOf("uint256", "bytes").Pack(cum, sig)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := r.ch.send(r.key, nil, "Bank", "claim", lock, f); err != nil {
			errs = append(errs, fmt.Errorf("claim on channel %s: %w", k, err))
			continue
		}
		earned.Add(earned, new(big.Int).Sub(cum, claimed))
		r.mu.Lock()
		now := r.vouchers[k] // a newer voucher may have come meanwhile: keep it
		now.Claimed = v.Cumulative
		r.vouchers[k] = now
		r.saveVouchers()
		r.mu.Unlock()
	}
	return earned, errors.Join(errs...)
}

// ---- mirroring other nodes' batches

func (r *Relay) mirror() (int, error) {
	evs, err := r.ch.eventsFrom(r.mirrored, "Inbox", "Committed")
	if err != nil {
		return 0, err
	}
	me := crypto.PubkeyToAddress(r.key.PublicKey)
	urls := stakedURLs(r.ch)
	n, failed, deadline := 0, map[string]bool{}, time.Now().Add(daBudget)
	for _, ev := range evs {
		poster := ev.Args["poster"].(common.Address)
		root := common.Hash(ev.Args["root"].([32]byte))
		if poster == me {
			continue
		}
		if _, ok := r.get(root); ok {
			continue
		}
		if !time.Now().Before(deadline) {
			return n, nil // the rest next time
		}
		if failed[urls[poster]] {
			// an unreachable node: skip it this pass
		} else if b, err := fetchBatch(urls[poster], root); err != nil {
			failed[urls[poster]] = true
		} else {
			if err := r.put(root, b); err != nil {
				return n, err
			}
			n++
		}
		if ev.Block >= r.mirrored {
			r.mirrored = ev.Block + 1
		}
	}
	return n, nil
}

// ---- the client side: paying a node with vouchers

// NodeChannel is a sender's open channel to one node.
type NodeChannel struct {
	Lock       uint64 `json:"lock"`
	Total      string `json:"total"`
	Cumulative string `json:"cumulative"`
}

// nodeInfos: what nodes said about themselves lately (payNode asks before
// every envelope; a minute-old answer is good enough for the price and name).
var nodeInfos sync.Map // url => nodeInfoAt

type nodeInfoAt struct {
	ni NodeInfo
	at time.Time
}

func nodeInfo(url string) (NodeInfo, error) {
	if perfFixes {
		if v, ok := nodeInfos.Load(url); ok && time.Since(v.(nodeInfoAt).at) < time.Minute {
			return v.(nodeInfoAt).ni, nil
		}
		ni, err := nodeInfoFetch(url)
		if err == nil {
			nodeInfos.Store(url, nodeInfoAt{ni, time.Now()})
		}
		return ni, err
	}
	return nodeInfoFetch(url)
}

func nodeInfoFetch(url string) (NodeInfo, error) {
	var ni NodeInfo
	resp, err := httpc.Get(strings.TrimRight(url, "/") + "/info")
	if err != nil {
		return ni, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ni, fmt.Errorf("node info: %s", resp.Status)
	}
	return ni, json.NewDecoder(resp.Body).Decode(&ni)
}

// claimDue: a channel's earnings are claimed once they pay this many envelopes (or the channel ends within a day).
const claimDue = 200

// channelEnvelopes: how many envelopes a new channel to a node prepays.
const channelEnvelopes = 1000

// payNode returns voucher headers for one more envelope to the node at url,
// opening (or reopening) a channel when needed.
func (c *Client) payNode(url string) (http.Header, error) {
	ni, err := nodeInfo(url)
	if err != nil {
		return nil, err
	}
	price, _ := new(big.Int).SetString(ni.Price, 10)
	if price == nil || price.Sign() == 0 {
		return nil, nil // a free node
	}
	if c.st.Channels == nil {
		c.st.Channels = map[string]NodeChannel{}
	}
	ch, ok := c.st.Channels[url]
	cum, total := new(big.Int), new(big.Int)
	if ok {
		cum.SetString(ch.Cumulative, 10)
		total.SetString(ch.Total, 10)
	}
	next := new(big.Int).Add(cum, price)
	if want := common.HexToAddress(ni.Token); want != c.asset { // the node's token, whatever this client pays in otherwise
		if ni.Token != "" && want != c.ch.settleToken() {
			return nil, fmt.Errorf("data node %s charges in %s, not this network's settlement token", url, ni.Token)
		}
		was := c.asset
		c.asset = want
		defer func() { c.asset = was }()
	}
	if !ok || next.Cmp(total) > 0 {
		amount := new(big.Int).Mul(price, big.NewInt(channelEnvelopes))
		open := c.openChannel
		if strings.TrimRight(url, "/") == strings.TrimRight(c.relay, "/") {
			open = c.openRelayChannel // the relay this client was set up with
		}
		lock, err := open(ni.Name, amount, uint64(c.chainNow())+30*86400)
		if err != nil {
			return nil, fmt.Errorf("opening a channel to data node %s: %w", ni.Name, err)
		}
		ch = NodeChannel{Lock: lock, Total: amount.String(), Cumulative: "0"}
		next = new(big.Int).Set(price)
	}
	sig, err := c.voucher(ch.Lock, next)
	if err != nil {
		return nil, err
	}
	ch.Cumulative = next.String()
	c.st.Channels[url] = ch
	if err := c.save(); err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("X-Voucher-Lock", fmt.Sprint(ch.Lock))
	h.Set("X-Voucher-Cumulative", next.String())
	h.Set("X-Voucher-Sig", hex.EncodeToString(sig))
	return h, nil
}

// newDataNode: a relayer that is a data node. dir keeps batches and earned
// vouchers across restarts; name and price make senders pay it.
func newDataNode(ch *Chain, key *ecdsa.PrivateKey, dir, name string, price *big.Int, mirrors bool) (*Relay, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if price != nil && price.Sign() > 0 && name == "" {
		return nil, errors.New("a paid node needs a Tern name to be paid to (--dir of an identity)")
	}
	r := &Relay{ch: ch, key: key, da: true, store: map[common.Hash][]byte{}, dir: dir, name: name, price: price, mirrors: mirrors}
	r.loadVouchers()
	r.loadQueue()
	r.loadWhere()
	r.loadClaims() // claims against its receipts: from where the last run stopped, or from the start (IP-3)
	if h, err := ch.b.BlockNumber(ctxb()); err == nil {
		r.watched = h
	}
	return r, nil
}
