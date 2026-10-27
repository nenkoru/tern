// Hash locks (HTLC, as BIP-199) and all-or-nothing crowdfunding: two
// standard conditions for Bank locks, and their recipes.
//
//	htlc open PAYEE AMOUNT HASH|--secret-file F --hours N   lock + HashlockCondition
//	htlc claim LOCK PREIMAGE                              reveal (an event) + claim, one tx
//	fund create|pledge|show|collect|refund                lock + PledgeCondition, registered
//
// An atomic swap across two networks is two hash locks on one hash: the
// side that knows the secret locks first with the longer timeout; the other
// locks back with a shorter one; claiming on one network publishes the
// preimage, which the other side reads and uses on the other.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	algoSHA256 = 0
	algoKeccak = 1
)

func algoOf(s string) (uint8, error) {
	switch s {
	case "", "sha256":
		return algoSHA256, nil
	case "keccak", "keccak256":
		return algoKeccak, nil
	}
	return 0, fmt.Errorf("algo %q: sha256 or keccak", s)
}

func algoName(a uint8) string {
	if a == algoKeccak {
		return "keccak256"
	}
	return "sha256"
}

func hashlock(preimage []byte, algo uint8) common.Hash {
	if algo == algoKeccak {
		return crypto.Keccak256Hash(preimage)
	}
	return sha256.Sum256(preimage)
}

// hex32: 32 bytes as 0x… hex, or the contents of a file holding that.
func hex32(s, what string) ([]byte, error) {
	if b, err := os.ReadFile(s); err == nil {
		s = strings.TrimSpace(string(b))
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("%s: 32 bytes as 0x… hex (64 hex digits)", what)
	}
	return b, nil
}

// newSecret writes a fresh 32-byte secret to path (0600, refusing to
// overwrite one) and returns it.
func newSecret(path string) ([]byte, error) {
	s := make([]byte, 32)
	if _, err := rand.Read(s); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("secret file: %w (it is never overwritten)", err)
	}
	defer f.Close()
	_, err = f.WriteString("0x" + hex.EncodeToString(s) + "\n")
	return s, err
}

func hashlockDemand(h common.Hash, algo uint8) []byte {
	d, _ := argsOf("bytes32", "uint8").Pack([32]byte(h), algo)
	return d
}

func (c *Client) htlcOpen(payee string, amount *big.Int, h common.Hash, algo uint8, refundAfter uint64) (uint64, error) {
	if payee == "" {
		return 0, errors.New("a hash lock needs a payee")
	}
	if p, _ := c.revealedPreimage(h); p != nil { // MO-13: anyone can claim a lock on a public preimage
		return 0, fmt.Errorf("the preimage of %s is already public on this network: a lock on it is anyone's to claim; use a fresh secret", h.Hex())
	}
	return c.lock(payee, amount, refundAfter, "HashlockCondition", hashlockDemand(h, algo))
}

// claimMargin: a claim reveals the preimage in the mempool; it is sent only
// with this much time left, so it can't be refunded before it lands while
// the secret is out.
const claimMargin = 300

// htlcTerms: what a swap's counterparty promised, checked on its lock
// before the preimage is revealed (audit 4 MO-1, MO-8).
type htlcTerms struct {
	Amount  *big.Int       // at least this must be in the lock (required)
	Token   common.Address // the asset: zero = the network coin
	Against *htlcLock      // our own leg of the swap on the other network, if known
}

// htlcCheck: a hash lock is what the terms say, and it pays us.
func (c *Client) htlcCheck(id uint64, l *htlcLock, t htlcTerms) error {
	if l.PayeeID != c.id() && l.PayeeID != idOf(c.f.Owner) {
		return fmt.Errorf("hash lock %d pays %s, not you: claiming it would publish your secret and pay someone else; refuse it", id, l.Payee)
	}
	if l.Token != t.Token {
		what := "the network coin"
		if t.Token != (common.Address{}) {
			what = t.Token.Hex()
		}
		return fmt.Errorf("hash lock %d holds token %s, not %s: refuse it (--token NAME if that is the deal)", id, l.Token.Hex(), what)
	}
	if t.Amount == nil {
		return fmt.Errorf("hash lock %d holds %s: say what the deal is (--expect AMOUNT) before revealing the secret", id, fmtCoins(l.Remaining))
	}
	if l.Remaining.Sign() == 0 || l.Remaining.Cmp(t.Amount) < 0 || l.Paid.Sign() > 0 {
		return fmt.Errorf("hash lock %d holds %s, less than the %s agreed (or it was already claimed or refunded): refuse it",
			id, fmtCoins(l.Remaining), fmtCoins(t.Amount))
	}
	if now := c.chainNowU(); now+claimMargin > l.RefundAfter {
		return fmt.Errorf("lock %d is at or past its refund time (%d s left, under %d s): only its payer can take it back now; revealing the secret would only help the other side",
			id, int64(l.RefundAfter)-int64(now), claimMargin)
	}
	if o := t.Against; o != nil {
		if o.Hash != l.Hash {
			return fmt.Errorf("your own leg is on hash %s, lock %d on %s: not the same swap", o.Hash.Hex(), id, l.Hash.Hex())
		}
		if ownerName(o.Payee) != ownerName(l.Payer) {
			return fmt.Errorf("your own leg pays %s, but lock %d is from %s: not the same counterparty", o.Payee, id, l.Payer)
		}
	}
	return nil
}

// htlcCheckLeg: before locking back, the counterparty's lock (their leg, on
// the other network) pays us what was agreed, on the same hash, and stays
// claimable for us long enough after ours times out (audit 4 MO-8).
func (c *Client) htlcCheckLeg(their *htlcLock, payee string, h common.Hash, refundAfter uint64, t htlcTerms, me string) error {
	if their.Hash != h {
		return fmt.Errorf("their lock is on hash %s, not %s", their.Hash.Hex(), h.Hex())
	}
	if ownerName(their.Payee) != ownerName(me) {
		return fmt.Errorf("their lock pays %s, not you", their.Payee)
	}
	if ownerName(their.Payer) != ownerName(payee) {
		return fmt.Errorf("their lock is from %s, but you lock back to %s", their.Payer, payee)
	}
	if their.Token != t.Token {
		return fmt.Errorf("their lock holds token %s, not the agreed asset", their.Token.Hex())
	}
	if t.Amount == nil || their.Remaining.Cmp(t.Amount) < 0 || their.Paid.Sign() > 0 {
		return fmt.Errorf("their lock holds %s, not the %v agreed (--expect AMOUNT)", fmtCoins(their.Remaining), t.Amount)
	}
	if their.RefundAfter < refundAfter+3600 {
		return fmt.Errorf("their lock times out at %d, not at least an hour after yours (%d): once they claim yours you need time to claim theirs; lock back with a shorter --hours",
			their.RefundAfter, refundAfter)
	}
	return nil
}

// htlcLock: a hash lock's terms.
type htlcLock struct {
	Payer, Payee     string
	PayeeID          common.Hash
	Token            common.Address // zero: the network coin
	Total, Remaining *big.Int
	Paid             *big.Int
	RefundAfter      uint64
	Hash             common.Hash
	Algo             uint8
}

func (c *Client) htlc(id uint64) (*htlcLock, error) {
	l, err := c.ch.call("Bank", "locks", u(id))
	if err != nil {
		return nil, fmt.Errorf("no lock %d", id)
	}
	if c.conditionName(l[7].(common.Address)) != "HashlockCondition" {
		return nil, fmt.Errorf("lock %d is not a hash lock", id)
	}
	v, err := argsOf("bytes32", "uint8").Unpack(l[8].([]byte))
	if err != nil {
		return nil, errors.New("bad hash lock demand")
	}
	rem, _ := c.ch.call("Bank", "remaining", u(id))
	return &htlcLock{Payer: c.nameOrHex(common.Hash(l[0].([32]byte))), Payee: c.nameOrHex(common.Hash(l[2].([32]byte))),
		PayeeID: common.Hash(l[2].([32]byte)), Token: l[9].(common.Address),
		Total: l[3].(*big.Int), Paid: l[4].(*big.Int), Remaining: rem[0].(*big.Int), RefundAfter: l[6].(uint64),
		Hash: common.Hash(v[0].([32]byte)), Algo: v[1].(uint8)}, nil
}

// revealedPreimage: the preimage of h if anyone revealed it on this network.
func (c *Client) revealedPreimage(h common.Hash) ([]byte, *event) {
	for _, topics := range [][][]common.Hash{{{h}}, {nil, {h}}} {
		evs, err := c.ch.events("HashlockCondition", "Revealed", topics...)
		if err == nil && len(evs) > 0 {
			return evs[0].Args["preimage"].([]byte), &evs[0]
		}
	}
	return nil, nil
}

// htlcClaim reveals the preimage and claims, only once the lock is checked
// against the terms: payee, asset, amount, hash and time left (MO-1).
func (c *Client) htlcClaim(id uint64, preimage []byte, t htlcTerms) error {
	l, err := c.htlc(id)
	if err != nil {
		return err
	}
	if hashlock(preimage, l.Algo) != l.Hash {
		return fmt.Errorf("wrong preimage: its %s is not lock %d's hash %s", algoName(l.Algo), id, l.Hash.Hex())
	}
	if err := c.htlcCheck(id, l, t); err != nil {
		return err
	}
	_, err = c.ch.send(c.key, nil, "HashlockCondition", "claim", u(id), preimage)
	return err
}

func (c *Client) htlcView(id uint64) (map[string]any, error) {
	l, err := c.htlc(id)
	if err != nil {
		return nil, err
	}
	now := c.chainNowU()
	m := map[string]any{"lock": id, "payer": l.Payer, "payee": l.Payee, "total": fmtCoins(l.Total), "remaining": fmtCoins(l.Remaining),
		"hash": l.Hash.Hex(), "algo": algoName(l.Algo), "refund_after": l.RefundAfter, "asset": c.assetName(l.Token),
		"refund_after_utc": time.Unix(int64(l.RefundAfter), 0).UTC().Format(time.RFC3339), "chain_id": c.ch.chainID.Int64()}
	switch {
	case l.Paid.Sign() > 0:
		m["state"] = "claimed"
	case l.Remaining.Sign() == 0:
		m["state"] = "refunded"
	case now > l.RefundAfter:
		m["state"] = "expired: 'lock refund " + fmt.Sprint(id) + "' returns it to the payer"
	default:
		m["state"], m["claimable_for_s"] = "open", l.RefundAfter-now
	}
	if p, ev := c.revealedPreimage(l.Hash); p != nil {
		m["preimage"], m["revealed_in_tx"], m["revealed_at_block"] = "0x"+hex.EncodeToString(p), ev.Tx.Hex(), ev.Block
	}
	return m, nil
}

type htlcFlags struct {
	hours      int
	minutes    int
	secretFile string
	algo       string
	expect     string // the amount the other side's lock must hold
	against    string // NET.json:LOCK, the other leg of the swap
}

// terms: --expect and --token, and --against's lock read from its network.
func (f htlcFlags) terms(c *Client) (htlcTerms, error) {
	t := htlcTerms{Token: c.asset}
	if f.expect != "" {
		a, err := parseCoins(f.expect)
		if err != nil {
			return t, err
		}
		t.Amount = a
	}
	if f.against != "" {
		i := strings.LastIndex(f.against, ":")
		if i <= 0 {
			return t, errors.New("--against NET.json:LOCK (the other leg, on its network)")
		}
		netf, lock := f.against[:i], f.against[i+1:]
		ch, _, err := connect(netf)
		if err != nil {
			return t, err
		}
		o := &Client{Ident: c.Ident, ch: ch, res: newResolver(ch), st: &State{}}
		l, err := o.htlc(atoi(lock))
		if err != nil {
			return t, err
		}
		t.Against = l
	}
	return t, nil
}

// assetName: "coin" or the token's address.
func (c *Client) assetName(a common.Address) string {
	if a == (common.Address{}) {
		return "coin"
	}
	return a.Hex()
}

func runHTLC(c *Client, cmd string, pos []string, f htlcFlags) error {
	switch cmd {
	case "htlc open":
		usage := "htlc open PAYEE AMOUNT HASH|--secret-file FILE --hours N [--algo sha256|keccak]"
		if err := need(pos, 2, usage); err != nil {
			return err
		}
		amt, err := parseCoins(pos[1])
		if err != nil {
			return err
		}
		algo, err := algoOf(f.algo)
		if err != nil {
			return err
		}
		var h common.Hash
		switch {
		case len(pos) > 2:
			b, err := hex32(pos[2], "HASH")
			if err != nil {
				return err
			}
			h = common.BytesToHash(b)
		case f.secretFile != "":
			s, err := newSecret(f.secretFile)
			if err != nil {
				return err
			}
			h = hashlock(s, algo)
		default:
			return errors.New("usage: " + usage)
		}
		secs := uint64(max(f.hours, 0))*3600 + uint64(max(f.minutes, 0))*60
		if secs == 0 {
			return errors.New("--hours N: how long the payee has to claim (the side that knows the secret takes the longer timeout)")
		}
		if f.against != "" { // locking back: check their leg first (MO-8)
			t, err := f.terms(c)
			if err != nil {
				return err
			}
			if err := c.htlcCheckLeg(t.Against, pos[0], h, c.chainNowU()+secs, t, c.f.Name); err != nil {
				return fmt.Errorf("--against: %w", err)
			}
		}
		id, err := c.htlcOpen(pos[0], amt, h, algo, c.chainNowU()+secs)
		if err != nil {
			return err
		}
		v := map[string]any{"lock": id, "payee": pos[0], "amount": pos[1], "hash": h.Hex(), "algo": algoName(algo),
			"refund_after_s": secs, "payee_claims": fmt.Sprintf("htlc claim %d PREIMAGE", id), "payer_refunds": fmt.Sprintf("lock refund %d (after the timeout)", id)}
		if f.secretFile != "" {
			v["secret_in"] = f.secretFile + " (keep it: revealing it is what lets the payee claim)"
		}
		out(v)
	case "htlc claim":
		if err := need(pos, 2, "htlc claim LOCK PREIMAGE|FILE --expect AMOUNT [--token NAME] [--against NET.json:YOUR-LOCK]"); err != nil {
			return err
		}
		p, err := hex32(pos[1], "PREIMAGE")
		if err != nil {
			return err
		}
		t, err := f.terms(c)
		if err != nil {
			return err
		}
		if err := c.htlcClaim(atoi(pos[0]), p, t); err != nil {
			return err
		}
		v, _ := c.htlcView(atoi(pos[0]))
		out(v)
	case "htlc show":
		if err := need(pos, 1, "htlc show LOCK"); err != nil {
			return err
		}
		v, err := c.htlcView(atoi(pos[0]))
		if err != nil {
			return err
		}
		out(v)
	case "htlc preimage":
		if err := need(pos, 1, "htlc preimage HASH"); err != nil {
			return err
		}
		b, err := hex32(pos[0], "HASH")
		if err != nil {
			return err
		}
		p, ev := c.revealedPreimage(common.BytesToHash(b))
		if p == nil {
			return errors.New("not revealed on this network yet")
		}
		out(map[string]any{"hash": "0x" + hex.EncodeToString(b), "preimage": "0x" + hex.EncodeToString(p), "tx": ev.Tx.Hex(), "block": ev.Block})
	default:
		return unknownCommand(cmd)
	}
	return nil
}
